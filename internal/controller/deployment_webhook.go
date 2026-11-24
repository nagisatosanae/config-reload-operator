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

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// InjectAnnotation switches the webhook on for a Deployment. Only a
	// Deployment that carries it with the value "true" is looked at.
	InjectAnnotation = "reload.nagisatosanae.github.io/inject"

	// injectPath is the URL path the webhook is served on.
	//
	// The chart names the annotation and the path too, in
	// templates/webhook.yaml. Change one side and you must change the other.
	injectPath = "/mutate-apps-v1-deployment"
)

// DeploymentInjector is a mutating admission webhook for Deployments.
//
// The reconciler can only fix a Deployment after it was stored. So the first
// Pods start without the watcher env, and are replaced a moment later. This
// webhook sets the same env while the Deployment is being created or
// updated, so the first Pods are already right.
//
// It is only a shortcut. It never rejects a request: when something is
// missing or goes wrong it lets the Deployment through as it is, and the
// reconciler brings it in line later. Both use the same functions, so they
// cannot disagree about what the env should be.
type DeploymentInjector struct {
	// Client reads the policies. The manager gives its cached client.
	Client client.Reader
}

// Default sets the watcher env on dep for every policy that points at it.
// The API server calls it before the Deployment is stored.
func (d *DeploymentInjector) Default(ctx context.Context, dep *appsv1.Deployment) error {
	log := logf.FromContext(ctx)

	// The API server already filters on the annotation. Check again here, so
	// the rule does not depend on how the webhook was registered.
	if dep.Annotations[InjectAnnotation] != "true" {
		return nil
	}

	// A new object may not have its namespace filled in yet. The request
	// always has it.
	namespace := dep.Namespace
	if req, err := admission.RequestFromContext(ctx); err == nil && req.Namespace != "" {
		namespace = req.Namespace
	}

	var policies reloadv1alpha1.ConfigReloadPolicyList
	if err := d.Client.List(ctx, &policies, client.InNamespace(namespace)); err != nil {
		log.Error(err, "could not read the policies, leaving the Deployment as it is", "deployment", dep.Name)
		return nil
	}

	for i := range policies.Items {
		policy := &policies.Items[i]
		if policy.Spec.WorkloadRef.Name != dep.Name {
			continue
		}
		// A policy on its way out is taking its env off the container. Do
		// not put it back.
		if !policy.DeletionTimestamp.IsZero() {
			continue
		}
		// What is wrong is reported by the reconciler, in the policy status.
		idx, watchDir, reason, _ := findWatchDir(dep, policy.Spec.ContainerName, policy.Spec.ConfigMapRef.Name)
		if reason != "" {
			continue
		}

		container := &dep.Spec.Template.Spec.Containers[idx]
		env, changed := syncEnv(container.Env, watcherEnv(policy, watchDir))
		if changed {
			container.Env = env
			log.Info("configured watcher at admission",
				"deployment", dep.Name, "container", container.Name, "dir", watchDir, "policy", policy.Name)
		}
	}
	return nil
}

// SetupWebhookWithManager serves the webhook on the manager's webhook server.
func (d *DeploymentInjector) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &appsv1.Deployment{}).
		WithDefaulter(d).
		WithDefaulterCustomPath(injectPath).
		Complete()
}
