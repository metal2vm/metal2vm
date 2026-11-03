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

package pki

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"
)

func TestIssueAndVerify(t *testing.T) {
	now := time.Now()
	ca, err := NewCA("metal2vm-gateway-ca", now)
	if err != nil {
		t.Fatal(err)
	}
	host := "metal2vm-gateway-metal2vm-system.apps.example.com"
	srv, err := NewServing(ca, []string{host}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(srv.CertPEM, srv.KeyPEM); err != nil {
		t.Fatalf("serving pair unusable: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM)
	cert, _ := parseCert(srv.CertPEM)
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: host, Roots: pool}); err != nil {
		t.Fatalf("verify: %v", err)
	}

	if NeedsRenewal(srv.CertPEM, host, now) {
		t.Error("fresh cert should not need renewal")
	}
	if !NeedsRenewal(srv.CertPEM, "other.example.com", now) {
		t.Error("cert for another host must be renewed")
	}
	if !NeedsRenewal(srv.CertPEM, host, now.Add(ServingValidity-RenewBefore/2)) {
		t.Error("cert close to expiry must be renewed")
	}
	if !NeedsRenewal(nil, host, now) {
		t.Error("missing cert must be renewed")
	}
}
