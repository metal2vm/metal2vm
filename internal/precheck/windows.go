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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/masterzen/winrm"

	"github.com/metal2vm/metal2vm/api/v1alpha1"
)

// WindowsScript prints one JSON document ParseWindows reads. Read-only.
const WindowsScript = `$ErrorActionPreference = 'SilentlyContinue'
$os = Get-CimInstance Win32_OperatingSystem
$cs = Get-CimInstance Win32_ComputerSystem
$nics = @(Get-NetAdapter -Physical | ForEach-Object {
  $a = $_
  $ip = @(Get-NetIPAddress -InterfaceIndex $a.ifIndex -AddressFamily IPv4)
  $gw = (Get-NetRoute -InterfaceIndex $a.ifIndex -DestinationPrefix '0.0.0.0/0' | Select-Object -First 1).NextHop
  [pscustomobject]@{
    name = $a.Name; mac = $a.MacAddress
    ips = @($ip | ForEach-Object { "$($_.IPAddress)/$($_.PrefixLength)" })
    dhcp = (@($ip | Where-Object PrefixOrigin -eq 'Dhcp').Count -gt 0)
    gateway = $gw
    dns = @((Get-DnsClientServerAddress -InterfaceIndex $a.ifIndex -AddressFamily IPv4).ServerAddresses)
  }
})
$bl = @(Get-BitLockerVolume | Where-Object ProtectionStatus -eq 'On' | ForEach-Object { $_.MountPoint })
[pscustomobject]@{ caption = $os.Caption; version = $os.Version; hostname = $cs.Name; nics = $nics; bitlocker = $bl } |
  ConvertTo-Json -Depth 4 -Compress`

// WinRMOptions selects the WinRM endpoint.
type WinRMOptions struct {
	HTTPS    bool
	Insecure bool
	CACert   []byte
}

// WinRM runs the Windows pre-check (NTLM authentication).
func WinRM(ctx context.Context, address string, port int32, creds Credentials, opts WinRMOptions) (*v1alpha1.OSConfig, error) {
	if port == 0 {
		port = 5986
		if !opts.HTTPS {
			port = 5985
		}
	}
	endpoint := winrm.NewEndpoint(address, int(port), opts.HTTPS, opts.Insecure, opts.CACert, nil, nil, 0)
	params := winrm.DefaultParameters
	params.TransportDecorator = func() winrm.Transporter { return &winrm.ClientNTLM{} }
	client, err := winrm.NewClientWithParameters(endpoint, creds.Username, creds.Password, params)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	code, err := client.RunWithContext(ctx, winrm.Powershell(WindowsScript), &stdout, &stderr)
	if err != nil {
		return nil, fmt.Errorf("running the pre-check: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("pre-check exited with %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return ParseWindows(stdout.Bytes())
}

// ParseWindows turns WindowsScript output into an OSConfig.
func ParseWindows(out []byte) (*v1alpha1.OSConfig, error) {
	var doc struct {
		Caption  string `json:"caption"`
		Version  string `json:"version"`
		Hostname string `json:"hostname"`
		NICs     []struct {
			Name    string   `json:"name"`
			MAC     string   `json:"mac"`
			IPs     []string `json:"ips"`
			DHCP    bool     `json:"dhcp"`
			Gateway string   `json:"gateway"`
			DNS     []string `json:"dns"`
		} `json:"nics"`
		BitLocker []string `json:"bitlocker"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &doc); err != nil {
		return nil, fmt.Errorf("unexpected pre-check output: %w", err)
	}
	osc := &v1alpha1.OSConfig{OS: v1alpha1.OSInfo{
		Family: v1alpha1.OSWindows, Distribution: doc.Caption, Version: doc.Version, Hostname: doc.Hostname,
	}}
	for _, n := range doc.NICs {
		osc.NICs = append(osc.NICs, v1alpha1.NIC{
			Name: n.Name, MAC: strings.ToLower(strings.ReplaceAll(n.MAC, "-", ":")),
			IPAddresses: n.IPs, DHCP: n.DHCP, Gateway: n.Gateway, DNSServers: n.DNS,
		})
	}
	for _, v := range doc.BitLocker {
		osc.Warnings = append(osc.Warnings, "BitLocker is on for "+v+": suspend it before the migration")
	}
	return osc, nil
}
