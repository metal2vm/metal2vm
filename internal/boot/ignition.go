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

package boot

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Paths inside the live environment.
const (
	AgentBinaryPath = "/usr/local/bin/metal2vm-agent"
	AgentConfigPath = "/etc/metal2vm/agent.json"
	AgentCAPath     = "/etc/metal2vm/ca.crt"
	// AgentDownloadPath is served by the gateway and fetched by Ignition at boot.
	AgentDownloadPath = "/boot/v1/agent/linux-amd64"
)

// AgentConfig is written to AgentConfigPath and read by the agent.
type AgentConfig struct {
	// GatewayURL is the base URL of the metal2vm gateway, e.g. https://host.
	GatewayURL string `json:"gatewayURL"`
	// Token authenticates the agent to the gateway.
	Token string `json:"token"`
	// CAFile is the CA bundle used to verify the gateway certificate.
	CAFile string `json:"caFile"`
	// Namespace and Source identify the P2VSource, for display only.
	Namespace string `json:"namespace"`
	Source    string `json:"source"`
}

// LiveConfig is the input for the live environment's Ignition config.
type LiveConfig struct {
	Agent AgentConfig
	// CACertPEM verifies the gateway, both for Ignition's agent download and the agent itself.
	CACertPEM []byte
	// SSHAuthorizedKeys are added to the "core" user, for troubleshooting only.
	SSHAuthorizedKeys []string
}

// Minimal Ignition v3.4.0 types; only the fields metal2vm uses.
type ignConfig struct {
	Ignition ignIgnition `json:"ignition"`
	Passwd   *ignPasswd  `json:"passwd,omitempty"`
	Storage  ignStorage  `json:"storage"`
	Systemd  ignSystemd  `json:"systemd"`
}
type ignIgnition struct {
	Version  string      `json:"version"`
	Security ignSecurity `json:"security"`
}
type ignSecurity struct {
	TLS ignTLS `json:"tls"`
}
type ignTLS struct {
	CertificateAuthorities []ignResource `json:"certificateAuthorities"`
}
type ignResource struct {
	Source       string           `json:"source"`
	Verification *ignVerification `json:"verification,omitempty"`
}
type ignVerification struct {
	Hash string `json:"hash"`
}
type ignPasswd struct {
	Users []ignUser `json:"users"`
}
type ignUser struct {
	Name              string   `json:"name"`
	SSHAuthorizedKeys []string `json:"sshAuthorizedKeys"`
}
type ignStorage struct {
	Files []ignFile `json:"files"`
}
type ignFile struct {
	Path      string      `json:"path"`
	Mode      int         `json:"mode"`
	Overwrite bool        `json:"overwrite"`
	Contents  ignResource `json:"contents"`
}
type ignSystemd struct {
	Units []ignUnit `json:"units"`
}
type ignUnit struct {
	Name     string `json:"name"`
	Enabled  *bool  `json:"enabled,omitempty"`
	Mask     bool   `json:"mask,omitempty"`
	Contents string `json:"contents,omitempty"`
}

const agentUnit = `[Unit]
Description=metal2vm migration agent
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=` + AgentBinaryPath + ` --config ` + AgentConfigPath + `
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

// LiveIgnition renders the Ignition config embedded into the live ISO.
//
// The live environment must never modify source disks, so units that could
// auto-assemble or write to local storage are masked. The agent additionally
// marks every disk read-only in the kernel before touching it.
func LiveIgnition(c LiveConfig) ([]byte, error) {
	if c.Agent.GatewayURL == "" || c.Agent.Token == "" || len(c.CACertPEM) == 0 {
		return nil, errors.New("gateway URL, token and CA are required")
	}
	agentCfg := c.Agent
	agentCfg.CAFile = AgentCAPath
	cfgJSON, err := json.Marshal(agentCfg)
	if err != nil {
		return nil, err
	}
	enabled := true
	cfg := ignConfig{
		Ignition: ignIgnition{
			Version: "3.4.0",
			Security: ignSecurity{TLS: ignTLS{
				CertificateAuthorities: []ignResource{{Source: dataURL(c.CACertPEM)}},
			}},
		},
		Storage: ignStorage{Files: []ignFile{
			{
				Path: AgentBinaryPath, Mode: 0o755, Overwrite: true,
				// No hash pin: an ISO must keep working after the operator (and so
				// the agent) is upgraded. Integrity comes from TLS to the gateway.
				Contents: ignResource{Source: c.Agent.GatewayURL + AgentDownloadPath},
			},
			{Path: AgentConfigPath, Mode: 0o600, Overwrite: true, Contents: ignResource{Source: dataURL(cfgJSON)}},
			{Path: AgentCAPath, Mode: 0o644, Overwrite: true, Contents: ignResource{Source: dataURL(c.CACertPEM)}},
		}},
		Systemd: ignSystemd{Units: []ignUnit{
			{Name: "metal2vm-agent.service", Enabled: &enabled, Contents: agentUnit},
			// Never activate or repair anything found on the source disks.
			{Name: "lvm2-monitor.service", Mask: true},
			{Name: "mdmonitor.service", Mask: true},
			{Name: "multipathd.service", Mask: true},
			{Name: "zincati.service", Mask: true},
		}},
	}
	if len(c.SSHAuthorizedKeys) > 0 {
		cfg.Passwd = &ignPasswd{Users: []ignUser{{Name: "core", SSHAuthorizedKeys: c.SSHAuthorizedKeys}}}
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encoding ignition: %w", err)
	}
	return out, nil
}

func dataURL(b []byte) string {
	return "data:;base64," + base64.StdEncoding.EncodeToString(b)
}
