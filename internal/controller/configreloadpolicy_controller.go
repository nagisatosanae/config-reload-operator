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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
	"github.com/nagisatosanae/config-reload-operator/internal/settings"
)

// ConfigReloadPolicyReconciler reports the state of a ConfigReloadPolicy.
//
// It writes nothing but the status of the policy, and it takes the
// finalizer of version 0.1 off a policy that still has it. The settings are
// put on new Pods by the webhook, see PodDefaulter in internal/webhook/v1. A
// Deployment and its Pods are only read, and a Pod that runs is never
// touched.
type ConfigReloadPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// conditionReady is the one condition this controller reports.
	conditionReady = "Ready"

	// retryAfter is how long to wait before looking again when the policy
	// is not Ready.
	retryAfter = 30 * time.Second

	// resyncAfter is how long a policy that is Ready goes without a look.
	// A Pod that comes or goes wakes the controller up at once. A change of
	// the Deployment does not, so it is seen this late at the latest.
	resyncAfter = 5 * time.Minute

	// oldFinalizer was put on policies by version 0.1, which took its env
	// off the Deployment before a policy could go. Nothing is taken off any
	// more, so the finalizer is only removed.
	oldFinalizer = "reload.nagisatosanae.github.io/revert"
)

// Reasons shown in the Ready condition.
const (
	// Ready is True.
	reasonInjected = "Injected" // every Pod carries the settings
	reasonAccepted = "Accepted" // there are no Pods now, new ones get the settings

	// Ready is False.
	reasonNotInjected       = "NotInjected" // a Pod runs without the settings
	reasonOutdated          = "Outdated"    // a Pod carries the settings from before a change
	reasonWorkloadNotFound  = "WorkloadNotFound"
	reasonContainerNotFound = "ContainerNotFound"
	reasonNotOptedIn        = "NotOptedIn"
)

// replaceHint tells the user how a Pod gets the settings of the policy as it
// is now. It ends with the name of the Deployment.
const replaceHint = "They get the current settings when they are replaced, for example by: kubectl rollout restart deployment/"

// +kubebuilder:rbac:groups=reload.nagisatosanae.github.io,resources=configreloadpolicies,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=reload.nagisatosanae.github.io,resources=configreloadpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile looks at what the policy points at and writes the result into
// the status of the policy.
//
// It counts the Pods of the Deployment that ask for the settings, and how
// many of them carry the settings of the policy as it is now. Ready is True
// when all of them do. Otherwise the reason and the message say what is
// missing. Nothing else is written, so it is safe to run again and again.
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
		return r.report(ctx, &policy, counts{}, metav1.ConditionFalse, reasonWorkloadNotFound, msg)
	}
	template := &dep.Spec.Template

	// 4. Every container the policy names must be in the Pod template. A
	// policy written for an earlier version has no list of containers.
	if len(policy.Spec.Containers) == 0 {
		msg := "the policy names no container. Write it again with spec.containers"
		return r.report(ctx, &policy, counts{}, metav1.ConditionFalse, reasonContainerNotFound, msg)
	}
	for i := range policy.Spec.Containers {
		name := policy.Spec.Containers[i].Name
		if settings.ContainerIndex(&template.Spec, name) < 0 {
			msg := fmt.Sprintf("Deployment %q has no container named %q", dep.Name, name)
			return r.report(ctx, &policy, counts{}, metav1.ConditionFalse, reasonContainerNotFound, msg)
		}
	}

	// 5. The Pod template must ask for the settings. Without the annotation
	// the API server does not send the Pods to the webhook.
	if !settings.AskedFor(template.Annotations) {
		msg := fmt.Sprintf("the Pod template of Deployment %q does not have the annotation %s: \"true\"",
			dep.Name, settings.InjectAnnotation)
		return r.report(ctx, &policy, counts{}, metav1.ConditionFalse, reasonNotOptedIn, msg)
	}

	// 6. Count the Pods, and say what was found.
	n, err := r.countPods(ctx, &dep, &policy)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch missing := n.pods - n.current - n.outdated; {
	case n.pods == 0:
		msg := fmt.Sprintf("Deployment %q has no Pods now. New Pods get the settings", dep.Name)
		return r.report(ctx, &policy, n, metav1.ConditionTrue, reasonAccepted, msg)
	case missing > 0:
		msg := fmt.Sprintf("%d of %d Pods of Deployment %q run without the settings. %s%s",
			missing, n.pods, dep.Name, replaceHint, dep.Name)
		return r.report(ctx, &policy, n, metav1.ConditionFalse, reasonNotInjected, msg)
	case n.outdated > 0:
		msg := fmt.Sprintf("%d of %d Pods of Deployment %q carry the settings from before the policy was changed. %s%s",
			n.outdated, n.pods, dep.Name, replaceHint, dep.Name)
		return r.report(ctx, &policy, n, metav1.ConditionFalse, reasonOutdated, msg)
	default:
		msg := fmt.Sprintf("%d of %d Pods of Deployment %q carry the settings", n.current, n.pods, dep.Name)
		return r.report(ctx, &policy, n, metav1.ConditionTrue, reasonInjected, msg)
	}
}

// counts is what countPods found.
type counts struct {
	pods     int32 // Pods of the Deployment that ask for the settings
	current  int32 // of those, the ones that carry the settings of the policy as it is now
	outdated int32 // of those, the ones that carry other settings than the policy gives now
}

// countPods looks at the Pods of the Deployment. Only their metadata is
// read: the annotation that asks for the settings, and the mark the webhook
// left. A Pod that is being deleted is not counted.
func (r *ConfigReloadPolicyReconciler) countPods(
	ctx context.Context, dep *appsv1.Deployment, policy *reloadv1alpha1.ConfigReloadPolicy,
) (counts, error) {
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return counts{}, err
	}
	var pods metav1.PartialObjectMetadataList
	pods.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("PodList"))
	err = r.List(ctx, &pods, client.InNamespace(dep.Namespace), client.MatchingLabelsSelector{Selector: selector})
	if err != nil {
		return counts{}, err
	}

	// The mark of a Pod that carries the settings of the policy as it is
	// now. Any other mark means other settings: the policy was changed
	// after the Pod was made, or an earlier version marked the Pod.
	want := settings.MarkFor(policy)

	var n counts
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() || !settings.AskedFor(pod.Annotations) {
			continue
		}
		// The selector can also match Pods that are not made by this
		// Deployment.
		if owner, err := settings.DeploymentOf(pod); err != nil || owner != dep.Name {
			continue
		}
		n.pods++
		switch pod.Annotations[settings.InjectedAnnotation] {
		case "":
		case want:
			n.current++
		default:
			n.outdated++
		}
	}
	return n, nil
}

// policiesForPod returns the policies that count this Pod. The controller
// uses it to look at a policy again when one of its Pods comes or goes.
func (r *ConfigReloadPolicyReconciler) policiesForPod(ctx context.Context, pod client.Object) []reconcile.Request {
	if !settings.AskedFor(pod.GetAnnotations()) {
		return nil
	}
	deployment, err := settings.DeploymentOf(pod)
	if err != nil {
		return nil
	}
	var policies reloadv1alpha1.ConfigReloadPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(pod.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "could not read the policies for a Pod",
			"namespace", pod.GetNamespace(), "pod", pod.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range policies.Items {
		if policies.Items[i].Spec.WorkloadRef.Name == deployment {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&policies.Items[i])})
		}
	}
	return requests
}

// report writes the Ready condition and the Pod counts, but only when
// something really changes. The result says when to look again.
func (r *ConfigReloadPolicyReconciler) report(
	ctx context.Context,
	policy *reloadv1alpha1.ConfigReloadPolicy,
	n counts,
	status metav1.ConditionStatus,
	reason, message string,
) (ctrl.Result, error) {
	result := ctrl.Result{RequeueAfter: retryAfter}
	if status == metav1.ConditionTrue {
		result.RequeueAfter = resyncAfter
	}

	changed := meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})
	if policy.Status.Pods != n.pods || policy.Status.InjectedPods != n.current {
		policy.Status.Pods, policy.Status.InjectedPods = n.pods, n.current
		changed = true
	}
	if !changed {
		return result, nil
	}
	return result, r.Status().Update(ctx, policy)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigReloadPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&reloadv1alpha1.ConfigReloadPolicy{}).
		// Only the metadata of Pods is kept in memory.
		WatchesMetadata(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.policiesForPod)).
		Named("configreloadpolicy").
		Complete(r)
}
