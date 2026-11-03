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

package transfer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseProgress(t *testing.T) {
	var got []int
	ParseProgress(strings.NewReader("0/100\n12/100\ngarbage\n100/100\n150/100\n"), func(p int) { got = append(got, p) })
	if len(got) != 3 || got[0] != 0 || got[1] != 12 || got[2] != 100 {
		t.Fatalf("got %v", got)
	}
}

// fakeNBD answers every connection with the NBD greeting, like nbdkit.
func fakeNBD(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("NBDMAGIC"))
			_ = c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// fakeNBDCopy writes a script that behaves like nbdcopy: it reports progress
// on fd 3 and copies a marker into the target.
func fakeNBDCopy(t *testing.T, failTimes int) string {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	script := "#!/bin/sh\n" +
		"n=$(cat " + counter + " 2>/dev/null || echo 0); n=$((n+1)); echo $n > " + counter + "\n" +
		"if [ $n -le " + strconv.Itoa(failTimes) + " ]; then echo 'connection reset' >&2; exit 1; fi\n" +
		"echo 0/100 >&3; echo 50/100 >&3; echo 100/100 >&3\n" +
		"for last; do :; done; echo copied > \"$last\"\n"
	path := filepath.Join(dir, "nbdcopy")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCopierRetriesAndCompletes(t *testing.T) {
	port := fakeNBD(t)
	target := filepath.Join(t.TempDir(), "disk.img")
	c := NewCopier(Plan{Disks: []DiskPlan{{DiskID: "d0", Port: port, Target: target, SizeBytes: 1 << 20, File: true}}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.NBDCopy = fakeNBDCopy(t, 1)
	if err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := c.Status()
	if !s.Done || s.Error != "" || s.Disks[0].Phase != DiskCopied || s.Disks[0].Attempts != 2 || s.Disks[0].BytesCopied != 1<<20 {
		t.Fatalf("status %+v", s)
	}
	if b, _ := os.ReadFile(target); strings.TrimSpace(string(b)) != "copied" {
		t.Fatalf("target content %q", b)
	}
}

func TestCopierGivesUp(t *testing.T) {
	port := fakeNBD(t)
	c := NewCopier(Plan{Disks: []DiskPlan{{DiskID: "d0", Port: port, Target: filepath.Join(t.TempDir(), "x"), SizeBytes: 1}}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.NBDCopy = fakeNBDCopy(t, MaxAttempts)
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	s := c.Status()
	if s.Disks[0].Phase != DiskFailed || !strings.Contains(s.Disks[0].Message, "connection reset") || s.Error == "" {
		t.Fatalf("status %+v", s)
	}
}

func TestWaitForExportNeedsGreeting(t *testing.T) {
	// A listener that accepts but never greets (like a chisel port whose far end is down).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
		}
	}()
	c := NewCopier(Plan{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.WaitTimeout = time.Second
	err = c.waitForExport(context.Background(), DiskPlan{Port: l.Addr().(*net.TCPAddr).Port})
	if err == nil {
		t.Fatal("silent listener must not count as a ready export")
	}
}

func TestWriteTermination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "term")
	WriteTermination(path, Status{Done: true, Disks: []DiskStatus{{DiskID: "d0", Message: strings.Repeat("x", 5000)}}})
	b, _ := os.ReadFile(path)
	if len(b) > 4096 {
		t.Fatalf("termination message %d bytes, max 4096", len(b))
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil || !s.Done {
		t.Fatalf("decode %v %+v", err, s)
	}
}
