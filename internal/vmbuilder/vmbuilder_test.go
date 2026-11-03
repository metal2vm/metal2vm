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

package vmbuilder

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/convert"
)

func migration() *v1alpha1.P2VMigration {
	return &v1alpha1.P2VMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "web01", Namespace: "apps"},
		Spec: v1alpha1.P2VMigrationSpec{
			SourceRef: corev1.LocalObjectReference{Name: "web01-server"},
			NICs: []v1alpha1.NICMapping{
				{SourceMAC: "3C:EC:EF:12:34:56", Type: v1alpha1.NetworkMultus, NetworkName: "vlan100", KeepMAC: true},
				{SourceMAC: "3c:ec:ef:12:34:57", Type: v1alpha1.NetworkPod},
			},
		},
	}
}

var uefiInventory = &v1alpha1.Inventory{
	Firmware:  v1alpha1.Firmware{Type: v1alpha1.FirmwareUEFI, SecureBoot: true, TPM: true},
	CPU:       v1alpha1.CPUInfo{Sockets: 2, CoresPerSocket: 8, ThreadsPerCore: 2},
	MemoryMiB: 65536,
}

func rhel() convert.Result {
	return convert.Result{Firmware: "uefi", OS: convert.OS{Name: "linux", OSInfo: "rhel9.8", ProductName: "RHEL 9.8"},
		Disks: []convert.DiskInfo{{Index: 0, BootOrder: 1}, {Index: 1, BootOrder: 3}}}
}

func TestBuildLinuxUEFI(t *testing.T) {
	vm, err := Build(Input{Migration: migration(), Inventory: uefiInventory, Guest: rhel(),
		Volumes: []Volume{{PVC: "web01-disk-1", Index: 1}, {PVC: "web01-disk-0", Index: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	if vm.Name != "web01-server" || vm.Labels[LabelMigration] != "web01" || vm.Annotations["vm.kubevirt.io/os"] != "rhel9.8" {
		t.Fatalf("metadata %+v", vm.ObjectMeta)
	}
	d := vm.Spec.Template.Spec.Domain
	if d.CPU.Sockets != 2 || d.CPU.Cores != 8 || d.CPU.Threads != 2 {
		t.Errorf("cpu %+v", d.CPU)
	}
	if want := resource.MustParse("64Gi"); d.Memory.Guest.Cmp(want) != 0 {
		t.Errorf("memory %s", d.Memory.Guest)
	}
	if d.Firmware == nil || !*d.Firmware.Bootloader.EFI.SecureBoot || d.Features.SMM == nil || !*d.Features.SMM.Enabled {
		t.Errorf("firmware %+v features %+v", d.Firmware, d.Features)
	}
	if d.Devices.TPM == nil || !*d.Devices.TPM.Persistent {
		t.Error("source TPM must become a persistent vTPM")
	}
	if d.Features.Hyperv != nil {
		t.Error("no Hyper-V enlightenments for Linux")
	}
	disks := d.Devices.Disks
	if len(disks) != 2 || disks[0].Name != "disk-0" || *disks[0].BootOrder != 1 || *disks[1].BootOrder != 3 {
		t.Fatalf("disks %+v", disks)
	}
	if string(disks[0].Disk.Bus) != "virtio" || vm.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "web01-disk-0" {
		t.Errorf("disk 0 %+v", disks[0])
	}

	ifaces, nets := d.Devices.Interfaces, vm.Spec.Template.Spec.Networks
	if len(ifaces) != 2 || ifaces[0].MacAddress != "3c:ec:ef:12:34:56" || ifaces[0].Bridge == nil ||
		nets[0].Multus.NetworkName != "vlan100" || ifaces[1].Masquerade == nil || nets[1].Pod == nil ||
		ifaces[1].MacAddress != "3c:ec:ef:12:34:57" {
		t.Errorf("networking %+v %+v", ifaces, nets)
	}
}

func TestTargetMAC(t *testing.T) {
	m := migration()
	bridged := v1alpha1.NICMapping{SourceMAC: "3C:EC:EF:12:34:56", Type: v1alpha1.NetworkMultus, NetworkName: "v"}
	got := TargetMAC(m, bridged)
	if got == "3c:ec:ef:12:34:56" || got[:3] != "02:" || len(got) != 17 {
		t.Fatalf("bridged without keepMAC: %q", got)
	}
	if TargetMAC(m, bridged) != got {
		t.Fatal("generated MAC must be stable")
	}
	m2 := migration()
	m2.Name = "other"
	if TargetMAC(m2, bridged) == got {
		t.Fatal("generated MAC must differ per migration")
	}
	bridged.KeepMAC = true
	if TargetMAC(m, bridged) != "3c:ec:ef:12:34:56" {
		t.Fatal("keepMAC must keep the source MAC")
	}
}

func TestBuildTestBootHasNoNICs(t *testing.T) {
	vm, err := Build(Input{Migration: migration(), Inventory: uefiInventory, Guest: rhel(), TestBoot: true,
		Volumes: []Volume{{PVC: "p", Index: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	dev := vm.Spec.Template.Spec.Domain.Devices
	if len(dev.Interfaces) != 0 || len(vm.Spec.Template.Spec.Networks) != 0 || dev.AutoattachPodInterface == nil || *dev.AutoattachPodInterface {
		t.Fatalf("test boot VM must have no network: %+v", dev)
	}
}

func TestBuildWindowsBIOS(t *testing.T) {
	m := migration()
	m.Spec.TargetVM.Name = "dc01"
	m.Spec.TargetVM.Memory = ptrQ("8Gi")
	m.Spec.TargetVM.CPU = &v1alpha1.VMCPU{Sockets: 1, Cores: 4}
	inv := &v1alpha1.Inventory{Firmware: v1alpha1.Firmware{Type: v1alpha1.FirmwareBIOS}, MemoryMiB: 32768}
	guest := convert.Result{Firmware: "bios", OS: convert.OS{Name: "windows", OSInfo: "win2k22"}}
	vm, err := Build(Input{Migration: m, Inventory: inv, Guest: guest, Volumes: []Volume{{PVC: "p", Index: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	d := vm.Spec.Template.Spec.Domain
	if vm.Name != "dc01" || d.Firmware != nil || d.Devices.TPM != nil {
		t.Errorf("BIOS VM: name %s firmware %+v tpm %+v", vm.Name, d.Firmware, d.Devices.TPM)
	}
	if d.Features.Hyperv == nil || d.Clock == nil || d.Clock.Timer.HPET == nil {
		t.Error("Windows needs Hyper-V enlightenments and timers")
	}
	if d.CPU.Sockets != 1 || d.CPU.Cores != 4 || d.CPU.Threads != 1 || d.Memory.Guest.String() != "8Gi" {
		t.Errorf("plan overrides ignored: cpu %+v memory %s", d.CPU, d.Memory.Guest)
	}
	if *d.Devices.Disks[0].BootOrder != 1 {
		t.Error("without boot hints the first disk boots")
	}
}

func TestBuildFirmwareOverride(t *testing.T) {
	m := migration()
	off := false
	m.Spec.TargetVM.Firmware = &v1alpha1.FirmwareOverride{SecureBoot: &off, TPM: &off}
	vm, err := Build(Input{Migration: m, Inventory: uefiInventory, Guest: rhel(), Volumes: []Volume{{PVC: "p", Index: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	d := vm.Spec.Template.Spec.Domain
	if *d.Firmware.Bootloader.EFI.SecureBoot || d.Features.SMM != nil || d.Devices.TPM != nil {
		t.Errorf("override ignored: %+v %+v", d.Firmware.Bootloader.EFI, d.Devices.TPM)
	}
}

func TestBuildNeedsMemory(t *testing.T) {
	if _, err := Build(Input{Migration: migration(), Inventory: &v1alpha1.Inventory{}, Volumes: []Volume{{PVC: "p"}}}); err == nil {
		t.Fatal("expected error when memory is unknown")
	}
}

func ptrQ(s string) *resource.Quantity { q := resource.MustParse(s); return &q }
