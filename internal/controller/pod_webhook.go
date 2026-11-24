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
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// InjectAnnotation asks for the watcher settings. It goes on the Pod
	// template of a Deployment, so every Pod of it carries it. Only a Pod
	// that has it with the value "true" is looked at.
	InjectAnnotation = "reload.nagisatosanae.github.io/inject"

	// InjectedAnnotation is put on a Pod by the webhook. It names the
	// policies whose settings the Pod carries, each as "name@generation",
	// with commas in between.
	InjectedAnnotation = "reload.nagisatosanae.github.io/injected"

	// injectPath is the URL path the webhook is served on.
	//
	// The chart names the annotation and the path too, in
	// templates/webhook.yaml. Change one side and you must change the other.
	injectPath = "/mutate--v1-pod"

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

// PodInjector is a mutating admission webhook for Pods.
//
// When a Pod of a Deployment is created and carries the annotation, the
// webhook reads the policies that point at that Deployment and puts their
// settings on the Pod: a volume with the ConfigMap, the mount of it in the
// named container, and the env the watcher reads. The Deployment itself is
// never changed, and neither is a Pod that already runs.
//
// It reads nothing but the policies. What the Pod belongs to is taken from
// the Pod itself.
type PodInjector struct {
	// Client reads the policies. The manager gives its cached client.
	Client client.Reader

	// FailurePolicy says what happens to a Pod that cannot get its settings:
	// FailurePolicyFail rejects it, FailurePolicyIgnore lets it start as the
	// user wrote it. An empty value means FailurePolicyFail.
	FailurePolicy string
}

// Default puts the watcher settings on pod. The API server calls it before
// the Pod is stored.
func (p *PodInjector) Default(ctx context.Context, pod *corev1.Pod) error {
	// The API server already filters on the annotation. Check again here, so
	// the rule does not depend on how the webhook was registered.
	if pod.Annotations[InjectAnnotation] != "true" {
		return nil
	}
	// The settings are already there: this Pod went through the webhook
	// before, in this request or as a copy of another Pod.
	if _, done := pod.Annotations[InjectedAnnotation]; done {
		return nil
	}

	// A new Pod may not have its namespace filled in yet. The request always
	// has it.
	namespace := pod.Namespace
	if req, err := admission.RequestFromContext(ctx); err == nil && req.Namespace != "" {
		namespace = req.Namespace
	}

	injected, err := p.inject(ctx, namespace, pod)
	if err != nil {
		return p.problem(ctx, namespace, pod, err)
	}
	*pod = *injected
	return nil
}

// problem decides what becomes of a Pod that cannot get its settings.
func (p *PodInjector) problem(ctx context.Context, namespace string, pod *corev1.Pod, reason error) error {
	name := pod.Name
	if name == "" {
		name = pod.GenerateName + "*"
	}
	if p.FailurePolicy == FailurePolicyIgnore {
		logf.FromContext(ctx).Info("the Pod starts without the watcher settings",
			"namespace", namespace, "pod", name, "reason", reason.Error())
		return nil
	}
	return fmt.Errorf("pod %s asks for the watcher settings with the annotation %s, but %w", name, InjectAnnotation, reason)
}

// inject returns a copy of pod with the settings of every policy that points
// at its Deployment. It is all or nothing: when one policy cannot be
// applied, the error says why and the Pod is not changed at all.
func (p *PodInjector) inject(ctx context.Context, namespace string, pod *corev1.Pod) (*corev1.Pod, error) {
	deployment, err := deploymentOf(pod)
	if err != nil {
		return nil, err
	}

	var policies reloadv1alpha1.ConfigReloadPolicyList
	if err := p.Client.List(ctx, &policies, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("the policies could not be read: %w", err)
	}

	out := pod.DeepCopy()
	var applied []string
	byContainer := map[string]string{}
	for i := range policies.Items {
		policy := &policies.Items[i]
		if policy.Spec.WorkloadRef.Name != deployment || !policy.DeletionTimestamp.IsZero() {
			continue
		}
		container := policy.Spec.ContainerName
		if other, taken := byContainer[container]; taken {
			return nil, fmt.Errorf("the policies %q and %q both name container %q", other, policy.Name, container)
		}
		byContainer[container] = policy.Name

		if err := injectPolicy(out, policy); err != nil {
			return nil, fmt.Errorf("policy %q cannot be applied: %w", policy.Name, err)
		}
		applied = append(applied, policy.Name+"@"+strconv.FormatInt(policy.Generation, 10))
	}
	if len(applied) == 0 {
		return nil, fmt.Errorf("no ConfigReloadPolicy in namespace %q points at Deployment %q", namespace, deployment)
	}

	slices.Sort(applied)
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	out.Annotations[InjectedAnnotation] = strings.Join(applied, ",")
	return out, nil
}

// deploymentOf returns the name of the Deployment a new Pod belongs to.
//
// A Deployment makes its Pods through a ReplicaSet named
// "<deployment>-<hash>", and gives the Pods the same hash in a label. So the
// name can be read from the Pod, without a call to the API server.
func deploymentOf(pod *corev1.Pod) (string, error) {
	owner := metav1.GetControllerOf(pod)
	hash := pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
	if owner == nil || owner.Kind != "ReplicaSet" || hash == "" || !strings.HasSuffix(owner.Name, "-"+hash) {
		return "", errors.New("it does not belong to a Deployment, and only Pods of a Deployment can get them")
	}
	return strings.TrimSuffix(owner.Name, "-"+hash), nil
}

// injectPolicy puts the settings of one policy on the Pod.
func injectPolicy(pod *corev1.Pod, policy *reloadv1alpha1.ConfigReloadPolicy) error {
	idx := containerIndex(&pod.Spec, policy.Spec.ContainerName)
	if idx < 0 {
		return fmt.Errorf("the Pod has no container named %q", policy.Spec.ContainerName)
	}
	container := &pod.Spec.Containers[idx]

	for _, e := range container.Env {
		if slices.Contains(ownedEnv, e.Name) {
			return fmt.Errorf("container %q already sets %s, and that name belongs to the operator", container.Name, e.Name)
		}
	}
	if err := ensureMount(pod, container, policy); err != nil {
		return err
	}
	container.Env = append(container.Env, watcherEnv(policy)...)
	return nil
}

// ensureMount makes sure the ConfigMap of the policy is mounted at the mount
// path of the policy. It adds a volume and a mount for it. A mount of the
// same ConfigMap that the user wrote at that path is used as it is.
func ensureMount(pod *corev1.Pod, container *corev1.Container, policy *reloadv1alpha1.ConfigReloadPolicy) error {
	configMap, dir := policy.Spec.ConfigMapRef.Name, policy.Spec.MountPath

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

// watcherEnv returns the env the watcher needs for this policy.
func watcherEnv(policy *reloadv1alpha1.ConfigReloadPolicy) []corev1.EnvVar {
	// The API server fills in the defaults. These are only a safety net.
	signal := policy.Spec.Reload.Signal
	if signal == "" {
		signal = "SIGHUP"
	}
	debounce := int32(3)
	if policy.Spec.Reload.DebounceSeconds != nil {
		debounce = *policy.Spec.Reload.DebounceSeconds
	}

	env := []corev1.EnvVar{
		{Name: envWatchDir, Value: policy.Spec.MountPath},
		{Name: envSignal, Value: signal},
		{Name: envDebounce, Value: strconv.Itoa(int(debounce))},
	}

	// The check command is a list of words. Env can only hold one string,
	// so it travels as JSON, for example ["nginx","-t"].
	if command := policy.Spec.Reload.ValidateCommand; len(command) > 0 {
		// A list of strings always turns into JSON, so the error is ignored.
		encoded, _ := json.Marshal(command)
		env = append(env, corev1.EnvVar{Name: envValidate, Value: string(encoded)})
	}
	return env
}

// SetupWebhookWithManager serves the webhook on the manager's webhook server.
func (p *PodInjector) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &corev1.Pod{}).
		WithDefaulter(p).
		WithDefaulterCustomPath(injectPath).
		Complete()
}
