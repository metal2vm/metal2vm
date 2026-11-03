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

// Package transfer serves source disks to a transfer pod: one read-only
// nbdkit per disk on a loopback port, reverse-forwarded over a chisel tunnel.
//
// nbdkit is not part of Fedora CoreOS, so it comes as a root filesystem
// downloaded from the gateway and run with `podman run --rootfs`. Nothing is
// pulled from a container registry, which keeps disconnected sites working.
package transfer

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	chclient "github.com/jpillora/chisel/client"

	"github.com/metal2vm/metal2vm/internal/protocol"
)

// Manager runs at most one transfer session at a time.
type Manager struct {
	// GatewayURL and HTTP download the live tools.
	GatewayURL string
	HTTP       *http.Client
	// CAFile verifies the tunnel endpoint (same CA as the gateway).
	CAFile string
	// ToolsDir is where the live-tools root filesystem is unpacked.
	ToolsDir string
	// DevicePath maps a disk ID to its device node, e.g. /dev/sda.
	DevicePath func(diskID string) (string, bool)
	Log        *slog.Logger

	mu      sync.Mutex
	current *protocol.TransferInstruction
	cancel  context.CancelFunc
	done    chan struct{}
}

// Ensure starts the session described by instr, replacing a different running
// one. A nil instr stops any running session.
func (m *Manager) Ensure(ctx context.Context, instr *protocol.TransferInstruction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if reflect.DeepEqual(instr, m.current) {
		return nil
	}
	m.stopLocked()
	if instr == nil {
		return nil
	}
	if err := m.ensureTools(ctx); err != nil {
		return fmt.Errorf("live tools: %w", err)
	}
	devs := make([]string, len(instr.Exports))
	for i, e := range instr.Exports {
		dev, ok := m.DevicePath(e.DiskID)
		if !ok {
			return fmt.Errorf("disk %s not found on this server", e.DiskID)
		}
		devs[i] = dev
	}

	sctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.current, m.cancel, m.done = instr, cancel, done
	go func() {
		defer close(done)
		m.run(sctx, *instr, devs)
	}()
	return nil
}

// Stop ends the running session, if any.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

func (m *Manager) stopLocked() {
	if m.cancel == nil {
		return
	}
	m.cancel()
	<-m.done
	m.current, m.cancel, m.done = nil, nil, nil
}

// Active reports the running session's migration, or "".
func (m *Manager) Active() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return ""
	}
	return m.current.Migration
}

func (m *Manager) run(ctx context.Context, instr protocol.TransferInstruction, devs []string) {
	var wg sync.WaitGroup
	remotes := make([]string, len(instr.Exports))
	for i, e := range instr.Exports {
		remotes[i] = fmt.Sprintf("R:127.0.0.1:%d:127.0.0.1:%d", e.Port, e.Port)
		wg.Go(func() {
			m.supervise(ctx, "nbdkit "+e.DiskID, func(ctx context.Context) error {
				return m.runNBDKit(ctx, e.Port, devs[i])
			})
		})
	}
	wg.Go(func() {
		m.supervise(ctx, "tunnel", func(ctx context.Context) error {
			c, err := chclient.NewClient(&chclient.Config{
				Server:           instr.TunnelURL,
				Auth:             instr.Auth,
				Remotes:          remotes,
				KeepAlive:        25 * time.Second,
				MaxRetryInterval: 30 * time.Second,
				TLS:              chclient.TLSConfig{CA: m.CAFile},
			})
			if err != nil {
				return err
			}
			if err := c.Start(ctx); err != nil {
				return err
			}
			return c.Wait()
		})
	})
	m.Log.Info("transfer session started", "migration", instr.Migration, "disks", len(devs), "tunnel", instr.TunnelURL)
	wg.Wait()
	m.Log.Info("transfer session stopped", "migration", instr.Migration)
}

// supervise restarts fn until ctx ends.
func (m *Manager) supervise(ctx context.Context, what string, fn func(context.Context) error) {
	for ctx.Err() == nil {
		err := fn(ctx)
		if ctx.Err() != nil {
			return
		}
		m.Log.Warn("restarting", "what", what, "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

// runNBDKit serves one device read-only on 127.0.0.1:port until ctx ends.
func (m *Manager) runNBDKit(ctx context.Context, port int, dev string) error {
	name := "metal2vm-nbdkit-" + strconv.Itoa(port)
	_ = exec.Command("podman", "rm", "-f", name).Run()
	cmd := exec.Command("podman", NBDKitArgs(name, m.ToolsDir, port, dev)...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Wait() }()
	select {
	case err := <-errCh:
		return fmt.Errorf("nbdkit exited: %w", err)
	case <-ctx.Done():
		_ = exec.Command("podman", "stop", "-t", "5", name).Run()
		if err := <-errCh; err != nil {
			m.Log.Debug("nbdkit stopped", "port", port, "error", err)
		}
		return ctx.Err()
	}
}

// NBDKitArgs are the podman arguments running nbdkit for one device.
func NBDKitArgs(name, rootfs string, port int, dev string) []string {
	return []string{
		"run", "--rm", "--name", name,
		"--privileged", "--network=host", "--security-opt", "label=disable",
		"-v", "/dev:/dev",
		"--rootfs", rootfs,
		"/usr/bin/nbdkit", "--foreground", "--readonly",
		"--ipaddr", "127.0.0.1", "--port", strconv.Itoa(port),
		"file", "file=" + dev, "cache=none",
	}
}

// ensureTools downloads and unpacks the live tools once.
func (m *Manager) ensureTools(ctx context.Context) error {
	marker := filepath.Join(m.ToolsDir, ".complete")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.GatewayURL+protocol.ToolsDownloadPath, nil)
	if err != nil {
		return err
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", resp.Status)
	}
	_ = os.RemoveAll(m.ToolsDir)
	if err := Untar(resp.Body, m.ToolsDir); err != nil {
		return err
	}
	return os.WriteFile(marker, nil, 0o644)
}

// Untar unpacks a gzipped tar into dir. All writes go through os.Root, so
// entries (including ones placed "through" symlinks) cannot escape dir.
func Untar(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(strings.TrimPrefix(h.Name, "/"))
		if name == "." {
			continue
		}
		mode := os.FileMode(h.Mode) & os.ModePerm
		if parent := filepath.Dir(name); parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, mode|0o700)
		case tar.TypeReg:
			err = writeFile(root, name, mode, tr)
		case tar.TypeSymlink:
			_ = root.Remove(name)
			err = root.Symlink(h.Linkname, name)
		case tar.TypeLink:
			_ = root.Remove(name)
			err = root.Link(filepath.Clean(strings.TrimPrefix(h.Linkname, "/")), name)
		default:
			// Device nodes and FIFOs are not needed in the tools rootfs.
		}
		if err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
	}
}

func writeFile(root *os.Root, name string, mode os.FileMode, r io.Reader) error {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
