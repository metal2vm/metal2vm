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

package boot

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/metal2vm/metal2vm/internal/boot/boottest"
)

func TestFindEmbedArea(t *testing.T) {
	for _, tc := range []struct {
		name, ignInfo string
		want          EmbedArea
	}{
		{"legacy ignition.img", "", EmbedArea{Offset: 24 * isoSectorSize, Length: 4 * isoSectorSize}},
		{"igninfo file only", `{"file":"images/ignition.img"}`, EmbedArea{Offset: 24 * isoSectorSize, Length: 4 * isoSectorSize}},
		{"igninfo offset+length", `{"file":"images/ignition.img","offset":1024,"length":2048}`,
			EmbedArea{Offset: 24*isoSectorSize + 1024, Length: 2048}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FindEmbedArea(bytes.NewReader(boottest.FakeISO(t, tc.ignInfo, 4)))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFindEmbedAreaRejectsNonISO(t *testing.T) {
	if _, err := FindEmbedArea(bytes.NewReader(make([]byte, 64*1024))); err == nil {
		t.Fatal("expected error for non-ISO input")
	}
}

func TestEmbeddedISO(t *testing.T) {
	base := boottest.FakeISO(t, "", 4)
	// Simulate an ISO that already carries an embedded config: it must be replaced.
	copy(base[24*isoSectorSize:], "stale config")
	ign := []byte(`{"ignition":{"version":"3.4.0"}}`)
	e, err := NewEmbeddedISO(bytes.NewReader(base), int64(len(base)), ign)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(io.NewSectionReader(e, 0, e.Size()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(base) {
		t.Fatalf("size %d, want %d", len(got), len(base))
	}
	area := e.area
	if !bytes.Equal(got[:area.Offset], base[:area.Offset]) ||
		!bytes.Equal(got[area.Offset+area.Length:], base[area.Offset+area.Length:]) {
		t.Fatal("bytes outside the embed area changed")
	}
	want := NewCPIO([]CPIOFile{{Name: "config.ign", Data: ign}})
	region := got[area.Offset : area.Offset+area.Length]
	if !bytes.Equal(region[:len(want)], want) {
		t.Fatal("embed area does not start with the cpio payload")
	}
	if bytes.ContainsFunc(region[len(want):], func(r rune) bool { return r != 0 }) {
		t.Fatal("embed area is not zero-padded")
	}

	// Unaligned reads straddling the area boundaries match the full read.
	for _, off := range []int64{area.Offset - 3, area.Offset + 5, area.Offset + area.Length - 7} {
		p := make([]byte, 100)
		if _, err := e.ReadAt(p, off); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(p, got[off:off+100]) {
			t.Fatalf("ReadAt(%d) mismatch", off)
		}
	}
}

func TestEmbeddedISOTooLarge(t *testing.T) {
	base := boottest.FakeISO(t, "", 1)
	if _, err := NewEmbeddedISO(bytes.NewReader(base), int64(len(base)), make([]byte, 3*isoSectorSize)); err == nil ||
		!strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected too-large error, got %v", err)
	}
}

func TestNewCPIO(t *testing.T) {
	a := NewCPIO([]CPIOFile{{Name: "config.ign", Data: []byte("hello")}})
	if len(a)%4 != 0 {
		t.Fatalf("archive length %d not 4-byte aligned", len(a))
	}
	s := string(a)
	if !strings.HasPrefix(s, "070701") || !strings.Contains(s, "config.ign\x00") ||
		!strings.Contains(s, "hello") || !strings.Contains(s, "TRAILER!!!\x00") {
		t.Fatalf("unexpected archive %q", s)
	}
	// filesize field (7th 8-hex-digit field) of the first entry.
	if got := s[6+6*8 : 6+7*8]; got != "00000005" {
		t.Fatalf("filesize field %q", got)
	}
}

// TestRealCoreOSISO runs against a real Fedora CoreOS live ISO when
// METAL2VM_TEST_ISO points at one. The produced ISO can be checked with
// `coreos-installer iso ignition show`.
func TestRealCoreOSISO(t *testing.T) {
	path := os.Getenv("METAL2VM_TEST_ISO")
	if path == "" {
		t.Skip("METAL2VM_TEST_ISO not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	area, err := FindEmbedArea(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("embed area at %d, %d bytes", area.Offset, area.Length)
	e, err := NewEmbeddedISO(f, st.Size(), []byte(`{"ignition":{"version":"3.4.0"},"metal2vm":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out := os.Getenv("METAL2VM_TEST_ISO_OUT"); out != "" {
		o, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(o, io.NewSectionReader(e, 0, e.Size())); err != nil {
			t.Fatal(err)
		}
		if err := o.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
