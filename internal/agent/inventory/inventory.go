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

// Package inventory discovers the hardware of a server booted into the live
// environment. It only reads /sys, /proc and the first sectors of partitions.
package inventory

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/metal2vm/metal2vm/api/v1alpha1"
)

// secureBootVar is the EFI global variable holding the Secure Boot state.
const secureBootVar = "SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c"

// liveMediaMount is where the CoreOS live environment mounts its boot medium.
const liveMediaMount = "/run/media/iso"

// Collector reads hardware facts. Root and DevRoot exist for tests.
type Collector struct {
	// Root is prepended to /sys and /proc paths. Empty means "/".
	Root string
	// DevRoot is where block device nodes live. Empty means "/dev".
	DevRoot string
}

func (c Collector) path(p string) string { return filepath.Join(c.root(), p) }

func (c Collector) root() string {
	if c.Root == "" {
		return "/"
	}
	return c.Root
}

func (c Collector) dev(name string) string {
	d := c.DevRoot
	if d == "" {
		d = "/dev"
	}
	return filepath.Join(d, name)
}

func (c Collector) read(p string) string {
	b, err := os.ReadFile(c.path(p))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (c Collector) exists(p string) bool {
	_, err := os.Stat(c.path(p))
	return err == nil
}

// Collect gathers the full inventory.
func (c Collector) Collect() (*v1alpha1.Inventory, error) {
	disks, warnings, err := c.Disks()
	if err != nil {
		return nil, err
	}
	nics, err := c.NICs()
	if err != nil {
		return nil, err
	}
	return &v1alpha1.Inventory{
		CollectedAt: metav1.Now(),
		CollectedBy: v1alpha1.InventoryFromLiveAgent,
		Firmware:    c.Firmware(),
		CPU:         c.CPU(),
		MemoryMiB:   c.MemoryMiB(),
		// The installed OS is identified during conversion, not from the live environment.
		OS:       v1alpha1.OSInfo{Family: v1alpha1.OSUnknown},
		Disks:    disks,
		NICs:     nics,
		Warnings: warnings,
	}, nil
}

// Firmware reports BIOS/UEFI, Secure Boot and TPM 2.0 presence.
func (c Collector) Firmware() v1alpha1.Firmware {
	fw := v1alpha1.Firmware{Type: v1alpha1.FirmwareBIOS}
	if c.exists("sys/firmware/efi") {
		fw.Type = v1alpha1.FirmwareUEFI
		// efivarfs: 4 bytes of attributes followed by the value.
		if b, err := os.ReadFile(c.path("sys/firmware/efi/efivars/" + secureBootVar)); err == nil && len(b) >= 5 {
			fw.SecureBoot = b[4] == 1
		}
	}
	fw.TPM = c.read("sys/class/tpm/tpm0/tpm_version_major") == "2"
	return fw
}

// CPU reports the model and topology from /proc/cpuinfo.
func (c Collector) CPU() v1alpha1.CPUInfo {
	f, err := os.Open(c.path("proc/cpuinfo"))
	if err != nil {
		return v1alpha1.CPUInfo{}
	}
	defer func() { _ = f.Close() }()
	var info v1alpha1.CPUInfo
	sockets := map[string]bool{}
	var cores, siblings int
	s := bufio.NewScanner(f)
	for s.Scan() {
		k, v, ok := strings.Cut(s.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "model name":
			info.Model = v
		case "physical id":
			sockets[v] = true
		case "cpu cores":
			cores, _ = strconv.Atoi(v)
		case "siblings":
			siblings, _ = strconv.Atoi(v)
		}
	}
	info.Sockets = int32(max(len(sockets), 1))
	if cores > 0 {
		info.CoresPerSocket = int32(cores)
		info.ThreadsPerCore = int32(max(siblings/cores, 1))
	}
	return info
}

// MemoryMiB reports installed memory. Online memory blocks count memory the
// kernel reserved, so they are closer to the installed size than MemTotal.
func (c Collector) MemoryMiB() int64 {
	if bs, err := strconv.ParseInt(c.read("sys/devices/system/memory/block_size_bytes"), 16, 64); err == nil && bs > 0 {
		blocks, _ := filepath.Glob(c.path("sys/devices/system/memory/memory*"))
		var online int64
		for _, b := range blocks {
			if s, err := os.ReadFile(filepath.Join(b, "online")); err == nil && strings.TrimSpace(string(s)) == "1" {
				online++
			}
		}
		if online > 0 {
			return online * bs / (1 << 20)
		}
	}
	f, err := os.Open(c.path("proc/meminfo"))
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "MemTotal:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			return kb / 1024
		}
	}
	return 0
}

// skipDisk matches virtual block devices that are never migrated.
func skipDisk(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "sr", "dm-", "md", "nbd", "fd"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Disks lists whole disks. Paths to the same LUN (same WWN) are merged and
// marked multipath. The live boot medium is excluded.
func (c Collector) Disks() ([]v1alpha1.Disk, []string, error) {
	entries, err := os.ReadDir(c.path("sys/block"))
	if err != nil {
		return nil, nil, fmt.Errorf("listing block devices: %w", err)
	}
	liveDisk := c.liveMediaDisk()
	byID := map[string]*v1alpha1.Disk{}
	var order []string
	var warnings []string
	for _, e := range entries {
		name := e.Name()
		if skipDisk(name) || name == liveDisk {
			continue
		}
		sys := "sys/block/" + name
		sectors, _ := strconv.ParseInt(c.read(sys+"/size"), 10, 64)
		if sectors == 0 {
			continue // empty card readers, unattached LUN placeholders
		}
		d := v1alpha1.Disk{
			Path:      "/dev/" + name,
			SizeBytes: sectors * 512,
			Model:     firstNonEmpty(c.read(sys+"/device/model"), c.read(sys+"/model")),
			Serial:    firstNonEmpty(c.read(sys+"/device/serial"), c.read(sys+"/serial")),
			WWN:       firstNonEmpty(c.read(sys+"/wwid"), c.read(sys+"/device/wwid")),
			Transport: c.transport(name),
		}
		d.ID = diskID(name, d)
		if prev, ok := byID[d.ID]; ok {
			prev.Multipath = true
			continue
		}
		for _, part := range c.partitions(name) {
			switch c.probeEncryption(part) {
			case "bitlocker":
				d.Encrypted = true
				warnings = append(warnings, fmt.Sprintf("BitLocker volume on %s: suspend BitLocker or provide the recovery key before migration", part))
			case "luks":
				d.Encrypted = true
				warnings = append(warnings, fmt.Sprintf("LUKS volume on %s: the passphrase or key is needed for conversion", part))
			}
		}
		byID[d.ID] = &d
		order = append(order, d.ID)
	}
	disks := make([]v1alpha1.Disk, 0, len(order))
	for _, id := range order {
		disks = append(disks, *byID[id])
	}
	return disks, warnings, nil
}

func diskID(name string, d v1alpha1.Disk) string {
	switch {
	case d.WWN != "":
		return sanitizeID("wwn-" + d.WWN)
	case d.Serial != "" && d.Model != "":
		return sanitizeID("serial-" + d.Model + "-" + d.Serial)
	case d.Serial != "":
		return sanitizeID("serial-" + d.Serial)
	default:
		return sanitizeID("name-" + name)
	}
}

// sanitizeID makes an identifier safe for use in Kubernetes names and labels.
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	// ATA WWIDs ("t10.ATA     MODEL      SERIAL") pad fields with spaces.
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-.")
}

// transport derives the attachment type from the device's sysfs path.
func (c Collector) transport(name string) v1alpha1.DiskTransport {
	if strings.HasPrefix(name, "nvme") {
		return v1alpha1.TransportNVMe
	}
	if strings.HasPrefix(name, "vd") {
		return v1alpha1.TransportVirtio
	}
	target, err := filepath.EvalSymlinks(c.path("sys/block/" + name))
	if err != nil {
		return v1alpha1.TransportUnknown
	}
	switch {
	case strings.Contains(target, "/usb"):
		return v1alpha1.TransportUSB
	case strings.Contains(target, "/rport-"):
		return v1alpha1.TransportFC
	case strings.Contains(target, "/session"):
		return v1alpha1.TransportISCSI
	case strings.Contains(target, "/end_device-"), strings.Contains(target, "/sas_"):
		return v1alpha1.TransportSAS
	case strings.Contains(target, "/ata"):
		return v1alpha1.TransportSATA
	case strings.Contains(target, "/host"):
		// Includes virtio-scsi (…/virtioN/hostM/…); virtio-blk disks are vdX.
		return v1alpha1.TransportSCSI
	case strings.Contains(target, "/virtio"):
		return v1alpha1.TransportVirtio
	}
	return v1alpha1.TransportUnknown
}

// partitions lists partition device names of a disk, e.g. sda1, nvme0n1p1.
func (c Collector) partitions(disk string) []string {
	entries, err := os.ReadDir(c.path("sys/block/" + disk))
	if err != nil {
		return nil
	}
	var parts []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), disk) && c.exists("sys/block/"+disk+"/"+e.Name()+"/partition") {
			parts = append(parts, e.Name())
		}
	}
	slices.Sort(parts)
	return parts
}

// probeEncryption reads the first 4 KiB of a partition and recognizes
// LUKS and BitLocker headers.
func (c Collector) probeEncryption(part string) string {
	f, err := os.Open(c.dev(part))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	hdr := make([]byte, 4096)
	n, _ := io.ReadFull(f, hdr)
	hdr = hdr[:n]
	switch {
	case bytes.HasPrefix(hdr, []byte("LUKS\xba\xbe")):
		return "luks"
	case len(hdr) >= 11 && string(hdr[3:11]) == "-FVE-FS-":
		return "bitlocker"
	}
	return ""
}

// liveMediaDisk returns the disk holding the live boot medium (e.g. a USB
// stick), or "" when booted from virtual media, PXE or an optical drive.
func (c Collector) liveMediaDisk() string {
	f, err := os.Open(c.path("proc/mounts"))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 || fields[1] != liveMediaMount {
			continue
		}
		dev := strings.TrimPrefix(fields[0], "/dev/")
		// Map a partition back to its disk via /sys/class/block/<part>/..
		if target, err := filepath.EvalSymlinks(c.path("sys/class/block/" + dev)); err == nil &&
			c.exists("sys/class/block/"+dev+"/partition") {
			return filepath.Base(filepath.Dir(target))
		}
		return dev
	}
	return ""
}

// NICs lists physical network interfaces (those backed by a device).
func (c Collector) NICs() ([]v1alpha1.NIC, error) {
	entries, err := os.ReadDir(c.path("sys/class/net"))
	if err != nil {
		return nil, fmt.Errorf("listing network interfaces: %w", err)
	}
	var nics []v1alpha1.NIC
	for _, e := range entries {
		name := e.Name()
		base := "sys/class/net/" + name
		if !c.exists(base + "/device") {
			continue // lo, bridges, bonds, VLANs, veths
		}
		// Bond members report the bond's MAC; the permanent address is the real one.
		mac := firstNonEmpty(c.read(base+"/bonding_slave/perm_hwaddr"), c.read(base+"/address"))
		nic := v1alpha1.NIC{Name: name, MAC: strings.ToLower(mac)}
		if speed, err := strconv.Atoi(c.read(base + "/speed")); err == nil && speed > 0 {
			nic.LinkSpeedMbps = int32(speed)
		}
		nics = append(nics, nic)
	}
	return nics, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
