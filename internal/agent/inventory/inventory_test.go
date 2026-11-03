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

package inventory

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/metal2vm/metal2vm/api/v1alpha1"
)

// fakeSystem writes a fake /sys, /proc and /dev tree for one server.
type fakeSystem struct {
	t    *testing.T
	root string
	dev  string
}

func newFakeSystem(t *testing.T) *fakeSystem {
	return &fakeSystem{t: t, root: t.TempDir(), dev: t.TempDir()}
}

func (f *fakeSystem) write(p, content string) {
	f.t.Helper()
	full := filepath.Join(f.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// disk creates /sys/devices/<devpath>/block/<name> and links /sys/block/<name> to it.
func (f *fakeSystem) disk(name, devpath string, sizeBytes int64, attrs map[string]string, parts ...string) {
	f.t.Helper()
	real := filepath.Join("sys/devices", devpath, "block", name)
	f.write(filepath.Join(real, "size"), itoa(sizeBytes/512))
	for k, v := range attrs {
		f.write(filepath.Join(real, k), v)
	}
	for _, p := range parts {
		f.write(filepath.Join(real, p, "partition"), "1")
		if err := os.MkdirAll(filepath.Join(f.root, "sys/class/block"), 0o755); err != nil {
			f.t.Fatal(err)
		}
		_ = os.Symlink(filepath.Join(f.root, real, p), filepath.Join(f.root, "sys/class/block", p))
	}
	if err := os.MkdirAll(filepath.Join(f.root, "sys/block"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.root, real), filepath.Join(f.root, "sys/block", name)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSystem) devFile(name string, content []byte) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dev, name), content, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func TestCollect(t *testing.T) {
	f := newFakeSystem(t)
	const gib = int64(1) << 30

	// UEFI with Secure Boot on, TPM 2.0.
	f.write("sys/firmware/efi/efivars/"+secureBootVar, "\x06\x00\x00\x00\x01")
	f.write("sys/class/tpm/tpm0/tpm_version_major", "2")

	// 2 sockets x 8 cores x 2 threads.
	f.write("proc/cpuinfo", "processor\t: 0\nmodel name\t: Intel(R) Xeon(R) Gold 6338\nphysical id\t: 0\nsiblings\t: 16\ncpu cores\t: 8\n\n"+
		"processor\t: 1\nmodel name\t: Intel(R) Xeon(R) Gold 6338\nphysical id\t: 1\nsiblings\t: 16\ncpu cores\t: 8\n")
	// 4 online 128 MiB blocks + 1 offline = 512 MiB.
	f.write("sys/devices/system/memory/block_size_bytes", "8000000")
	for i, on := range []string{"1", "1", "1", "1", "0"} {
		f.write("sys/devices/system/memory/memory"+itoa(int64(i))+"/online", on)
	}

	// SAS RAID volume with a BitLocker partition (Windows) and a plain one.
	f.disk("sda", "pci0000:00/0000:00:01.0/host0/port-0:0/end_device-0:0/target0:0:0/0:0:0:0", 480*gib,
		map[string]string{"device/model": "PERC H755", "wwid": "naa.6d0946606b0b3c00"}, "sda1", "sda2")
	bitlocker := make([]byte, 4096)
	copy(bitlocker[3:], "-FVE-FS-")
	f.devFile("sda1", make([]byte, 4096))
	f.devFile("sda2", bitlocker)

	// The same FC LUN seen over two paths, with a LUKS partition.
	for _, n := range []string{"sdb", "sdc"} {
		f.disk(n, "pci0000:00/0000:00:02.0/host1/rport-1:0-"+n+"/target1:0:0/1:0:0:1", 2048*gib,
			map[string]string{"device/model": "FlashArray", "wwid": "naa.624a9370b1d2"}, n+"1")
	}
	f.devFile("sdb1", append([]byte("LUKS\xba\xbe"), make([]byte, 100)...))

	// NVMe without WWN, identified by serial.
	f.disk("nvme0n1", "pci0000:00/0000:00:03.0/nvme/nvme0", 960*gib,
		map[string]string{"device/model": "Samsung PM9A3", "device/serial": "S64HNE0R1"})

	// A virtio-scsi disk in a VM is SCSI, not virtio.
	f.disk("sdf", "pci0000:00/0000:00:05.0/virtio2/host4/target4:0:0/4:0:0:0", 5*gib,
		map[string]string{"device/model": "QEMU HARDDISK", "device/serial": "M2V-DATA-01"})

	// Live USB stick (excluded), loop device (excluded), empty card reader (excluded).
	f.disk("sdd", "pci0000:00/0000:00:14.0/usb1/1-1/1-1:1.0/host2/target2:0:0/2:0:0:0", 16*gib,
		map[string]string{"device/model": "USB Flash"}, "sdd1")
	f.write("proc/mounts", "/dev/sdd1 /run/media/iso iso9660 ro 0 0\n")
	f.disk("loop0", "virtual", 1*gib, nil)
	f.disk("sde", "pci0000:00/0000:00:14.0/usb1/1-2/host3/target3:0:0/3:0:0:0", 0, nil)

	// One physical NIC at 10G, one bond member, and loopback (excluded).
	f.write("sys/class/net/eno1/address", "3C:EC:EF:12:34:56")
	f.write("sys/class/net/eno1/speed", "10000")
	f.write("sys/class/net/eno1/device/vendor", "0x8086")
	f.write("sys/class/net/eno2/address", "3c:ec:ef:12:34:56")
	f.write("sys/class/net/eno2/bonding_slave/perm_hwaddr", "3c:ec:ef:12:34:57")
	f.write("sys/class/net/eno2/speed", "-1")
	f.write("sys/class/net/eno2/device/vendor", "0x8086")
	f.write("sys/class/net/lo/address", "00:00:00:00:00:00")

	inv, err := Collector{Root: f.root, DevRoot: f.dev}.Collect()
	if err != nil {
		t.Fatal(err)
	}

	if inv.Firmware != (v1alpha1.Firmware{Type: v1alpha1.FirmwareUEFI, SecureBoot: true, TPM: true}) {
		t.Errorf("firmware %+v", inv.Firmware)
	}
	if inv.CPU.Sockets != 2 || inv.CPU.CoresPerSocket != 8 || inv.CPU.ThreadsPerCore != 2 || inv.CPU.Model == "" {
		t.Errorf("cpu %+v", inv.CPU)
	}
	if inv.MemoryMiB != 512 {
		t.Errorf("memory %d MiB, want 512", inv.MemoryMiB)
	}

	want := map[string]v1alpha1.Disk{
		"wwn-naa.6d0946606b0b3c00":         {Transport: v1alpha1.TransportSAS, SizeBytes: 480 * gib, Encrypted: true},
		"wwn-naa.624a9370b1d2":             {Transport: v1alpha1.TransportFC, SizeBytes: 2048 * gib, Encrypted: true, Multipath: true},
		"serial-samsung-pm9a3-s64hne0r1":   {Transport: v1alpha1.TransportNVMe, SizeBytes: 960 * gib},
		"serial-qemu-harddisk-m2v-data-01": {Transport: v1alpha1.TransportSCSI, SizeBytes: 5 * gib},
	}
	if len(inv.Disks) != len(want) {
		t.Fatalf("got %d disks %+v, want %d", len(inv.Disks), inv.Disks, len(want))
	}
	for _, d := range inv.Disks {
		w, ok := want[d.ID]
		if !ok {
			t.Errorf("unexpected disk %q", d.ID)
			continue
		}
		if d.Transport != w.Transport || d.SizeBytes != w.SizeBytes || d.Encrypted != w.Encrypted || d.Multipath != w.Multipath {
			t.Errorf("disk %s = %+v, want %+v", d.ID, d, w)
		}
	}
	if len(inv.Warnings) != 2 {
		t.Errorf("warnings %v, want BitLocker and LUKS", inv.Warnings)
	}

	if len(inv.NICs) != 2 {
		t.Fatalf("nics %+v", inv.NICs)
	}
	if inv.NICs[0].MAC != "3c:ec:ef:12:34:56" || inv.NICs[0].LinkSpeedMbps != 10000 {
		t.Errorf("eno1 %+v", inv.NICs[0])
	}
	if inv.NICs[1].MAC != "3c:ec:ef:12:34:57" || inv.NICs[1].LinkSpeedMbps != 0 {
		t.Errorf("eno2 should use permanent MAC and unknown speed: %+v", inv.NICs[1])
	}
}

func TestFirmwareBIOS(t *testing.T) {
	f := newFakeSystem(t)
	if fw := (Collector{Root: f.root}).Firmware(); fw.Type != v1alpha1.FirmwareBIOS || fw.SecureBoot || fw.TPM {
		t.Fatalf("firmware %+v", fw)
	}
}

func TestMemoryFallsBackToMeminfo(t *testing.T) {
	f := newFakeSystem(t)
	f.write("proc/meminfo", "MemTotal:       65756344 kB\nMemFree: 1 kB\n")
	if got := (Collector{Root: f.root}).MemoryMiB(); got != 64215 {
		t.Fatalf("memory %d", got)
	}
}
