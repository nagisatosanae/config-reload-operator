/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ConfigReloadPolicyReconciler reconciles a ConfigReloadPolicy object
type ConfigReloadPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// conditionReady is the one condition this controller reports.
	conditionReady = "Ready"

	// The watcher reads these from its environment.
	envWatchDir = "WATCH_DIR"
	envSignal   = "WATCH_SIGNAL"
	envDebounce = "WATCH_DEBOUNCE_SECONDS"
	envValidate = "WATCH_VALIDATE_COMMAND"

	// retryAfter is how long to wait before looking again when something
	// the policy points at is missing or wrong.
	retryAfter = 30 * time.Second
	// finalizer marks a policy whose Deployment carries our env. The env is
	// taken off again before the policy is allowed to go.
	finalizer = "reload.nagisatosanae.github.io/revert"
)

// ownedEnv lists the env names this controller owns on the container. It
// sets them, and it removes the ones the policy no longer asks for. It
// never touches any other env.
var ownedEnv = []string{envWatchDir, envSignal, envDebounce, envValidate}

// Reasons shown in the Ready condition.
const (
	reasonConfigured          = "Configured"
	reasonWorkloadNotFound    = "WorkloadNotFound"
	reasonContainerNotFound   = "ContainerNotFound"
	reasonConfigMapNotMounted = "ConfigMapNotMounted"
	reasonSubPathNotSupported = "SubPathNotSupported"
)

// +kubebuilder:rbac:groups=reload.nagisatosanae.github.io,resources=configreloadpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=reload.nagisatosanae.github.io,resources=configreloadpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=reload.nagisatosanae.github.io,resources=configreloadpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch

// Reconcile makes the target Deployment match the policy.
//
// It finds the container named in the policy, finds where that container
// mounts the ConfigMap, and sets env on the container so the watcher knows
// which directory to watch, which signal to send and how long to wait. It
// writes nothing when everything is already right, so it is safe to run
// again and again. When the policy is deleted, the env is taken off again.
func (r *ConfigReloadPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Read the policy. If it is gone, there is nothing to do.
	var policy reloadv1alpha1.ConfigReloadPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. A policy on its way out takes its env off the container first.
	// Then the finalizer is removed and the policy is let go.
	if !policy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finish(ctx, &policy)
	}

	// 3. Read the Deployment the policy points at.
	var dep appsv1.Deployment
	depKey := types.NamespacedName{Namespace: policy.Namespace, Name: policy.Spec.WorkloadRef.Name}
	if err := r.Get(ctx, depKey, &dep); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		msg := fmt.Sprintf("Deployment %q was not found", depKey.Name)
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setReady(ctx, &policy, metav1.ConditionFalse, reasonWorkloadNotFound, msg)
	}

	// 4. Find the container and the directory where it mounts the ConfigMap.
	idx, watchDir, reason, msg := findWatchDir(&dep, policy.Spec.ContainerName, policy.Spec.ConfigMapRef.Name)
	if reason != "" {
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setReady(ctx, &policy, metav1.ConditionFalse, reason, msg)
	}

	// 5. From here on the Deployment carries our env. Make sure the policy
	// cannot go away without taking it off again.
	if controllerutil.AddFinalizer(&policy, finalizer) {
		if err := r.Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 6. Tell the watcher what to do through env on the container, but only
	// write when something is not right yet.
	container := &dep.Spec.Template.Spec.Containers[idx]
	env, changed := syncEnv(container.Env, watcherEnv(&policy, watchDir))
	if changed {
		before := dep.DeepCopy()
		container.Env = env
		if err := r.Patch(ctx, &dep, client.StrategicMergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("configured watcher", "deployment", dep.Name, "container", container.Name, "dir", watchDir)
	}

	// 7. Report success.
	msg = fmt.Sprintf("container %q watches %s", container.Name, watchDir)
	return ctrl.Result{}, r.setReady(ctx, &policy, metav1.ConditionTrue, reasonConfigured, msg)
}

// finish runs when the policy is being deleted. It takes the watcher env
// off the container and then removes the finalizer so the policy can go.
func (r *ConfigReloadPolicyReconciler) finish(ctx context.Context, policy *reloadv1alpha1.ConfigReloadPolicy) error {
	if !controllerutil.ContainsFinalizer(policy, finalizer) {
		return nil
	}
	if err := r.revert(ctx, policy); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(policy, finalizer)
	return r.Update(ctx, policy)
}

// revert takes the watcher env off the container the policy points at. A
// Deployment or container that is already gone needs nothing.
func (r *ConfigReloadPolicyReconciler) revert(ctx context.Context, policy *reloadv1alpha1.ConfigReloadPolicy) error {
	var dep appsv1.Deployment
	depKey := types.NamespacedName{Namespace: policy.Namespace, Name: policy.Spec.WorkloadRef.Name}
	if err := r.Get(ctx, depKey, &dep); err != nil {
		return client.IgnoreNotFound(err)
	}
	idx := containerIndex(&dep.Spec.Template.Spec, policy.Spec.ContainerName)
	if idx < 0 {
		return nil
	}

	container := &dep.Spec.Template.Spec.Containers[idx]
	env, changed := syncEnv(container.Env, nil)
	if !changed {
		return nil
	}
	before := dep.DeepCopy()
	container.Env = env
	if err := r.Patch(ctx, &dep, client.StrategicMergeFrom(before)); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("removed watcher env", "deployment", dep.Name, "container", container.Name)
	return nil
}

// containerIndex returns the position of the named container, or -1.
func containerIndex(podSpec *corev1.PodSpec, name string) int {
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == name {
			return i
		}
	}
	return -1
}

// findWatchDir looks for the named container and for the directory where it
// mounts the named ConfigMap. When all is well, reason is empty. Otherwise
// reason and msg say what is wrong.
func findWatchDir(dep *appsv1.Deployment, containerName, configMapName string) (idx int, dir, reason, msg string) {
	podSpec := &dep.Spec.Template.Spec

	idx = containerIndex(podSpec, containerName)
	if idx < 0 {
		return -1, "", reasonContainerNotFound,
			fmt.Sprintf("Deployment %q has no container named %q", dep.Name, containerName)
	}

	// Which volumes come from this ConfigMap?
	fromConfigMap := map[string]bool{}
	for _, v := range podSpec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == configMapName {
			fromConfigMap[v.Name] = true
		}
	}

	for _, m := range podSpec.Containers[idx].VolumeMounts {
		if !fromConfigMap[m.Name] {
			continue
		}
		if m.SubPath != "" || m.SubPathExpr != "" {
			return idx, "", reasonSubPathNotSupported,
				fmt.Sprintf("ConfigMap %q is mounted with subPath, so its files are never updated", configMapName)
		}
		return idx, m.MountPath, "", ""
	}
	return idx, "", reasonConfigMapNotMounted,
		fmt.Sprintf("container %q does not mount ConfigMap %q", containerName, configMapName)
}

// watcherEnv returns the env the watcher needs for this policy.
func watcherEnv(policy *reloadv1alpha1.ConfigReloadPolicy, watchDir string) []corev1.EnvVar {
	// The API server fills in the defaults. These are only a safety net.
	signal := policy.Spec.Reload.Signal
	if signal == "" {
		signal = "SIGHUP"
	}
	debounce := int32(3)
	if policy.Spec.Reload.DebounceSeconds != nil {
		debounce = *policy.Spec.Reload.DebounceSeconds
	}

	env := []corev1.EnvVar{
		{Name: envWatchDir, Value: watchDir},
		{Name: envSignal, Value: signal},
		{Name: envDebounce, Value: strconv.Itoa(int(debounce))},
	}

	// The check command is a list of words. Env can only hold one string,
	// so it travels as JSON, for example ["nginx","-t"].
	if command := policy.Spec.Reload.ValidateCommand; len(command) > 0 {
		// A list of strings always turns into JSON, so the error is ignored.
		encoded, _ := json.Marshal(command)
		env = append(env, corev1.EnvVar{Name: envValidate, Value: string(encoded)})
	}
	return env
}

// syncEnv returns env with the watcher entries made equal to want: wanted
// entries are set, and owned entries that are not wanted are removed. Every
// other entry stays as it is. The second result says if anything changed.
// The input is not modified.
func syncEnv(env, want []corev1.EnvVar) ([]corev1.EnvVar, bool) {
	wanted := map[string]string{}
	for _, w := range want {
		wanted[w.Name] = w.Value
	}
	owned := map[string]bool{}
	for _, name := range ownedEnv {
		owned[name] = true
	}

	out := make([]corev1.EnvVar, 0, len(env)+len(want))
	done := map[string]bool{}
	changed := false

	for _, e := range env {
		if !owned[e.Name] {
			out = append(out, e) // not ours: keep as is
			continue
		}
		value, ok := wanted[e.Name]
		if !ok {
			changed = true // ours, but no longer wanted: drop it
			continue
		}
		if e.Value != value || e.ValueFrom != nil {
			changed = true
		}
		out = append(out, corev1.EnvVar{Name: e.Name, Value: value})
		done[e.Name] = true
	}

	for _, w := range want {
		if !done[w.Name] {
			out = append(out, w) // wanted, but not there yet: add it
			changed = true
		}
	}
	return out, changed
}

// setReady writes the Ready condition, but only when it really changes.
func (r *ConfigReloadPolicyReconciler) setReady(
	ctx context.Context,
	policy *reloadv1alpha1.ConfigReloadPolicy,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	changed := meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})
	if !changed {
		return nil
	}
	return r.Status().Update(ctx, policy)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigReloadPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&reloadv1alpha1.ConfigReloadPolicy{}).
		Named("configreloadpolicy").
		Complete(r)
}
