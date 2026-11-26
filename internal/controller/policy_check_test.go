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
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
)

// The check in these tests is called by the envtest API server when a
// policy is created, the same way a real cluster calls it.

// policyCheckWebhookConfig registers the check like the chart does.
func policyCheckWebhookConfig() *admissionv1.ValidatingWebhookConfiguration {
	fail := admissionv1.Fail
	none := admissionv1.SideEffectClassNone
	path := policyCheckPath
	return &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "config-reload-operator-policy"},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:                    "policy.reload.nagisatosanae.github.io",
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &none,
			FailurePolicy:           &fail,
			ClientConfig: admissionv1.WebhookClientConfig{
				// envtest turns this into the address of the test server.
				Service: &admissionv1.ServiceReference{Name: webhookService, Namespace: testNamespace, Path: &path},
			},
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create},
				Rule: admissionv1.Rule{
					APIGroups:   []string{reloadv1alpha1.GroupVersion.Group},
					APIVersions: []string{reloadv1alpha1.GroupVersion.Version},
					Resources:   []string{"configreloadpolicies"},
				},
			}},
		}},
	}
}

// policyCheckSwitch is the check the API server calls in these tests. A
// spec can turn it off, to make the two policies for one Deployment that
// the check keeps apart.
type policyCheckSwitch struct {
	mu       sync.Mutex
	disabled bool
	client   client.Reader
}

var testPolicyCheck = &policyCheckSwitch{}

// off lets every policy in until on is called or the spec is over.
func (s *policyCheckSwitch) off() {
	s.mu.Lock()
	s.disabled = true
	s.mu.Unlock()
	DeferCleanup(s.on)
}

// on makes the check work again.
func (s *policyCheckSwitch) on() {
	s.mu.Lock()
	s.disabled = false
	s.mu.Unlock()
}

func (s *policyCheckSwitch) ValidateCreate(ctx context.Context, policy *reloadv1alpha1.ConfigReloadPolicy) (admission.Warnings, error) {
	s.mu.Lock()
	disabled := s.disabled
	s.mu.Unlock()
	if disabled {
		return nil, nil
	}
	return (&PolicyChecker{Client: s.client}).ValidateCreate(ctx, policy)
}

func (s *policyCheckSwitch) ValidateUpdate(ctx context.Context, old, policy *reloadv1alpha1.ConfigReloadPolicy) (admission.Warnings, error) {
	return (&PolicyChecker{Client: s.client}).ValidateUpdate(ctx, old, policy)
}

func (s *policyCheckSwitch) ValidateDelete(ctx context.Context, policy *reloadv1alpha1.ConfigReloadPolicy) (admission.Warnings, error) {
	return (&PolicyChecker{Client: s.client}).ValidateDelete(ctx, policy)
}

var _ = Describe("Policy check", func() {
	It("turns down a second policy for a Deployment, and names the first", func() {
		create(newPolicy("first-reload", "single"))

		second := newPolicy("second-reload", "single")
		second.Spec.Containers = []reloadv1alpha1.ContainerSettings{helperSettings()}
		err := k8sClient.Create(ctx, second)
		Expect(err).To(MatchError(apierrors.IsForbidden, "a Forbidden error"))
		Expect(err.Error()).To(ContainSubstring(`"single" already has the policy "first-reload"`))
		Expect(policyExists("second-reload")).To(BeFalse())
	})

	It("lets a policy in for another Deployment of the namespace", func() {
		create(newPolicy("left-reload", "left"))
		create(newPolicy("right-reload", "right"))
	})

	It("lets a new policy in once the first one is deleted", func() {
		first := newPolicy("before-reload", "turn")
		create(first)
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		create(newPolicy("after-reload", "turn"))
	})

	It("does not count a policy that is on its way out", func() {
		leaving := newPolicy("leaving-policy", "handover")
		leaving.Finalizers = []string{holdFinalizer}
		create(leaving)
		Expect(k8sClient.Delete(ctx, leaving)).To(Succeed())
		Expect(policyExists("leaving-policy")).To(BeTrue(), "the finalizer holds it")
		DeferCleanup(func() {
			held := getPolicy("leaving-policy")
			held.Finalizers = nil
			Expect(k8sClient.Update(ctx, held)).To(Succeed())
		})

		create(newPolicy("coming-policy", "handover"))
	})

	It("is not asked when a policy is changed", func() {
		create(newPolicy("one-reload", "shared"))
		// Two policies for one Deployment, as after two creates at the same
		// moment. Each of them can still be changed, so one can be fixed or
		// emptied before the other is deleted.
		testPolicyCheck.off()
		create(newPolicy("two-reload", "shared"))
		testPolicyCheck.on()

		pol := getPolicy("two-reload")
		pol.Spec.Containers[0].Reload.Signal = sigUSR1
		Expect(k8sClient.Update(ctx, pol)).To(Succeed())
	})
})
