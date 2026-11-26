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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Run "make manifests generate" after you change this file.

// TODO: add an optional namespace field once cross-namespace refs are supported.

// WorkloadRef points at the workload that should be reloaded.
type WorkloadRef struct {
	// kind is the kind of the workload. Only Deployment is supported for now.
	// +kubebuilder:validation:Enum=Deployment
	// +kubebuilder:default=Deployment
	// +optional
	Kind string `json:"kind,omitempty"`

	// name is the name of the workload. It must be in the same namespace as
	// the policy.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`
}

// ConfigMapRef points at the ConfigMap that holds the config files.
type ConfigMapRef struct {
	// name is the name of the ConfigMap. It must be in the same namespace as
	// the policy.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`
}

// ReloadSpec says how to tell the app to reload its config.
type ReloadSpec struct {
	// signal is the signal sent to the app when the config changes.
	// +kubebuilder:validation:Enum=SIGHUP;SIGUSR1;SIGUSR2
	// +kubebuilder:default=SIGHUP
	// +optional
	Signal string `json:"signal,omitempty"`

	// debounceSeconds is how long to wait after the last change before
	// reloading. It is a quiet period, not a polling interval.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=3
	// +optional
	DebounceSeconds *int32 `json:"debounceSeconds,omitempty"`

	// validateCommand is a command that checks the new config before the
	// signal is sent, for example ["nginx", "-t"]. It runs inside the
	// container, without a shell. If it exits with a non-zero code, the
	// signal is not sent and the app keeps its current config. Leave it out
	// to skip the check.
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +optional
	ValidateCommand []string `json:"validateCommand,omitempty"`
}

// ContainerSettings says what one container of the workload gets: which
// ConfigMap, where it is mounted, and how the app is told to reload.
type ContainerSettings struct {
	// name is the container that runs the watcher and the app. A container
	// can be named only once in a policy.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name"`

	// configMapRef is the ConfigMap mounted into that container.
	// +required
	ConfigMapRef ConfigMapRef `json:"configMapRef"`

	// mountPath is the directory in that container where the ConfigMap is
	// mounted. The watcher watches this directory. It must be an absolute
	// path with no slash at the end, like /etc/nginx/conf.d.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:XValidation:rule="self.startsWith('/') && !self.endsWith('/')",message="mountPath must be an absolute path with no slash at the end, like /etc/nginx/conf.d"
	// +required
	MountPath string `json:"mountPath"`

	// reload says how to reload the app. If you leave it out, the app gets
	// SIGHUP three seconds after the last change.
	// +kubebuilder:default={}
	// +optional
	Reload ReloadSpec `json:"reload,omitzero"`
}

// ConfigReloadPolicySpec defines the desired state of ConfigReloadPolicy.
type ConfigReloadPolicySpec struct {
	// workloadRef is the workload to reload. A workload can have only one
	// policy. It cannot be changed after the policy is created. To point at
	// another workload, delete the policy and create a new one. Pods of the
	// old workload keep their settings until they are replaced.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workloadRef cannot be changed. Delete the policy and create a new one."
	// +required
	WorkloadRef WorkloadRef `json:"workloadRef"`

	// containers lists the containers of the workload that get the watcher
	// settings, one entry for each container.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	// +required
	Containers []ContainerSettings `json:"containers"`
}

// ConfigReloadPolicyStatus defines the observed state of ConfigReloadPolicy.
type ConfigReloadPolicyStatus struct {
	// conditions holds the state of the policy. The controller sets one
	// condition, "Ready". It is True when every Pod of the workload that asks
	// for the settings carries them as the policy is now, and also when
	// there are no Pods yet. When it is False, the reason and the message
	// say what is wrong.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// pods is the number of Pods of the workload that this policy applies
	// to. A zero is written out, so kubectl shows 0 and not an empty column.
	// +optional
	Pods int32 `json:"pods"`

	// injectedPods is how many of those Pods carry the settings of this
	// policy as it is now.
	// +optional
	InjectedPods int32 `json:"injectedPods"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=crp
// +kubebuilder:printcolumn:name="Workload",type=string,JSONPath=`.spec.workloadRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.pods`
// +kubebuilder:printcolumn:name="Injected",type=integer,JSONPath=`.status.injectedPods`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ConfigReloadPolicy is the Schema for the configreloadpolicies API
type ConfigReloadPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ConfigReloadPolicy
	// +required
	Spec ConfigReloadPolicySpec `json:"spec"`

	// status defines the observed state of ConfigReloadPolicy
	// +optional
	Status ConfigReloadPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ConfigReloadPolicyList contains a list of ConfigReloadPolicy
type ConfigReloadPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ConfigReloadPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ConfigReloadPolicy{}, &ConfigReloadPolicyList{})
		return nil
	})
}
