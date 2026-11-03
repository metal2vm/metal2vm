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

// Package netfix keeps a Linux guest's network working after it moves from
// physical NICs to VM NICs. It edits the guest's own config files:
//
//  1. MAC addresses of source NICs are rewritten to the VM NICs' MACs.
//  2. The installed OS's interface names are pinned with systemd .link
//     files, so configs bound to names (DEVICE=eno1) keep matching.
//  3. NICs moved to a new network have their static IP config switched to DHCP.
//
// It is pure: it takes file contents and returns edits, so every config
// format can be unit-tested. The conversion pod reads and writes the files.
package netfix

import (
	"bytes"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// NIC maps one source NIC to its VM NIC.
type NIC struct {
	SourceMAC string `json:"sourceMAC"`
	TargetMAC string `json:"targetMAC"`
	// OldName is the interface name in the installed OS (from the OS
	// pre-check). Empty means: discover it from the config files.
	OldName string `json:"oldName,omitempty"`
	// DHCP switches this NIC's config to DHCP (the VM is on a new network).
	DHCP bool `json:"dhcp,omitempty"`
}

// Plan is the network fix-up for one guest.
type Plan struct {
	NICs []NIC `json:"nics"`
}

// GuestPath is a file or directory netfix reads from the guest.
type GuestPath struct {
	Path string
	Dir  bool
}

// Paths lists the guest files and directories netfix reads.
var Paths = []GuestPath{
	{"/etc/NetworkManager/system-connections", true},
	{"/etc/sysconfig/network-scripts", true},
	{"/etc/sysconfig/network", true},
	{"/etc/netplan", true},
	{"/etc/network/interfaces", false},
	{"/etc/network/interfaces.d", true},
	{"/etc/systemd/network", true},
	{"/etc/udev/rules.d/70-persistent-net.rules", false},
	{"/etc/default/grub", false},
	{"/etc/kernel/cmdline", false},
	{"/boot/loader/entries", true},
}

// Result holds the files to write back (changed or new) and what was done.
type Result struct {
	Files map[string][]byte
	Notes []string
}

// Apply computes the edits for files (guest path → content).
func Apply(files map[string][]byte, p Plan) Result {
	res := Result{Files: map[string][]byte{}}
	cur := maps.Clone(files)
	if cur == nil {
		cur = map[string][]byte{}
	}
	set := func(p string, b []byte) {
		if !bytes.Equal(cur[p], b) {
			cur[p] = b
			res.Files[p] = b
		}
	}
	note := func(format string, a ...any) { res.Notes = append(res.Notes, fmt.Sprintf(format, a...)) }

	kernelNames := usesKernelNames(cur)
	for _, n := range p.NICs {
		src, dst := normMAC(n.SourceMAC), normMAC(n.TargetMAC)
		name := n.OldName
		if name == "" {
			name = discoverName(cur, src)
			if name != "" {
				note("interface for %s found in the guest config: %s", src, name)
			}
		}

		// 1. Rewrite MAC references.
		if src != dst {
			for _, p := range sortedNetworkFiles(cur) {
				if b := replaceMAC(cur[p], src, dst); !bytes.Equal(b, cur[p]) {
					set(p, b)
					note("%s: MAC %s -> %s", p, src, dst)
				}
			}
		}

		// 2. Pin the interface name.
		switch {
		case name == "":
			note("no interface name known for %s; the guest's default naming applies", src)
		case kernelNames || ethName.MatchString(name):
			note("%s uses kernel (ethN) naming; not pinned", name)
		default:
			link := fmt.Sprintf("/etc/systemd/network/10-metal2vm-%s.link", name)
			set(link, []byte(fmt.Sprintf("# Written by metal2vm: keep the interface name of the migrated server.\n"+
				"[Match]\nMACAddress=%s\n\n[Link]\nName=%s\n", dst, name)))
			note("pinned %s to %s", dst, name)
		}

		// 3. Switch to DHCP.
		if n.DHCP {
			changed := false
			for _, p := range sortedNetworkFiles(cur) {
				if b, ok := toDHCP(p, cur[p], name, dst); ok && !bytes.Equal(b, cur[p]) {
					set(p, b)
					note("%s: %s switched to DHCP", p, firstNonEmpty(name, dst))
					changed = true
				}
			}
			if !changed {
				note("no static config found for %s; the guest's default (usually DHCP) applies", firstNonEmpty(name, dst))
			}
		}
	}
	return res
}

var (
	ethName   = regexp.MustCompile(`^eth[0-9]+$`)
	macInText = regexp.MustCompile(`(?i)\b([0-9a-f]{2}[:-]){5}[0-9a-f]{2}\b`)
)

func normMAC(m string) string { return strings.ToLower(strings.ReplaceAll(m, "-", ":")) }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// isNetworkFile reports whether p holds network config (not boot config).
func isNetworkFile(p string) bool {
	switch {
	case strings.HasPrefix(p, "/etc/default/"), strings.HasPrefix(p, "/boot/"), p == "/etc/kernel/cmdline":
		return false
	case strings.HasPrefix(p, "/etc/sysconfig/network-scripts/"), strings.HasPrefix(p, "/etc/sysconfig/network/"):
		return strings.HasPrefix(path.Base(p), "ifcfg-")
	}
	return true
}

func sortedNetworkFiles(files map[string][]byte) []string {
	var out []string
	for p := range files {
		if isNetworkFile(p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// usesKernelNames reports net.ifnames=0 on the kernel command line.
func usesKernelNames(files map[string][]byte) bool {
	for p, b := range files {
		if !isNetworkFile(p) && bytes.Contains(b, []byte("net.ifnames=0")) {
			return true
		}
	}
	return false
}

// replaceMAC rewrites every occurrence of src (any case, ':' or '-') to dst,
// keeping the original case and separator style.
func replaceMAC(b []byte, src, dst string) []byte {
	return macInText.ReplaceAllFunc(b, func(m []byte) []byte {
		if normMAC(string(m)) != src {
			return m
		}
		out := dst
		if bytes.ContainsRune(m, '-') {
			out = strings.ReplaceAll(out, ":", "-")
		}
		if bytes.ToUpper(m)[0] == m[0] && bytes.ContainsAny(m, "ABCDEF") {
			out = strings.ToUpper(out)
		}
		return []byte(out)
	})
}

// discoverName finds the interface name bound to mac in any config format.
func discoverName(files map[string][]byte, mac string) string {
	for _, p := range sortedNetworkFiles(files) {
		b := files[p]
		if !macInText.Match(b) {
			continue
		}
		hasMAC := false
		for _, m := range macInText.FindAll(b, -1) {
			if normMAC(string(m)) == mac {
				hasMAC = true
			}
		}
		if !hasMAC {
			continue
		}
		switch {
		case strings.Contains(p, "/ifcfg-"):
			v := shellVars(b)
			if n := firstNonEmpty(v["DEVICE"], strings.TrimPrefix(path.Base(p), "ifcfg-")); n != "" {
				return n
			}
		case strings.HasSuffix(p, ".nmconnection"):
			if n := iniGet(b, "connection", "interface-name"); n != "" {
				return n
			}
		case strings.HasPrefix(p, "/etc/netplan/"):
			if n := netplanNameForMAC(b, mac); n != "" {
				return n
			}
		case strings.HasSuffix(p, ".link"):
			if n := iniGet(b, "Link", "Name"); n != "" {
				return n
			}
		case strings.HasSuffix(p, "70-persistent-net.rules"):
			for line := range strings.SplitSeq(string(b), "\n") {
				if strings.Contains(strings.ToLower(line), mac) {
					if m := udevName.FindStringSubmatch(line); m != nil {
						return m[1]
					}
				}
			}
		}
	}
	return ""
}

var udevName = regexp.MustCompile(`NAME="([^"]+)"`)

// toDHCP rewrites file p to use DHCP for the NIC (by name or MAC). It
// returns false when the file does not configure that NIC.
func toDHCP(p string, b []byte, name, mac string) ([]byte, bool) {
	switch {
	case strings.Contains(p, "/ifcfg-"):
		v := shellVars(b)
		match := (name != "" && (v["DEVICE"] == name || path.Base(p) == "ifcfg-"+name)) ||
			normMAC(firstNonEmpty(v["HWADDR"], v["MACADDR"], v["LLADDR"])) == mac
		if !match {
			return nil, false
		}
		return ifcfgToDHCP(b), true
	case strings.HasSuffix(p, ".nmconnection"):
		match := (name != "" && iniGet(b, "connection", "interface-name") == name) ||
			normMAC(iniGet(b, "ethernet", "mac-address")) == mac
		if !match {
			return nil, false
		}
		return nmToDHCP(b), true
	case strings.HasPrefix(p, "/etc/netplan/"):
		return netplanToDHCP(b, name, mac)
	case p == "/etc/network/interfaces" || strings.HasPrefix(p, "/etc/network/interfaces.d/"):
		if name == "" {
			return nil, false
		}
		return eniToDHCP(b, name)
	case strings.HasPrefix(p, "/etc/systemd/network/") && strings.HasSuffix(p, ".network"):
		match := (name != "" && iniGet(b, "Match", "Name") == name) || normMAC(iniGet(b, "Match", "MACAddress")) == mac
		if !match {
			return nil, false
		}
		return networkdToDHCP(b), true
	}
	return nil, false
}

// ifcfg (RHEL, SUSE): shell-style KEY=value files.

func shellVars(b []byte) map[string]string {
	v := map[string]string{}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, val, ok := strings.Cut(line, "=")
		if ok {
			v[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(val), `"'`)
		}
	}
	return v
}

var ifcfgStatic = regexp.MustCompile(`^(IPADDR|PREFIX|NETMASK|GATEWAY|BROADCAST|NETWORK|DNS)[0-9_]*=`)

func ifcfgToDHCP(b []byte) []byte {
	var out []string
	hasProto := false
	for line := range strings.SplitSeq(string(b), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case ifcfgStatic.MatchString(t):
			continue
		case strings.HasPrefix(t, "BOOTPROTO="):
			out = append(out, "BOOTPROTO=dhcp")
			hasProto = true
		default:
			out = append(out, line)
		}
	}
	if !hasProto {
		out = insertBeforeTrailingEmpty(out, "BOOTPROTO=dhcp")
	}
	return []byte(strings.Join(out, "\n"))
}

func insertBeforeTrailingEmpty(lines []string, l string) []string {
	if n := len(lines); n > 0 && lines[n-1] == "" {
		return append(lines[:n-1], l, "")
	}
	return append(lines, l)
}

// INI-style files (NetworkManager keyfiles, systemd-networkd).

func iniGet(b []byte, section, key string) string {
	cur := ""
	for line := range strings.SplitSeq(string(b), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			cur = t[1 : len(t)-1]
			continue
		}
		if k, v, ok := strings.Cut(t, "="); ok && cur == section && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

var nmStaticKey = regexp.MustCompile(`^(address[0-9]*|addresses|gateway|dns|dns-search|route[0-9]*)\s*=`)

func nmToDHCP(b []byte) []byte {
	var out []string
	section := ""
	sawIPv4 := false
	for line := range strings.SplitSeq(string(b), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = t[1 : len(t)-1]
			if section == "ipv4" {
				sawIPv4 = true
			}
			out = append(out, line)
			continue
		}
		if section == "ipv4" || section == "ipv6" {
			if nmStaticKey.MatchString(t) {
				continue
			}
			if strings.HasPrefix(t, "method=") && strings.TrimSpace(strings.TrimPrefix(t, "method=")) == "manual" {
				out = append(out, "method=auto")
				continue
			}
		}
		out = append(out, line)
	}
	if !sawIPv4 {
		out = insertBeforeTrailingEmpty(out, "[ipv4]\nmethod=auto")
	}
	return []byte(strings.Join(out, "\n"))
}

var networkdStatic = regexp.MustCompile(`^(Address|Gateway|DNS|Domains)\s*=`)

func networkdToDHCP(b []byte) []byte {
	var out []string
	section := ""
	skip := false
	for line := range strings.SplitSeq(string(b), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = t[1 : len(t)-1]
			// Static [Address] and [Route] sections belong to the old network.
			skip = section == "Address" || section == "Route"
			if skip {
				continue
			}
			out = append(out, line)
			if section == "Network" {
				out = append(out, "DHCP=yes")
			}
			continue
		}
		if skip || (section == "Network" && (networkdStatic.MatchString(t) || strings.HasPrefix(t, "DHCP="))) {
			continue
		}
		out = append(out, line)
	}
	return []byte(strings.Join(out, "\n"))
}

// Debian /etc/network/interfaces.

var eniOption = regexp.MustCompile(`^(address|netmask|gateway|broadcast|network|dns-nameservers|dns-search|pointopoint)\b`)

func eniToDHCP(b []byte, name string) ([]byte, bool) {
	lines := strings.Split(string(b), "\n")
	var out []string
	in, found := false, false
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) > 0 && slices.Contains([]string{"iface", "auto", "allow-hotplug", "mapping", "source", "source-directory"}, f[0]) {
			in = false
			if f[0] == "iface" && len(f) >= 4 && f[1] == name && f[2] == "inet" && f[3] == "static" {
				line = strings.Replace(line, "static", "dhcp", 1)
				in, found = true, true
			}
		} else if in && len(f) > 0 && eniOption.MatchString(f[0]) {
			continue
		}
		out = append(out, line)
	}
	if !found {
		return nil, false
	}
	return []byte(strings.Join(out, "\n")), true
}

// netplan (Ubuntu): YAML, edited as a node tree to keep comments and order.

func netplanEthernets(b []byte) (*yaml.Node, *yaml.Node) {
	var doc yaml.Node
	if yaml.Unmarshal(b, &doc) != nil || len(doc.Content) == 0 {
		return nil, nil
	}
	network := mapGet(doc.Content[0], "network")
	if network == nil {
		return &doc, nil
	}
	return &doc, mapGet(network, "ethernets")
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func value(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	return n.Value
}

func mapDelete(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func mapSet(m *yaml.Node, key, value string) {
	if v := mapGet(m, key); v != nil {
		v.Kind, v.Tag, v.Value = yaml.ScalarNode, "!!bool", value
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: value})
}

func netplanNameForMAC(b []byte, mac string) string {
	_, eth := netplanEthernets(b)
	if eth == nil {
		return ""
	}
	for i := 0; i+1 < len(eth.Content); i += 2 {
		cfg := eth.Content[i+1]
		if normMAC(value(mapGet(mapGet(cfg, "match"), "macaddress"))) == mac {
			if sn := mapGet(cfg, "set-name"); sn != nil {
				return sn.Value
			}
			return eth.Content[i].Value
		}
	}
	return ""
}

func netplanToDHCP(b []byte, name, mac string) ([]byte, bool) {
	doc, eth := netplanEthernets(b)
	if eth == nil {
		return nil, false
	}
	found := false
	for i := 0; i+1 < len(eth.Content); i += 2 {
		key, cfg := eth.Content[i].Value, eth.Content[i+1]
		setName := value(mapGet(cfg, "set-name"))
		macMatch := normMAC(value(mapGet(mapGet(cfg, "match"), "macaddress"))) == mac
		if (name == "" || (key != name && setName != name)) && !macMatch {
			continue
		}
		for _, k := range []string{"addresses", "gateway4", "gateway6", "routes", "nameservers"} {
			mapDelete(cfg, k)
		}
		mapSet(cfg, "dhcp4", "true")
		found = true
	}
	if !found {
		return nil, false
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}
