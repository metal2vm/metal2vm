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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// MigrationMode selects cold (source offline during copy) or warm migration.
// +kubebuilder:validation:Enum=cold;warm
type MigrationMode string

const (
	ModeCold MigrationMode = "cold"
	ModeWarm MigrationMode = "warm"
)

// BootMethod selects how the source is booted into the live environment.
// +kubebuilder:validation:Enum=redfish;pxe;manual
type BootMethod string

const (
	BootRedfish BootMethod = "redfish"
	BootPXE     BootMethod = "pxe"
	BootManual  BootMethod = "manual"
)

// P2VMigrationSpec is the admin's plan for migrating one physical server.
// The VM, its PVCs and the transfer pod are created in the migration's namespace.
type P2VMigrationSpec struct {
	// sourceRef names a P2VSource in the same namespace.
	// +required
	SourceRef corev1.LocalObjectReference `json:"sourceRef"`

	// +kubebuilder:default=cold
	// +optional
	Mode MigrationMode `json:"mode,omitempty"`

	// +kubebuilder:default=manual
	// +optional
	BootMethod BootMethod `json:"bootMethod,omitempty"`

	// targetVM describes the VirtualMachine to create. Everything defaults
	// from the source inventory.
	// +optional
	TargetVM TargetVM `json:"targetVM,omitzero"`

	// disks maps each source disk to an action. Disks not listed are skipped.
	// +listType=map
	// +listMapKey=sourceDiskID
	// +kubebuilder:validation:MinItems=1
	// +required
	Disks []DiskMapping `json:"disks"`

	// nics maps each source NIC to a target network. NICs not listed are dropped.
	// +listType=map
	// +listMapKey=sourceMAC
	// +optional
	NICs []NICMapping `json:"nics,omitempty"`

	// testBoot starts the converted VM without production NICs and waits for
	// cutover, so the admin can verify it first.
	// +kubebuilder:default=true
	// +optional
	TestBoot *bool `json:"testBoot,omitempty"`

	// autoShutdownSource powers the source off at cutover.
	// +kubebuilder:default=true
	// +optional
	AutoShutdownSource *bool `json:"autoShutdownSource,omitempty"`

	// cutover, when set and in the past, switches from test boot to production.
	// Same semantics as Forklift's Migration.spec.cutover.
	// +optional
	Cutover *metav1.Time `json:"cutover,omitempty"`

	// rollback stops the VM and powers the source back on. Ignored once archived.
	// +optional
	Rollback bool `json:"rollback,omitempty"`

	// archived finalizes the migration: rollback is no longer possible and
	// temporary resources (transfer pod, tokens, boot media) are removed.
	// +optional
	Archived bool `json:"archived,omitempty"`
}

// TargetVM describes the VirtualMachine created for the source.
type TargetVM struct {
	// name of the VirtualMachine. Defaults to the P2VSource name.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Name string `json:"name,omitempty"`

	// cpu topology. Defaults to the source topology.
	// +optional
	CPU *VMCPU `json:"cpu,omitempty"`

	// memory. Defaults to the source memory.
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`

	// firmware overrides the detected firmware.
	// +optional
	Firmware *FirmwareOverride `json:"firmware,omitempty"`

	// diskBus for converted disks. sata is a fallback for guests virt-v2v cannot convert.
	// +kubebuilder:default=virtio
	// +kubebuilder:validation:Enum=virtio;sata
	// +optional
	DiskBus string `json:"diskBus,omitempty"`
}

type VMCPU struct {
	// +kubebuilder:validation:Minimum=1
	// +optional
	Sockets int32 `json:"sockets,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +optional
	Cores int32 `json:"cores,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +optional
	Threads int32 `json:"threads,omitempty"`
}

type FirmwareOverride struct {
	// +optional
	Type FirmwareType `json:"type,omitempty"`
	// +optional
	SecureBoot *bool `json:"secureBoot,omitempty"`
	// tpm adds a persistent vTPM.
	// +optional
	TPM *bool `json:"tpm,omitempty"`
}

// DiskAction is what to do with a source disk.
// +kubebuilder:validation:Enum=copy;attach;skip
type DiskAction string

const (
	DiskCopy   DiskAction = "copy"
	DiskAttach DiskAction = "attach"
	DiskSkip   DiskAction = "skip"
)

// DiskMapping maps one source disk. Like a Forklift StorageMap entry, per disk.
// +kubebuilder:validation:XValidation:rule="self.action != 'attach' || has(self.lun)",message="lun is required when action is attach"
// +kubebuilder:validation:XValidation:rule="self.action != 'copy' || !has(self.lun)",message="lun is only valid when action is attach"
type DiskMapping struct {
	// sourceDiskID matches status.inventory.disks[].id of the P2VSource.
	// +required
	SourceDiskID string `json:"sourceDiskID"`

	// +required
	Action DiskAction `json:"action"`

	// storageClassName for action=copy. Empty uses P2VConfig's default, then the cluster default.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// lun describes how worker nodes reach an existing SAN LUN for action=attach.
	// +optional
	LUN *LUNAttachment `json:"lun,omitempty"`
}

// LUNAttachment identifies an existing SAN LUN. Exactly one of fc or iscsi is set.
// +kubebuilder:validation:XValidation:rule="has(self.fc) != has(self.iscsi)",message="exactly one of fc or iscsi must be set"
type LUNAttachment struct {
	// +optional
	FC *FCLUN `json:"fc,omitempty"`
	// +optional
	ISCSI *ISCSILUN `json:"iscsi,omitempty"`
}

type FCLUN struct {
	// wwids of the LUN (preferred), as seen by worker nodes.
	// +optional
	WWIDs []string `json:"wwids,omitempty"`
	// targetWWNs and lun, used when wwids is empty.
	// +optional
	TargetWWNs []string `json:"targetWWNs,omitempty"`
	// +optional
	LUN *int32 `json:"lun,omitempty"`
}

type ISCSILUN struct {
	// +required
	TargetPortal string `json:"targetPortal"`
	// +optional
	Portals []string `json:"portals,omitempty"`
	// +required
	IQN string `json:"iqn"`
	// +required
	LUN int32 `json:"lun"`
	// chapSecretRef names a Secret of type kubernetes.io/iscsi-chap.
	// +optional
	CHAPSecretRef *corev1.LocalObjectReference `json:"chapSecretRef,omitempty"`
}

// NetworkType is the kind of target network. Same values as Forklift's NetworkMap.
// +kubebuilder:validation:Enum=pod;multus
type NetworkType string

const (
	NetworkPod    NetworkType = "pod"
	NetworkMultus NetworkType = "multus"
)

// NICMapping maps one source NIC to a target network.
// +kubebuilder:validation:XValidation:rule="self.type != 'multus' || has(self.networkName)",message="networkName is required for multus networks"
// +kubebuilder:validation:XValidation:rule="!(has(self.keepIP) && self.keepIP) || self.type == 'multus'",message="keepIP requires a multus (bridged/localnet) network"
type NICMapping struct {
	// sourceMAC matches status.inventory.nics[].mac of the P2VSource.
	// +required
	SourceMAC string `json:"sourceMAC"`

	// +required
	Type NetworkType `json:"type"`

	// networkName is a NetworkAttachmentDefinition as "name" or "namespace/name".
	// +optional
	NetworkName string `json:"networkName,omitempty"`

	// keepMAC gives the VM NIC the source MAC address on multus networks.
	// On the pod network the source MAC is always kept: it never leaves the
	// pod, and guests often bind network config to it.
	// +optional
	KeepMAC bool `json:"keepMAC,omitempty"`

	// keepIP re-applies the source static IP configuration inside the guest.
	// +optional
	KeepIP bool `json:"keepIP,omitempty"`
}

// MigrationPhase is the coarse state of a migration.
// +kubebuilder:validation:Enum=Pending;Preflight;Booting;Connected;Copying;Converting;ReadyForTest;Testing;CuttingOver;Completed;RollingBack;RolledBack;Failed
type MigrationPhase string

const (
	PhasePending      MigrationPhase = "Pending"
	PhasePreflight    MigrationPhase = "Preflight"
	PhaseBooting      MigrationPhase = "Booting"
	PhaseConnected    MigrationPhase = "Connected"
	PhaseCopying      MigrationPhase = "Copying"
	PhaseConverting   MigrationPhase = "Converting"
	PhaseReadyForTest MigrationPhase = "ReadyForTest"
	PhaseTesting      MigrationPhase = "Testing"
	PhaseCuttingOver  MigrationPhase = "CuttingOver"
	PhaseCompleted    MigrationPhase = "Completed"
	PhaseRollingBack  MigrationPhase = "RollingBack"
	PhaseRolledBack   MigrationPhase = "RolledBack"
	PhaseFailed       MigrationPhase = "Failed"
)

// P2VMigrationStatus is the observed progress of a migration.
type P2VMigrationStatus struct {
	// +optional
	Phase MigrationPhase `json:"phase,omitempty"`

	// message is a human-readable summary of the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// vmName is the VirtualMachine created for this migration.
	// +optional
	VMName string `json:"vmName,omitempty"`

	// guest is what guest conversion found inside the disks.
	// +optional
	Guest *GuestInfo `json:"guest,omitempty"`

	// agent is the live agent connection state.
	// +optional
	Agent *AgentStatus `json:"agent,omitempty"`

	// +listType=map
	// +listMapKey=sourceDiskID
	// +optional
	Disks []DiskStatus `json:"disks,omitempty"`

	// preflight holds the result of each pre-flight check.
	// +listType=map
	// +listMapKey=name
	// +optional
	Preflight []PreflightResult `json:"preflight,omitempty"`

	// conditions: Ready, PreflightPassed, AgentConnected, Copied, Converted, SourcePoweredOff, VMReady.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GuestInfo describes the operating system found during conversion.
type GuestInfo struct {
	// family is linux or windows.
	// +optional
	Family OSFamily `json:"family,omitempty"`
	// distro as reported by libguestfs, e.g. rhel, ubuntu, windows.
	// +optional
	Distro string `json:"distro,omitempty"`
	// osinfo is the libosinfo short ID, e.g. rhel9.8, win2k22.
	// +optional
	OSInfo string `json:"osinfo,omitempty"`
	// productName, e.g. "Red Hat Enterprise Linux 9.8 (Plow)".
	// +optional
	ProductName string `json:"productName,omitempty"`
	// firmware the converted guest needs to boot.
	// +optional
	Firmware FirmwareType `json:"firmware,omitempty"`
	// networkNotes explains how the guest's network config was adapted.
	// +optional
	NetworkNotes []string `json:"networkNotes,omitempty"`
}

type AgentStatus struct {
	// +optional
	Connected bool `json:"connected,omitempty"`
	// +optional
	LastHeartbeat *metav1.Time `json:"lastHeartbeat,omitempty"`
	// +optional
	Version string `json:"version,omitempty"`
}

// DiskStatus is per-disk progress.
type DiskStatus struct {
	// +required
	SourceDiskID string `json:"sourceDiskID"`
	// +optional
	PVCName string `json:"pvcName,omitempty"`
	// +optional
	BytesTotal int64 `json:"bytesTotal,omitempty"`
	// +optional
	BytesCopied int64 `json:"bytesCopied,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Copying;Copied;Attached;Skipped;Failed
	// +optional
	Phase string `json:"phase,omitempty"`
	// bootOrder from guest conversion; 0 means not bootable.
	// +optional
	BootOrder int32 `json:"bootOrder,omitempty"`
}

// PreflightSeverity tells whether a failed check blocks the migration.
// +kubebuilder:validation:Enum=Blocker;Warning;Info
type PreflightSeverity string

const (
	SeverityBlocker PreflightSeverity = "Blocker"
	SeverityWarning PreflightSeverity = "Warning"
	SeverityInfo    PreflightSeverity = "Info"
)

type PreflightResult struct {
	// +required
	Name string `json:"name"`
	// +required
	Passed bool `json:"passed"`
	// +required
	Severity PreflightSeverity `json:"severity"`
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=p2vm
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceRef.name`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="VM",type=string,JSONPath=`.status.vmName`
// +kubebuilder:printcolumn:name="Guest",type=string,JSONPath=`.status.guest.osinfo`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// P2VMigration migrates one P2VSource to a VirtualMachine.
type P2VMigration struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of P2VMigration
	// +required
	Spec P2VMigrationSpec `json:"spec"`

	// status defines the observed state of P2VMigration
	// +optional
	Status P2VMigrationStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// P2VMigrationList contains a list of P2VMigration
type P2VMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []P2VMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &P2VMigration{}, &P2VMigrationList{})
		return nil
	})
}
