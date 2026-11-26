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
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// The check in these tests is called by the envtest API server when a
// Deployment is created or updated, the same way a real cluster calls it.

// checkCondition is the rule the API server uses to pick the Deployments it
// asks the webhook about. The chart has the same rule.
const checkCondition = "has(object.spec.template.metadata) && " +
	"has(object.spec.template.metadata.annotations) && " +
	"'" + InjectAnnotation + "' in object.spec.template.metadata.annotations && " +
	"object.spec.template.metadata.annotations['" + InjectAnnotation + "'] == 'true'"

// notDeletedCondition keeps a Deployment that is being deleted away from the
// webhook. The chart has the same rule.
const notDeletedCondition = "!has(object.metadata.deletionTimestamp)"

// checkerWebhookConfig registers the check like the chart does.
func checkerWebhookConfig() *admissionv1.ValidatingWebhookConfiguration {
	fail := admissionv1.Fail
	none := admissionv1.SideEffectClassNone
	path := checkPath
	return &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "config-reload-operator"},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:                    "check.reload.nagisatosanae.github.io",
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &none,
			FailurePolicy:           &fail,
			ClientConfig: admissionv1.WebhookClientConfig{
				// envtest turns this into the address of the test server.
				Service: &admissionv1.ServiceReference{Name: webhookService, Namespace: testNamespace, Path: &path},
			},
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
				Rule: admissionv1.Rule{
					APIGroups:   []string{"apps"},
					APIVersions: []string{"v1"},
					Resources:   []string{"deployments"},
				},
			}},
			MatchConditions: []admissionv1.MatchCondition{
				{Name: "pod-template-has-inject-annotation", Expression: checkCondition},
				{Name: "not-being-deleted", Expression: notDeletedCondition},
			},
		}},
	}
}

// checkerSwitch is the Deployment check the API server calls in these tests.
// It follows the failure policy of testInjector.
type checkerSwitch struct{ *policySwitch }

func (c checkerSwitch) checker() *DeploymentChecker {
	return &DeploymentChecker{Injector: c.injector()}
}

func (c checkerSwitch) ValidateCreate(ctx context.Context, dep *appsv1.Deployment) (admission.Warnings, error) {
	return c.checker().ValidateCreate(ctx, dep)
}

func (c checkerSwitch) ValidateUpdate(ctx context.Context, old, dep *appsv1.Deployment) (admission.Warnings, error) {
	return c.checker().ValidateUpdate(ctx, old, dep)
}

func (c checkerSwitch) ValidateDelete(ctx context.Context, dep *appsv1.Deployment) (admission.Warnings, error) {
	return c.checker().ValidateDelete(ctx, dep)
}

// deploymentFor returns the Deployment whose Pods would have the problem.
func deploymentFor(p problem) *appsv1.Deployment {
	pod := newPod(p.name)
	p.prepare(pod)
	dep := newDeployment(p.name)
	dep.Spec.Template.Spec = pod.Spec
	return dep
}

// warnings keeps what the API server warns the client about.
type warnings struct {
	mu   sync.Mutex
	text []string
}

func (w *warnings) HandleWarningHeader(_ int, _ string, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.text = append(w.text, text)
}

func (w *warnings) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.text...)
}

// warningClient returns a client that keeps the warnings it gets.
func warningClient() (client.Client, *warnings) {
	GinkgoHelper()
	w := &warnings{}
	config := rest.CopyConfig(cfg)
	config.WarningHandler = w
	c, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	return c, w
}

var _ = Describe("Deployment check", func() {
	It("lets a Deployment in when its Pods can get their settings, and does not change it", func() {
		create(newPolicy("fine-reload", "fine"))
		c, w := warningClient()
		dep := newDeployment("fine")
		Expect(c.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, dep))).To(Succeed()) })

		Expect(w.all()).To(BeEmpty())
		got := getDeployment("fine")
		Expect(got.Spec.Template.Spec.Volumes).To(BeEmpty(), "the check changed the Deployment")
		Expect(got.Spec.Template.Spec.Containers[1].Env).To(BeEmpty(), "the check changed the Deployment")
	})

	DescribeTable("turns down a Deployment whose Pods could not get their settings, and says why",
		func(p problem) {
			err := k8sClient.Create(ctx, deploymentFor(p))
			Expect(err).To(MatchError(apierrors.IsForbidden, "a Forbidden error"))
			Expect(err.Error()).To(ContainSubstring(p.message))
			Expect(err.Error()).To(ContainSubstring(InjectAnnotation), "the message names the annotation")
		},
		problemEntries(true),
	)

	DescribeTable("lets the same Deployment in with a warning when the failure policy is Ignore",
		func(p problem) {
			testInjector.use(FailurePolicyIgnore)
			c, w := warningClient()
			dep := deploymentFor(p)
			Expect(c.Create(ctx, dep)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, dep))).To(Succeed()) })

			Expect(w.all()).To(ContainElement(ContainSubstring(p.message)))
		},
		problemEntries(true),
	)

	It("is not asked about a Deployment whose Pod template has no annotation", func() {
		// No policy points at it, and it is let in all the same.
		dep := newDeployment("unasked")
		dep.Spec.Template.Annotations = nil
		create(dep)
	})

	It("turns down a change once the policy is gone, until the annotation is taken off", func() {
		pol := newPolicy("gone-reload", "gone")
		create(pol)
		create(newDeployment("gone"))
		Expect(k8sClient.Delete(ctx, pol)).To(Succeed())

		dep := getDeployment("gone")
		dep.Spec.Template.Spec.Containers[1].Image = "nginx:mainline-alpine"
		Expect(k8sClient.Update(ctx, dep)).To(MatchError(apierrors.IsForbidden, "a Forbidden error"))

		dep = getDeployment("gone")
		dep.Spec.Template.Spec.Containers[1].Image = "nginx:mainline-alpine"
		dep.Spec.Template.Annotations = nil
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
	})

	It("never holds back a Deployment that is being deleted", func() {
		pol := newPolicy("ending-reload", "ending")
		create(pol)
		dep := newDeployment("ending")
		dep.Finalizers = []string{holdFinalizer}
		create(dep)
		Expect(k8sClient.Delete(ctx, pol)).To(Succeed())
		Expect(k8sClient.Delete(ctx, dep)).To(Succeed())

		// The finalizer keeps it there. Taking the finalizer off is an update
		// of a Deployment that has the annotation and no policy.
		held := getDeployment("ending")
		Expect(held.DeletionTimestamp).NotTo(BeNil())
		held.Finalizers = nil
		Expect(k8sClient.Update(ctx, held)).To(Succeed())
	})
})
