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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// P2VSourceSpec describes a physical server and the ways metal2vm can reach it.
// At least one access method must be set. Without bmc or pxe the admin boots the
// live ISO manually (USB / KVM).
type P2VSourceSpec struct {
	// bmc enables remote boot of the live environment and power control via Redfish.
	// +optional
	BMC *BMCAccess `json:"bmc,omitempty"`

	// pxe enables network boot of the live environment.
	// +optional
	PXE *PXEAccess `json:"pxe,omitempty"`

	// osAccess enables a read-only pre-check of the running OS (inventory and
	// blockers such as BitLocker) before any downtime is scheduled.
	// +optional
	OSAccess *OSAccess `json:"osAccess,omitempty"`
}

// BMCAccess points at a Redfish-capable baseboard management controller.
type BMCAccess struct {
	// address is the Redfish base URL, e.g. https://10.0.0.10.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +required
	Address string `json:"address"`

	// systemID selects the Redfish ComputerSystem. Empty means the only system.
	// +optional
	SystemID string `json:"systemID,omitempty"`

	// credentialsSecretRef names a Secret in the same namespace with keys
	// "username" and "password".
	// +required
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`

	// insecureSkipVerify disables TLS verification of the BMC certificate.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// PXEAccess identifies the server's PXE boot interface.
type PXEAccess struct {
	// bootMACAddress is the MAC address of the NIC that PXE boots.
	// +kubebuilder:validation:Pattern=`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`
	// +required
	BootMACAddress string `json:"bootMACAddress"`
}

// OSAccessType is the remote management protocol of the running OS.
// +kubebuilder:validation:Enum=ssh;winrm
type OSAccessType string

const (
	OSAccessSSH   OSAccessType = "ssh"
	OSAccessWinRM OSAccessType = "winrm"
)

// OSAccess describes how to log into the running source OS.
type OSAccess struct {
	// +required
	Type OSAccessType `json:"type"`

	// address is the hostname or IP of the running OS.
	// +required
	Address string `json:"address"`

	// port defaults to 22 (ssh) or 5986 (winrm).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// credentialsSecretRef names a Secret with "username" and either "password"
	// or "ssh-privatekey". For WinRM over HTTPS it may also hold "ca.crt".
	// +required
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`

	// insecureSkipVerify disables WinRM HTTPS certificate verification
	// (self-signed WinRM listeners).
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// P2VSourceStatus is the observed state of a physical server.
type P2VSourceStatus struct {
	// inventory is the most recently discovered hardware and OS.
	// +optional
	Inventory *Inventory `json:"inventory,omitempty"`

	// bootMedia describes the live environment boot media for this source.
	// +optional
	BootMedia *BootMediaStatus `json:"bootMedia,omitempty"`

	// agent is the state of the live agent, when one is running.
	// +optional
	Agent *AgentStatus `json:"agent,omitempty"`

	// osConfig is what the OS pre-check found on the running source OS
	// (spec.osAccess). The live agent never overwrites it.
	// +optional
	OSConfig *OSConfig `json:"osConfig,omitempty"`

	// conditions: BootMediaReady, InventoryCollected, BMCReachable, OSReachable, Blocked.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// OSConfig is the running OS's identity and network configuration.
type OSConfig struct {
	// +required
	CollectedAt metav1.Time `json:"collectedAt"`
	// +optional
	OS OSInfo `json:"os,omitzero"`
	// nics as configured in the installed OS. name is the OS interface name.
	// +listType=map
	// +listMapKey=mac
	// +optional
	NICs []NIC `json:"nics,omitempty"`
	// hostKeyFingerprint is the SSH host key (SHA256) trusted on first use;
	// later pre-checks must present the same key.
	// +optional
	HostKeyFingerprint string `json:"hostKeyFingerprint,omitempty"`
	// warnings are migration risks found in the OS (e.g. BitLocker).
	// +optional
	Warnings []string `json:"warnings,omitempty"`
}

// BootMediaStatus points at the Secret holding the boot media URLs and token.
// The URLs embed credentials, so they are kept out of the status.
type BootMediaStatus struct {
	// secretName is a Secret in the same namespace with keys "isoURL",
	// "token" and "expiresAt".
	// +required
	SecretName string `json:"secretName"`
	// expiresAt is when the boot media and agent token stop working.
	// +required
	ExpiresAt metav1.Time `json:"expiresAt"`
}

// InventorySource tells who collected an inventory.
// +kubebuilder:validation:Enum=PreCheck;LiveAgent
type InventorySource string

const (
	InventoryFromPreCheck  InventorySource = "PreCheck"
	InventoryFromLiveAgent InventorySource = "LiveAgent"
)

// Inventory is the discovered description of a physical server.
type Inventory struct {
	// +required
	CollectedAt metav1.Time `json:"collectedAt"`
	// +required
	CollectedBy InventorySource `json:"collectedBy"`

	// +optional
	Firmware Firmware `json:"firmware,omitzero"`
	// +optional
	CPU CPUInfo `json:"cpu,omitzero"`
	// +optional
	MemoryMiB int64 `json:"memoryMiB,omitempty"`
	// +optional
	OS OSInfo `json:"os,omitzero"`

	// +listType=map
	// +listMapKey=id
	// +optional
	Disks []Disk `json:"disks,omitempty"`

	// +listType=map
	// +listMapKey=mac
	// +optional
	NICs []NIC `json:"nics,omitempty"`

	// warnings are human-readable migration risks (e.g. "BitLocker enabled on C:").
	// +optional
	Warnings []string `json:"warnings,omitempty"`
}

// FirmwareType is the boot firmware of a machine.
// +kubebuilder:validation:Enum=bios;uefi
type FirmwareType string

const (
	FirmwareBIOS FirmwareType = "bios"
	FirmwareUEFI FirmwareType = "uefi"
)

type Firmware struct {
	// +optional
	Type FirmwareType `json:"type,omitempty"`
	// +optional
	SecureBoot bool `json:"secureBoot,omitempty"`
	// tpm is true when a TPM 2.0 device is present.
	// +optional
	TPM bool `json:"tpm,omitempty"`
}

type CPUInfo struct {
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	Sockets int32 `json:"sockets,omitempty"`
	// +optional
	CoresPerSocket int32 `json:"coresPerSocket,omitempty"`
	// +optional
	ThreadsPerCore int32 `json:"threadsPerCore,omitempty"`
}

// OSFamily is the broad operating system family.
// +kubebuilder:validation:Enum=linux;windows;unknown
type OSFamily string

const (
	OSLinux   OSFamily = "linux"
	OSWindows OSFamily = "windows"
	OSUnknown OSFamily = "unknown"
)

type OSInfo struct {
	// +optional
	Family OSFamily `json:"family,omitempty"`
	// distribution, e.g. "rhel", "ubuntu", "sles", "windows-server".
	// +optional
	Distribution string `json:"distribution,omitempty"`
	// +optional
	Version string `json:"version,omitempty"`
	// +optional
	Hostname string `json:"hostname,omitempty"`
}

// DiskTransport is how a disk is attached to the source server.
// +kubebuilder:validation:Enum=sata;sas;nvme;scsi;fc;iscsi;usb;virtio;unknown
type DiskTransport string

const (
	TransportSATA    DiskTransport = "sata"
	TransportSAS     DiskTransport = "sas"
	TransportNVMe    DiskTransport = "nvme"
	TransportSCSI    DiskTransport = "scsi"
	TransportFC      DiskTransport = "fc"
	TransportISCSI   DiskTransport = "iscsi"
	TransportUSB     DiskTransport = "usb"
	TransportVirtio  DiskTransport = "virtio"
	TransportUnknown DiskTransport = "unknown"
)

// Disk is a whole block device on the source server.
type Disk struct {
	// id is stable across reboots (WWN, serial or /dev/disk/by-id name) and is
	// used to match pre-check and live-agent inventories.
	// +required
	ID string `json:"id"`
	// path is the kernel device path seen by the collector, e.g. /dev/sda.
	// +optional
	Path string `json:"path,omitempty"`
	// +required
	SizeBytes int64 `json:"sizeBytes"`
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	Serial string `json:"serial,omitempty"`
	// +optional
	WWN string `json:"wwn,omitempty"`
	// +optional
	Transport DiskTransport `json:"transport,omitempty"`
	// multipath is true when the disk is reachable over several paths.
	// +optional
	Multipath bool `json:"multipath,omitempty"`
	// boot is true when the OS boots from this disk.
	// +optional
	Boot bool `json:"boot,omitempty"`
	// encrypted is true when a BitLocker or LUKS volume was found on the disk.
	// +optional
	Encrypted bool `json:"encrypted,omitempty"`
}

// NIC is a physical network interface and its OS-level IP configuration.
type NIC struct {
	// +required
	MAC string `json:"mac"`
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	LinkSpeedMbps int32 `json:"linkSpeedMbps,omitempty"`
	// ipAddresses in CIDR notation.
	// +optional
	IPAddresses []string `json:"ipAddresses,omitempty"`
	// +optional
	Gateway string `json:"gateway,omitempty"`
	// +optional
	DNSServers []string `json:"dnsServers,omitempty"`
	// dhcp is true when the address was obtained via DHCP.
	// +optional
	DHCP bool `json:"dhcp,omitempty"`
	// vlanID is set for tagged sub-interfaces.
	// +optional
	VLANID int32 `json:"vlanID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=p2vs
// +kubebuilder:printcolumn:name="OS",type=string,JSONPath=`.status.inventory.os.distribution`
// +kubebuilder:printcolumn:name="Firmware",type=string,JSONPath=`.status.inventory.firmware.type`
// +kubebuilder:printcolumn:name="Inventory",type=string,JSONPath=`.status.inventory.collectedBy`
// +kubebuilder:printcolumn:name="Agent",type=boolean,JSONPath=`.status.agent.connected`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// P2VSource is a physical server that can be migrated to a VM.
type P2VSource struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of P2VSource
	// +required
	Spec P2VSourceSpec `json:"spec"`

	// status defines the observed state of P2VSource
	// +optional
	Status P2VSourceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// P2VSourceList contains a list of P2VSource
type P2VSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []P2VSource `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &P2VSource{}, &P2VSourceList{})
		return nil
	})
}
