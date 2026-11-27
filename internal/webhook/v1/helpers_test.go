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
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
	"github.com/nagisatosanae/config-reload-operator/internal/testutil"
)

// These are plain Go tests for the parts of the webhook that never talk to
// the API server. They need no envtest, so they also run with
// "go test -short".

func ev(name, value string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, Value: value}
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
	old := testutil.NewPolicy("web-reload", "web")
	old.Spec.Containers = nil
	defaulter := &PodDefaulter{Client: onePolicy{policy: *old}}

	_, err := defaulter.injectFor(context.Background(), testutil.Namespace, "web", testutil.NewPod("web"))
	if err == nil || !strings.Contains(err.Error(), "names no container") {
		t.Fatalf("err = %v, want the reason that the policy names no container", err)
	}
}

func TestEnsureMountAddsAVolumeWithANewName(t *testing.T) {
	pol := testutil.NewPolicy("web-reload", "web")
	pod := testutil.NewPod("web")
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
	want := []corev1.VolumeMount{{Name: added.Name, MountPath: testutil.MountPath, ReadOnly: true}}
	if !reflect.DeepEqual(app.VolumeMounts, want) {
		t.Fatalf("mounts = %v, want %v", app.VolumeMounts, want)
	}
}

func TestEnsureMountKnowsThePathWithASlashAtTheEnd(t *testing.T) {
	pol := testutil.NewPolicy("web-reload", "web")
	pod := testutil.MountConfigMap(testutil.NewPod("web"), "web-config", testutil.MountPath+"/")

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
		Signal:          testutil.SigUSR1,
		DebounceSeconds: &ten,
		ValidateCommand: []string{"sh", "-c", `echo "a b"`},
	}
	got = watcherEnv(&c)
	want = []corev1.EnvVar{
		ev(envWatchDir, "/d"),
		ev(envSignal, testutil.SigUSR1),
		ev(envDebounce, "10"),
		ev(envValidate, `["sh","-c","echo \"a b\""]`),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("all set: got %v, want %v", got, want)
	}
}
