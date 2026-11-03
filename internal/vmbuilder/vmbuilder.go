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

// Package vmbuilder maps a migration plan, the source inventory and the
// conversion result to a KubeVirt VirtualMachine.
package vmbuilder

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/convert"
)

// Labels on created VMs.
const (
	LabelMigration = "metal2vm.io/migration"
	LabelSource    = "metal2vm.io/source"
)

// Volume is a copied disk: its PVC and its index in the conversion plan.
type Volume struct {
	PVC   string
	Index int
}

// Input is everything needed to build the VM.
type Input struct {
	Migration *v1alpha1.P2VMigration
	Inventory *v1alpha1.Inventory
	Guest     convert.Result
	Volumes   []Volume
	// TestBoot builds the VM without any NIC, so it cannot clash with the
	// source on the network.
	TestBoot bool
}

// Name is the VM name for a migration.
func Name(m *v1alpha1.P2VMigration) string {
	if m.Spec.TargetVM.Name != "" {
		return m.Spec.TargetVM.Name
	}
	return m.Spec.SourceRef.Name
}

// Build returns the VirtualMachine for in.
func Build(in Input) (*kubevirtv1.VirtualMachine, error) {
	m, inv := in.Migration, in.Inventory
	if inv == nil {
		return nil, fmt.Errorf("source inventory is required")
	}
	if len(in.Volumes) == 0 {
		return nil, fmt.Errorf("no disks")
	}
	windows := in.Guest.OS.Name == "windows"

	cpu := cpuTopology(m.Spec.TargetVM.CPU, inv.CPU)
	mem, err := memory(m.Spec.TargetVM.Memory, inv.MemoryMiB)
	if err != nil {
		return nil, err
	}

	domain := kubevirtv1.DomainSpec{
		CPU:     cpu,
		Memory:  &kubevirtv1.Memory{Guest: &mem},
		Machine: &kubevirtv1.Machine{Type: "q35"},
		Devices: kubevirtv1.Devices{
			Rng: &kubevirtv1.Rng{},
		},
		Features: &kubevirtv1.Features{ACPI: kubevirtv1.FeatureState{}},
	}
	applyFirmware(&domain, m.Spec.TargetVM.Firmware, inv.Firmware, in.Guest.Firmware)
	if windows {
		applyWindows(&domain)
	}

	bus := kubevirtv1.DiskBusVirtio
	if m.Spec.TargetVM.DiskBus == "sata" {
		bus = kubevirtv1.DiskBusSATA
	}
	bootOrder := map[int]uint{}
	for _, d := range in.Guest.Disks {
		if d.BootOrder > 0 {
			bootOrder[d.Index] = uint(d.BootOrder)
		}
	}
	vols := append([]Volume(nil), in.Volumes...)
	slices.SortFunc(vols, func(a, b Volume) int { return a.Index - b.Index })
	var volumes []kubevirtv1.Volume
	for _, v := range vols {
		name := fmt.Sprintf("disk-%d", v.Index)
		disk := kubevirtv1.Disk{
			Name:       name,
			DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: bus}},
			Serial:     fmt.Sprintf("M2V%d", v.Index),
		}
		if bo, ok := bootOrder[v.Index]; ok {
			disk.BootOrder = &bo
		}
		domain.Devices.Disks = append(domain.Devices.Disks, disk)
		volumes = append(volumes, kubevirtv1.Volume{Name: name, VolumeSource: kubevirtv1.VolumeSource{
			PersistentVolumeClaim: &kubevirtv1.PersistentVolumeClaimVolumeSource{
				PersistentVolumeClaimVolumeSource: corePVC(v.PVC),
			},
		}})
	}
	// Without boot order hints, boot from the first disk.
	if len(bootOrder) == 0 {
		domain.Devices.Disks[0].BootOrder = new(uint(1))
	}

	var networks []kubevirtv1.Network
	if in.TestBoot {
		domain.Devices.AutoattachPodInterface = new(false)
	} else {
		domain.Devices.Interfaces, networks = Networking(m)
		if len(networks) == 0 {
			domain.Devices.AutoattachPodInterface = new(false)
		}
	}

	labels := map[string]string{
		"app.kubernetes.io/part-of": "metal2vm",
		LabelMigration:              m.Name,
		LabelSource:                 m.Spec.SourceRef.Name,
	}
	annotations := map[string]string{}
	if in.Guest.OS.OSInfo != "" {
		annotations["vm.kubevirt.io/os"] = in.Guest.OS.OSInfo
	}
	if in.Guest.OS.ProductName != "" {
		annotations["description"] = "Migrated from physical server " + m.Spec.SourceRef.Name + ": " + in.Guest.OS.ProductName
	}

	return &kubevirtv1.VirtualMachine{
		TypeMeta:   metav1.TypeMeta{APIVersion: kubevirtv1.SchemeGroupVersion.String(), Kind: "VirtualMachine"},
		ObjectMeta: metav1.ObjectMeta{Name: Name(m), Namespace: m.Namespace, Labels: labels, Annotations: annotations},
		Spec: kubevirtv1.VirtualMachineSpec{
			RunStrategy: new(kubevirtv1.RunStrategyAlways),
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelMigration: m.Name}},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain:   domain,
					Networks: networks,
					Volumes:  volumes,
				},
			},
		},
	}, nil
}

// Networking returns the production NICs from the plan's NIC mappings, in order.
func Networking(m *v1alpha1.P2VMigration) ([]kubevirtv1.Interface, []kubevirtv1.Network) {
	ifaces := make([]kubevirtv1.Interface, 0, len(m.Spec.NICs))
	nets := make([]kubevirtv1.Network, 0, len(m.Spec.NICs))
	for i, n := range m.Spec.NICs {
		name := fmt.Sprintf("nic-%d", i)
		iface := kubevirtv1.Interface{Name: name, Model: "virtio", MacAddress: TargetMAC(m, n)}
		net := kubevirtv1.Network{Name: name}
		switch n.Type {
		case v1alpha1.NetworkPod:
			iface.InterfaceBindingMethod = kubevirtv1.InterfaceBindingMethod{Masquerade: &kubevirtv1.InterfaceMasquerade{}}
			net.Pod = &kubevirtv1.PodNetwork{}
		case v1alpha1.NetworkMultus:
			iface.InterfaceBindingMethod = kubevirtv1.InterfaceBindingMethod{Bridge: &kubevirtv1.InterfaceBridge{}}
			net.Multus = &kubevirtv1.MultusNetwork{NetworkName: n.NetworkName}
		}
		ifaces = append(ifaces, iface)
		nets = append(nets, net)
	}
	return ifaces, nets
}

// TargetMAC is the VM NIC's MAC for a NIC mapping. It is decided before the
// VM exists, so guest conversion can point the network config at it.
//
// Guests often bind network config to the MAC (ifcfg HWADDR, NetworkManager
// mac-address, cloud-init "match: macaddress"). With masquerade the MAC never
// leaves the pod, so the pod network always keeps the source MAC; on bridged
// networks keepMAC decides. Otherwise the MAC is a stable, locally
// administered one derived from the migration and the source MAC.
func TargetMAC(m *v1alpha1.P2VMigration, n v1alpha1.NICMapping) string {
	if n.KeepMAC || n.Type == v1alpha1.NetworkPod {
		return strings.ToLower(n.SourceMAC)
	}
	sum := sha256.Sum256([]byte(m.Namespace + "/" + m.Name + "/" + strings.ToLower(n.SourceMAC)))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

func cpuTopology(plan *v1alpha1.VMCPU, src v1alpha1.CPUInfo) *kubevirtv1.CPU {
	c := &kubevirtv1.CPU{
		Sockets: uint32(max(src.Sockets, 1)),
		Cores:   uint32(max(src.CoresPerSocket, 1)),
		Threads: uint32(max(src.ThreadsPerCore, 1)),
	}
	if plan != nil {
		if plan.Sockets > 0 {
			c.Sockets = uint32(plan.Sockets)
		}
		if plan.Cores > 0 {
			c.Cores = uint32(plan.Cores)
		}
		if plan.Threads > 0 {
			c.Threads = uint32(plan.Threads)
		}
	}
	return c
}

func memory(plan *resource.Quantity, srcMiB int64) (resource.Quantity, error) {
	if plan != nil && !plan.IsZero() {
		return plan.DeepCopy(), nil
	}
	if srcMiB <= 0 {
		return resource.Quantity{}, fmt.Errorf("memory is unknown: set targetVM.memory")
	}
	return *resource.NewQuantity(srcMiB*(1<<20), resource.BinarySI), nil
}

// applyFirmware picks BIOS/UEFI: explicit override, then what conversion
// says the guest needs, then the source's firmware.
func applyFirmware(d *kubevirtv1.DomainSpec, o *v1alpha1.FirmwareOverride, src v1alpha1.Firmware, guestFW string) {
	fw := src.Type
	if guestFW == "uefi" || guestFW == "bios" {
		fw = v1alpha1.FirmwareType(guestFW)
	}
	secureBoot, tpm := src.SecureBoot, src.TPM
	if o != nil {
		if o.Type != "" {
			fw = o.Type
		}
		if o.SecureBoot != nil {
			secureBoot = *o.SecureBoot
		}
		if o.TPM != nil {
			tpm = *o.TPM
		}
	}
	if fw == v1alpha1.FirmwareUEFI {
		d.Firmware = &kubevirtv1.Firmware{Bootloader: &kubevirtv1.Bootloader{
			EFI: &kubevirtv1.EFI{SecureBoot: new(secureBoot), Persistent: new(true)},
		}}
		if secureBoot {
			d.Features.SMM = &kubevirtv1.FeatureState{Enabled: new(true)}
		}
	}
	if tpm {
		// Persistent so BitLocker/measured boot state survives restarts.
		d.Devices.TPM = &kubevirtv1.TPMDevice{Persistent: new(true)}
	}
}

// applyWindows adds the Hyper-V enlightenments and timers KubeVirt's Windows
// templates use.
func applyWindows(d *kubevirtv1.DomainSpec) {
	on := kubevirtv1.FeatureState{}
	d.Features.APIC = &kubevirtv1.FeatureAPIC{}
	d.Features.Hyperv = &kubevirtv1.FeatureHyperv{
		Relaxed: &on, VAPIC: &on, VPIndex: &on, Runtime: &on, SyNIC: &on,
		SyNICTimer: &kubevirtv1.SyNICTimer{Direct: &on}, Frequencies: &on, Reenlightenment: &on,
		TLBFlush: &kubevirtv1.TLBFlush{}, IPI: &on, Reset: &on,
		Spinlocks: &kubevirtv1.FeatureSpinlocks{Retries: new(uint32(8191))},
	}
	d.Clock = &kubevirtv1.Clock{
		ClockOffset: kubevirtv1.ClockOffset{UTC: &kubevirtv1.ClockOffsetUTC{}},
		Timer: &kubevirtv1.Timer{
			HPET:   &kubevirtv1.HPETTimer{Enabled: new(false)},
			PIT:    &kubevirtv1.PITTimer{TickPolicy: kubevirtv1.PITTickPolicyDelay},
			RTC:    &kubevirtv1.RTCTimer{TickPolicy: kubevirtv1.RTCTickPolicyCatchup},
			Hyperv: &kubevirtv1.HypervTimer{},
		},
	}
}

func corePVC(name string) corev1.PersistentVolumeClaimVolumeSource {
	return corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}
}
