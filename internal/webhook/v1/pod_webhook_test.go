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

package v1

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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nagisatosanae/config-reload-operator/internal/settings"
	"github.com/nagisatosanae/config-reload-operator/internal/testutil"
	webhookv1alpha1 "github.com/nagisatosanae/config-reload-operator/internal/webhook/v1alpha1"
)

// The webhook in these tests is called by the envtest API server, the same
// way a real cluster calls it: the API server looks at the annotation first,
// and only then sends the Pod to the webhook. There is no ReplicaSet
// controller here, so the tests make the Pods a ReplicaSet would make.

// policySwitch is the webhook the API server calls in these tests. It is a
// PodDefaulter whose failure policy a spec can change.
type policySwitch struct {
	mu     sync.Mutex
	policy string
	client client.Reader
}

var testDefaulter = &policySwitch{policy: FailurePolicyFail}

// defaulter returns a PodDefaulter with the failure policy of this moment.
func (s *policySwitch) defaulter() *PodDefaulter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &PodDefaulter{Client: s.client, FailurePolicy: s.policy}
}

func (s *policySwitch) Default(ctx context.Context, pod *corev1.Pod) error {
	return s.defaulter().Default(ctx, pod)
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

// wantMark is the mark a new Pod must carry for the policy as it is now.
func wantMark(policy string) string {
	GinkgoHelper()
	return settings.MarkFor(getPolicy(policy))
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
	Expect(pod.Annotations).NotTo(HaveKey(settings.InjectedAnnotation), "the Pod is marked as injected")
}

var defaultEnv = []corev1.EnvVar{
	{Name: envWatchDir, Value: testutil.MountPath},
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
		prepare: func(*corev1.Pod) { create(testutil.NewPolicy("alone-reload", "someone-else")) },
		message: `no ConfigReloadPolicy in namespace "default" points at Deployment "alone"`,
	},
	{
		what: "the policy names a container it does not have", name: "odd",
		prepare: func(*corev1.Pod) {
			pol := testutil.NewPolicy("odd-reload", "odd")
			pol.Spec.Containers[0].Name = testutil.OtherContainer
			create(pol)
		},
		message: `the Pod has no container named "sidecar"`,
	},
	{
		what: "the container already sets a name that belongs to the operator", name: "taken",
		prepare: func(pod *corev1.Pod) {
			create(testutil.NewPolicy("taken-reload", "taken"))
			pod.Spec.Containers[1].Env = []corev1.EnvVar{{Name: envWatchDir, Value: "/mine"}}
		},
		message: "already sets " + envWatchDir,
	},
	{
		what: "something else is mounted at the path", name: "busy",
		prepare: func(pod *corev1.Pod) {
			create(testutil.NewPolicy("busy-reload", "busy"))
			testutil.MountConfigMap(pod, "another-config", testutil.MountPath)
		},
		message: "already mounts something else at " + testutil.MountPath,
	},
	{
		what: "the ConfigMap is mounted at the path with subPath", name: "sub",
		prepare: func(pod *corev1.Pod) {
			create(testutil.NewPolicy("sub-reload", "sub"))
			testutil.MountConfigMap(pod, "sub-config", testutil.MountPath)
			pod.Spec.Containers[1].VolumeMounts[0].SubPath = "default.conf"
		},
		message: "with subPath",
	},
	{
		what: "one of the two containers of the policy cannot get its settings", name: "half",
		prepare: func(pod *corev1.Pod) {
			pol := testutil.NewPolicy("half-reload", "half")
			pol.Spec.Containers = append(pol.Spec.Containers, testutil.HelperSettings())
			create(pol)
			pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: envSignal, Value: "mine"}}
		},
		message: "already sets " + envSignal,
	},
	{
		what: "two policies point at its Deployment", name: "twice",
		prepare: func(*corev1.Pod) {
			create(testutil.NewPolicy("twice-a", "twice"))
			// The check of a new policy does not let this happen. It is not
			// part of this suite, so the second policy gets in, as when two
			// policies are made at the same moment.
			create(testutil.NewPolicy("twice-b", "twice"))
		},
		message: `both point at Deployment "twice"`,
	},
	{
		what: "it does not belong to a Deployment", name: "bare",
		prepare: func(pod *corev1.Pod) {
			create(testutil.NewPolicy("bare-reload", "bare"))
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
		userEnv := []corev1.EnvVar{{Name: "TZ", Value: "Asia/Seoul"}}
		create(testutil.NewPolicy("early-reload", "early"))
		pod := testutil.NewPod("early")
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
			corev1.VolumeMount{Name: volumes[0].Name, MountPath: testutil.MountPath, ReadOnly: true}))

		helper := got.Spec.Containers[0]
		Expect(helper.Env).To(BeEmpty(), "the other container is left alone")
		for _, m := range helper.VolumeMounts {
			Expect(m.Name).NotTo(Equal(volumes[0].Name), "the other container is left alone")
		}
		Expect(got.Annotations).To(HaveKeyWithValue(settings.InjectedAnnotation, wantMark("early-reload")))
	})

	It("carries the reload settings of the policy", func() {
		pol := testutil.NewPolicy("tuned-reload", "tuned")
		ten := int32(10)
		pol.Spec.Containers[0].Reload.Signal = testutil.SigUSR1
		pol.Spec.Containers[0].Reload.DebounceSeconds = &ten
		pol.Spec.Containers[0].Reload.ValidateCommand = []string{"nginx", "-t"}
		create(pol)
		pod := testutil.NewPod("tuned")
		create(pod)

		Expect(stored(pod).Spec.Containers[1].Env).To(Equal([]corev1.EnvVar{
			{Name: envWatchDir, Value: testutil.MountPath},
			{Name: envSignal, Value: testutil.SigUSR1},
			{Name: envDebounce, Value: "10"},
			{Name: envValidate, Value: `["nginx","-t"]`},
		}))
	})

	It("uses a mount of the same ConfigMap that is already at the path", func() {
		create(testutil.NewPolicy("own-reload", "own"))
		pod := testutil.MountConfigMap(testutil.NewPod("own"), "own-config", testutil.MountPath)
		create(pod)

		got := stored(pod)
		Expect(addedVolumes(got)).To(BeEmpty(), "the user's own volume is enough")
		Expect(got.Spec.Containers[1].Env).To(Equal(defaultEnv))
		Expect(got.Annotations).To(HaveKey(settings.InjectedAnnotation))
	})

	It("gives every container of the policy its own settings", func() {
		pol := testutil.NewPolicy("pair-reload", "pair")
		pol.Spec.Containers = append(pol.Spec.Containers, testutil.HelperSettings())
		create(pol)
		pod := testutil.NewPod("pair")
		create(pod)

		got := stored(pod)
		Expect(addedVolumes(got)).To(HaveLen(2))
		Expect(got.Spec.Containers[0].Env[0]).To(Equal(corev1.EnvVar{Name: envWatchDir, Value: testutil.HelperMountPath}))
		Expect(got.Spec.Containers[1].Env[0]).To(Equal(corev1.EnvVar{Name: envWatchDir, Value: testutil.MountPath}))

		By("the mark names both containers, in the order of their names")
		entries := getPolicy("pair-reload").Spec.Containers
		Expect(got.Annotations).To(HaveKeyWithValue(settings.InjectedAnnotation,
			testutil.HelperContainer+"="+settings.Hash(&entries[1])+","+testutil.AppContainer+"="+settings.Hash(&entries[0])))
	})

	DescribeTable("rejects a Pod that cannot get its settings, and says why",
		func(p problem) {
			pod := testutil.NewPod(p.name)
			p.prepare(pod)

			err := k8sClient.Create(ctx, pod)
			Expect(err).To(MatchError(apierrors.IsForbidden, "a Forbidden error"))
			Expect(err.Error()).To(ContainSubstring(p.message))
			Expect(err.Error()).To(ContainSubstring(settings.InjectAnnotation), "the message names the annotation")
		},
		problemEntries(false),
	)

	DescribeTable("lets the same Pod start as it was written when the failure policy is Ignore",
		func(p problem) {
			testDefaulter.use(FailurePolicyIgnore)
			pod := testutil.NewPod(p.name)
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
			create(testutil.NewPolicy("plain-reload", "plain"))
			pod := testutil.NewPod("plain")
			pod.Annotations = annotations
			create(pod)

			got := stored(pod)
			expectUntouched(got)
			Expect(operatorEnv(got.Spec.Containers[1])).To(BeEmpty())
		},
		Entry("no annotation at all", nil),
		Entry("the annotation set to false", map[string]string{settings.InjectAnnotation: "false"}),
	)

	It("takes a Pod that already carries the settings as it is", func() {
		// A copy of a Pod that went through the webhook, made again by hand.
		create(testutil.NewPolicy("copy-reload", "copy"))
		pod := testutil.NewPod("copy")
		pod.Annotations[settings.InjectedAnnotation] = "copy-reload@1"
		pod.Spec.Containers[1].Env = slices.Clone(defaultEnv)
		create(pod)

		got := stored(pod)
		Expect(got.Spec.Containers[1].Env).To(Equal(defaultEnv), "nothing is added twice")
		Expect(addedVolumes(got)).To(BeEmpty())
	})

	It("leaves a Pod that runs alone when its policy is deleted", func() {
		pol := testutil.NewPolicy("leaving-reload", "leaving")
		create(pol)
		pod := testutil.NewPod("leaving")
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
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "config-reload-operator", "templates", "webhook.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	chart := strings.Join(strings.Fields(string(raw)), " ")

	for what, want := range map[string]string{
		"path":                     "path: " + PodWebhookPath,
		"rule":                     testutil.InjectCondition,
		"path of the check":        "path: " + DeploymentWebhookPath,
		"path of the policy check": "path: " + webhookv1alpha1.ConfigReloadPolicyWebhookPath,
		"rule of the check":        checkCondition,
		"rule for deleted objects": `expression: "` + notDeletedCondition + `"`,
	} {
		if !strings.Contains(chart, want) {
			t.Errorf("the chart does not have the %s the webhook uses: %s", what, want)
		}
	}
}
