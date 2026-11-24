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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// checkPath is the URL path the Deployment check is served on.
//
// The chart names the path too, in templates/webhook.yaml. Change one side
// and you must change the other.
const checkPath = "/validate-apps-v1-deployment"

// DeploymentChecker is a validating admission webhook for Deployments.
//
// The settings go on Pods, and a Pod is made after "kubectl apply" has come
// back. Without this check a Deployment whose Pods cannot get their settings
// would be accepted, and the reason would only show up later, on its
// ReplicaSet. The checker says it at once. It makes a Pod from the Pod
// template, lets the Pod webhook work on it, and throws the result away.
//
// It changes nothing. It runs in the same webhook server as the Pod webhook.
type DeploymentChecker struct {
	// Injector is the Pod webhook. Its checks and its failure policy are
	// used here.
	Injector *PodInjector
}

// ValidateCreate checks a new Deployment.
func (c *DeploymentChecker) ValidateCreate(ctx context.Context, dep *appsv1.Deployment) (admission.Warnings, error) {
	return c.check(ctx, dep)
}

// ValidateUpdate checks a Deployment that was changed.
func (c *DeploymentChecker) ValidateUpdate(ctx context.Context, _, dep *appsv1.Deployment) (admission.Warnings, error) {
	return c.check(ctx, dep)
}

// ValidateDelete lets every Deployment go.
func (c *DeploymentChecker) ValidateDelete(context.Context, *appsv1.Deployment) (admission.Warnings, error) {
	return nil, nil
}

// check tells if the Pods of dep can get their settings. With the failure
// policy Fail a Deployment whose Pods cannot is rejected. With Ignore it is
// let in, and the user gets a warning.
func (c *DeploymentChecker) check(ctx context.Context, dep *appsv1.Deployment) (admission.Warnings, error) {
	// The API server already filters on the annotation. Check again here, so
	// the rule does not depend on how the webhook was registered.
	template := &dep.Spec.Template
	if !asksForSettings(template.Annotations) {
		return nil, nil
	}
	// A Deployment on its way out must not be held back.
	if !dep.DeletionTimestamp.IsZero() {
		return nil, nil
	}

	namespace := dep.Namespace
	if req, err := admission.RequestFromContext(ctx); err == nil && req.Namespace != "" {
		namespace = req.Namespace
	}

	pod := &corev1.Pod{ObjectMeta: template.ObjectMeta, Spec: template.Spec}
	_, reason := c.Injector.injectFor(ctx, namespace, dep.Name, pod)
	if reason == nil {
		return nil, nil
	}
	if c.Injector.FailurePolicy == FailurePolicyIgnore {
		return admission.Warnings{fmt.Sprintf(
			"the Pods of Deployment %s will start without the watcher settings: %v", dep.Name, reason)}, nil
	}
	return nil, fmt.Errorf("deployment %s asks for the watcher settings with the annotation %s on its Pod template, but %w",
		dep.Name, InjectAnnotation, reason)
}

// SetupWebhookWithManager serves the check on the manager's webhook server.
func (c *DeploymentChecker) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &appsv1.Deployment{}).
		WithValidator(c).
		WithValidatorCustomPath(checkPath).
		Complete()
}
