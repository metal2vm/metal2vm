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

	"github.com/metal2vm/metal2vm/internal/netfix"
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
	_, err := Run(context.Background(), Plan{Name: "x", Disks: []Disk{{Path: "/dev/a"}}}, fake, "guestfish", dir, &log)
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
	r, err := Run(context.Background(), Plan{Name: "x", Disks: []Disk{{Path: "/dev/a"}}}, fake, "guestfish", dir, &strings.Builder{})
	if err != nil || r.OS.OSInfo != "rhel9.8" {
		t.Fatalf("%+v %v", r, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "domain.xml")); !strings.Contains(string(b), "/dev/a") {
		t.Fatal("domain.xml not written")
	}
}

// fakeGuestfish serves fixture files to tar-out/download and records the
// write-pass commands, so FixNetwork can be tested without libguestfs.
func fakeGuestfish(t *testing.T, fixtures map[string]string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "guest")
	for p, c := range fixtures {
		full := filepath.Join(root, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte(c), 0o644)
	}
	log = filepath.Join(dir, "commands.log")
	bin = filepath.Join(dir, "guestfish")
	script := `#!/bin/sh
ROOT=` + root + `
while read -r cmd a b; do
  echo "$cmd $a $b" >> ` + log + `
  case "$cmd" in
    -tar-out) [ -d "$ROOT$a" ] && tar -C "$ROOT$a" -cf "$b" . ;;
    -download) [ -f "$ROOT$a" ] && cp "$ROOT$a" "$b" ;;
    upload) mkdir -p "$(dirname "$ROOT$b")"; cp "$a" "$ROOT$b" ;;
  esac
done
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestFixNetwork(t *testing.T) {
	gf, log := fakeGuestfish(t, map[string]string{
		"/etc/sysconfig/network-scripts/ifcfg-eno1": "DEVICE=eno1\nHWADDR=3c:ec:ef:12:34:56\nBOOTPROTO=none\nIPADDR=10.1.2.3\nPREFIX=24\n",
	})
	work := t.TempDir()
	p := Plan{Name: "x", Disks: []Disk{{Path: "/dev/a"}}, Network: netfix.Plan{NICs: []netfix.NIC{
		{SourceMAC: "3c:ec:ef:12:34:56", TargetMAC: "02:00:00:00:00:01", DHCP: true},
	}}}
	notes, err := FixNetwork(context.Background(), p, gf, work, &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) == 0 {
		t.Error("expected notes")
	}
	guest := filepath.Join(filepath.Dir(gf), "guest")
	b, _ := os.ReadFile(filepath.Join(guest, "etc/sysconfig/network-scripts/ifcfg-eno1"))
	if !strings.Contains(string(b), "BOOTPROTO=dhcp") || !strings.Contains(string(b), "02:00:00:00:00:01") {
		t.Fatalf("ifcfg after fix:\n%s", b)
	}
	link, _ := os.ReadFile(filepath.Join(guest, "etc/systemd/network/10-metal2vm-eno1.link"))
	if !strings.Contains(string(link), "Name=eno1") {
		t.Fatalf("link file:\n%s", link)
	}
	cmds, _ := os.ReadFile(log)
	if !strings.Contains(string(cmds), "chmod 0644 /etc/systemd/network/10-metal2vm-eno1.link") ||
		strings.Contains(string(cmds), "chmod 0644 /etc/sysconfig") {
		t.Fatalf("new files get 0644, existing keep their mode:\n%s", cmds)
	}
}

func TestFixNetworkNoNICs(t *testing.T) {
	notes, err := FixNetwork(context.Background(), Plan{}, "/nonexistent", t.TempDir(), &strings.Builder{})
	if err != nil || notes != nil {
		t.Fatalf("%v %v", notes, err)
	}
}
