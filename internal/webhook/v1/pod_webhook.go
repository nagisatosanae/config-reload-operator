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
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
	"github.com/nagisatosanae/config-reload-operator/internal/settings"
)

const (
	// PodWebhookPath is the URL path the webhook is served on.
	//
	// The chart names the path too, in templates/webhook.yaml. Change one
	// side and you must change the other.
	PodWebhookPath = "/mutate--v1-pod"

	// volumePrefix starts the name of every volume the webhook adds.
	volumePrefix = "config-reload-"

	// The watcher reads these from its environment.
	envWatchDir = "CONFIG_RELOAD_DIR"
	envSignal   = "CONFIG_RELOAD_SIGNAL"
	envDebounce = "CONFIG_RELOAD_DEBOUNCE_SECONDS"
	envValidate = "CONFIG_RELOAD_VALIDATE_COMMAND"
)

// What to do with a Pod that asks for the watcher settings but cannot get
// them. The values are the ones of failurePolicy in the webhook registration,
// and the chart gives both the same value.
const (
	FailurePolicyFail   = "Fail"
	FailurePolicyIgnore = "Ignore"
)

// ownedEnv lists the env names that belong to the operator. A Pod that
// already sets one of them cannot get the settings.
var ownedEnv = []string{envWatchDir, envSignal, envDebounce, envValidate}

// PodDefaulter is a mutating admission webhook for Pods.
//
// When a Pod of a Deployment is created and carries the annotation, the
// webhook reads the policy that points at that Deployment and puts its
// settings on the Pod. Every container the policy names gets a volume with
// its ConfigMap, the mount of it, and the env the watcher reads. The
// Deployment itself is never changed, and neither is a Pod that already
// runs.
//
// It reads nothing but the policies. What the Pod belongs to is taken from
// the Pod itself.
type PodDefaulter struct {
	// Client reads the policies. The manager gives its cached client.
	Client client.Reader

	// FailurePolicy says what happens to a Pod that cannot get its settings:
	// FailurePolicyFail rejects it, FailurePolicyIgnore lets it start as the
	// user wrote it. An empty value means FailurePolicyFail.
	FailurePolicy string
}

// Default puts the watcher settings on pod. The API server calls it before
// the Pod is stored.
func (d *PodDefaulter) Default(ctx context.Context, pod *corev1.Pod) error {
	// The API server already filters on the annotation. Check again here, so
	// the rule does not depend on how the webhook was registered.
	if !settings.AskedFor(pod.Annotations) {
		return nil
	}
	// The settings are already there: this Pod went through the webhook
	// before, in this request or as a copy of another Pod.
	if _, done := pod.Annotations[settings.InjectedAnnotation]; done {
		return nil
	}

	// A new Pod may not have its namespace filled in yet. The request always
	// has it.
	namespace := pod.Namespace
	if req, err := admission.RequestFromContext(ctx); err == nil && req.Namespace != "" {
		namespace = req.Namespace
	}

	injected, err := d.inject(ctx, namespace, pod)
	if err != nil {
		return d.problem(ctx, namespace, pod, err)
	}
	*pod = *injected
	return nil
}

// problem decides what becomes of a Pod that cannot get its settings.
func (d *PodDefaulter) problem(ctx context.Context, namespace string, pod *corev1.Pod, reason error) error {
	name := pod.Name
	if name == "" {
		name = pod.GenerateName + "*"
	}
	if d.FailurePolicy == FailurePolicyIgnore {
		logf.FromContext(ctx).Info("the Pod starts without the watcher settings",
			"namespace", namespace, "pod", name, "reason", reason.Error())
		return nil
	}
	return fmt.Errorf("pod %s asks for the watcher settings with the annotation %s, but %w", name, settings.InjectAnnotation, reason)
}

// inject returns a copy of pod with the settings of the policy that points
// at its Deployment.
func (d *PodDefaulter) inject(ctx context.Context, namespace string, pod *corev1.Pod) (*corev1.Pod, error) {
	deployment, err := settings.DeploymentOf(pod)
	if err != nil {
		return nil, err
	}
	return d.injectFor(ctx, namespace, deployment, pod)
}

// injectFor returns a copy of pod with the settings of the policy that
// points at the named Deployment. It is all or nothing: when one container
// cannot get its settings, the error says why and nothing is changed.
//
// The check of a Deployment calls it too, with a Pod made from the Pod
// template. So what is said when a Deployment is applied and what happens
// when its Pods are made cannot differ.
func (d *PodDefaulter) injectFor(ctx context.Context, namespace, deployment string, pod *corev1.Pod) (*corev1.Pod, error) {
	policies, err := settings.PoliciesFor(ctx, d.Client, namespace, deployment)
	if err != nil {
		return nil, err
	}
	// A Deployment has one policy. The check of a new policy keeps it so,
	// but two policies made at the same moment can both get past it.
	switch len(policies) {
	case 0:
		return nil, fmt.Errorf("no ConfigReloadPolicy in namespace %q points at Deployment %q", namespace, deployment)
	case 1:
	default:
		return nil, fmt.Errorf("the policies %q and %q both point at Deployment %q, and a Deployment can have only one",
			policies[0].Name, policies[1].Name, deployment)
	}
	policy := &policies[0]
	// A policy written for an earlier version has no list of containers.
	if len(policy.Spec.Containers) == 0 {
		return nil, fmt.Errorf("policy %q names no container. Write it again with spec.containers", policy.Name)
	}

	out := pod.DeepCopy()
	for i := range policy.Spec.Containers {
		if err := injectContainer(out, &policy.Spec.Containers[i]); err != nil {
			return nil, fmt.Errorf("policy %q cannot be applied: %w", policy.Name, err)
		}
	}
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	out.Annotations[settings.InjectedAnnotation] = settings.MarkFor(policy)
	return out, nil
}

// injectContainer puts the settings of one container on the Pod.
func injectContainer(pod *corev1.Pod, c *reloadv1alpha1.ContainerSettings) error {
	idx := settings.ContainerIndex(&pod.Spec, c.Name)
	if idx < 0 {
		return fmt.Errorf("the Pod has no container named %q", c.Name)
	}
	container := &pod.Spec.Containers[idx]

	for _, e := range container.Env {
		if slices.Contains(ownedEnv, e.Name) {
			return fmt.Errorf("container %q already sets %s, and that name belongs to the operator", container.Name, e.Name)
		}
	}
	if err := ensureMount(pod, container, c); err != nil {
		return err
	}
	container.Env = append(container.Env, watcherEnv(c)...)
	return nil
}

// ensureMount makes sure the ConfigMap is mounted at the mount path the
// policy gives for this container. It adds a volume and a mount for it. A
// mount of the same ConfigMap that the user wrote at that path is used as
// it is.
func ensureMount(pod *corev1.Pod, container *corev1.Container, c *reloadv1alpha1.ContainerSettings) error {
	configMap, dir := c.ConfigMapRef.Name, c.MountPath

	for _, m := range container.VolumeMounts {
		if path.Clean(m.MountPath) != dir {
			continue
		}
		volume := volumeNamed(&pod.Spec, m.Name)
		if volume == nil || volume.ConfigMap == nil || volume.ConfigMap.Name != configMap {
			return fmt.Errorf("container %q already mounts something else at %s", container.Name, dir)
		}
		if m.SubPath != "" || m.SubPathExpr != "" {
			return fmt.Errorf("container %q mounts ConfigMap %q at %s with subPath, so its files are never updated",
				container.Name, configMap, dir)
		}
		return nil
	}

	name := newVolumeName(&pod.Spec)
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configMap},
		}},
	})
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      name,
		MountPath: dir,
		ReadOnly:  true,
	})
	return nil
}

// volumeNamed returns the volume with this name, or nil.
func volumeNamed(podSpec *corev1.PodSpec, name string) *corev1.Volume {
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == name {
			return &podSpec.Volumes[i]
		}
	}
	return nil
}

// newVolumeName returns a name no volume of the Pod has yet: the prefix and
// a few random characters, like the end of a Pod name.
func newVolumeName(podSpec *corev1.PodSpec) string {
	for {
		name := volumePrefix + rand.String(8)
		if volumeNamed(podSpec, name) == nil {
			return name
		}
	}
}

// watcherEnv returns the env the watcher needs in this container.
func watcherEnv(c *reloadv1alpha1.ContainerSettings) []corev1.EnvVar {
	// The API server fills in the defaults. These are only a safety net.
	signal := c.Reload.Signal
	if signal == "" {
		signal = "SIGHUP"
	}
	debounce := int32(3)
	if c.Reload.DebounceSeconds != nil {
		debounce = *c.Reload.DebounceSeconds
	}

	env := []corev1.EnvVar{
		{Name: envWatchDir, Value: c.MountPath},
		{Name: envSignal, Value: signal},
		{Name: envDebounce, Value: strconv.Itoa(int(debounce))},
	}

	// The check command is a list of words. Env can only hold one string,
	// so it travels as JSON, for example ["nginx","-t"].
	if command := c.Reload.ValidateCommand; len(command) > 0 {
		// A list of strings always turns into JSON, so the error is ignored.
		encoded, _ := json.Marshal(command)
		env = append(env, corev1.EnvVar{Name: envValidate, Value: string(encoded)})
	}
	return env
}

// SetupPodWebhookWithManager serves the Pod webhook on the manager's webhook
// server. failurePolicy says what happens to a Pod that cannot get its
// settings: FailurePolicyFail or FailurePolicyIgnore.
func SetupPodWebhookWithManager(mgr ctrl.Manager, failurePolicy string) error {
	return ctrl.NewWebhookManagedBy(mgr, &corev1.Pod{}).
		WithDefaulter(&PodDefaulter{Client: mgr.GetClient(), FailurePolicy: failurePolicy}).
		WithDefaulterCustomPath(PodWebhookPath).
		Complete()
}
