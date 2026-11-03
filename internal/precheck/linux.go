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

// Package precheck reads the identity and network configuration of a
// source server while its own OS is still running: over SSH for Linux and
// WinRM for Windows. It only runs read-only commands.
package precheck

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/metal2vm/metal2vm/api/v1alpha1"
)

// Credentials authenticate to the source OS.
type Credentials struct {
	Username   string
	Password   string
	PrivateKey []byte
}

// LinuxScript prints the sections ParseLinux reads. Every command is
// read-only and available on any distribution with iproute2.
const LinuxScript = `export LC_ALL=C
echo ===OS; cat /etc/os-release 2>/dev/null
echo ===HOSTNAME; hostname
echo ===LINK; ip -o link show
echo ===ADDR; ip -o addr show
echo ===ROUTE; ip -o route show default; ip -o -6 route show default
echo ===DNS; cat /etc/resolv.conf 2>/dev/null
echo ===BLOCK; lsblk -rno NAME,FSTYPE 2>/dev/null
echo ===END`

// SSH runs the Linux pre-check. knownFingerprint pins the host key (SHA256
// fingerprint); when empty the key is trusted on first use and returned.
func SSH(ctx context.Context, address string, port int32, creds Credentials, knownFingerprint string) (*v1alpha1.OSConfig, error) {
	var auth []ssh.AuthMethod
	if len(creds.PrivateKey) > 0 {
		signer, err := ssh.ParsePrivateKey(creds.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("parsing ssh-privatekey: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if creds.Password != "" {
		auth = append(auth, ssh.Password(creds.Password),
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				answers := make([]string, len(qs))
				for i := range answers {
					answers[i] = creds.Password
				}
				return answers, nil
			}))
	}
	if len(auth) == 0 {
		return nil, errors.New("the credentials Secret needs \"password\" or \"ssh-privatekey\"")
	}
	var seen string
	cfg := &ssh.ClientConfig{
		User: creds.Username,
		Auth: auth,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen = ssh.FingerprintSHA256(key)
			if knownFingerprint != "" && subtle.ConstantTimeCompare([]byte(seen), []byte(knownFingerprint)) != 1 {
				return fmt.Errorf("host key changed: expected %s, got %s", knownFingerprint, seen)
			}
			return nil
		},
		Timeout: 15 * time.Second,
	}
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(address, strconv.Itoa(int(port)))
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	defer func() { _ = client.Close() }()
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	out, err := sess.Output(LinuxScript)
	if err != nil {
		return nil, fmt.Errorf("running the pre-check: %w", err)
	}
	osc, err := ParseLinux(string(out))
	if err != nil {
		return nil, err
	}
	osc.HostKeyFingerprint = seen
	return osc, nil
}

// iface is one interface from "ip -o link/addr".
type iface struct {
	name    string
	mac     string
	kind    string // ifPhysical, ifVLAN or ifMember
	addrs   []string
	dynamic bool
}

const (
	ifPhysical = ""
	ifVLAN     = "vlan"
	ifMember   = "member"
)

// ParseLinux turns LinuxScript output into an OSConfig.
func ParseLinux(out string) (*v1alpha1.OSConfig, error) {
	sec := splitSections(out)
	if _, ok := sec["END"]; !ok && len(sec["LINK"]) == 0 {
		return nil, errors.New("unexpected pre-check output (is this a Linux host with iproute2?)")
	}
	osrel := keyValues(sec["OS"])
	osc := &v1alpha1.OSConfig{OS: v1alpha1.OSInfo{
		Family: v1alpha1.OSLinux, Distribution: osrel["ID"], Version: osrel["VERSION_ID"],
		Hostname: strings.TrimSpace(strings.Join(sec["HOSTNAME"], "")),
	}}
	ifaces := parseLinks(sec["LINK"])
	parseAddrs(sec["ADDR"], ifaces)
	gateways := parseGateways(sec["ROUTE"])
	dns := parseNameservers(sec["DNS"])

	for _, it := range ifaces {
		switch {
		case it.kind != ifPhysical:
			osc.Warnings = append(osc.Warnings, fmt.Sprintf(
				"%s is a VLAN, bond or bridge member: its IP configuration is migrated as-is; review it after migration", it.name))
		case isVirtualName(it.name):
			if len(it.addrs) > 0 {
				osc.Warnings = append(osc.Warnings, fmt.Sprintf(
					"%s is a virtual interface (bridge/bond/container): review its configuration after migration", it.name))
			}
		default:
			nic := v1alpha1.NIC{MAC: it.mac, Name: it.name, IPAddresses: it.addrs, DHCP: it.dynamic, Gateway: gateways[it.name]}
			if nic.Gateway != "" {
				nic.DNSServers = dns
			}
			osc.NICs = append(osc.NICs, nic)
		}
	}
	for _, l := range sec["BLOCK"] {
		if f := strings.Fields(l); len(f) == 2 && f[1] == "crypto_LUKS" {
			osc.Warnings = append(osc.Warnings, "LUKS-encrypted device "+f[0]+": conversion needs its passphrase or key")
		}
	}
	return osc, nil
}

func splitSections(out string) map[string][]string {
	sections := map[string][]string{}
	cur := ""
	s := bufio.NewScanner(strings.NewReader(out))
	s.Buffer(make([]byte, 1024*1024), 1024*1024)
	for s.Scan() {
		line := s.Text()
		if name, ok := strings.CutPrefix(line, "==="); ok {
			cur = name
			sections[cur] = nil // mark the section as present
			continue
		}
		sections[cur] = append(sections[cur], line)
	}
	return sections
}

func keyValues(lines []string) map[string]string {
	kv := map[string]string{}
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, "="); ok {
			kv[k] = strings.Trim(v, `"`)
		}
	}
	return kv
}

// parseLinks reads "2: eno1: <...> ... link/ether 3c:ec:ef:12:34:56 brd ...",
// keeping only Ethernet interfaces, in order.
func parseLinks(lines []string) []*iface {
	var out []*iface
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		it := &iface{name: strings.TrimSuffix(f[1], ":")}
		if base, _, ok := strings.Cut(it.name, "@"); ok {
			it.name, it.kind = base, ifVLAN
		}
		for i := range f {
			if f[i] == "link/ether" && i+1 < len(f) {
				it.mac = strings.ToLower(f[i+1])
			}
		}
		if it.mac == "" {
			continue // loopback, tunnels
		}
		if it.kind == ifPhysical && strings.Contains(l, "master ") {
			it.kind = ifMember
		}
		out = append(out, it)
	}
	return out
}

// parseAddrs reads "2: eno1    inet 10.1.2.3/24 ... scope global dynamic ...".
func parseAddrs(lines []string, ifaces []*iface) {
	byName := map[string]*iface{}
	for _, it := range ifaces {
		byName[it.name] = it
	}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 4 || (f[2] != "inet" && f[2] != "inet6") {
			continue
		}
		it := byName[f[1]]
		if it == nil || strings.Contains(l, "scope link") || strings.Contains(l, "scope host") {
			continue
		}
		it.addrs = append(it.addrs, f[3])
		if strings.Contains(l, " dynamic ") {
			it.dynamic = true
		}
	}
}

// parseGateways reads IPv4 "default via 10.1.2.1 dev eno1 ..." routes.
func parseGateways(lines []string) map[string]string {
	gws := map[string]string{}
	for _, l := range lines {
		f := strings.Fields(l)
		var via, dev string
		for i := 0; i+1 < len(f); i++ {
			switch f[i] {
			case "via":
				via = f[i+1]
			case "dev":
				dev = f[i+1]
			}
		}
		if via != "" && dev != "" && gws[dev] == "" && !strings.Contains(via, ":") {
			gws[dev] = via
		}
	}
	return gws
}

func parseNameservers(lines []string) []string {
	var dns []string
	for _, l := range lines {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "nameserver" {
			dns = append(dns, f[1])
		}
	}
	return dns
}

// isVirtualName matches common software interfaces that are not physical NICs.
func isVirtualName(n string) bool {
	for _, p := range []string{"br", "bond", "team", "virbr", "docker", "cni", "veth", "vnet", "tun", "tap", "podman", "flannel", "cali", "ovs"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}
