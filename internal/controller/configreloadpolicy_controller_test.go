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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
)

// These tests run against the envtest API server: a real kube-apiserver and
// etcd, but no kubelet and no other controllers. So the API server fills in
// defaults and rejects bad values for real, and Reconcile reads a real
// Deployment, but no Pod is ever started.

const (
	testNamespace   = "default"
	appContainer    = "nginx"
	helperContainer = "helper"
	// otherContainer is a name no container has.
	otherContainer = "sidecar"
	mountPath      = "/etc/nginx/conf.d"
	tz             = "Asia/Seoul"
	sigUSR1        = "SIGUSR1"
)

// newDeployment returns a Deployment like the demo: a helper container
// first, then the app container "nginx". Its Pod template asks for the
// watcher settings with the annotation.
func newDeployment(name string) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	replicas := int32(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{InjectAnnotation: "true"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: helperContainer, Image: "busybox"},
					{Name: appContainer, Image: "nginx:stable-alpine"},
				}},
			},
		},
	}
}

// newPolicy returns a policy for the Deployment made by newDeployment.
func newPolicy(name, deployment string) *reloadv1alpha1.ConfigReloadPolicy {
	return &reloadv1alpha1.ConfigReloadPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: reloadv1alpha1.ConfigReloadPolicySpec{
			WorkloadRef:   reloadv1alpha1.WorkloadRef{Name: deployment},
			ContainerName: appContainer,
			ConfigMapRef:  reloadv1alpha1.ConfigMapRef{Name: deployment + "-config"},
			MountPath:     mountPath,
		},
	}
}

// create makes the object and removes it again when the spec is over.
func create(obj client.Object) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, obj)).To(Succeed())
	DeferCleanup(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
	})
}

func runReconcile(policy string) (ctrl.Result, error) {
	r := &ConfigReloadPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	key := types.NamespacedName{Name: policy, Namespace: testNamespace}
	return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
}

func getDeployment(name string) *appsv1.Deployment {
	GinkgoHelper()
	dep := &appsv1.Deployment{}
	key := types.NamespacedName{Name: name, Namespace: testNamespace}
	Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
	return dep
}

func getPolicy(name string) *reloadv1alpha1.ConfigReloadPolicy {
	GinkgoHelper()
	pol := &reloadv1alpha1.ConfigReloadPolicy{}
	key := types.NamespacedName{Name: name, Namespace: testNamespace}
	Expect(k8sClient.Get(ctx, key, pol)).To(Succeed())
	return pol
}

func policyExists(name string) bool {
	GinkgoHelper()
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &reloadv1alpha1.ConfigReloadPolicy{})
	if err == nil {
		return true
	}
	Expect(err).To(MatchError(apierrors.IsNotFound, "a NotFound error"))
	return false
}

// readyCondition returns the Ready condition of the policy.
func readyCondition(policy string) *metav1.Condition {
	GinkgoHelper()
	c := meta.FindStatusCondition(getPolicy(policy).Status.Conditions, conditionReady)
	Expect(c).NotTo(BeNil(), "the policy has no Ready condition")
	return c
}

var _ = Describe("ConfigReloadPolicy controller", func() {
	// These specs are about what the controller reports. Some of them need a
	// Deployment that the Deployment check would turn down, so here the check
	// only warns.
	BeforeEach(func() {
		testInjector.use(FailurePolicyIgnore)
	})

	It("gets its defaults from the API server", func() {
		create(newPolicy("defaults-reload", "defaults"))
		pol := getPolicy("defaults-reload")
		Expect(pol.Spec.WorkloadRef.Kind).To(Equal("Deployment"))
		Expect(pol.Spec.Reload.Signal).To(Equal("SIGHUP"))
		Expect(pol.Spec.Reload.DebounceSeconds).To(HaveValue(BeEquivalentTo(3)))
		Expect(pol.Spec.Reload.ValidateCommand).To(BeEmpty())
	})

	It("reports Ready when the Deployment asks for the settings", func() {
		create(newDeployment("web"))
		create(newPolicy("web-reload", "web"))

		result, err := runReconcile("web-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		c := readyCondition("web-reload")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(reasonAccepted))
		Expect(c.ObservedGeneration).To(Equal(int64(1)))
	})

	It("never writes the Deployment, and writes the status only when it changes", func() {
		create(newDeployment("idle"))
		depVersion := getDeployment("idle").ResourceVersion
		create(newPolicy("idle-reload", "idle"))
		_, err := runReconcile("idle-reload")
		Expect(err).NotTo(HaveOccurred())
		polVersion := getPolicy("idle-reload").ResourceVersion

		for range 3 {
			_, err := runReconcile("idle-reload")
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(getDeployment("idle").ResourceVersion).To(Equal(depVersion), "the Deployment was written")
		Expect(getPolicy("idle-reload").ResourceVersion).To(Equal(polVersion), "the status was written again")
	})

	DescribeTable("reports what is wrong in the Ready condition and looks again later",
		func(name string, change func(dep *appsv1.Deployment, pol *reloadv1alpha1.ConfigReloadPolicy), reason string) {
			dep := newDeployment(name)
			pol := newPolicy(name+"-reload", name)
			change(dep, pol)
			create(dep)
			create(pol)

			result, err := runReconcile(pol.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(retryAfter))

			c := readyCondition(pol.Name)
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal(reason))
			Expect(c.Message).NotTo(BeEmpty())
			Expect(getDeployment(name).Generation).To(Equal(int64(1)), "the Deployment must not be touched")
		},
		Entry("the Deployment does not exist", "missing",
			func(_ *appsv1.Deployment, pol *reloadv1alpha1.ConfigReloadPolicy) {
				pol.Spec.WorkloadRef.Name = "nowhere"
			}, reasonWorkloadNotFound),
		Entry("the container is not there", "nocontainer",
			func(_ *appsv1.Deployment, pol *reloadv1alpha1.ConfigReloadPolicy) {
				pol.Spec.ContainerName = otherContainer
			}, reasonContainerNotFound),
		Entry("the Pod template does not ask for the settings", "plain",
			func(dep *appsv1.Deployment, _ *reloadv1alpha1.ConfigReloadPolicy) {
				dep.Spec.Template.Annotations = nil
			}, reasonNotOptedIn),
		Entry("the annotation is on the Deployment and not on its Pod template", "misplaced",
			func(dep *appsv1.Deployment, _ *reloadv1alpha1.ConfigReloadPolicy) {
				dep.Annotations = dep.Spec.Template.Annotations
				dep.Spec.Template.Annotations = nil
			}, reasonNotOptedIn),
	)

	It("becomes Ready once the Deployment shows up", func() {
		create(newPolicy("late-reload", "late"))
		_, err := runReconcile("late-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(readyCondition("late-reload").Reason).To(Equal(reasonWorkloadNotFound))

		create(newDeployment("late"))
		_, err = runReconcile("late-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(readyCondition("late-reload").Status).To(Equal(metav1.ConditionTrue))
	})

	DescribeTable("is rejected by the API server when a value is bad",
		func(change func(pol *reloadv1alpha1.ConfigReloadPolicy)) {
			pol := newPolicy("invalid-reload", "invalid")
			change(pol)
			err := k8sClient.Create(ctx, pol)
			Expect(err).To(MatchError(apierrors.IsInvalid, "an Invalid error"))
		},
		Entry("a signal that is not in the list", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.Reload.Signal = "SIGKILL"
		}),
		Entry("a negative quiet time", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			minus := int32(-1)
			pol.Spec.Reload.DebounceSeconds = &minus
		}),
		Entry("an empty word in the check command", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.Reload.ValidateCommand = []string{"nginx", ""}
		}),
		Entry("no workload name", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.WorkloadRef.Name = ""
		}),
		Entry("no mount path", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.MountPath = ""
		}),
		Entry("a mount path that does not start at the root", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.MountPath = "etc/nginx/conf.d"
		}),
		Entry("a mount path with a slash at the end", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.MountPath = mountPath + "/"
		}),
	)

	DescribeTable("does not let the target of a policy change",
		func(change func(pol *reloadv1alpha1.ConfigReloadPolicy), field string) {
			const name = "fixed"
			policy := name + "-reload"
			create(newDeployment(name))
			create(newPolicy(policy, name))

			pol := getPolicy(policy)
			change(pol)
			err := k8sClient.Update(ctx, pol)
			Expect(err).To(MatchError(apierrors.IsInvalid, "an Invalid error"))
			Expect(err.Error()).To(ContainSubstring(field + " cannot be changed"))

			By("leaving the stored policy as it was")
			kept := getPolicy(policy)
			Expect(kept.Spec.WorkloadRef.Name).To(Equal(name))
			Expect(kept.Spec.ContainerName).To(Equal(appContainer))
		},
		Entry("to another Deployment", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.WorkloadRef.Name = "elsewhere"
		}, "workloadRef"),
		Entry("to another container", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.ContainerName = otherContainer
		}, "containerName"),
	)

	It("still lets the other fields of a policy change", func() {
		const name = "free"
		policy := name + "-reload"
		create(newDeployment(name))
		create(newPolicy(policy, name))

		pol := getPolicy(policy)
		pol.Spec.ConfigMapRef.Name = name + "-settings"
		pol.Spec.MountPath = "/etc/nginx/templates"
		pol.Spec.Reload.Signal = sigUSR1
		Expect(k8sClient.Update(ctx, pol)).To(Succeed())
	})

	It("takes the finalizer of version 0.1 off a policy", func() {
		create(newDeployment("old"))
		pol := newPolicy("old-reload", "old")
		pol.Finalizers = []string{oldFinalizer}
		create(pol)

		_, err := runReconcile("old-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(getPolicy("old-reload").Finalizers).To(BeEmpty())
		Expect(readyCondition("old-reload").Status).To(Equal(metav1.ConditionTrue))
	})

	It("lets a policy go that was deleted while it still had that finalizer", func() {
		pol := newPolicy("stuck-reload", "stuck")
		pol.Finalizers = []string{oldFinalizer}
		create(pol)
		Expect(k8sClient.Delete(ctx, pol)).To(Succeed())
		Expect(policyExists("stuck-reload")).To(BeTrue(), "the finalizer holds it")

		_, err := runReconcile("stuck-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(policyExists("stuck-reload")).To(BeFalse())
	})
})
