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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The webhook in these tests is called by the envtest API server, the same
// way a real cluster calls it: the API server looks at the annotation first,
// and only then sends the Pod to the webhook. There is no ReplicaSet
// controller here, so the tests make the Pods a ReplicaSet would make.

// injectCondition is the rule the API server uses to pick the Pods it sends
// to the webhook. The chart has the same rule.
const injectCondition = "has(object.metadata.annotations) && " +
	"'" + InjectAnnotation + "' in object.metadata.annotations && " +
	"object.metadata.annotations['" + InjectAnnotation + "'] == 'true'"

// injectorWebhookConfig registers the webhook like the chart does.
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
				Service: &admissionv1.ServiceReference{Name: webhookService, Namespace: testNamespace, Path: &path},
			},
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create},
				Rule: admissionv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"pods"},
				},
			}},
			MatchConditions: []admissionv1.MatchCondition{{
				Name:       "has-inject-annotation",
				Expression: injectCondition,
			}},
		}},
	}
}

// policySwitch is the webhook the API server calls in these tests. It is a
// PodInjector whose failure policy a spec can change.
type policySwitch struct {
	mu     sync.Mutex
	policy string
	client client.Reader
}

var testInjector = &policySwitch{policy: FailurePolicyFail}

// injector returns a PodInjector with the failure policy of this moment.
func (s *policySwitch) injector() *PodInjector {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &PodInjector{Client: s.client, FailurePolicy: s.policy}
}

func (s *policySwitch) Default(ctx context.Context, pod *corev1.Pod) error {
	return s.injector().Default(ctx, pod)
}

// use sets the failure policy until the spec is over.
func (s *policySwitch) use(policy string) {
	s.mu.Lock()
	before := s.policy
	s.policy = policy
	s.mu.Unlock()
	DeferCleanup(func() {
		s.mu.Lock()
		s.policy = before
		s.mu.Unlock()
	})
}

// podHash stands for the hash a Deployment gives to a ReplicaSet and to the
// Pods of it.
const podHash = "5d4f8c7b9"

// newPod returns a Pod the way the ReplicaSet of a Deployment makes it: it
// has no name of its own yet, it carries the hash label and the annotation
// from the Pod template, and the ReplicaSet is its owner. The containers are
// the ones of newDeployment.
func newPod(deployment string) *corev1.Pod {
	replicaSet := deployment + "-" + podHash
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: replicaSet + "-",
			Namespace:    testNamespace,
			Labels:       map[string]string{"app": deployment, appsv1.DefaultDeploymentUniqueLabelKey: podHash},
			Annotations:  map[string]string{InjectAnnotation: "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       replicaSet,
				UID:        types.UID("11111111-2222-3333-4444-555555555555"),
				Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: helperContainer, Image: "busybox"},
			{Name: appContainer, Image: "nginx:stable-alpine"},
		}},
	}
}

// mountConfigMap gives the app container of the Pod its own mount of a
// ConfigMap, the way a user writes it.
func mountConfigMap(pod *corev1.Pod, configMap, dir string) *corev1.Pod {
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: "config",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configMap},
		}},
	})
	app := &pod.Spec.Containers[1]
	app.VolumeMounts = append(app.VolumeMounts, corev1.VolumeMount{Name: "config", MountPath: dir})
	return pod
}

// wantMark is the mark a new Pod must carry for the policy as it is now.
func wantMark(policy string) string {
	GinkgoHelper()
	return markFor(getPolicy(policy))
}

// stored reads the Pod back from the API server.
func stored(pod *corev1.Pod) *corev1.Pod {
	GinkgoHelper()
	got := &corev1.Pod{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), got)).To(Succeed())
	return got
}

// addedVolumes returns the volumes the webhook put on the Pod.
func addedVolumes(pod *corev1.Pod) []corev1.Volume {
	var added []corev1.Volume
	for _, v := range pod.Spec.Volumes {
		if strings.HasPrefix(v.Name, volumePrefix) {
			added = append(added, v)
		}
	}
	return added
}

// operatorEnv returns the env of the container that carries one of the
// operator's names.
func operatorEnv(container corev1.Container) []corev1.EnvVar {
	var found []corev1.EnvVar
	for _, e := range container.Env {
		if slices.Contains(ownedEnv, e.Name) {
			found = append(found, e)
		}
	}
	return found
}

// expectUntouched checks that the webhook put nothing on the Pod.
func expectUntouched(pod *corev1.Pod) {
	GinkgoHelper()
	Expect(addedVolumes(pod)).To(BeEmpty(), "a volume was added")
	Expect(pod.Annotations).NotTo(HaveKey(InjectedAnnotation), "the Pod is marked as injected")
}

var defaultEnv = []corev1.EnvVar{
	{Name: envWatchDir, Value: mountPath},
	{Name: envSignal, Value: "SIGHUP"},
	{Name: envDebounce, Value: "3"},
}

// A problem is one way a Pod with the annotation cannot get its settings.
type problem struct {
	what string
	// name is the Deployment the Pod belongs to.
	name string
	// prepare makes the policies and changes the Pod.
	prepare func(pod *corev1.Pod)
	// message is a part of what the user is told.
	message string
	// podOnly is true for a problem that a Deployment cannot have.
	podOnly bool
}

var problems = []problem{
	{
		what: "no policy points at its Deployment", name: "alone",
		prepare: func(*corev1.Pod) { create(newPolicy("alone-reload", "someone-else")) },
		message: `no ConfigReloadPolicy in namespace "default" points at Deployment "alone"`,
	},
	{
		what: "the policy names a container it does not have", name: "odd",
		prepare: func(*corev1.Pod) {
			pol := newPolicy("odd-reload", "odd")
			pol.Spec.Containers[0].Name = otherContainer
			create(pol)
		},
		message: `the Pod has no container named "sidecar"`,
	},
	{
		what: "the container already sets a name that belongs to the operator", name: "taken",
		prepare: func(pod *corev1.Pod) {
			create(newPolicy("taken-reload", "taken"))
			pod.Spec.Containers[1].Env = []corev1.EnvVar{{Name: envWatchDir, Value: "/mine"}}
		},
		message: "already sets " + envWatchDir,
	},
	{
		what: "something else is mounted at the path", name: "busy",
		prepare: func(pod *corev1.Pod) {
			create(newPolicy("busy-reload", "busy"))
			mountConfigMap(pod, "another-config", mountPath)
		},
		message: "already mounts something else at " + mountPath,
	},
	{
		what: "the ConfigMap is mounted at the path with subPath", name: "sub",
		prepare: func(pod *corev1.Pod) {
			create(newPolicy("sub-reload", "sub"))
			mountConfigMap(pod, "sub-config", mountPath)
			pod.Spec.Containers[1].VolumeMounts[0].SubPath = "default.conf"
		},
		message: "with subPath",
	},
	{
		what: "one of the two containers of the policy cannot get its settings", name: "half",
		prepare: func(pod *corev1.Pod) {
			pol := newPolicy("half-reload", "half")
			pol.Spec.Containers = append(pol.Spec.Containers, helperSettings())
			create(pol)
			pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: envSignal, Value: "mine"}}
		},
		message: "already sets " + envSignal,
	},
	{
		what: "two policies point at its Deployment", name: "twice",
		prepare: func(*corev1.Pod) {
			create(newPolicy("twice-a", "twice"))
			// The check of a new policy does not let this happen. It is
			// off here, as when two policies are made at the same moment.
			testPolicyCheck.off()
			create(newPolicy("twice-b", "twice"))
		},
		message: `both point at Deployment "twice"`,
	},
	{
		what: "it does not belong to a Deployment", name: "bare",
		prepare: func(pod *corev1.Pod) {
			create(newPolicy("bare-reload", "bare"))
			pod.OwnerReferences = nil
		},
		message: "it does not belong to a Deployment",
		podOnly: true,
	},
}

// problemEntries turns problems into the entries of a table. With
// forDeployments, the ones that only a Pod can have are left out.
func problemEntries(forDeployments bool) []TableEntry {
	entries := make([]TableEntry, 0, len(problems))
	for _, p := range problems {
		if forDeployments && p.podOnly {
			continue
		}
		entries = append(entries, Entry(p.what, p))
	}
	return entries
}

var _ = Describe("Pod webhook", func() {
	It("puts the volume, the mount and the env on a new Pod", func() {
		userEnv := []corev1.EnvVar{{Name: "TZ", Value: tz}}
		create(newPolicy("early-reload", "early"))
		pod := newPod("early")
		pod.Spec.Containers[1].Env = userEnv
		create(pod)

		got := stored(pod)
		app := got.Spec.Containers[1]
		Expect(app.Env).To(Equal(slices.Concat(userEnv, defaultEnv)), "user env stays, watcher env is added")

		volumes := addedVolumes(got)
		Expect(volumes).To(HaveLen(1))
		Expect(volumes[0].ConfigMap).NotTo(BeNil())
		Expect(volumes[0].ConfigMap.Name).To(Equal("early-config"))
		Expect(app.VolumeMounts).To(ContainElement(
			corev1.VolumeMount{Name: volumes[0].Name, MountPath: mountPath, ReadOnly: true}))

		helper := got.Spec.Containers[0]
		Expect(helper.Env).To(BeEmpty(), "the other container is left alone")
		for _, m := range helper.VolumeMounts {
			Expect(m.Name).NotTo(Equal(volumes[0].Name), "the other container is left alone")
		}
		Expect(got.Annotations).To(HaveKeyWithValue(InjectedAnnotation, wantMark("early-reload")))
	})

	It("carries the reload settings of the policy", func() {
		pol := newPolicy("tuned-reload", "tuned")
		ten := int32(10)
		pol.Spec.Containers[0].Reload.Signal = sigUSR1
		pol.Spec.Containers[0].Reload.DebounceSeconds = &ten
		pol.Spec.Containers[0].Reload.ValidateCommand = []string{"nginx", "-t"}
		create(pol)
		pod := newPod("tuned")
		create(pod)

		Expect(stored(pod).Spec.Containers[1].Env).To(Equal([]corev1.EnvVar{
			{Name: envWatchDir, Value: mountPath},
			{Name: envSignal, Value: sigUSR1},
			{Name: envDebounce, Value: "10"},
			{Name: envValidate, Value: `["nginx","-t"]`},
		}))
	})

	It("uses a mount of the same ConfigMap that is already at the path", func() {
		create(newPolicy("own-reload", "own"))
		pod := mountConfigMap(newPod("own"), "own-config", mountPath)
		create(pod)

		got := stored(pod)
		Expect(addedVolumes(got)).To(BeEmpty(), "the user's own volume is enough")
		Expect(got.Spec.Containers[1].Env).To(Equal(defaultEnv))
		Expect(got.Annotations).To(HaveKey(InjectedAnnotation))
	})

	It("gives every container of the policy its own settings", func() {
		pol := newPolicy("pair-reload", "pair")
		pol.Spec.Containers = append(pol.Spec.Containers, helperSettings())
		create(pol)
		pod := newPod("pair")
		create(pod)

		got := stored(pod)
		Expect(addedVolumes(got)).To(HaveLen(2))
		Expect(got.Spec.Containers[0].Env[0]).To(Equal(corev1.EnvVar{Name: envWatchDir, Value: helperMountPath}))
		Expect(got.Spec.Containers[1].Env[0]).To(Equal(corev1.EnvVar{Name: envWatchDir, Value: mountPath}))

		By("the mark names both containers, in the order of their names")
		entries := getPolicy("pair-reload").Spec.Containers
		Expect(got.Annotations).To(HaveKeyWithValue(InjectedAnnotation,
			helperContainer+"="+settingsHash(&entries[1])+","+appContainer+"="+settingsHash(&entries[0])))
	})

	DescribeTable("rejects a Pod that cannot get its settings, and says why",
		func(p problem) {
			pod := newPod(p.name)
			p.prepare(pod)

			err := k8sClient.Create(ctx, pod)
			Expect(err).To(MatchError(apierrors.IsForbidden, "a Forbidden error"))
			Expect(err.Error()).To(ContainSubstring(p.message))
			Expect(err.Error()).To(ContainSubstring(InjectAnnotation), "the message names the annotation")
		},
		problemEntries(false),
	)

	DescribeTable("lets the same Pod start as it was written when the failure policy is Ignore",
		func(p problem) {
			testInjector.use(FailurePolicyIgnore)
			pod := newPod(p.name)
			p.prepare(pod)
			written := pod.DeepCopy()
			create(pod)

			got := stored(pod)
			expectUntouched(got)
			Expect(got.Spec.Containers[1].Env).To(Equal(written.Spec.Containers[1].Env))
			Expect(got.Spec.Containers[1].VolumeMounts).To(HaveLen(len(written.Spec.Containers[1].VolumeMounts)))
		},
		problemEntries(false),
	)

	DescribeTable("is not sent to the webhook without the annotation",
		func(annotations map[string]string) {
			create(newPolicy("plain-reload", "plain"))
			pod := newPod("plain")
			pod.Annotations = annotations
			create(pod)

			got := stored(pod)
			expectUntouched(got)
			Expect(operatorEnv(got.Spec.Containers[1])).To(BeEmpty())
		},
		Entry("no annotation at all", nil),
		Entry("the annotation set to false", map[string]string{InjectAnnotation: "false"}),
	)

	It("takes a Pod that already carries the settings as it is", func() {
		// A copy of a Pod that went through the webhook, made again by hand.
		create(newPolicy("copy-reload", "copy"))
		pod := newPod("copy")
		pod.Annotations[InjectedAnnotation] = "copy-reload@1"
		pod.Spec.Containers[1].Env = slices.Clone(defaultEnv)
		create(pod)

		got := stored(pod)
		Expect(got.Spec.Containers[1].Env).To(Equal(defaultEnv), "nothing is added twice")
		Expect(addedVolumes(got)).To(BeEmpty())
	})

	It("leaves a Pod that runs alone when its policy is deleted", func() {
		pol := newPolicy("leaving-reload", "leaving")
		create(pol)
		pod := newPod("leaving")
		create(pod)
		before := stored(pod)
		Expect(operatorEnv(before.Spec.Containers[1])).To(Equal(defaultEnv))

		Expect(k8sClient.Delete(ctx, pol)).To(Succeed())
		_, err := runReconcile("leaving-reload")
		Expect(err).NotTo(HaveOccurred())
		Expect(policyExists("leaving-reload")).To(BeFalse())
		Expect(stored(pod).ResourceVersion).To(Equal(before.ResourceVersion), "the Pod was written")
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
		"path":                     "path: " + injectPath,
		"rule":                     injectCondition,
		"path of the check":        "path: " + checkPath,
		"path of the policy check": "path: " + policyCheckPath,
		"rule of the check":        checkCondition,
		"rule for deleted objects": `expression: "` + notDeletedCondition + `"`,
	} {
		if !strings.Contains(chart, want) {
			t.Errorf("the chart does not have the %s the webhook uses: %s", what, want)
		}
	}
}
