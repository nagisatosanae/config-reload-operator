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
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
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

	// retryAfter is how long to wait before looking again when something
	// the policy points at is missing or wrong.
	retryAfter = 30 * time.Second
)

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
// again and again.
func (r *ConfigReloadPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Read the policy. If it is gone, there is nothing to do.
	var policy reloadv1alpha1.ConfigReloadPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Read the Deployment the policy points at.
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

	// 3. Find the container and the directory where it mounts the ConfigMap.
	idx, watchDir, reason, msg := findWatchDir(&dep, policy.Spec.ContainerName, policy.Spec.ConfigMapRef.Name)
	if reason != "" {
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setReady(ctx, &policy, metav1.ConditionFalse, reason, msg)
	}

	// 4. Tell the watcher what to do through env on the container, but only
	// write when something is not right yet.
	container := &dep.Spec.Template.Spec.Containers[idx]
	want := watcherEnv(&policy, watchDir)
	if !hasAllEnv(container.Env, want) {
		before := dep.DeepCopy()
		for _, e := range want {
			container.Env = setEnv(container.Env, e.Name, e.Value)
		}
		if err := r.Patch(ctx, &dep, client.StrategicMergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("configured watcher", "deployment", dep.Name, "container", container.Name,
			"dir", watchDir, "signal", want[1].Value, "debounceSeconds", want[2].Value)
	}

	// 5. Report success.
	msg = fmt.Sprintf("container %q watches %s", container.Name, watchDir)
	return ctrl.Result{}, r.setReady(ctx, &policy, metav1.ConditionTrue, reasonConfigured, msg)
}

// findWatchDir looks for the named container and for the directory where it
// mounts the named ConfigMap. When all is well, reason is empty. Otherwise
// reason and msg say what is wrong.
func findWatchDir(dep *appsv1.Deployment, containerName, configMapName string) (idx int, dir, reason, msg string) {
	podSpec := &dep.Spec.Template.Spec

	idx = -1
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == containerName {
			idx = i
			break
		}
	}
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

// watcherEnv returns the env the watcher needs for this policy: the
// directory, the signal and the quiet time, in that order.
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

	return []corev1.EnvVar{
		{Name: envWatchDir, Value: watchDir},
		{Name: envSignal, Value: signal},
		{Name: envDebounce, Value: strconv.Itoa(int(debounce))},
	}
}

// hasAllEnv reports whether every wanted entry is already set.
func hasAllEnv(env, want []corev1.EnvVar) bool {
	for _, w := range want {
		if !hasEnv(env, w.Name, w.Value) {
			return false
		}
	}
	return true
}

// hasEnv reports whether the list already has name set to value.
func hasEnv(env []corev1.EnvVar, name, value string) bool {
	for _, e := range env {
		if e.Name == name {
			return e.Value == value && e.ValueFrom == nil
		}
	}
	return false
}

// setEnv returns the list with name set to value. It adds the entry if it
// is not there yet.
func setEnv(env []corev1.EnvVar, name, value string) []corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			env[i].Value = value
			env[i].ValueFrom = nil
			return env
		}
	}
	return append(env, corev1.EnvVar{Name: name, Value: value})
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
