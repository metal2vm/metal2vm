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

// Package transfer runs inside a migration's transfer pod. It accepts the
// live agent's tunnel and copies each exported disk into its PVC with nbdcopy.
package transfer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Plan is the transfer pod's input, passed as JSON in $TRANSFER_PLAN.
type Plan struct {
	Disks []DiskPlan `json:"disks"`
}

// DiskPlan copies one exported disk to a target.
type DiskPlan struct {
	DiskID string `json:"diskID"`
	// Port is the loopback port the agent's export appears on (via the tunnel).
	Port int `json:"port"`
	// Target is a block device (volumeMode Block) or a disk.img file path.
	Target string `json:"target"`
	// SizeBytes is the source disk size.
	SizeBytes int64 `json:"sizeBytes"`
	// File is true when Target is a file that must be created with SizeBytes.
	File bool `json:"file,omitempty"`
}

// Disk phases reported in Status.
const (
	DiskWaiting = "Waiting"
	DiskCopying = "Copying"
	DiskCopied  = "Copied"
	DiskFailed  = "Failed"
)

// Status is served on /status and written to the termination log at exit.
type Status struct {
	Disks []DiskStatus `json:"disks"`
	Done  bool         `json:"done"`
	Error string       `json:"error,omitempty"`
}

// DiskStatus is the progress of one disk.
type DiskStatus struct {
	DiskID      string `json:"diskID"`
	Phase       string `json:"phase"`
	BytesTotal  int64  `json:"bytesTotal"`
	BytesCopied int64  `json:"bytesCopied"`
	Attempts    int    `json:"attempts,omitempty"`
	Message     string `json:"message,omitempty"`
}

// MaxAttempts bounds nbdcopy retries per disk (it cannot resume).
const MaxAttempts = 3

// Copier copies disks and tracks status. NBDCopy is the nbdcopy binary.
type Copier struct {
	Plan    Plan
	NBDCopy string
	Log     *slog.Logger
	// WaitTimeout bounds how long to wait for the agent's exports.
	WaitTimeout time.Duration

	mu     sync.Mutex
	status Status
}

// NewCopier prepares a Copier with every disk waiting.
func NewCopier(plan Plan, log *slog.Logger) *Copier {
	c := &Copier{Plan: plan, NBDCopy: "nbdcopy", Log: log, WaitTimeout: 24 * time.Hour}
	for _, d := range plan.Disks {
		c.status.Disks = append(c.status.Disks, DiskStatus{DiskID: d.DiskID, Phase: DiskWaiting, BytesTotal: d.SizeBytes})
	}
	return c
}

// Status returns a snapshot of the progress.
func (c *Copier) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Disks = append([]DiskStatus(nil), c.status.Disks...)
	return s
}

func (c *Copier) update(i int, fn func(*DiskStatus)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(&c.status.Disks[i])
}

// Run copies all disks in order. It returns when all are copied or one fails.
func (c *Copier) Run(ctx context.Context) error {
	err := c.run(ctx)
	c.mu.Lock()
	c.status.Done = true
	if err != nil {
		c.status.Error = err.Error()
	}
	c.mu.Unlock()
	return err
}

func (c *Copier) run(ctx context.Context) error {
	for i, d := range c.Plan.Disks {
		if err := c.waitForExport(ctx, d); err != nil {
			c.update(i, func(s *DiskStatus) { s.Phase, s.Message = DiskFailed, err.Error() })
			return fmt.Errorf("disk %s: %w", d.DiskID, err)
		}
		if d.File {
			if err := createSparse(d.Target, d.SizeBytes); err != nil {
				c.update(i, func(s *DiskStatus) { s.Phase, s.Message = DiskFailed, err.Error() })
				return fmt.Errorf("disk %s: %w", d.DiskID, err)
			}
		}
		var lastErr error
		for attempt := 1; attempt <= MaxAttempts; attempt++ {
			c.update(i, func(s *DiskStatus) { s.Phase, s.Attempts, s.BytesCopied, s.Message = DiskCopying, attempt, 0, "" })
			c.Log.Info("copying disk", "disk", d.DiskID, "attempt", attempt, "bytes", d.SizeBytes)
			lastErr = c.copyDisk(ctx, i, d)
			if lastErr == nil {
				break
			}
			c.Log.Warn("copy failed", "disk", d.DiskID, "attempt", attempt, "error", lastErr)
			if ctx.Err() != nil {
				break
			}
			// The tunnel may have dropped; wait for the export to come back.
			if err := c.waitForExport(ctx, d); err != nil {
				lastErr = err
				break
			}
		}
		if lastErr != nil {
			c.update(i, func(s *DiskStatus) { s.Phase, s.Message = DiskFailed, lastErr.Error() })
			return fmt.Errorf("disk %s: %w", d.DiskID, lastErr)
		}
		c.update(i, func(s *DiskStatus) { s.Phase, s.BytesCopied = DiskCopied, d.SizeBytes })
		c.Log.Info("disk copied", "disk", d.DiskID)
	}
	return nil
}

// waitForExport waits until the agent's reverse tunnel accepts connections on
// the disk's port and the NBD server greets us.
func (c *Copier) waitForExport(ctx context.Context, d DiskPlan) error {
	deadline := time.Now().Add(c.WaitTimeout)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(d.Port))
	for {
		if nbdGreets(addr) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("export not reachable on %s after %s", addr, c.WaitTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// nbdGreets reports whether an NBD server answers on addr ("NBDMAGIC" greeting).
// Through chisel, the local port accepts connections even when the far end is
// down, so a TCP connect alone proves nothing.
func nbdGreets(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 8)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return false
	}
	return string(buf) == "NBDMAGIC"
}

func createSparse(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o660)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// NBDCopyArgs are the nbdcopy arguments for one disk; progress goes to fd 3.
func NBDCopyArgs(d DiskPlan) []string {
	return []string{
		"--progress=3",
		"--flush",
		fmt.Sprintf("nbd://127.0.0.1:%d", d.Port),
		d.Target,
	}
}

func (c *Copier) copyDisk(ctx context.Context, i int, d DiskPlan) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.NBDCopy, NBDCopyArgs(d)...)
	cmd.ExtraFiles = []*os.File{pw}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return err
	}
	_ = pw.Close()
	go func() {
		ParseProgress(pr, func(pct int) {
			c.update(i, func(s *DiskStatus) { s.BytesCopied = d.SizeBytes * int64(pct) / 100 })
		})
		_ = pr.Close()
	}()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("nbdcopy: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ParseProgress reads nbdcopy --progress=FD output ("N/100" lines).
func ParseProgress(r io.Reader, fn func(pct int)) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		num, _, ok := strings.Cut(strings.TrimSpace(s.Text()), "/")
		if !ok {
			continue
		}
		if pct, err := strconv.Atoi(num); err == nil && pct >= 0 && pct <= 100 {
			fn(pct)
		}
	}
}

// WriteTermination writes the final status where Kubernetes reads it as the
// container's termination message (max 4 KiB; messages are trimmed).
func WriteTermination(path string, s Status) {
	for i := range s.Disks {
		if len(s.Disks[i].Message) > 300 {
			s.Disks[i].Message = s.Disks[i].Message[:300]
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}
