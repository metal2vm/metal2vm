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

package precheck

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const rhelOutput = `===OS
NAME="Red Hat Enterprise Linux"
ID="rhel"
VERSION_ID="8.10"
===HOSTNAME
db01.example.com
===LINK
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN mode DEFAULT group default qlen 1000\    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
2: eno1: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc mq state UP mode DEFAULT group default qlen 1000\    link/ether 3c:ec:ef:12:34:56 brd ff:ff:ff:ff:ff:ff
3: eno2: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc mq state UP mode DEFAULT group default qlen 1000\    link/ether 3C:EC:EF:12:34:57 brd ff:ff:ff:ff:ff:ff
4: eno2.100@eno2: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP mode DEFAULT group default qlen 1000\    link/ether 3c:ec:ef:12:34:57 brd ff:ff:ff:ff:ff:ff
5: ens3f0: <BROADCAST,MULTICAST,SLAVE,UP,LOWER_UP> mtu 1500 qdisc mq master bond0 state UP mode DEFAULT group default qlen 1000\    link/ether 3c:ec:ef:00:00:01 brd ff:ff:ff:ff:ff:ff
6: docker0: <NO-CARRIER,BROADCAST,MULTICAST,UP> mtu 1500 qdisc noqueue state DOWN mode DEFAULT group default\    link/ether 02:42:ac:11:00:01 brd ff:ff:ff:ff:ff:ff
===ADDR
1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
2: eno1    inet 10.1.2.3/24 brd 10.1.2.255 scope global noprefixroute eno1\       valid_lft forever preferred_lft forever
2: eno1    inet6 fe80::3eec:efff:fe12:3456/64 scope link noprefixroute \       valid_lft forever preferred_lft forever
3: eno2    inet 192.168.50.7/24 brd 192.168.50.255 scope global dynamic noprefixroute eno2\       valid_lft 85000sec preferred_lft 85000sec
4: eno2.100    inet 172.16.100.4/24 brd 172.16.100.255 scope global noprefixroute eno2.100\       valid_lft forever preferred_lft forever
6: docker0    inet 172.17.0.1/16 brd 172.17.255.255 scope global docker0\       valid_lft forever preferred_lft forever
===ROUTE
default via 10.1.2.1 dev eno1 proto static metric 100
===DNS
search example.com
nameserver 10.1.2.53
nameserver 10.1.2.54
===BLOCK
sda
sda1 xfs
sdb1 crypto_LUKS
===END
`

func TestParseLinux(t *testing.T) {
	osc, err := ParseLinux(rhelOutput)
	if err != nil {
		t.Fatal(err)
	}
	if osc.OS.Distribution != "rhel" || osc.OS.Version != "8.10" || osc.OS.Hostname != "db01.example.com" {
		t.Errorf("os %+v", osc.OS)
	}
	if len(osc.NICs) != 2 {
		t.Fatalf("want eno1 and eno2 only, got %+v", osc.NICs)
	}
	eno1, eno2 := osc.NICs[0], osc.NICs[1]
	if eno1.Name != "eno1" || eno1.MAC != "3c:ec:ef:12:34:56" || eno1.DHCP || eno1.Gateway != "10.1.2.1" ||
		len(eno1.IPAddresses) != 1 || eno1.IPAddresses[0] != "10.1.2.3/24" || len(eno1.DNSServers) != 2 {
		t.Errorf("eno1 %+v", eno1)
	}
	if eno2.MAC != "3c:ec:ef:12:34:57" || !eno2.DHCP || eno2.Gateway != "" || len(eno2.DNSServers) != 0 {
		t.Errorf("eno2 %+v", eno2)
	}
	w := strings.Join(osc.Warnings, "\n")
	for _, want := range []string{"eno2.100", "ens3f0", "docker0", "sdb1"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing warning about %s in:\n%s", want, w)
		}
	}
}

func TestParseLinuxRejectsGarbage(t *testing.T) {
	if _, err := ParseLinux("Microsoft Windows [Version 10.0]"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseWindows(t *testing.T) {
	out := `{"caption":"Microsoft Windows Server 2022 Standard","version":"10.0.20348","hostname":"APP01",
"nics":[{"name":"Ethernet0","mac":"3C-EC-EF-12-34-56","ips":["10.5.0.20/24"],"dhcp":false,"gateway":"10.5.0.1","dns":["10.5.0.10","10.5.0.11"]}],
"bitlocker":["C:"]}`
	osc, err := ParseWindows([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	n := osc.NICs[0]
	if osc.OS.Hostname != "APP01" || n.MAC != "3c:ec:ef:12:34:56" || n.DHCP || n.Gateway != "10.5.0.1" || len(n.DNSServers) != 2 {
		t.Fatalf("%+v %+v", osc.OS, n)
	}
	if len(osc.Warnings) != 1 || !strings.Contains(osc.Warnings[0], "BitLocker") {
		t.Fatalf("warnings %v", osc.Warnings)
	}
}

// sshServer serves LinuxScript output to password-authenticated clients.
func sshServer(t *testing.T, password string) (addr string, fingerprint string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	cfg := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if string(p) == password {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, creqs, _ := nc.Accept()
					go func() {
						for r := range creqs {
							if r.Type == "exec" {
								_ = r.Reply(true, nil)
								_, _ = io.WriteString(ch, rhelOutput)
								_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{0}))
								_ = ch.Close()
							}
						}
					}()
				}
			}()
		}
	}()
	return l.Addr().String(), ssh.FingerprintSHA256(signer.PublicKey())
}

func TestSSHTrustOnFirstUse(t *testing.T) {
	addr, fp := sshServer(t, "s3cret")
	host, portStr, _ := net.SplitHostPort(addr)
	var port int32
	_, _ = fmt.Sscan(portStr, &port)
	ctx := context.Background()

	osc, err := SSH(ctx, host, port, Credentials{Username: "admin", Password: "s3cret"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if osc.HostKeyFingerprint != fp || len(osc.NICs) != 2 {
		t.Fatalf("fingerprint %q (want %q), nics %d", osc.HostKeyFingerprint, fp, len(osc.NICs))
	}
	if _, err := SSH(ctx, host, port, Credentials{Username: "admin", Password: "s3cret"}, fp); err != nil {
		t.Fatalf("pinned key rejected: %v", err)
	}
	if _, err := SSH(ctx, host, port, Credentials{Username: "admin", Password: "s3cret"}, "SHA256:something-else"); err == nil ||
		!strings.Contains(err.Error(), "host key changed") {
		t.Fatalf("changed host key must be refused, got %v", err)
	}
	if _, err := SSH(ctx, host, port, Credentials{Username: "admin", Password: "wrong"}, ""); err == nil {
		t.Fatal("wrong password must fail")
	}
}
