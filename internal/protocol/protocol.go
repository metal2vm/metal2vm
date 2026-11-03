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

// Package protocol defines the HTTP API between the live agent and the gateway.
// All agent endpoints take "Authorization: Bearer <token>".
package protocol

import "github.com/metal2vm/metal2vm/api/v1alpha1"

const (
	// InventoryPath accepts POST InventoryRequest.
	InventoryPath = "/agent/v1/inventory"
	// HeartbeatPath accepts POST HeartbeatRequest and returns HeartbeatResponse.
	HeartbeatPath = "/agent/v1/heartbeat"
)

// InventoryRequest reports the discovered hardware.
type InventoryRequest struct {
	AgentVersion string             `json:"agentVersion"`
	Inventory    v1alpha1.Inventory `json:"inventory"`
}

// HeartbeatRequest tells the gateway the agent is alive.
type HeartbeatRequest struct {
	AgentVersion string `json:"agentVersion"`
}

// HeartbeatResponse carries instructions back to the agent.
type HeartbeatResponse struct {
	// ResendInventory asks the agent to report its inventory again.
	ResendInventory bool `json:"resendInventory,omitempty"`
	// Transfer, when set, asks the agent to export disks through a tunnel.
	// When it disappears, the agent stops exporting.
	Transfer *TransferInstruction `json:"transfer,omitempty"`
}

// TransferInstruction tells the agent how to serve disks to a transfer pod.
type TransferInstruction struct {
	// Migration is "namespace/name", for logs and the console banner.
	Migration string `json:"migration"`
	// TunnelURL is the transfer pod's chisel server, e.g. https://m2v-x.apps.example.com.
	TunnelURL string `json:"tunnelURL"`
	// Auth is the chisel "user:password".
	Auth string `json:"auth"`
	// Exports lists the disks to serve, each on its own loopback port.
	Exports []Export `json:"exports"`
}

// Export maps a source disk to the loopback port its NBD server listens on.
// The same port is reverse-forwarded to the transfer pod.
type Export struct {
	DiskID string `json:"diskID"`
	Port   int    `json:"port"`
}

// FirstExportPort is the first loopback port used for disk exports.
const FirstExportPort = 10809

// ToolsDownloadPath serves the live-tools root filesystem (nbdkit), as tar.gz.
const ToolsDownloadPath = "/boot/v1/tools/live-tools.tar.gz"
