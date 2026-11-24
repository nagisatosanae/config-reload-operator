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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The webhook in these tests is called by the envtest API server, the same
// way a real cluster calls it: the API server looks at the annotation first,
// and only then sends the Deployment to the webhook.

// injectCondition is the rule the API server uses to pick the Deployments it
// sends to the webhook. The chart has the same rule.
const injectCondition = "has(object.metadata.annotations) && " +
	"'" + InjectAnnotation + "' in object.metadata.annotations && " +
	"object.metadata.annotations['" + InjectAnnotation + "'] == 'true'"

// injectorWebhookConfig registers the webhook like the chart does. One thing
// is different: here a webhook that cannot be reached fails the request, so
// a broken test setup is seen at once.
func injectorWebhookConfig() *admissionv1.MutatingWebhookConfiguration {
	fail := admissionv1.Fail
	none := admissionv1.SideEffectClassNone
	path := injectPath
	return &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "config-reload-operator"},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name:                    "inject.reload.nagisatosanae.github.io",
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &none,
			FailurePolicy:           &fail,
			ClientConfig: admissionv1.WebhookClientConfig{
				// envtest turns this into the address of the test server.
				Service: &admissionv1.ServiceReference{Name: "webhook", Namespace: testNamespace, Path: &path},
			},
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
				Rule: admissionv1.Rule{
					APIGroups:   []string{"apps"},
					APIVersions: []string{"v1"},
					Resources:   []string{"deployments"},
				},
			}},
			MatchConditions: []admissionv1.MatchCondition{{
				Name:       "has-inject-annotation",
				Expression: injectCondition,
			}},
		}},
	}
}

// inject marks the Deployment for the webhook.
func inject(dep *appsv1.Deployment, value string) *appsv1.Deployment {
	dep.Annotations = map[string]string{InjectAnnotation: value}
	return dep
}

var _ = Describe("Deployment webhook", func() {
	It("sets the watcher env while the Deployment is created", func() {
		userEnv := []corev1.EnvVar{{Name: "TZ", Value: tz}}
		create(newPolicy("early-reload", "early"))
		create(inject(newDeployment("early", userEnv...), "true"))

		dep := getDeployment("early")
		Expect(appEnv(dep)).To(Equal(slices.Concat(userEnv, defaultEnv)), "user env stays, watcher env is added")
		Expect(dep.Spec.Template.Spec.Containers[0].Env).To(BeEmpty(), "the other container is left alone")
		Expect(dep.Generation).To(Equal(int64(1)), "stored once, with the env already on it")

		By("the reconciler finds nothing left to do")
		_, err := runReconcile("early-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(getDeployment("early").Generation).To(Equal(int64(1)), "no second write, so no second rollout")
		Expect(readyCondition("early-reload").Status).To(Equal(metav1.ConditionTrue))
	})

	It("sets the watcher env when a Deployment that was there first is updated", func() {
		create(inject(newDeployment("late"), "true"))
		Expect(appEnv(getDeployment("late"))).To(BeEmpty(), "no policy yet")
		create(newPolicy("late-reload", "late"))

		dep := getDeployment("late")
		dep.Labels = map[string]string{"touched": "yes"}
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		Expect(appEnv(getDeployment("late"))).To(Equal(defaultEnv))
	})

	DescribeTable("leaves the Deployment as it is",
		func(name string, prepare func(dep *appsv1.Deployment)) {
			dep := newDeployment(name)
			prepare(dep)
			create(dep)
			Expect(appEnv(getDeployment(name))).To(BeEmpty())
		},
		Entry("without the annotation", "plain", func(*appsv1.Deployment) {
			create(newPolicy("plain-reload", "plain"))
		}),
		Entry("with the annotation set to false", "off", func(dep *appsv1.Deployment) {
			create(newPolicy("off-reload", "off"))
			inject(dep, "false")
		}),
		Entry("when no policy points at it", "alone", func(dep *appsv1.Deployment) {
			create(newPolicy("alone-reload", "someone-else"))
			inject(dep, "true")
		}),
		Entry("when the policy names a container it does not have", "odd", func(dep *appsv1.Deployment) {
			pol := newPolicy("odd-reload", "odd")
			pol.Spec.ContainerName = "missing"
			create(pol)
			inject(dep, "true")
		}),
	)

	It("does not put the env back while the policy is being deleted", func() {
		pol := newPolicy("leaving-reload", "leaving")
		create(pol)
		create(inject(newDeployment("leaving"), "true"))
		_, err := runReconcile("leaving-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(appEnv(getDeployment("leaving"))).To(Equal(defaultEnv))

		By("deleting the policy: the reconciler takes the env off, through the webhook")
		Expect(k8sClient.Delete(ctx, pol)).To(Succeed())
		_, err = runReconcile("leaving-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(appEnv(getDeployment("leaving"))).To(BeEmpty())
		Expect(policyExists("leaving-reload")).To(BeFalse())
	})
})

// The annotation, the path and the rule are written twice: in the Go code
// and in the chart. This test fails when the two no longer agree.
func TestChartMatchesTheWebhook(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "config-reload-operator", "templates", "webhook.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	chart := strings.Join(strings.Fields(string(raw)), " ")

	for what, want := range map[string]string{
		"path": "path: " + injectPath,
		"rule": injectCondition,
	} {
		if !strings.Contains(chart, want) {
			t.Errorf("the chart does not have the %s the webhook uses: %s", what, want)
		}
	}
}
