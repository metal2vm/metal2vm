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

// Package pki issues the gateway's private CA and serving certificate.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Validity periods. Serving certs are renewed well before expiry.
const (
	CAValidity      = 10 * 365 * 24 * time.Hour
	ServingValidity = 2 * 365 * 24 * time.Hour
	RenewBefore     = 30 * 24 * time.Hour
)

// KeyPair is a PEM-encoded certificate and private key.
type KeyPair struct {
	CertPEM []byte
	KeyPEM  []byte
}

// NewCA creates a self-signed CA.
func NewCA(commonName string, now time.Time) (KeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"metal2vm"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return KeyPair{}, err
	}
	return encode(der, key)
}

// NewServing issues a serving certificate for the given DNS names.
func NewServing(ca KeyPair, dnsNames []string, now time.Time) (KeyPair, error) {
	caCert, caKey, err := ca.parse()
	if err != nil {
		return KeyPair{}, fmt.Errorf("parsing CA: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(ServingValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return KeyPair{}, err
	}
	return encode(der, key)
}

// NeedsRenewal reports whether a serving cert is missing, unparsable, close
// to expiry, or does not cover host.
func NeedsRenewal(certPEM []byte, host string, now time.Time) bool {
	cert, err := parseCert(certPEM)
	if err != nil {
		return true
	}
	return now.Add(RenewBefore).After(cert.NotAfter) || cert.VerifyHostname(host) != nil
}

func (kp KeyPair) parse() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := parseCert(kp.CertPEM)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(kp.KeyPEM)
	if block == nil {
		return nil, nil, errors.New("no PEM key")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	return cert, key, err
}

func parseCert(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func encode(der []byte, key *ecdsa.PrivateKey) (KeyPair, error) {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}
