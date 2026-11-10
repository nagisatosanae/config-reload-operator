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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
)

// What happens when a policy goes away.

func policyExists(name string) bool {
	GinkgoHelper()
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &reloadv1alpha1.ConfigReloadPolicy{})
	if err == nil {
		return true
	}
	Expect(err).To(MatchError(apierrors.IsNotFound, "a NotFound error"))
	return false
}

var _ = Describe("ConfigReloadPolicy deletion", func() {
	It("takes its env off the container when it is deleted", func() {
		create(newDeployment("gone", corev1.EnvVar{Name: "TZ", Value: tz}))
		pol := newPolicy("gone-reload", "gone")
		create(pol)
		_, err := runReconcile("gone-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(controllerutil.ContainsFinalizer(getPolicy("gone-reload"), finalizer)).To(BeTrue(), "the finalizer is set once env is on the Deployment")
		Expect(appEnv(getDeployment("gone"))).To(HaveLen(4))

		By("deleting the policy: it stays until Reconcile has cleaned up")
		Expect(k8sClient.Delete(ctx, pol)).To(Succeed())
		Expect(policyExists("gone-reload")).To(BeTrue())

		_, err = runReconcile("gone-reload")
		Expect(err).NotTo(HaveOccurred())
		dep := getDeployment("gone")
		Expect(appEnv(dep)).To(Equal([]corev1.EnvVar{{Name: "TZ", Value: tz}}), "only the user env is left")
		Expect(dep.Generation).To(Equal(int64(3)), "set once, removed once")
		Expect(policyExists("gone-reload")).To(BeFalse(), "the finalizer is gone, so the policy is gone")
	})

	It("lets the policy go when the Deployment is already gone", func() {
		dep := newDeployment("vanished")
		create(dep)
		pol := newPolicy("vanished-reload", "vanished")
		create(pol)
		_, err := runReconcile("vanished-reload")
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pol)).To(Succeed())
		_, err = runReconcile("vanished-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(policyExists("vanished-reload")).To(BeFalse())
	})

	It("never had env on the Deployment, so there is nothing to hold it", func() {
		create(newDeployment("untouched"))
		pol := newPolicy("untouched-reload", "untouched")
		pol.Spec.ContainerName = otherContainer // not there: Reconcile reports it and writes nothing
		create(pol)
		_, err := runReconcile("untouched-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(controllerutil.ContainsFinalizer(getPolicy("untouched-reload"), finalizer)).To(BeFalse())

		Expect(k8sClient.Delete(ctx, pol)).To(Succeed())
		Expect(policyExists("untouched-reload")).To(BeFalse(), "no finalizer, so it is gone at once")
	})
})
