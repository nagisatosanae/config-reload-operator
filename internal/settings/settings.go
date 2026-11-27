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

// Package settings holds what the webhooks and the controller must agree on:
// the two annotations, which Pods ask for the watcher settings, which policy
// gives them, and the mark a Pod carries for them.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	reloadv1alpha1 "github.com/nagisatosanae/config-reload-operator/api/v1alpha1"
)

const (
	// InjectAnnotation asks for the watcher settings. It goes on the Pod
	// template of a Deployment, so every Pod of it carries it. Only a Pod
	// that has it with the value "true" is looked at.
	//
	// The chart names it too, in templates/webhook.yaml. Change one side and
	// you must change the other.
	InjectAnnotation = "reload.nagisatosanae.github.io/inject"

	// InjectedAnnotation is put on a Pod by the webhook. It names the
	// containers that got settings, each as "container=hash", with commas
	// in between. The hash is made from what the policy says about that
	// container.
	InjectedAnnotation = "reload.nagisatosanae.github.io/injected"
)

// AskedFor tells if these annotations ask for the watcher settings.
func AskedFor(annotations map[string]string) bool {
	return annotations[InjectAnnotation] == "true"
}

// DeploymentOf returns the name of the Deployment a new Pod belongs to.
//
// A Deployment makes its Pods through a ReplicaSet named
// "<deployment>-<hash>", and gives the Pods the same hash in a label. So the
// name can be read from the Pod, without a call to the API server.
//
// The error is written to end a sentence that names the Pod.
func DeploymentOf(pod metav1.Object) (string, error) {
	owner := metav1.GetControllerOf(pod)
	hash := pod.GetLabels()[appsv1.DefaultDeploymentUniqueLabelKey]
	if owner == nil || owner.Kind != "ReplicaSet" || hash == "" || !strings.HasSuffix(owner.Name, "-"+hash) {
		return "", errors.New("it does not belong to a Deployment, and only Pods of a Deployment can get them")
	}
	return strings.TrimSuffix(owner.Name, "-"+hash), nil
}

// PoliciesFor returns the policies of the namespace that point at the
// named Deployment. A policy on its way out is left out.
func PoliciesFor(ctx context.Context, c client.Reader, namespace, deployment string) ([]reloadv1alpha1.ConfigReloadPolicy, error) {
	var all reloadv1alpha1.ConfigReloadPolicyList
	if err := c.List(ctx, &all, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("the policies could not be read: %w", err)
	}
	var found []reloadv1alpha1.ConfigReloadPolicy
	for i := range all.Items {
		if all.Items[i].Spec.WorkloadRef.Name == deployment && all.Items[i].DeletionTimestamp.IsZero() {
			found = append(found, all.Items[i])
		}
	}
	return found, nil
}

// Hash returns a short hash of what a policy says about one container.
// Everything the webhook puts on that container comes from there, so the
// same entry gives a Pod the same settings, whatever the policy is called.
//
// The content goes in as JSON. The hash is FNV-1a with 32 bits, written in
// letters and digits that cannot spell a word. Kubernetes uses the same
// function and the same letters for the pod-template-hash of a ReplicaSet.
func Hash(c *reloadv1alpha1.ContainerSettings) string {
	// A struct of strings, numbers and lists always turns into JSON, so the
	// error is ignored.
	data, _ := json.Marshal(c)
	h := fnv.New32a()
	_, _ = h.Write(data)
	return rand.SafeEncodeString(fmt.Sprint(h.Sum32()))
}

// MarkFor returns the mark of a Pod that carries the settings of the policy
// as it is now: every container as "name=hash", sorted, so the order in the
// policy does not matter.
//
// The webhook writes it on a new Pod, and the controller compares it with
// what a Pod carries. That is a comparison of content: a policy that is
// deleted and made again with the same containers gives the same mark.
func MarkFor(policy *reloadv1alpha1.ConfigReloadPolicy) string {
	entries := make([]string, 0, len(policy.Spec.Containers))
	for i := range policy.Spec.Containers {
		c := &policy.Spec.Containers[i]
		entries = append(entries, c.Name+"="+Hash(c))
	}
	slices.Sort(entries)
	return strings.Join(entries, ",")
}

// ContainerIndex returns the position of the named container, or -1.
func ContainerIndex(podSpec *corev1.PodSpec, name string) int {
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == name {
			return i
		}
	}
	return -1
}
