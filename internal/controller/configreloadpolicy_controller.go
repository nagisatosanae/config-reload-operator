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
)

// ConfigReloadPolicyReconciler reports the state of a ConfigReloadPolicy.
//
// It writes nothing but the status of the policy. The settings are put on
// new Pods by the webhook, see PodInjector. A Deployment is only read, and
// a Pod that runs is never touched.
type ConfigReloadPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// conditionReady is the one condition this controller reports.
	conditionReady = "Ready"

	// retryAfter is how long to wait before looking again when something
	// the policy points at is missing or wrong.
	retryAfter = 30 * time.Second

	// oldFinalizer was put on policies by version 0.1, which took its env
	// off the Deployment before a policy could go. Nothing is taken off any
	// more, so the finalizer is only removed.
	oldFinalizer = "reload.nagisatosanae.github.io/revert"
)

// Reasons shown in the Ready condition.
const (
	reasonAccepted          = "Accepted"
	reasonWorkloadNotFound  = "WorkloadNotFound"
	reasonContainerNotFound = "ContainerNotFound"
	reasonNotOptedIn        = "NotOptedIn"
)

// +kubebuilder:rbac:groups=reload.nagisatosanae.github.io,resources=configreloadpolicies,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=reload.nagisatosanae.github.io,resources=configreloadpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// Reconcile looks at what the policy points at and writes the result into
// the Ready condition.
//
// Ready is True when new Pods of the Deployment will get the settings: the
// Deployment is there, it has the named container, and its Pod template
// asks for the settings with the annotation. Otherwise the reason and the
// message say what is missing. Nothing else is written, so it is safe to
// run again and again.
func (r *ConfigReloadPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// 1. Read the policy. If it is gone, there is nothing to do.
	var policy reloadv1alpha1.ConfigReloadPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. A policy made by 0.1 may still carry its finalizer. Take it off, so
	// the policy can be deleted. Deleting a policy changes nothing else:
	// Pods that run keep their settings.
	if controllerutil.RemoveFinalizer(&policy, oldFinalizer) {
		if err := r.Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !policy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
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
	template := &dep.Spec.Template

	// 4. The container must be in the Pod template.
	if containerIndex(&template.Spec, policy.Spec.ContainerName) < 0 {
		msg := fmt.Sprintf("Deployment %q has no container named %q", dep.Name, policy.Spec.ContainerName)
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setReady(ctx, &policy, metav1.ConditionFalse, reasonContainerNotFound, msg)
	}

	// 5. The Pod template must ask for the settings. Without the annotation
	// the API server does not send the Pods to the webhook.
	if !asksForSettings(template.Annotations) {
		msg := fmt.Sprintf("the Pod template of Deployment %q does not have the annotation %s: \"true\"",
			dep.Name, InjectAnnotation)
		return ctrl.Result{RequeueAfter: retryAfter},
			r.setReady(ctx, &policy, metav1.ConditionFalse, reasonNotOptedIn, msg)
	}

	// 6. Report success.
	msg := fmt.Sprintf("new Pods of Deployment %q get the settings for container %q", dep.Name, policy.Spec.ContainerName)
	return ctrl.Result{}, r.setReady(ctx, &policy, metav1.ConditionTrue, reasonAccepted, msg)
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
