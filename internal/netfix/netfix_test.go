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

package netfix

import (
	"strings"
	"testing"
)

const (
	srcMAC = "3c:ec:ef:12:34:56"
	dstMAC = "02:a1:b2:c3:d4:e5"
)

func apply(t *testing.T, files map[string]string, nics ...NIC) Result {
	t.Helper()
	in := map[string][]byte{}
	for k, v := range files {
		in[k] = []byte(v)
	}
	return Apply(in, Plan{NICs: nics})
}

func file(t *testing.T, r Result, p string) string {
	t.Helper()
	b, ok := r.Files[p]
	if !ok {
		t.Fatalf("%s not written; notes: %v", p, r.Notes)
	}
	return string(b)
}

// The lab case: cloud-init rendered ifcfg-eth0 bound to the source MAC.
func TestIfcfgMACRewriteKernelNames(t *testing.T) {
	r := apply(t, map[string]string{
		"/etc/sysconfig/network-scripts/ifcfg-eth0": "# Created by cloud-init\nBOOTPROTO=dhcp\nDEVICE=eth0\nHWADDR=02:5D:16:EC:B4:F0\nONBOOT=yes\nTYPE=Ethernet\n",
	}, NIC{SourceMAC: "02:5d:16:ec:b4:f0", TargetMAC: "02:5d:16:ec:b4:f2"})
	got := file(t, r, "/etc/sysconfig/network-scripts/ifcfg-eth0")
	if !strings.Contains(got, "HWADDR=02:5D:16:EC:B4:F2") {
		t.Fatalf("MAC not rewritten (case kept):\n%s", got)
	}
	for p := range r.Files {
		if strings.HasSuffix(p, ".link") {
			t.Errorf("ethN names must not be pinned, got %s", p)
		}
	}
}

func TestRHELStaticKeepIPPinsName(t *testing.T) {
	ifcfg := "DEVICE=eno1\nHWADDR=" + srcMAC + "\nBOOTPROTO=none\nIPADDR=10.1.2.3\nPREFIX=24\nGATEWAY=10.1.2.1\nDNS1=10.1.2.2\nONBOOT=yes\n"
	r := apply(t, map[string]string{"/etc/sysconfig/network-scripts/ifcfg-eno1": ifcfg},
		NIC{SourceMAC: srcMAC, TargetMAC: srcMAC})
	if _, changed := r.Files["/etc/sysconfig/network-scripts/ifcfg-eno1"]; changed {
		t.Error("same MAC and keepIP: ifcfg must be untouched")
	}
	link := file(t, r, "/etc/systemd/network/10-metal2vm-eno1.link")
	if !strings.Contains(link, "MACAddress="+srcMAC) || !strings.Contains(link, "Name=eno1") {
		t.Fatalf("link file:\n%s", link)
	}
}

func TestIfcfgToDHCP(t *testing.T) {
	ifcfg := "DEVICE=eno1\nBOOTPROTO=none\nIPADDR=10.1.2.3\nPREFIX=24\nGATEWAY=10.1.2.1\nDNS1=10.1.2.2\nONBOOT=yes\n"
	r := apply(t, map[string]string{"/etc/sysconfig/network-scripts/ifcfg-eno1": ifcfg},
		NIC{SourceMAC: srcMAC, TargetMAC: dstMAC, OldName: "eno1", DHCP: true})
	got := file(t, r, "/etc/sysconfig/network-scripts/ifcfg-eno1")
	if !strings.Contains(got, "BOOTPROTO=dhcp") || strings.Contains(got, "IPADDR") || strings.Contains(got, "GATEWAY") ||
		strings.Contains(got, "DNS1") || !strings.Contains(got, "ONBOOT=yes") {
		t.Fatalf("ifcfg:\n%s", got)
	}
	if !strings.Contains(file(t, r, "/etc/systemd/network/10-metal2vm-eno1.link"), "MACAddress="+dstMAC) {
		t.Error("link must match the VM MAC")
	}
}

func TestSUSEIfcfgDiscoveryAndDHCP(t *testing.T) {
	r := apply(t, map[string]string{
		"/etc/sysconfig/network/ifcfg-eth1": "BOOTPROTO='static'\nSTARTMODE='auto'\nIPADDR='192.168.10.5/24'\nLLADDR='" + strings.ToUpper(srcMAC) + "'\n",
		"/etc/sysconfig/network/config":     "NETCONFIG_DNS_POLICY='auto'\n",
		"/etc/sysconfig/network/ifcfg-lo":   "IPADDR=127.0.0.1\n",
	}, NIC{SourceMAC: srcMAC, TargetMAC: dstMAC, DHCP: true})
	got := file(t, r, "/etc/sysconfig/network/ifcfg-eth1")
	if !strings.Contains(got, "BOOTPROTO=dhcp") || strings.Contains(got, "IPADDR") || !strings.Contains(got, strings.ToUpper(dstMAC)) {
		t.Fatalf("SUSE ifcfg:\n%s", got)
	}
	if _, ok := r.Files["/etc/sysconfig/network/ifcfg-lo"]; ok {
		t.Error("loopback config must not change")
	}
	if _, ok := r.Files["/etc/sysconfig/network/config"]; ok {
		t.Error("non-ifcfg files in /etc/sysconfig/network are not network configs")
	}
}

func TestNMKeyfile(t *testing.T) {
	key := "[connection]\nid=prod\ntype=ethernet\ninterface-name=ens192\n\n[ethernet]\nmac-address=" + strings.ToUpper(srcMAC) +
		"\n\n[ipv4]\naddress1=10.20.0.15/24,10.20.0.1\ndns=10.20.0.2;\nmethod=manual\n\n[ipv6]\nmethod=disabled\n"
	files := map[string]string{"/etc/NetworkManager/system-connections/prod.nmconnection": key}

	keep := apply(t, files, NIC{SourceMAC: srcMAC, TargetMAC: dstMAC})
	got := file(t, keep, "/etc/NetworkManager/system-connections/prod.nmconnection")
	if !strings.Contains(got, "mac-address="+strings.ToUpper(dstMAC)) || !strings.Contains(got, "address1=10.20.0.15/24") {
		t.Fatalf("keep IP keyfile:\n%s", got)
	}
	if !strings.Contains(file(t, keep, "/etc/systemd/network/10-metal2vm-ens192.link"), "Name=ens192") {
		t.Error("name discovered from keyfile must be pinned")
	}

	dhcp := apply(t, files, NIC{SourceMAC: srcMAC, TargetMAC: dstMAC, DHCP: true})
	got = file(t, dhcp, "/etc/NetworkManager/system-connections/prod.nmconnection")
	if !strings.Contains(got, "method=auto") || strings.Contains(got, "address1") || strings.Contains(got, "dns=") ||
		!strings.Contains(got, "[ipv6]\nmethod=disabled") {
		t.Fatalf("DHCP keyfile:\n%s", got)
	}
}

func TestNetplan(t *testing.T) {
	np := "# server config\nnetwork:\n  version: 2\n  ethernets:\n    eno1:\n      match:\n        macaddress: " + srcMAC +
		"\n      set-name: eno1\n      addresses: [10.0.5.20/24]\n      routes:\n        - to: default\n          via: 10.0.5.1\n      nameservers:\n        addresses: [10.0.5.2]\n"
	r := apply(t, map[string]string{"/etc/netplan/50-cloud-init.yaml": np}, NIC{SourceMAC: srcMAC, TargetMAC: dstMAC, DHCP: true})
	got := file(t, r, "/etc/netplan/50-cloud-init.yaml")
	if !strings.Contains(got, "macaddress: "+dstMAC) || !strings.Contains(got, "dhcp4: true") ||
		strings.Contains(got, "10.0.5.20") || strings.Contains(got, "nameservers") || !strings.Contains(got, "# server config") {
		t.Fatalf("netplan:\n%s", got)
	}
	if _, ok := r.Files["/etc/systemd/network/10-metal2vm-eno1.link"]; !ok {
		t.Error("set-name must be discovered and pinned")
	}
}

func TestDebianInterfaces(t *testing.T) {
	eni := "auto lo\niface lo inet loopback\n\nauto enp3s0\niface enp3s0 inet static\n    address 172.16.0.10\n    netmask 255.255.255.0\n    gateway 172.16.0.1\n    dns-nameservers 172.16.0.2\n    mtu 1500\n"
	r := apply(t, map[string]string{"/etc/network/interfaces": eni},
		NIC{SourceMAC: srcMAC, TargetMAC: dstMAC, OldName: "enp3s0", DHCP: true})
	got := file(t, r, "/etc/network/interfaces")
	if !strings.Contains(got, "iface enp3s0 inet dhcp") || strings.Contains(got, "address 172") ||
		strings.Contains(got, "gateway") || !strings.Contains(got, "mtu 1500") || !strings.Contains(got, "iface lo inet loopback") {
		t.Fatalf("interfaces:\n%s", got)
	}
}

func TestNetworkd(t *testing.T) {
	nw := "[Match]\nName=eno2\n\n[Network]\nAddress=10.9.9.9/24\nGateway=10.9.9.1\nDNS=10.9.9.2\n\n[Route]\nDestination=10.50.0.0/16\nGateway=10.9.9.254\n"
	r := apply(t, map[string]string{"/etc/systemd/network/20-eno2.network": nw},
		NIC{SourceMAC: srcMAC, TargetMAC: dstMAC, OldName: "eno2", DHCP: true})
	got := file(t, r, "/etc/systemd/network/20-eno2.network")
	if !strings.Contains(got, "DHCP=yes") || strings.Contains(got, "Address=") || strings.Contains(got, "[Route]") ||
		!strings.Contains(got, "Name=eno2") {
		t.Fatalf("networkd:\n%s", got)
	}
}

func TestUdevPersistentNet(t *testing.T) {
	rules := "SUBSYSTEM==\"net\", ACTION==\"add\", DRIVERS==\"?*\", ATTR{address}==\"" + srcMAC + "\", NAME=\"em1\"\n"
	r := apply(t, map[string]string{"/etc/udev/rules.d/70-persistent-net.rules": rules}, NIC{SourceMAC: srcMAC, TargetMAC: dstMAC})
	if got := file(t, r, "/etc/udev/rules.d/70-persistent-net.rules"); !strings.Contains(got, dstMAC) || !strings.Contains(got, "em1") {
		t.Fatalf("udev rule:\n%s", got)
	}
	if _, ok := r.Files["/etc/systemd/network/10-metal2vm-em1.link"]; !ok {
		t.Error("name from udev rule must be pinned")
	}
}

func TestKernelNamesOnCmdline(t *testing.T) {
	r := apply(t, map[string]string{
		"/etc/default/grub":                         "GRUB_CMDLINE_LINUX=\"crashkernel=auto net.ifnames=0 biosdevname=0\"\n",
		"/etc/sysconfig/network-scripts/ifcfg-eno1": "DEVICE=eno1\nHWADDR=" + srcMAC + "\n",
	}, NIC{SourceMAC: srcMAC, TargetMAC: dstMAC})
	for p := range r.Files {
		if strings.HasSuffix(p, ".link") {
			t.Fatalf("net.ifnames=0 guests must not get .link files: %s", p)
		}
	}
	if _, ok := r.Files["/etc/default/grub"]; ok {
		t.Fatal("boot config must never be modified")
	}
}

func TestNoConfigFound(t *testing.T) {
	r := apply(t, map[string]string{}, NIC{SourceMAC: srcMAC, TargetMAC: dstMAC, DHCP: true})
	if len(r.Files) != 0 {
		t.Fatalf("nothing to change, got %v", r.Files)
	}
	if len(r.Notes) == 0 {
		t.Fatal("expected a note explaining that defaults apply")
	}
}

func TestReplaceMACStyles(t *testing.T) {
	in := "a=3C:EC:EF:12:34:56 b=3c-ec-ef-12-34-56 c=3c:ec:ef:12:34:57"
	got := string(replaceMAC([]byte(in), srcMAC, dstMAC))
	if got != "a=02:A1:B2:C3:D4:E5 b=02-a1-b2-c3-d4-e5 c=3c:ec:ef:12:34:57" {
		t.Fatalf("got %q", got)
	}
}
