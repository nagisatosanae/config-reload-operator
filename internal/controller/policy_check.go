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

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
)

// policyCheckPath is the URL path the check of a new policy is served on.
//
// The chart names the path too, in templates/webhook.yaml. Change one side
// and you must change the other.
const policyCheckPath = "/validate-reload-nagisatosanae-github-io-v1alpha1-configreloadpolicy"

// PolicyChecker is a validating admission webhook for ConfigReloadPolicy.
//
// A Deployment can have only one policy. The schema of the CRD cannot say
// that, because a rule there sees one object and not the others. So the
// checker looks at the other policies when a new one is created, and turns
// the new one down when its Deployment already has a policy.
//
// Only a new policy is checked. The workload of a policy cannot be changed
// later, so a second policy for a Deployment can only come from a create.
//
// It changes nothing. It runs in the same webhook server as the Pod webhook.
type PolicyChecker struct {
	// Client reads the policies. The manager gives its cached client.
	Client client.Reader
}

// ValidateCreate turns a new policy down when its Deployment already has
// one.
func (c *PolicyChecker) ValidateCreate(ctx context.Context, policy *reloadv1alpha1.ConfigReloadPolicy) (admission.Warnings, error) {
	// A new object may not have its namespace filled in yet. The request
	// always has it.
	namespace := policy.Namespace
	if req, err := admission.RequestFromContext(ctx); err == nil && req.Namespace != "" {
		namespace = req.Namespace
	}

	deployment := policy.Spec.WorkloadRef.Name
	others, err := policiesFor(ctx, c.Client, namespace, deployment)
	if err != nil {
		return nil, err
	}
	if len(others) > 0 {
		return nil, fmt.Errorf("deployment %q already has the policy %q, and a Deployment can have only one. "+
			"Put the container into that policy", deployment, others[0].Name)
	}
	return nil, nil
}

// ValidateUpdate lets every change through. The schema keeps the workload of
// a policy as it is.
func (c *PolicyChecker) ValidateUpdate(context.Context, *reloadv1alpha1.ConfigReloadPolicy, *reloadv1alpha1.ConfigReloadPolicy) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete lets every policy go.
func (c *PolicyChecker) ValidateDelete(context.Context, *reloadv1alpha1.ConfigReloadPolicy) (admission.Warnings, error) {
	return nil, nil
}

// SetupWebhookWithManager serves the check on the manager's webhook server.
func (c *PolicyChecker) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &reloadv1alpha1.ConfigReloadPolicy{}).
		WithValidator(c).
		WithValidatorCustomPath(policyCheckPath).
		Complete()
}
