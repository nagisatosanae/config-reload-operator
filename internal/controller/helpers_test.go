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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
)

// These are plain Go tests for the parts of Reconcile that never talk to
// the API server. They need no envtest, so they also run with
// "go test -short".

func ev(name, value string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, Value: value}
}

func TestFindWatchDir(t *testing.T) {
	tests := []struct {
		name       string
		container  string
		change     func(dep *appsv1.Deployment)
		wantIdx    int
		wantDir    string
		wantReason string
	}{
		{
			name:      "the mount is found",
			container: appContainer,
			wantIdx:   1,
			wantDir:   mountPath,
		},
		{
			name:      "the volume name does not matter, only the ConfigMap name",
			container: appContainer,
			change: func(dep *appsv1.Deployment) {
				dep.Spec.Template.Spec.Volumes[0].Name = "cfg"
				dep.Spec.Template.Spec.Containers[1].VolumeMounts[0].Name = "cfg"
			},
			wantIdx: 1,
			wantDir: mountPath,
		},
		{
			name:       "no such container",
			container:  otherContainer,
			wantIdx:    -1,
			wantReason: reasonContainerNotFound,
		},
		{
			name:      "the ConfigMap is not mounted",
			container: appContainer,
			change: func(dep *appsv1.Deployment) {
				dep.Spec.Template.Spec.Volumes[0].ConfigMap.Name = "other-config"
			},
			wantIdx:    1,
			wantReason: reasonConfigMapNotMounted,
		},
		{
			name:      "the ConfigMap is mounted with subPath",
			container: appContainer,
			change: func(dep *appsv1.Deployment) {
				dep.Spec.Template.Spec.Containers[1].VolumeMounts[0].SubPath = "default.conf"
			},
			wantIdx:    1,
			wantReason: reasonSubPathNotSupported,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dep := newDeployment("web")
			if tc.change != nil {
				tc.change(dep)
			}
			idx, dir, reason, msg := findWatchDir(dep, tc.container, "web-config")
			if idx != tc.wantIdx || dir != tc.wantDir || reason != tc.wantReason {
				t.Fatalf("got (%d, %q, %q), want (%d, %q, %q)", idx, dir, reason, tc.wantIdx, tc.wantDir, tc.wantReason)
			}
			if reason != "" && msg == "" {
				t.Fatal("a reason needs a message")
			}
		})
	}
}

func TestSyncEnv(t *testing.T) {
	want := []corev1.EnvVar{ev(envWatchDir, "/d"), ev(envSignal, "SIGHUP")}
	tests := []struct {
		name    string
		env     []corev1.EnvVar
		want    []corev1.EnvVar
		out     []corev1.EnvVar
		changed bool
	}{
		{
			name:    "adds everything to an empty list",
			want:    want,
			out:     want,
			changed: true,
		},
		{
			name:    "keeps other env where it is",
			env:     []corev1.EnvVar{ev("TZ", tz)},
			want:    want,
			out:     []corev1.EnvVar{ev("TZ", tz), ev(envWatchDir, "/d"), ev(envSignal, "SIGHUP")},
			changed: true,
		},
		{
			name:    "fixes a wrong value in place and adds what is missing",
			env:     []corev1.EnvVar{ev(envWatchDir, "/old"), ev("TZ", tz)},
			want:    want,
			out:     []corev1.EnvVar{ev(envWatchDir, "/d"), ev("TZ", tz), ev(envSignal, "SIGHUP")},
			changed: true,
		},
		{
			name: "replaces a valueFrom with the plain value",
			env: []corev1.EnvVar{{Name: envWatchDir, ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			}}},
			want:    want,
			out:     want,
			changed: true,
		},
		{
			name:    "drops our env that is no longer wanted",
			env:     []corev1.EnvVar{ev(envWatchDir, "/d"), ev(envValidate, `["nginx","-t"]`), ev(envSignal, "SIGHUP")},
			want:    want,
			out:     want,
			changed: true,
		},
		{
			name:    "changes nothing when all is equal",
			env:     []corev1.EnvVar{ev("TZ", tz), ev(envWatchDir, "/d"), ev(envSignal, "SIGHUP")},
			want:    want,
			out:     []corev1.EnvVar{ev("TZ", tz), ev(envWatchDir, "/d"), ev(envSignal, "SIGHUP")},
			changed: false,
		},
		{
			name:    "removes all of ours when nothing is wanted",
			env:     []corev1.EnvVar{ev("TZ", tz), ev(envWatchDir, "/d")},
			out:     []corev1.EnvVar{ev("TZ", tz)},
			changed: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]corev1.EnvVar(nil), tc.env...)
			out, changed := syncEnv(tc.env, tc.want)
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v", changed, tc.changed)
			}
			if !reflect.DeepEqual(out, tc.out) {
				t.Fatalf("out = %v, want %v", out, tc.out)
			}
			if !reflect.DeepEqual(tc.env, before) {
				t.Fatal("the input was modified")
			}
		})
	}
}

func TestWatcherEnv(t *testing.T) {
	var pol reloadv1alpha1.ConfigReloadPolicy

	// Nothing set, as when the object never went through the API server:
	// the safety-net defaults apply.
	got := watcherEnv(&pol, "/d")
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
	got = watcherEnv(&pol, "/d")
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
