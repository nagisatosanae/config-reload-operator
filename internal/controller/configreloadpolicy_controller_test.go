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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	helperMountPath = "/etc/helper"
	// otherContainer is a name no container has.
	otherContainer = "sidecar"
	mountPath      = "/etc/nginx/conf.d"
	tz             = "Asia/Seoul"
	sigUSR1        = "SIGUSR1"
	// holdFinalizer keeps an object there after it was deleted.
	holdFinalizer = "example.com/hold"
	// webhookService is the name the test webhooks are registered under.
	// envtest turns it into the address of the test server.
	webhookService = "webhook"
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

// newPolicy returns a policy for the Deployment made by newDeployment. It
// names one container, the app.
func newPolicy(name, deployment string) *reloadv1alpha1.ConfigReloadPolicy {
	return &reloadv1alpha1.ConfigReloadPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: reloadv1alpha1.ConfigReloadPolicySpec{
			WorkloadRef: reloadv1alpha1.WorkloadRef{Name: deployment},
			Containers: []reloadv1alpha1.ContainerSettings{{
				Name:         appContainer,
				ConfigMapRef: reloadv1alpha1.ConfigMapRef{Name: deployment + "-config"},
				MountPath:    mountPath,
			}},
		},
	}
}

// helperSettings returns settings for the other container of newDeployment.
func helperSettings() reloadv1alpha1.ContainerSettings {
	return reloadv1alpha1.ContainerSettings{
		Name:         helperContainer,
		ConfigMapRef: reloadv1alpha1.ConfigMapRef{Name: "helper-config"},
		MountPath:    helperMountPath,
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
		Expect(pol.Spec.Containers[0].Reload.Signal).To(Equal("SIGHUP"))
		Expect(pol.Spec.Containers[0].Reload.DebounceSeconds).To(HaveValue(BeEquivalentTo(3)))
		Expect(pol.Spec.Containers[0].Reload.ValidateCommand).To(BeEmpty())
	})

	It("is Ready before there are Pods, when the Deployment asks for the settings", func() {
		create(newDeployment("web"))
		create(newPolicy("web-reload", "web"))

		result, err := runReconcile("web-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(resyncAfter))

		c := readyCondition("web-reload")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(reasonAccepted))
		Expect(c.ObservedGeneration).To(Equal(int64(1)))
		Expect(getPolicy("web-reload").Status.Pods).To(BeZero())
	})

	It("counts the Pods that carry the settings", func() {
		create(newDeployment("full"))
		create(newPolicy("full-reload", "full"))
		create(newPod("full"))
		create(newPod("full"))

		_, err := runReconcile("full-reload")
		Expect(err).NotTo(HaveOccurred())

		status := getPolicy("full-reload").Status
		Expect(status.Pods).To(BeEquivalentTo(2))
		Expect(status.InjectedPods).To(BeEquivalentTo(2))
		c := readyCondition("full-reload")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(reasonInjected))
	})

	It("reports a Pod that runs without the settings", func() {
		create(newDeployment("partial"))
		// This Pod starts before the policy is there. The failure policy is
		// Ignore in these specs, so it starts as it was written.
		create(newPod("partial"))
		create(newPolicy("partial-reload", "partial"))
		create(newPod("partial"))

		result, err := runReconcile("partial-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(retryAfter))

		status := getPolicy("partial-reload").Status
		Expect(status.Pods).To(BeEquivalentTo(2))
		Expect(status.InjectedPods).To(BeEquivalentTo(1))
		c := readyCondition("partial-reload")
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(reasonNotInjected))
		Expect(c.Message).To(ContainSubstring("1 of 2 Pods"))
	})

	It("reports Pods that carry the settings from before the policy was changed", func() {
		create(newDeployment("aged"))
		create(newPolicy("aged-reload", "aged"))
		create(newPod("aged"))

		pol := getPolicy("aged-reload")
		pol.Spec.Containers[0].Reload.Signal = sigUSR1
		Expect(k8sClient.Update(ctx, pol)).To(Succeed())
		_, err := runReconcile("aged-reload")
		Expect(err).NotTo(HaveOccurred())

		status := getPolicy("aged-reload").Status
		Expect(status.Pods).To(BeEquivalentTo(1))
		Expect(status.InjectedPods).To(BeZero())
		Expect(readyCondition("aged-reload").Reason).To(Equal(reasonOutdated))

		By("the zero is stored, so kubectl shows 0 and not an empty column")
		raw := &unstructured.Unstructured{}
		raw.SetGroupVersionKind(reloadv1alpha1.SchemeGroupVersion.WithKind("ConfigReloadPolicy"))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "aged-reload", Namespace: testNamespace}, raw)).To(Succeed())
		_, found, err := unstructured.NestedInt64(raw.Object, "status", "injectedPods")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue(), "status.injectedPods is not in the stored object")

		By("a new Pod gets the new settings, the old one is still reported")
		create(newPod("aged"))
		_, err = runReconcile("aged-reload")
		Expect(err).NotTo(HaveOccurred())
		status = getPolicy("aged-reload").Status
		Expect(status.Pods).To(BeEquivalentTo(2))
		Expect(status.InjectedPods).To(BeEquivalentTo(1))
		Expect(readyCondition("aged-reload").Reason).To(Equal(reasonOutdated))
	})

	It("finds its Pods current when it is deleted and made again with the same settings", func() {
		create(newDeployment("again"))
		create(newPolicy("again-reload", "again"))
		create(newPod("again"))
		first := getPolicy("again-reload")

		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		Expect(policyExists("again-reload")).To(BeFalse())
		create(newPolicy("again-reload", "again"))
		Expect(getPolicy("again-reload").UID).NotTo(Equal(first.UID), "it is a new object")

		_, err := runReconcile("again-reload")
		Expect(err).NotTo(HaveOccurred())

		status := getPolicy("again-reload").Status
		Expect(status.Pods).To(BeEquivalentTo(1))
		Expect(status.InjectedPods).To(BeEquivalentTo(1))
		Expect(readyCondition("again-reload").Reason).To(Equal(reasonInjected))
	})

	It("reports Pods that carry the settings of an earlier policy with the same name", func() {
		create(newDeployment("reborn"))
		create(newPolicy("reborn-reload", "reborn"))
		create(newPod("reborn"))
		first := getPolicy("reborn-reload")

		// The policy is deleted and made again with another signal. The new
		// one starts at generation 1 again, the generation the Pod was
		// made under. What tells them apart is the hash of the settings.
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		Expect(policyExists("reborn-reload")).To(BeFalse())
		again := newPolicy("reborn-reload", "reborn")
		again.Spec.Containers[0].Reload.Signal = sigUSR1
		create(again)
		second := getPolicy("reborn-reload")
		Expect(second.Generation).To(Equal(first.Generation))
		Expect(second.UID).NotTo(Equal(first.UID))

		_, err := runReconcile("reborn-reload")
		Expect(err).NotTo(HaveOccurred())

		status := getPolicy("reborn-reload").Status
		Expect(status.Pods).To(BeEquivalentTo(1))
		Expect(status.InjectedPods).To(BeZero())
		Expect(readyCondition("reborn-reload").Reason).To(Equal(reasonOutdated))

		By("a new Pod gets the settings of the new policy and is counted")
		create(newPod("reborn"))
		_, err = runReconcile("reborn-reload")
		Expect(err).NotTo(HaveOccurred())
		status = getPolicy("reborn-reload").Status
		Expect(status.Pods).To(BeEquivalentTo(2))
		Expect(status.InjectedPods).To(BeEquivalentTo(1))
	})

	It("reports a Pod that got its settings before a container was added to the policy", func() {
		create(newDeployment("grown"))
		create(newPolicy("grown-reload", "grown"))
		create(newPod("grown"))

		pol := getPolicy("grown-reload")
		pol.Spec.Containers = append(pol.Spec.Containers, helperSettings())
		Expect(k8sClient.Update(ctx, pol)).To(Succeed())
		_, err := runReconcile("grown-reload")
		Expect(err).NotTo(HaveOccurred())

		status := getPolicy("grown-reload").Status
		Expect(status.Pods).To(BeEquivalentTo(1))
		Expect(status.InjectedPods).To(BeZero())
		Expect(readyCondition("grown-reload").Reason).To(Equal(reasonOutdated))

		By("a new Pod gets the settings of both containers and is counted")
		create(newPod("grown"))
		_, err = runReconcile("grown-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(getPolicy("grown-reload").Status.InjectedPods).To(BeEquivalentTo(1))
	})

	It("reports a Pod that an earlier version marked as outdated", func() {
		create(newDeployment("marked"))
		create(newPolicy("marked-reload", "marked"))
		pol := getPolicy("marked-reload")
		// Versions 0.2.0 and 0.2.1 wrote the name and the generation of the
		// policy, and 0.2.1 its UID too. A policy of that time has to be
		// written again for this version, so such a mark never stands for
		// the settings of now. The webhook leaves a Pod alone that has a
		// mark.
		for _, mark := range []string{"marked-reload@1", "marked-reload@1@" + string(pol.UID)} {
			pod := newPod("marked")
			pod.Annotations[InjectedAnnotation] = mark
			create(pod)
		}

		_, err := runReconcile("marked-reload")
		Expect(err).NotTo(HaveOccurred())
		status := getPolicy("marked-reload").Status
		Expect(status.Pods).To(BeEquivalentTo(2))
		Expect(status.InjectedPods).To(BeZero())
		Expect(readyCondition("marked-reload").Reason).To(Equal(reasonOutdated))
	})

	It("counts only Pods of its own Deployment that ask for the settings and are not on their way out", func() {
		create(newDeployment("picky"))
		create(newPolicy("picky-reload", "picky"))
		create(newPod("picky"))

		By("a Pod with the same labels that another Deployment made")
		stranger := newPod("picky")
		stranger.OwnerReferences[0].Name = "elsewhere-" + podHash
		create(stranger)

		By("a Pod that does not ask for the settings")
		plain := newPod("picky")
		plain.Annotations = nil
		create(plain)

		By("a Pod that is being deleted")
		leaving := newPod("picky")
		leaving.Finalizers = []string{holdFinalizer}
		create(leaving)
		Expect(k8sClient.Delete(ctx, leaving)).To(Succeed())
		DeferCleanup(func() {
			held := stored(leaving)
			held.Finalizers = nil
			Expect(k8sClient.Update(ctx, held)).To(Succeed())
		})

		_, err := runReconcile("picky-reload")
		Expect(err).NotTo(HaveOccurred())
		status := getPolicy("picky-reload").Status
		Expect(status.Pods).To(BeEquivalentTo(1))
		Expect(status.InjectedPods).To(BeEquivalentTo(1))
		Expect(readyCondition("picky-reload").Reason).To(Equal(reasonInjected))
	})

	It("never writes the Deployment or a Pod, and writes the status only when it changes", func() {
		create(newDeployment("idle"))
		depVersion := getDeployment("idle").ResourceVersion
		create(newPolicy("idle-reload", "idle"))
		pod := newPod("idle")
		create(pod)
		podVersion := stored(pod).ResourceVersion
		_, err := runReconcile("idle-reload")
		Expect(err).NotTo(HaveOccurred())
		polVersion := getPolicy("idle-reload").ResourceVersion

		for range 3 {
			_, err := runReconcile("idle-reload")
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(getDeployment("idle").ResourceVersion).To(Equal(depVersion), "the Deployment was written")
		Expect(stored(pod).ResourceVersion).To(Equal(podVersion), "the Pod was written")
		Expect(getPolicy("idle-reload").ResourceVersion).To(Equal(polVersion), "the status was written again")
	})

	It("knows which policy to look at again when a Pod comes or goes", func() {
		create(newPolicy("wake-reload", "wake"))
		create(newPolicy("other-reload", "other"))
		r := &ConfigReloadPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		requests := r.policiesForPod(ctx, newPod("wake"))
		names := make([]string, 0, len(requests))
		for _, request := range requests {
			names = append(names, request.Name)
		}
		Expect(names).To(ConsistOf("wake-reload"))

		plain := newPod("wake")
		plain.Annotations = nil
		Expect(r.policiesForPod(ctx, plain)).To(BeEmpty(), "a Pod that does not ask wakes nobody up")
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
				pol.Spec.Containers[0].Name = otherContainer
			}, reasonContainerNotFound),
		Entry("one of two containers is not there", "halfthere",
			func(_ *appsv1.Deployment, pol *reloadv1alpha1.ConfigReloadPolicy) {
				missing := helperSettings()
				missing.Name = otherContainer
				pol.Spec.Containers = append(pol.Spec.Containers, missing)
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
			pol.Spec.Containers[0].Reload.Signal = "SIGKILL"
		}),
		Entry("a negative quiet time", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			minus := int32(-1)
			pol.Spec.Containers[0].Reload.DebounceSeconds = &minus
		}),
		Entry("an empty word in the check command", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.Containers[0].Reload.ValidateCommand = []string{"nginx", ""}
		}),
		Entry("no workload name", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.WorkloadRef.Name = ""
		}),
		Entry("no mount path", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.Containers[0].MountPath = ""
		}),
		Entry("a mount path that does not start at the root", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.Containers[0].MountPath = "etc/nginx/conf.d"
		}),
		Entry("a mount path with a slash at the end", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.Containers[0].MountPath = mountPath + "/"
		}),
		Entry("no container", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			pol.Spec.Containers = nil
		}),
		Entry("the same container twice", func(pol *reloadv1alpha1.ConfigReloadPolicy) {
			again := helperSettings()
			again.Name = appContainer
			pol.Spec.Containers = append(pol.Spec.Containers, again)
		}),
	)

	It("does not let the Deployment of a policy change", func() {
		const name = "fixed"
		policy := name + "-reload"
		create(newDeployment(name))
		create(newPolicy(policy, name))

		pol := getPolicy(policy)
		pol.Spec.WorkloadRef.Name = "elsewhere"
		err := k8sClient.Update(ctx, pol)
		Expect(err).To(MatchError(apierrors.IsInvalid, "an Invalid error"))
		Expect(err.Error()).To(ContainSubstring("workloadRef cannot be changed"))

		By("leaving the stored policy as it was")
		Expect(getPolicy(policy).Spec.WorkloadRef.Name).To(Equal(name))
	})

	It("lets the containers of a policy change", func() {
		const name = "free"
		policy := name + "-reload"
		create(newDeployment(name))
		create(newPolicy(policy, name))

		By("other settings for a container")
		pol := getPolicy(policy)
		pol.Spec.Containers[0].ConfigMapRef.Name = name + "-settings"
		pol.Spec.Containers[0].MountPath = "/etc/nginx/templates"
		pol.Spec.Containers[0].Reload.Signal = sigUSR1
		Expect(k8sClient.Update(ctx, pol)).To(Succeed())

		By("one more container")
		pol = getPolicy(policy)
		pol.Spec.Containers = append(pol.Spec.Containers, helperSettings())
		Expect(k8sClient.Update(ctx, pol)).To(Succeed())
		Expect(getPolicy(policy).Spec.Containers[1].Reload.Signal).To(Equal("SIGHUP"), "the new entry gets the defaults")

		By("one container less")
		pol = getPolicy(policy)
		pol.Spec.Containers = pol.Spec.Containers[1:]
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
