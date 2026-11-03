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

// Package tokens creates and finds per-source boot credentials.
//
// Each P2VSource owns one Secret holding a boot key (in the ISO download URL)
// and an agent token (embedded in the ISO, sent by the agent as a bearer
// token). Secrets are found by a label carrying a hash of either value, so
// the gateway never lists Secrets and the label leaks nothing usable.
package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// Labels and keys used on boot Secrets.
const (
	LabelManagedBy   = "app.kubernetes.io/managed-by"
	ManagedByValue   = "metal2vm"
	LabelSource      = "metal2vm.io/source"
	LabelBootKeyHash = "metal2vm.io/boot-key-hash"
	LabelTokenHash   = "metal2vm.io/token-hash"
	KeyBootKey       = "bootKey"
	KeyToken         = "token"
	KeyExpiresAt     = "expiresAt"
	KeyISOURL        = "isoURL"
	KeyISOHTTPURL    = "isoHTTPURL"
	KeyIPXEURL       = "ipxeURL"
	KeyIPXEHTTPURL   = "ipxeHTTPURL"
)

// Generate returns a new random 256-bit URL-safe secret.
func Generate() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return base64.RawURLEncoding.EncodeToString(b)
}

// Hash returns a label-safe digest of a secret (SHA-256, 40 hex chars).
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:40]
}
