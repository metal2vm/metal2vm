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

// Package names derives Kubernetes-safe names for per-migration resources,
// shared by the controllers and the gateway.
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// MaxDNSLabel is the maximum length of names used for Services and labels.
const MaxDNSLabel = 63

// Short returns base+suffix, shortened with a hash to fit in 63 characters.
func Short(base, suffix string) string {
	if len(base)+len(suffix) <= MaxDNSLabel {
		return base + suffix
	}
	h := Hash(base)[:8]
	keep := MaxDNSLabel - len(suffix) - len(h) - 1
	return base[:keep] + "-" + h + suffix
}

// Hash is a hex SHA-256 of s.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Transfer is the name of a migration's transfer pod, Service, Route and Secret.
func Transfer(migration string) string { return Short(migration, "-transfer") }

// DiskPVC is the name of the PVC receiving the i-th copied disk of a migration.
func DiskPVC(migration string, i int) string { return Short(migration, fmt.Sprintf("-disk-%d", i)) }

// TunnelHost is the Route host of a migration's tunnel under the ingress domain.
func TunnelHost(namespace, migration, domain string) string {
	return "m2v-" + Hash(namespace + "/" + migration)[:12] + "." + domain
}

// Keys of a migration's transfer Secret.
const (
	TransferKeyCert      = "tls.crt"
	TransferKeyKey       = "tls.key"
	TransferKeyUsers     = "users.json"
	TransferKeyAuth      = "auth"
	TransferKeyTunnelURL = "tunnelURL"
	TransferKeyExports   = "exports.json"
)

// LabelMigration marks resources belonging to a migration (value: names.Short-safe name).
const LabelMigration = "metal2vm.io/migration"
