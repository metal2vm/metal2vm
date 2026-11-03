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

// Package agent holds the live agent's host-side helpers.
package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const issueFile = "/etc/issue.d/50-metal2vm.issue"

// ShowStatus prints the agent status on the server's login console, so an
// admin looking at the screen (KVM, BMC console) knows what is going on.
func ShowStatus(lines ...string) {
	var b strings.Builder
	b.WriteString("\n=== metal2vm migration environment ===\n")
	for _, l := range lines {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	b.WriteString("  Source disks are read-only. Do not power off unless told to by the admin.\n\n")
	if err := os.MkdirAll("/etc/issue.d", 0o755); err != nil {
		return
	}
	if err := os.WriteFile(issueFile, []byte(b.String()), 0o644); err != nil {
		return
	}
	_ = exec.Command("agetty", "--reload").Run()
}

// PowerOff shuts the server down. Source disks are untouched; booting the
// server from its own disks again rolls the migration back.
func PowerOff() error {
	return exec.Command("systemctl", "poweroff").Run()
}
