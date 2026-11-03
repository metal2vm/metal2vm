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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type entry struct {
	name, link, body string
	typ              byte
}

func tarball(t *testing.T, entries ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: 0o755, Size: int64(len(e.body))}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return &buf
}

func TestUntar(t *testing.T) {
	dir := t.TempDir()
	err := Untar(tarball(t,
		entry{name: "./usr/sbin/", typ: tar.TypeDir},
		entry{name: "./usr/sbin/nbdkit", body: "ELF", typ: tar.TypeReg},
		entry{name: "./sbin", link: "usr/sbin", typ: tar.TypeSymlink},
		entry{name: "./usr/sbin/nbdkit-hard", link: "./usr/sbin/nbdkit", typ: tar.TypeLink},
	), dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "sbin", "nbdkit"))
	if err != nil || string(b) != "ELF" {
		t.Fatalf("read through symlink: %q %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "usr/sbin/nbdkit-hard")); string(b) != "ELF" {
		t.Fatalf("hard link content %q", b)
	}
}

func TestUntarRejectsEscapes(t *testing.T) {
	outside := t.TempDir()
	for name, entries := range map[string][]entry{
		"dotdot": {{name: "../escape", body: "x", typ: tar.TypeReg}},
		"via symlink": {
			{name: "evil", link: outside, typ: tar.TypeSymlink},
			{name: "evil/escape", body: "x", typ: tar.TypeReg},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			err := Untar(tarball(t, entries...), dir)
			if _, statErr := os.Stat(filepath.Join(outside, "escape")); statErr == nil {
				t.Fatal("file written outside the target directory")
			}
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); statErr == nil {
				t.Fatal("file written next to the target directory")
			}
			if err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNBDKitArgs(t *testing.T) {
	args := NBDKitArgs("n", "/run/metal2vm/tools", 10810, "/dev/sdb")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--readonly", "--ipaddr 127.0.0.1", "--port 10810", "file=/dev/sdb", "--rootfs /run/metal2vm/tools"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	if slices.Contains(args, "0.0.0.0") {
		t.Error("nbdkit must only listen on loopback")
	}
}
