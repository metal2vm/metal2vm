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

// Package boot builds per-source boot media for the live environment.
//
// CoreOS live ISOs reserve a zero-filled "Ignition embed area" that the kernel
// loads as an extra initramfs. Writing a cpio archive containing config.ign
// into that area is all `coreos-installer iso ignition embed` does; this
// package does the same while streaming, so no per-source ISO copy is stored.
package boot

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	isoSectorSize = 2048
	// Primary volume descriptor location and root directory record offset (ECMA-119).
	pvdOffset         = 16 * isoSectorSize
	pvdRootDirOffset  = 156
	ignInfoPath       = "COREOS/IGNINFO.JSO"
	defaultEmbedPath  = "IMAGES/IGNITION.IMG"
	initrdIgnitionCfg = "config.ign"
)

// EmbedArea is the byte range of the Ignition embed area within the ISO.
type EmbedArea struct {
	Offset int64
	Length int64
}

type isoFile struct {
	offset int64
	length int64
	dir    bool
}

// FindEmbedArea locates the Ignition embed area of a CoreOS live ISO.
func FindEmbedArea(iso io.ReaderAt) (EmbedArea, error) {
	pvd := make([]byte, isoSectorSize)
	if _, err := iso.ReadAt(pvd, pvdOffset); err != nil {
		return EmbedArea{}, fmt.Errorf("reading primary volume descriptor: %w", err)
	}
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		return EmbedArea{}, errors.New("not an ISO9660 image")
	}
	root := parseDirRecord(pvd[pvdRootDirOffset:])

	// Newer ISOs describe the embed area in igninfo.json; older ones use ignition.img.
	info := struct {
		File   string `json:"file"`
		Offset *int64 `json:"offset"`
		Length *int64 `json:"length"`
	}{File: defaultEmbedPath}
	if f, err := lookup(iso, root, ignInfoPath); err == nil {
		buf := make([]byte, f.length)
		if _, err := iso.ReadAt(buf, f.offset); err != nil {
			return EmbedArea{}, fmt.Errorf("reading %s: %w", ignInfoPath, err)
		}
		if err := json.Unmarshal(buf, &info); err != nil {
			return EmbedArea{}, fmt.Errorf("decoding %s: %w", ignInfoPath, err)
		}
	} else if !errors.Is(err, errNotFound) {
		return EmbedArea{}, err
	}

	f, err := lookup(iso, root, strings.ToUpper(info.File))
	if err != nil {
		return EmbedArea{}, fmt.Errorf("finding embed area %s: %w", info.File, err)
	}
	area := EmbedArea{Offset: f.offset, Length: f.length}
	if info.Offset != nil {
		area.Offset += *info.Offset
		area.Length -= *info.Offset
	}
	if info.Length != nil {
		area.Length = *info.Length
	}
	if area.Length <= 0 || area.Offset+area.Length > f.offset+f.length {
		return EmbedArea{}, fmt.Errorf("invalid embed area %+v", area)
	}
	return area, nil
}

var errNotFound = errors.New("not found")

func parseDirRecord(rec []byte) isoFile {
	return isoFile{
		offset: int64(binary.LittleEndian.Uint32(rec[2:6])) * isoSectorSize,
		length: int64(binary.LittleEndian.Uint32(rec[10:14])),
		dir:    rec[25]&0x02 != 0,
	}
}

// lookup resolves a slash-separated ISO9660 path, ignoring ";1" version suffixes.
func lookup(iso io.ReaderAt, dir isoFile, path string) (isoFile, error) {
	cur := dir
	for name := range strings.SplitSeq(path, "/") {
		if !cur.dir {
			return isoFile{}, fmt.Errorf("%s: not a directory", path)
		}
		next, err := findInDir(iso, cur, name)
		if err != nil {
			return isoFile{}, fmt.Errorf("%s: %w", path, err)
		}
		cur = next
	}
	return cur, nil
}

func findInDir(iso io.ReaderAt, dir isoFile, name string) (isoFile, error) {
	data := make([]byte, dir.length)
	if _, err := iso.ReadAt(data, dir.offset); err != nil {
		return isoFile{}, err
	}
	for pos := 0; pos < len(data); {
		recLen := int(data[pos])
		if recLen == 0 {
			// Records never span sectors; skip padding to the next sector.
			pos = (pos/isoSectorSize + 1) * isoSectorSize
			continue
		}
		if pos+recLen > len(data) || recLen < 34 {
			break
		}
		rec := data[pos : pos+recLen]
		nameLen := int(rec[32])
		if 33+nameLen <= recLen {
			id := string(rec[33 : 33+nameLen])
			if i := strings.IndexByte(id, ';'); i >= 0 {
				id = id[:i]
			}
			id = strings.TrimSuffix(id, ".")
			if strings.EqualFold(id, name) {
				return parseDirRecord(rec), nil
			}
		}
		pos += recLen
	}
	return isoFile{}, errNotFound
}

// NewCPIO returns an uncompressed newc cpio archive with the given regular
// files (mode 0644), in the given order. The kernel and coreos-installer both
// accept uncompressed archives in the embed area.
func NewCPIO(files []CPIOFile) []byte {
	var buf bytes.Buffer
	ino := 1
	write := func(name string, mode uint32, data []byte) {
		fmt.Fprintf(&buf, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
			ino, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
		buf.WriteString(name)
		buf.WriteByte(0)
		pad4(&buf)
		buf.Write(data)
		pad4(&buf)
		ino++
	}
	for _, f := range files {
		write(f.Name, 0o100644, f.Data)
	}
	write("TRAILER!!!", 0, nil)
	return buf.Bytes()
}

// CPIOFile is a regular file in a cpio archive.
type CPIOFile struct {
	Name string
	Data []byte
}

func pad4(buf *bytes.Buffer) {
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}

// EmbeddedISO serves a base ISO with the embed area replaced by payload
// (zero-padded). It implements io.ReaderAt and is safe for concurrent use.
type EmbeddedISO struct {
	base    io.ReaderAt
	size    int64
	area    EmbedArea
	payload []byte
}

// NewEmbeddedISO embeds an Ignition config into a CoreOS live ISO of the given size.
func NewEmbeddedISO(base io.ReaderAt, size int64, ignition []byte) (*EmbeddedISO, error) {
	area, err := FindEmbedArea(base)
	if err != nil {
		return nil, err
	}
	payload := NewCPIO([]CPIOFile{{Name: initrdIgnitionCfg, Data: ignition}})
	if int64(len(payload)) > area.Length {
		return nil, fmt.Errorf("ignition config too large for embed area: %d > %d bytes", len(payload), area.Length)
	}
	return &EmbeddedISO{base: base, size: size, area: area, payload: payload}, nil
}

// Size is the ISO size in bytes.
func (e *EmbeddedISO) Size() int64 { return e.size }

// ReadAt implements io.ReaderAt.
func (e *EmbeddedISO) ReadAt(p []byte, off int64) (int, error) {
	if off >= e.size {
		return 0, io.EOF
	}
	if rem := e.size - off; int64(len(p)) > rem {
		p = p[:rem]
		n, err := e.ReadAt(p, off)
		if err == nil {
			err = io.EOF
		}
		return n, err
	}
	n, err := e.base.ReadAt(p, off)
	if err != nil && (!errors.Is(err, io.EOF) || n != len(p)) {
		return n, err
	}
	// Overlay the part of [off, off+len(p)) that intersects the embed area.
	start := max(off, e.area.Offset)
	end := min(off+int64(len(p)), e.area.Offset+e.area.Length)
	for i := start; i < end; i++ {
		var b byte
		if j := i - e.area.Offset; j < int64(len(e.payload)) {
			b = e.payload[j]
		}
		p[i-off] = b
	}
	return len(p), nil
}
