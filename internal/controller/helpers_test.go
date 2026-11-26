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
	"errors"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
)

// These are plain Go tests for the parts of the webhook that never talk to
// the API server. They need no envtest, so they also run with
// "go test -short".

func ev(name, value string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, Value: value}
}

func TestDeploymentOf(t *testing.T) {
	tests := []struct {
		name   string
		change func(pod *corev1.Pod)
		want   string
	}{
		{
			name: "a Pod of a Deployment",
			want: "web",
		},
		{
			name: "a Deployment name with dashes in it",
			change: func(pod *corev1.Pod) {
				pod.OwnerReferences[0].Name = "my-web-app-" + podHash
			},
			want: "my-web-app",
		},
		{
			name:   "no owner",
			change: func(pod *corev1.Pod) { pod.OwnerReferences = nil },
		},
		{
			name:   "the owner is not a ReplicaSet",
			change: func(pod *corev1.Pod) { pod.OwnerReferences[0].Kind = "StatefulSet" },
		},
		{
			name:   "the owner is not the controller of the Pod",
			change: func(pod *corev1.Pod) { pod.OwnerReferences[0].Controller = nil },
		},
		{
			name:   "no hash label",
			change: func(pod *corev1.Pod) { delete(pod.Labels, appsv1.DefaultDeploymentUniqueLabelKey) },
		},
		{
			name:   "a ReplicaSet that no Deployment made",
			change: func(pod *corev1.Pod) { pod.OwnerReferences[0].Name = "web" },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := newPod("web")
			if tc.change != nil {
				tc.change(pod)
			}
			got, err := deploymentOf(pod)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if (err == nil) != (tc.want != "") {
				t.Fatalf("error = %v, but the name is %q", err, got)
			}
		})
	}
}

func TestSettingsHash(t *testing.T) {
	base := &newPolicy("web-reload", "web").Spec.Containers[0]

	// The value is pinned. A Pod carries it for as long as it runs, so a
	// change in how the hash is made would show every running Pod as
	// outdated after an upgrade.
	if got, want := settingsHash(base), "f7cb8bccb"; got != want {
		t.Fatalf("settingsHash = %q, want %q", got, want)
	}

	// The same for an entry as the API server gives it back: with the
	// defaults filled in, and with a check command. It is the entry of the
	// sample policy, and the value is the one its Pods carry.
	three := int32(3)
	full := &reloadv1alpha1.ContainerSettings{
		Name:         appContainer,
		ConfigMapRef: reloadv1alpha1.ConfigMapRef{Name: "nginx-config"},
		MountPath:    mountPath,
		Reload: reloadv1alpha1.ReloadSpec{
			Signal:          "SIGHUP",
			DebounceSeconds: &three,
			ValidateCommand: strings.Fields("nginx -t"),
		},
	}
	if got, want := settingsHash(full), "8476bb7d6b"; got != want {
		t.Fatalf("settingsHash of an entry with reload settings = %q, want %q", got, want)
	}

	debounce := int32(9)
	changes := map[string]func(*reloadv1alpha1.ContainerSettings){
		"the container":     func(c *reloadv1alpha1.ContainerSettings) { c.Name = helperContainer },
		"the ConfigMap":     func(c *reloadv1alpha1.ContainerSettings) { c.ConfigMapRef.Name = "other-config" },
		"the path":          func(c *reloadv1alpha1.ContainerSettings) { c.MountPath = "/etc/other" },
		"the signal":        func(c *reloadv1alpha1.ContainerSettings) { c.Reload.Signal = sigUSR1 },
		"the quiet time":    func(c *reloadv1alpha1.ContainerSettings) { c.Reload.DebounceSeconds = &debounce },
		"the check command": func(c *reloadv1alpha1.ContainerSettings) { c.Reload.ValidateCommand = []string{"haproxy", "-c"} },
	}
	for name, change := range changes {
		changed := newPolicy("web-reload", "web").Spec.Containers[0]
		change(&changed)
		if settingsHash(&changed) == settingsHash(base) {
			t.Errorf("an entry with another value for %s has the same hash", name)
		}
	}
}

func TestMarkFor(t *testing.T) {
	policy := newPolicy("web-reload", "web")
	app := policy.Spec.Containers[0]
	if got, want := markFor(policy), appContainer+"="+settingsHash(&app); got != want {
		t.Fatalf("markFor = %q, want %q", got, want)
	}

	// The name, the UID and the generation of the policy are not in the mark.
	same := newPolicy("another-name", "web")
	same.UID, same.Generation = "0b9d2c8e-6f1a-4d37-9a55-3c1e7f2b8a40", 7
	if markFor(same) != markFor(policy) {
		t.Fatal("two policies that say the same about the container have different marks")
	}

	// Two containers come in the order of their names, whatever the order in
	// the policy is.
	helper := helperSettings()
	want := helperContainer + "=" + settingsHash(&helper) + "," + appContainer + "=" + settingsHash(&app)
	policy.Spec.Containers = []reloadv1alpha1.ContainerSettings{app, helper}
	if got := markFor(policy); got != want {
		t.Fatalf("markFor = %q, want %q", got, want)
	}
	policy.Spec.Containers = []reloadv1alpha1.ContainerSettings{helper, app}
	if got := markFor(policy); got != want {
		t.Fatalf("with the containers the other way round markFor = %q, want %q", got, want)
	}
}

// onePolicy is a reader that knows one policy and nothing else.
type onePolicy struct {
	policy reloadv1alpha1.ConfigReloadPolicy
}

func (onePolicy) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("not used")
}

func (o onePolicy) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	policies, ok := list.(*reloadv1alpha1.ConfigReloadPolicyList)
	if !ok {
		return errors.New("only policies can be listed")
	}
	policies.Items = []reloadv1alpha1.ConfigReloadPolicy{o.policy}
	return nil
}

// A policy that was stored by an earlier version has no list of containers.
// The API server does not let such a policy in any more, so the test hands
// it to the webhook directly.
func TestInjectForTurnsDownAPolicyOfAnEarlierVersion(t *testing.T) {
	old := newPolicy("web-reload", "web")
	old.Spec.Containers = nil
	injector := &PodInjector{Client: onePolicy{policy: *old}}

	_, err := injector.injectFor(context.Background(), testNamespace, "web", newPod("web"))
	if err == nil || !strings.Contains(err.Error(), "names no container") {
		t.Fatalf("err = %v, want the reason that the policy names no container", err)
	}
}

func TestEnsureMountAddsAVolumeWithANewName(t *testing.T) {
	pol := newPolicy("web-reload", "web")
	pod := newPod("web")
	// The Pod already has a volume whose name starts like ours.
	pod.Spec.Volumes = []corev1.Volume{{Name: volumePrefix + "mine"}}
	app := &pod.Spec.Containers[1]

	if err := ensureMount(pod, app, &pol.Spec.Containers[0]); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Volumes) != 2 {
		t.Fatalf("the Pod has %d volumes, want 2", len(pod.Spec.Volumes))
	}
	added := pod.Spec.Volumes[1]
	if !strings.HasPrefix(added.Name, volumePrefix) || added.Name == pod.Spec.Volumes[0].Name {
		t.Fatalf("the new volume is named %q", added.Name)
	}
	if added.ConfigMap == nil || added.ConfigMap.Name != "web-config" {
		t.Fatalf("the new volume is %+v, want the ConfigMap web-config", added.VolumeSource)
	}
	want := []corev1.VolumeMount{{Name: added.Name, MountPath: mountPath, ReadOnly: true}}
	if !reflect.DeepEqual(app.VolumeMounts, want) {
		t.Fatalf("mounts = %v, want %v", app.VolumeMounts, want)
	}
}

func TestEnsureMountKnowsThePathWithASlashAtTheEnd(t *testing.T) {
	pol := newPolicy("web-reload", "web")
	pod := mountConfigMap(newPod("web"), "web-config", mountPath+"/")

	if err := ensureMount(pod, &pod.Spec.Containers[1], &pol.Spec.Containers[0]); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Volumes) != 1 {
		t.Fatalf("the Pod has %d volumes, want only the user's own", len(pod.Spec.Volumes))
	}
}

func TestWatcherEnv(t *testing.T) {
	c := reloadv1alpha1.ContainerSettings{MountPath: "/d"}

	// Nothing else set, as when the object never went through the API
	// server: the safety-net defaults apply.
	got := watcherEnv(&c)
	want := []corev1.EnvVar{ev(envWatchDir, "/d"), ev(envSignal, "SIGHUP"), ev(envDebounce, "3")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults: got %v, want %v", got, want)
	}

	// Everything set. The check command becomes JSON, so a quote inside a
	// word survives the trip through one env string.
	ten := int32(10)
	c.Reload = reloadv1alpha1.ReloadSpec{
		Signal:          sigUSR1,
		DebounceSeconds: &ten,
		ValidateCommand: []string{"sh", "-c", `echo "a b"`},
	}
	got = watcherEnv(&c)
	want = []corev1.EnvVar{
		ev(envWatchDir, "/d"),
		ev(envSignal, sigUSR1),
		ev(envDebounce, "10"),
		ev(envValidate, `["sh","-c","echo \"a b\""]`),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("all set: got %v, want %v", got, want)
	}
}
