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

// Package testutil makes the objects the tests of several packages use: the
// same Deployment, policy and Pods, and the registration of the Pod webhook
// for the envtest API server.
package testutil

import (
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
	"github.com/nagisatosanae/config-reload-operator/internal/settings"
)

const (
	// Namespace is where the tests make their objects.
	Namespace       = "default"
	AppContainer    = "nginx"
	HelperContainer = "helper"
	HelperMountPath = "/etc/helper"
	// OtherContainer is a name no container has.
	OtherContainer = "sidecar"
	MountPath      = "/etc/nginx/conf.d"
	SigUSR1        = "SIGUSR1"
	// HoldFinalizer keeps an object there after it was deleted.
	HoldFinalizer = "example.com/hold"
	// WebhookService is the name the test webhooks are registered under.
	// envtest turns it into the address of the test server.
	WebhookService = "webhook"
	// PodHash stands for the hash a Deployment gives to a ReplicaSet and to
	// the Pods of it.
	PodHash = "5d4f8c7b9"
)

// InjectCondition is the rule the API server uses to pick the Pods it sends
// to the webhook. The chart has the same rule.
const InjectCondition = "has(object.metadata.annotations) && " +
	"'" + settings.InjectAnnotation + "' in object.metadata.annotations && " +
	"object.metadata.annotations['" + settings.InjectAnnotation + "'] == 'true'"

// NewDeployment returns a Deployment with two containers: a helper first,
// then the app container "nginx". Its Pod template asks for the watcher
// settings with the annotation.
func NewDeployment(name string) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	replicas := int32(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{settings.InjectAnnotation: "true"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: HelperContainer, Image: "busybox"},
					{Name: AppContainer, Image: "nginx:stable-alpine"},
				}},
			},
		},
	}
}

// NewPolicy returns a policy for the Deployment made by NewDeployment. It
// names one container, the app.
func NewPolicy(name, deployment string) *reloadv1alpha1.ConfigReloadPolicy {
	return &reloadv1alpha1.ConfigReloadPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace},
		Spec: reloadv1alpha1.ConfigReloadPolicySpec{
			WorkloadRef: reloadv1alpha1.WorkloadRef{Name: deployment},
			Containers: []reloadv1alpha1.ContainerSettings{{
				Name:         AppContainer,
				ConfigMapRef: reloadv1alpha1.ConfigMapRef{Name: deployment + "-config"},
				MountPath:    MountPath,
			}},
		},
	}
}

// HelperSettings returns settings for the other container of NewDeployment.
func HelperSettings() reloadv1alpha1.ContainerSettings {
	return reloadv1alpha1.ContainerSettings{
		Name:         HelperContainer,
		ConfigMapRef: reloadv1alpha1.ConfigMapRef{Name: "helper-config"},
		MountPath:    HelperMountPath,
	}
}

// NewPod returns a Pod the way the ReplicaSet of a Deployment makes it: it
// has no name of its own yet, it carries the hash label and the annotation
// from the Pod template, and the ReplicaSet is its owner. The containers are
// the ones of NewDeployment.
func NewPod(deployment string) *corev1.Pod {
	replicaSet := deployment + "-" + PodHash
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: replicaSet + "-",
			Namespace:    Namespace,
			Labels:       map[string]string{"app": deployment, appsv1.DefaultDeploymentUniqueLabelKey: PodHash},
			Annotations:  map[string]string{settings.InjectAnnotation: "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       replicaSet,
				UID:        types.UID("11111111-2222-3333-4444-555555555555"),
				Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: HelperContainer, Image: "busybox"},
			{Name: AppContainer, Image: "nginx:stable-alpine"},
		}},
	}
}

// MountConfigMap gives the app container of the Pod its own mount of a
// ConfigMap, the way a user writes it.
func MountConfigMap(pod *corev1.Pod, configMap, dir string) *corev1.Pod {
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

// PodWebhookConfig registers the Pod webhook like the chart does. path is
// the URL path the webhook is served on.
func PodWebhookConfig(path string) *admissionv1.MutatingWebhookConfiguration {
	fail := admissionv1.Fail
	none := admissionv1.SideEffectClassNone
	return &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "config-reload-operator"},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name:                    "inject.reload.nagisatosanae.github.io",
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &none,
			FailurePolicy:           &fail,
			ClientConfig: admissionv1.WebhookClientConfig{
				// envtest turns this into the address of the test server.
				Service: &admissionv1.ServiceReference{Name: WebhookService, Namespace: Namespace, Path: &path},
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
				Expression: InjectCondition,
			}},
		}},
	}
}
