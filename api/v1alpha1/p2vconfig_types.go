/*
Copyright 2026 The metal2vm Authors.

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

// P2VConfigSingletonName is the only accepted name for a P2VConfig.
const P2VConfigSingletonName = "cluster"

// GatewayExposure selects how live agents reach the cluster.
// +kubebuilder:validation:Enum=Route;LoadBalancer;NodePort
type GatewayExposure string

const (
	ExposureRoute        GatewayExposure = "Route"
	ExposureLoadBalancer GatewayExposure = "LoadBalancer"
	ExposureNodePort     GatewayExposure = "NodePort"
)

// P2VConfigSpec holds cluster-wide metal2vm settings.
type P2VConfigSpec struct {
	// +optional
	Gateway GatewayConfig `json:"gateway,omitzero"`

	// images overrides component images, e.g. to point at a mirror registry.
	// Empty fields use the images the operator was released with.
	// +optional
	Images ImageOverrides `json:"images,omitzero"`

	// defaultStorageClassName for copied disks when a migration does not set one.
	// +optional
	DefaultStorageClassName string `json:"defaultStorageClassName,omitempty"`

	// agentTokenTTL is how long a live-environment boot token stays valid.
	// +kubebuilder:default="24h"
	// +optional
	AgentTokenTTL *metav1.Duration `json:"agentTokenTTL,omitempty"`
}

type GatewayConfig struct {
	// exposure of the agent gateway. Route needs only port 443 on the ingress VIP.
	// +kubebuilder:default=Route
	// +optional
	Exposure GatewayExposure `json:"exposure,omitempty"`

	// hostname overrides the generated Route host.
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// bootMediaHTTP also serves boot artifacts (ISO, PXE) over plain HTTP,
	// for BMCs and iPXE that cannot use HTTPS. The boot token then crosses the
	// network unencrypted: use it on trusted provisioning networks only. The
	// agent API always stays on HTTPS.
	// +optional
	BootMediaHTTP bool `json:"bootMediaHTTP,omitempty"`
}

type ImageOverrides struct {
	// transfer runs nbdcopy and virt-v2v-in-place (includes virtio-win).
	// +optional
	Transfer string `json:"transfer,omitempty"`
	// bootArtifacts carries the live ISO and PXE artifacts.
	// +optional
	BootArtifacts string `json:"bootArtifacts,omitempty"`
	// virtioWin replaces the virtio-win drivers bundled in the transfer image.
	// +optional
	VirtioWin string `json:"virtioWin,omitempty"`
}

// P2VConfigStatus is the observed state of the metal2vm installation.
type P2VConfigStatus struct {
	// gatewayURL is the URL live agents connect to.
	// +optional
	GatewayURL string `json:"gatewayURL,omitempty"`

	// bootMediaHTTPURL serves boot artifacts over plain HTTP (bootMediaHTTP).
	// +optional
	BootMediaHTTPURL string `json:"bootMediaHTTPURL,omitempty"`

	// conditions: Available, Degraded.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="P2VConfig must be named 'cluster'"
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.status.gatewayURL`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// P2VConfig is the cluster-wide metal2vm configuration. It must be named "cluster".
type P2VConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of P2VConfig
	// +optional
	Spec P2VConfigSpec `json:"spec,omitzero"`

	// status defines the observed state of P2VConfig
	// +optional
	Status P2VConfigStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// P2VConfigList contains a list of P2VConfig
type P2VConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []P2VConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &P2VConfig{}, &P2VConfigList{})
		return nil
	})
}
