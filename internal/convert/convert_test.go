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

package convert

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDomainXML(t *testing.T) {
	out, err := DomainXML(Plan{
		Name: "web01 & co", MemoryMiB: 4096, VCPUs: 2, Firmware: "uefi",
		Disks: []Disk{{Path: "/dev/m2v/disk0"}, {Path: "/m2v/disk1/disk.img", File: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<name>web01 &amp; co</name>", "firmware=\"efi\"", "machine=\"q35\"",
		"<source dev=\"/dev/m2v/disk0\"></source>", "<source file=\"/m2v/disk1/disk.img\"></source>",
		"dev=\"sdb\"", "<memory unit=\"MiB\">4096</memory>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("domain XML missing %q:\n%s", want, out)
		}
	}
	var v any
	if err := xml.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("domain XML does not parse: %v", err)
	}
	bios, _ := DomainXML(Plan{Name: "x", Firmware: "bios", Disks: []Disk{{Path: "/dev/a"}}})
	if strings.Contains(bios, "firmware=") {
		t.Error("BIOS guests must not request EFI firmware")
	}
	if _, err := DomainXML(Plan{Name: "x"}); err == nil {
		t.Error("expected error without disks")
	}
}

// The fixture is real virt-v2v 2.13.8 output for the lab RHEL 9.8 server.
func TestParseInspection(t *testing.T) {
	f, err := os.Open("testdata/rhel98-inspection.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	r, err := ParseInspection(f)
	if err != nil {
		t.Fatal(err)
	}
	if r.Firmware != "uefi" || r.OS.Name != "linux" || r.OS.Distro != "rhel" || r.OS.OSInfo != "rhel9.8" ||
		r.OS.Major != 9 || r.OS.Minor != 8 || !strings.HasPrefix(r.OS.ProductName, "Red Hat Enterprise Linux 9.8") {
		t.Fatalf("result %+v", r)
	}
	if len(r.Disks) != 2 || r.Disks[0].BootOrder != 1 || r.Disks[1].BootOrder != 3 {
		t.Fatalf("disks %+v", r.Disks)
	}
}

func TestArgs(t *testing.T) {
	got := strings.Join(Args(Plan{StaticIPs: []string{"aa:bb:cc:dd:ee:ff:ip:10.0.0.5,10.0.0.1,24,10.0.0.2"}}, "/w/d.xml", "/w/i.xml"), " ")
	want := "-i libvirtxml /w/d.xml -O /w/i.xml --mac aa:bb:cc:dd:ee:ff:ip:10.0.0.5,10.0.0.1,24,10.0.0.2"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestRunReportsLastError(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "v2v")
	_ = os.WriteFile(fake, []byte("#!/bin/sh\necho '[ 1.0] Opening the source'\necho 'virt-v2v-in-place: error: inspection could not detect the source guest' >&2\nexit 1\n"), 0o755)
	var log strings.Builder
	_, err := Run(context.Background(), Plan{Name: "x", Disks: []Disk{{Path: "/dev/a"}}}, fake, dir, &log)
	if err == nil || !strings.Contains(err.Error(), "could not detect the source guest") {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(log.String(), "Opening the source") {
		t.Error("output not streamed to the log")
	}
}

func TestRunSuccess(t *testing.T) {
	dir := t.TempDir()
	fixture, _ := filepath.Abs("testdata/rhel98-inspection.xml")
	fake := filepath.Join(dir, "v2v")
	// Copy the fixture to the -O path (5th argument), like virt-v2v does.
	_ = os.WriteFile(fake, []byte("#!/bin/sh\ncp "+fixture+" \"$5\"\n"), 0o755)
	r, err := Run(context.Background(), Plan{Name: "x", Disks: []Disk{{Path: "/dev/a"}}}, fake, dir, &strings.Builder{})
	if err != nil || r.OS.OSInfo != "rhel9.8" {
		t.Fatalf("%+v %v", r, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "domain.xml")); !strings.Contains(string(b), "/dev/a") {
		t.Fatal("domain.xml not written")
	}
}
