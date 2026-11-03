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
	"os"
	"strings"
	"testing"
)

func testLiveConfig() LiveConfig {
	return LiveConfig{
		Agent: AgentConfig{
			GatewayURL: "https://gw.apps.example.com", Token: "secret-token",
			Namespace: "apps", Source: "web01",
		},
		CACertPEM: []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"),
	}
}

func TestLiveIgnition(t *testing.T) {
	out, err := LiveIgnition(testLiveConfig())
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("METAL2VM_TEST_IGN_OUT"); path != "" {
		_ = os.WriteFile(path, out, 0o644)
	}
	var cfg ignConfig
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Ignition.Version != "3.4.0" || len(cfg.Ignition.Security.TLS.CertificateAuthorities) != 1 {
		t.Fatalf("unexpected ignition section %+v", cfg.Ignition)
	}
	files := map[string]ignFile{}
	for _, f := range cfg.Storage.Files {
		files[f.Path] = f
	}
	bin := files[AgentBinaryPath]
	// The agent must not be hash-pinned, or ISOs break when the operator is upgraded.
	if bin.Contents.Source != "https://gw.apps.example.com"+AgentDownloadPath || bin.Contents.Verification != nil {
		t.Fatalf("agent binary entry %+v", bin)
	}
	agentCfg := files[AgentConfigPath]
	if agentCfg.Mode != 0o600 {
		t.Fatalf("agent config mode %o, want 600", agentCfg.Mode)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(agentCfg.Contents.Source, "data:;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	var ac AgentConfig
	if err := json.Unmarshal(raw, &ac); err != nil {
		t.Fatal(err)
	}
	if ac.Token != "secret-token" || ac.CAFile != AgentCAPath {
		t.Fatalf("agent config %+v", ac)
	}
	masked := map[string]bool{}
	for _, u := range cfg.Systemd.Units {
		masked[u.Name] = u.Mask
	}
	for _, u := range []string{"lvm2-monitor.service", "mdmonitor.service", "multipathd.service"} {
		if !masked[u] {
			t.Errorf("%s not masked", u)
		}
	}
	if cfg.Passwd != nil {
		t.Error("no users expected without SSH keys")
	}
	// The whole config must fit the embed area with plenty of room.
	if n := len(NewCPIO([]CPIOFile{{Name: "config.ign", Data: out}})); n > 64*1024 {
		t.Errorf("payload %d bytes, expected well under the 256 KiB embed area", n)
	}
}

func TestLiveIgnitionRequiresInputs(t *testing.T) {
	c := testLiveConfig()
	c.Agent.Token = ""
	if _, err := LiveIgnition(c); err == nil {
		t.Fatal("expected error without token")
	}
}
