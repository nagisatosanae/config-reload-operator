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
	"slices"

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
// defaults and rejects bad values for real, and Reconcile patches a real
// Deployment, but no Pod is ever started.

const (
	testNamespace = "default"
	appContainer  = "nginx"
	mountPath     = "/etc/nginx/conf.d"
)

// newDeployment returns a Deployment like the demo: a helper container
// first, then the app container "nginx" that mounts the ConfigMap
// "<name>-config" at mountPath. env goes on the app container.
func newDeployment(name string, env ...corev1.EnvVar) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	replicas := int32(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "helper", Image: "busybox"},
						{
							Name:         appContainer,
							Image:        "nginx:stable-alpine",
							Env:          env,
							VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: mountPath}},
						},
					},
					Volumes: []corev1.Volume{{
						Name: "config",
						VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: name + "-config"},
						}},
					}},
				},
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

// appEnv returns the env of the app container.
func appEnv(dep *appsv1.Deployment) []corev1.EnvVar {
	return dep.Spec.Template.Spec.Containers[1].Env
}

// readyCondition returns the Ready condition of the policy.
func readyCondition(policy string) *metav1.Condition {
	GinkgoHelper()
	c := meta.FindStatusCondition(getPolicy(policy).Status.Conditions, conditionReady)
	Expect(c).NotTo(BeNil(), "the policy has no Ready condition")
	return c
}

var defaultEnv = []corev1.EnvVar{
	{Name: envWatchDir, Value: mountPath},
	{Name: envSignal, Value: "SIGHUP"},
	{Name: envDebounce, Value: "3"},
}

var _ = Describe("ConfigReloadPolicy controller", func() {
	It("gets its defaults from the API server", func() {
		create(newPolicy("defaults-reload", "defaults"))
		pol := getPolicy("defaults-reload")
		Expect(pol.Spec.WorkloadRef.Kind).To(Equal("Deployment"))
		Expect(pol.Spec.Reload.Signal).To(Equal("SIGHUP"))
		Expect(pol.Spec.Reload.DebounceSeconds).To(HaveValue(BeEquivalentTo(3)))
		Expect(pol.Spec.Reload.ValidateCommand).To(BeEmpty())
	})

	It("sets the watcher env on the named container and reports Ready", func() {
		userEnv := []corev1.EnvVar{
			{Name: "TZ", Value: "Asia/Seoul"},
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"},
			}},
		}
		create(newDeployment("web", userEnv...))
		create(newPolicy("web-reload", "web"))

		result, err := runReconcile("web-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		dep := getDeployment("web")
		Expect(appEnv(dep)).To(Equal(slices.Concat(userEnv, defaultEnv)), "user env stays, watcher env is added")
		Expect(dep.Spec.Template.Spec.Containers[0].Env).To(BeEmpty(), "the other container is left alone")
		Expect(dep.Generation).To(Equal(int64(2)), "one write")

		c := readyCondition("web-reload")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(reasonConfigured))
		Expect(c.ObservedGeneration).To(Equal(int64(1)))
	})

	It("writes nothing when everything is already right", func() {
		create(newDeployment("idle"))
		create(newPolicy("idle-reload", "idle"))
		_, err := runReconcile("idle-reload")
		Expect(err).NotTo(HaveOccurred())
		depVersion := getDeployment("idle").ResourceVersion
		polVersion := getPolicy("idle-reload").ResourceVersion

		for range 3 {
			_, err := runReconcile("idle-reload")
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(getDeployment("idle").ResourceVersion).To(Equal(depVersion), "the Deployment was written again")
		Expect(getPolicy("idle-reload").ResourceVersion).To(Equal(polVersion), "the status was written again")
	})

	It("fixes watcher env that was set by hand", func() {
		create(newDeployment("manual", corev1.EnvVar{Name: envWatchDir, Value: "/somewhere/else"}))
		create(newPolicy("manual-reload", "manual"))
		_, err := runReconcile("manual-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(appEnv(getDeployment("manual"))).To(Equal(defaultEnv))
	})

	It("follows the policy when it changes, and removes what is no longer asked for", func() {
		create(newDeployment("change"))
		create(newPolicy("change-reload", "change"))
		_, err := runReconcile("change-reload")
		Expect(err).NotTo(HaveOccurred())

		By("asking for another signal, a longer quiet time and a check command")
		pol := getPolicy("change-reload")
		ten := int32(10)
		pol.Spec.Reload.Signal = "SIGUSR2"
		pol.Spec.Reload.DebounceSeconds = &ten
		pol.Spec.Reload.ValidateCommand = []string{"nginx", "-t"}
		Expect(k8sClient.Update(ctx, pol)).To(Succeed())
		_, err = runReconcile("change-reload")
		Expect(err).NotTo(HaveOccurred())
		changed := []corev1.EnvVar{
			{Name: envWatchDir, Value: mountPath},
			{Name: envSignal, Value: "SIGUSR2"},
			{Name: envDebounce, Value: "10"},
			{Name: envValidate, Value: `["nginx","-t"]`},
		}
		Expect(appEnv(getDeployment("change"))).To(Equal(changed))

		By("dropping the check command again")
		pol = getPolicy("change-reload")
		pol.Spec.Reload.ValidateCommand = nil
		Expect(k8sClient.Update(ctx, pol)).To(Succeed())
		_, err = runReconcile("change-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(appEnv(getDeployment("change"))).To(Equal(changed[:3]), "the check command env is gone")

		pol = getPolicy("change-reload")
		Expect(pol.Generation).To(Equal(int64(3)))
		Expect(readyCondition("change-reload").ObservedGeneration).To(Equal(pol.Generation))
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
				pol.Spec.ContainerName = "sidecar"
			}, reasonContainerNotFound),
		Entry("the ConfigMap is not mounted", "nomount",
			func(dep *appsv1.Deployment, _ *reloadv1alpha1.ConfigReloadPolicy) {
				dep.Spec.Template.Spec.Volumes[0].ConfigMap.Name = "other-config"
			}, reasonConfigMapNotMounted),
		Entry("the ConfigMap is mounted with subPath", "subpath",
			func(dep *appsv1.Deployment, _ *reloadv1alpha1.ConfigReloadPolicy) {
				dep.Spec.Template.Spec.Containers[1].VolumeMounts[0].SubPath = "default.conf"
			}, reasonSubPathNotSupported),
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
		Expect(appEnv(getDeployment("late"))).To(Equal(defaultEnv))
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
	)
})
