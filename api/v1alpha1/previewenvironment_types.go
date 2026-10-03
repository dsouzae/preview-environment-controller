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

// PreviewEnvironmentSpec defines the desired namespace for a preview.
// Repository and Argo CD fields will be introduced with application reconciliation.
// +kubebuilder:validation:XValidation:rule="has(self.namespace) == has(oldSelf.namespace)",message="namespace cannot be added or removed"
type PreviewEnvironmentSpec struct {
	// Namespace is immutable. If omitted, a name is derived from the CR UID.
	// Explicit names must stay within the reserved preview- prefix.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^preview-[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="namespace is immutable"
	Namespace string `json:"namespace,omitempty"`
}

// PreviewEnvironmentStatus describes the last reconciliation result.
type PreviewEnvironmentStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Namespace is the computed target namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Ready currently means namespace and baseline provisioned, not application health.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// PreviewEnvironment represents an ephemeral application environment.
type PreviewEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              PreviewEnvironmentSpec `json:"spec"`
	// +optional
	Status PreviewEnvironmentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PreviewEnvironmentList contains a list of PreviewEnvironment.
type PreviewEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PreviewEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &PreviewEnvironment{}, &PreviewEnvironmentList{})
		return nil
	})
}
