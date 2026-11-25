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
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

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

func TestInjectedMark(t *testing.T) {
	const uid = "0b9d2c8e-6f1a-4d37-9a55-3c1e7f2b8a40"
	tests := []struct {
		name    string
		mark    string
		wantUID string
		want    int64
		found   bool
	}{
		{name: "no mark at all"},
		{name: "the policy alone", mark: "web-reload@3@" + uid, wantUID: uid, want: 3, found: true},
		{name: "the policy among others", mark: "another@1@" + uid + ",web-reload@7@" + uid, wantUID: uid, want: 7, found: true},
		{name: "only another policy", mark: "another@1@" + uid},
		{name: "a policy whose name starts the same", mark: "web-reload-2@4@" + uid},
		{name: "a mark without a generation", mark: "web-reload"},
		{name: "a generation that is not a number", mark: "web-reload@new@" + uid},
		{name: "a mark of version 0.2.0, without a UID", mark: "web-reload@3", want: 3, found: true},
		{name: "a mark of version 0.2.0 among others", mark: "another@1,web-reload@7", want: 7, found: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			annotations := map[string]string{}
			if tc.mark != "" {
				annotations[InjectedAnnotation] = tc.mark
			}
			gotUID, got, found := injectedMark(annotations, "web-reload")
			if found != tc.found || (found && (got != tc.want || gotUID != tc.wantUID)) {
				t.Fatalf("got (%q, %d, %v), want (%q, %d, %v)", gotUID, got, found, tc.wantUID, tc.want, tc.found)
			}
		})
	}
}

func TestEnsureMountAddsAVolumeWithANewName(t *testing.T) {
	pol := newPolicy("web-reload", "web")
	pod := newPod("web")
	// The Pod already has a volume whose name starts like ours.
	pod.Spec.Volumes = []corev1.Volume{{Name: volumePrefix + "mine"}}
	app := &pod.Spec.Containers[1]

	if err := ensureMount(pod, app, pol); err != nil {
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

	if err := ensureMount(pod, &pod.Spec.Containers[1], pol); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Volumes) != 1 {
		t.Fatalf("the Pod has %d volumes, want only the user's own", len(pod.Spec.Volumes))
	}
}

func TestWatcherEnv(t *testing.T) {
	var pol reloadv1alpha1.ConfigReloadPolicy
	pol.Spec.MountPath = "/d"

	// Nothing else set, as when the object never went through the API
	// server: the safety-net defaults apply.
	got := watcherEnv(&pol)
	want := []corev1.EnvVar{ev(envWatchDir, "/d"), ev(envSignal, "SIGHUP"), ev(envDebounce, "3")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults: got %v, want %v", got, want)
	}

	// Everything set. The check command becomes JSON, so a quote inside a
	// word survives the trip through one env string.
	ten := int32(10)
	pol.Spec.Reload = reloadv1alpha1.ReloadSpec{
		Signal:          sigUSR1,
		DebounceSeconds: &ten,
		ValidateCommand: []string{"sh", "-c", `echo "a b"`},
	}
	got = watcherEnv(&pol)
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
