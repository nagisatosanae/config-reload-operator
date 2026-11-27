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

package settings_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
	"github.com/nagisatosanae/config-reload-operator/internal/settings"
	"github.com/nagisatosanae/config-reload-operator/internal/testutil"
)

// These are plain Go tests. They need no envtest, so they also run with
// "go test -short".

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
				pod.OwnerReferences[0].Name = "my-web-app-" + testutil.PodHash
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
			pod := testutil.NewPod("web")
			if tc.change != nil {
				tc.change(pod)
			}
			got, err := settings.DeploymentOf(pod)
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
	base := &testutil.NewPolicy("web-reload", "web").Spec.Containers[0]

	// The value is pinned. A Pod carries it for as long as it runs, so a
	// change in how the hash is made would show every running Pod as
	// outdated after an upgrade.
	if got, want := settings.Hash(base), "f7cb8bccb"; got != want {
		t.Fatalf("settings.Hash = %q, want %q", got, want)
	}

	// The same for an entry as the API server gives it back: with the
	// defaults filled in, and with a check command. It is the entry of the
	// sample policy of version 0.4.0, and the value is the one its Pods
	// carry.
	three := int32(3)
	full := &reloadv1alpha1.ContainerSettings{
		Name:         testutil.AppContainer,
		ConfigMapRef: reloadv1alpha1.ConfigMapRef{Name: "nginx-config"},
		MountPath:    testutil.MountPath,
		Reload: reloadv1alpha1.ReloadSpec{
			Signal:          "SIGHUP",
			DebounceSeconds: &three,
			ValidateCommand: strings.Fields("nginx -t"),
		},
	}
	if got, want := settings.Hash(full), "8476bb7d6b"; got != want {
		t.Fatalf("settings.Hash of an entry with reload settings = %q, want %q", got, want)
	}

	// The entry of the sample policy as it is now, for the Fluentd demo. The
	// value is the one the Pods of the demo carry.
	sample := &reloadv1alpha1.ContainerSettings{
		Name:         "fluentd",
		ConfigMapRef: reloadv1alpha1.ConfigMapRef{Name: "fluentd-config"},
		MountPath:    "/fluentd/etc",
		Reload: reloadv1alpha1.ReloadSpec{
			Signal:          "SIGHUP",
			DebounceSeconds: &three,
			ValidateCommand: []string{"fluentd", "--dry-run", "-q", "-c", "/fluentd/etc/fluent.conf"},
		},
	}
	if got, want := settings.Hash(sample), "74d9dbc6c6"; got != want {
		t.Fatalf("settings.Hash of the entry of the sample policy = %q, want %q", got, want)
	}

	debounce := int32(9)
	changes := map[string]func(*reloadv1alpha1.ContainerSettings){
		"the container":     func(c *reloadv1alpha1.ContainerSettings) { c.Name = testutil.HelperContainer },
		"the ConfigMap":     func(c *reloadv1alpha1.ContainerSettings) { c.ConfigMapRef.Name = "other-config" },
		"the path":          func(c *reloadv1alpha1.ContainerSettings) { c.MountPath = "/etc/other" },
		"the signal":        func(c *reloadv1alpha1.ContainerSettings) { c.Reload.Signal = testutil.SigUSR1 },
		"the quiet time":    func(c *reloadv1alpha1.ContainerSettings) { c.Reload.DebounceSeconds = &debounce },
		"the check command": func(c *reloadv1alpha1.ContainerSettings) { c.Reload.ValidateCommand = []string{"haproxy", "-c"} },
	}
	for name, change := range changes {
		changed := testutil.NewPolicy("web-reload", "web").Spec.Containers[0]
		change(&changed)
		if settings.Hash(&changed) == settings.Hash(base) {
			t.Errorf("an entry with another value for %s has the same hash", name)
		}
	}
}

func TestMarkFor(t *testing.T) {
	policy := testutil.NewPolicy("web-reload", "web")
	app := policy.Spec.Containers[0]
	if got, want := settings.MarkFor(policy), testutil.AppContainer+"="+settings.Hash(&app); got != want {
		t.Fatalf("settings.MarkFor = %q, want %q", got, want)
	}

	// The name, the UID and the generation of the policy are not in the mark.
	same := testutil.NewPolicy("another-name", "web")
	same.UID, same.Generation = "0b9d2c8e-6f1a-4d37-9a55-3c1e7f2b8a40", 7
	if settings.MarkFor(same) != settings.MarkFor(policy) {
		t.Fatal("two policies that say the same about the container have different marks")
	}

	// Two containers come in the same order, whatever the order in the
	// policy is.
	helper := testutil.HelperSettings()
	want := testutil.HelperContainer + "=" + settings.Hash(&helper) + "," + testutil.AppContainer + "=" + settings.Hash(&app)
	policy.Spec.Containers = []reloadv1alpha1.ContainerSettings{app, helper}
	if got := settings.MarkFor(policy); got != want {
		t.Fatalf("settings.MarkFor = %q, want %q", got, want)
	}
	policy.Spec.Containers = []reloadv1alpha1.ContainerSettings{helper, app}
	if got := settings.MarkFor(policy); got != want {
		t.Fatalf("with the containers the other way round settings.MarkFor = %q, want %q", got, want)
	}
}
