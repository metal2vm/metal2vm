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

// Package convert runs guest conversion (virt-v2v-in-place) on copied disks
// inside a conversion pod and summarizes what it found.
package convert

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Plan is the conversion pod's input, passed as JSON in $CONVERT_PLAN.
type Plan struct {
	Name      string `json:"name"`
	MemoryMiB int64  `json:"memoryMiB"`
	VCPUs     int32  `json:"vcpus"`
	// Firmware is "bios" or "uefi", as detected on the source.
	Firmware string `json:"firmware"`
	Disks    []Disk `json:"disks"`
	// StaticIPs are virt-v2v --mac values, "MAC:ip:IP,GW,LEN,DNS..." (Windows only).
	StaticIPs []string `json:"staticIPs,omitempty"`
}

// Disk is one copied disk: a block device or a disk.img file.
type Disk struct {
	Path string `json:"path"`
	File bool   `json:"file,omitempty"`
}

// Result is what the conversion pod reports in its termination message.
type Result struct {
	Firmware string     `json:"firmware,omitempty"`
	OS       OS         `json:"os"`
	Disks    []DiskInfo `json:"disks,omitempty"`
	Error    string     `json:"error,omitempty"`
}

// OS identifies the converted guest.
type OS struct {
	Name        string `json:"name,omitempty"`
	Distro      string `json:"distro,omitempty"`
	OSInfo      string `json:"osinfo,omitempty"`
	ProductName string `json:"productName,omitempty"`
	Major       int    `json:"major,omitempty"`
	Minor       int    `json:"minor,omitempty"`
}

// DiskInfo is per-disk inspection output. BootOrder is 0 when not bootable.
type DiskInfo struct {
	Index     int `json:"index"`
	BootOrder int `json:"bootOrder,omitempty"`
}

// DomainXML renders the libvirt domain virt-v2v-in-place reads as input.
// Values are XML-escaped; the plan comes from the operator, not from users.
func DomainXML(p Plan) (string, error) {
	type source struct {
		Dev  string `xml:"dev,attr,omitempty"`
		File string `xml:"file,attr,omitempty"`
	}
	type disk struct {
		Type   string `xml:"type,attr"`
		Device string `xml:"device,attr"`
		Driver struct {
			Name string `xml:"name,attr"`
			Type string `xml:"type,attr"`
		} `xml:"driver"`
		Source source `xml:"source"`
		Target struct {
			Dev string `xml:"dev,attr"`
			Bus string `xml:"bus,attr"`
		} `xml:"target"`
	}
	type domain struct {
		XMLName xml.Name `xml:"domain"`
		Type    string   `xml:"type,attr"`
		Name    string   `xml:"name"`
		Memory  struct {
			Unit  string `xml:"unit,attr"`
			Value int64  `xml:",chardata"`
		} `xml:"memory"`
		VCPU int32 `xml:"vcpu"`
		OS   struct {
			Firmware string `xml:"firmware,attr,omitempty"`
			Type     struct {
				Arch    string `xml:"arch,attr"`
				Machine string `xml:"machine,attr"`
				Value   string `xml:",chardata"`
			} `xml:"type"`
		} `xml:"os"`
		Disks []disk `xml:"devices>disk"`
	}
	if len(p.Disks) == 0 {
		return "", fmt.Errorf("no disks to convert")
	}
	if len(p.Disks) > 26 {
		return "", fmt.Errorf("at most 26 disks are supported, got %d", len(p.Disks))
	}
	d := domain{Type: "kvm", Name: p.Name, VCPU: max(p.VCPUs, 1)}
	d.Memory.Unit, d.Memory.Value = "MiB", max(p.MemoryMiB, 1024)
	if p.Firmware == "uefi" {
		d.OS.Firmware = "efi"
	}
	d.OS.Type.Arch, d.OS.Type.Machine, d.OS.Type.Value = "x86_64", "q35", "hvm"
	for i, pd := range p.Disks {
		var x disk
		x.Device = "disk"
		x.Driver.Name, x.Driver.Type = "qemu", "raw"
		if pd.File {
			x.Type, x.Source.File = "file", pd.Path
		} else {
			x.Type, x.Source.Dev = "block", pd.Path
		}
		// sata is what the source "had" from virt-v2v's point of view; it
		// rewrites the guest for virtio regardless.
		x.Target.Dev, x.Target.Bus = "sd"+string(rune('a'+i)), "sata"
		d.Disks = append(d.Disks, x)
	}
	out, err := xml.MarshalIndent(d, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// ParseInspection reads virt-v2v's inspector XML (-O output).
func ParseInspection(r io.Reader) (Result, error) {
	var doc struct {
		Disks []struct {
			Index     int `xml:"index,attr"`
			BootOrder int `xml:"boot-order"`
		} `xml:"disks>disk"`
		Firmware struct {
			Type string `xml:"type,attr"`
		} `xml:"firmware"`
		OS struct {
			Name        string `xml:"name"`
			Distro      string `xml:"distro"`
			OSInfo      string `xml:"osinfo"`
			ProductName string `xml:"product_name"`
			Major       int    `xml:"major_version"`
			Minor       int    `xml:"minor_version"`
		} `xml:"operatingsystem"`
	}
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return Result{}, fmt.Errorf("decoding inspector XML: %w", err)
	}
	res := Result{
		Firmware: doc.Firmware.Type,
		OS: OS{Name: doc.OS.Name, Distro: doc.OS.Distro, OSInfo: doc.OS.OSInfo,
			ProductName: doc.OS.ProductName, Major: doc.OS.Major, Minor: doc.OS.Minor},
	}
	for _, d := range doc.Disks {
		res.Disks = append(res.Disks, DiskInfo{Index: d.Index, BootOrder: d.BootOrder})
	}
	return res, nil
}

// Args are the virt-v2v-in-place arguments for a plan.
func Args(p Plan, domainXML, inspectionXML string) []string {
	args := make([]string, 0, 5+2*len(p.StaticIPs))
	args = append(args, "-i", "libvirtxml", domainXML, "-O", inspectionXML)
	for _, ip := range p.StaticIPs {
		args = append(args, "--mac", ip)
	}
	return args
}

// Run converts the guest described by p, streaming virt-v2v's output to log.
func Run(ctx context.Context, p Plan, v2v, workDir string, log io.Writer) (Result, error) {
	domain, err := DomainXML(p)
	if err != nil {
		return Result{}, err
	}
	domainPath := filepath.Join(workDir, "domain.xml")
	inspectPath := filepath.Join(workDir, "inspection.xml")
	if err := os.WriteFile(domainPath, []byte(domain), 0o644); err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, v2v, Args(p, domainPath, inspectPath)...)
	var tail tailBuffer
	// One writer for both streams: exec then serializes Write calls (two
	// distinct writers would be written from two goroutines at once).
	out := io.MultiWriter(log, &tail)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("virt-v2v-in-place: %w: %s", err, tail.lastError())
	}
	f, err := os.Open(inspectPath)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = f.Close() }()
	return ParseInspection(f)
}

// tailBuffer keeps the last lines of output to explain failures.
type tailBuffer struct{ lines []string }

func (t *tailBuffer) Write(p []byte) (int, error) {
	for l := range strings.SplitSeq(string(p), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			t.lines = append(t.lines, l)
			if len(t.lines) > 20 {
				t.lines = t.lines[1:]
			}
		}
	}
	return len(p), nil
}

// lastError returns the last "error:" line, or the last line.
func (t *tailBuffer) lastError() string {
	for _, l := range slices.Backward(t.lines) {
		if strings.Contains(l, "error:") {
			return l
		}
	}
	if len(t.lines) > 0 {
		return t.lines[len(t.lines)-1]
	}
	return ""
}

// WriteTermination writes r as the container's termination message.
func WriteTermination(path string, r Result) {
	if len(r.Error) > 1500 {
		r.Error = r.Error[:1500]
	}
	if b, err := json.Marshal(r); err == nil {
		_ = os.WriteFile(path, b, 0o644)
	}
}
