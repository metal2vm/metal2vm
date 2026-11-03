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

// Package client talks to the metal2vm gateway.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/metal2vm/metal2vm/internal/boot"
	"github.com/metal2vm/metal2vm/internal/protocol"
)

// Client is an authenticated gateway client.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New builds a client from the agent config, trusting only the configured CA.
func New(cfg boot.AgentConfig) (*Client, error) {
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("no certificates in CA file")
	}
	return NewWithTransport(cfg.GatewayURL, cfg.Token, &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}), nil
}

// NewWithTransport builds a client with a custom transport (tests).
func NewWithTransport(base, token string, rt http.RoundTripper) *Client {
	return &Client{base: base, token: token, http: &http.Client{Transport: rt, Timeout: 30 * time.Second}}
}

// HTTP returns the client's HTTP client (trusting only the gateway CA).
func (c *Client) HTTP() *http.Client { return c.http }

// SendInventory reports the inventory.
func (c *Client) SendInventory(ctx context.Context, req protocol.InventoryRequest) error {
	return c.post(ctx, protocol.InventoryPath, req, nil)
}

// Heartbeat reports liveness and returns the gateway's instructions.
func (c *Client) Heartbeat(ctx context.Context, req protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error) {
	var resp protocol.HeartbeatResponse
	err := c.post(ctx, protocol.HeartbeatPath, req, &resp)
	return resp, err
}

// StatusError is a non-2xx gateway response.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("gateway returned %d: %s", e.Code, e.Body) }

// Permanent reports whether retrying cannot help (bad or expired token).
func (e *StatusError) Permanent() bool {
	return e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return &StatusError{Code: resp.StatusCode, Body: string(bytes.TrimSpace(data))}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
