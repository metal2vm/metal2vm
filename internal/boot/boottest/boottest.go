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
// Package boottest provides test fixtures for package boot.
package boottest

import (
	"encoding/binary"
	"testing"
)

const sector = 2048

// FakeISO builds a minimal ISO9660 image: / → COREOS/IGNINFO.JSO and IMAGES/IGNITION.IMG.
// Sector layout: 16 PVD, 20 root, 21 COREOS, 22 IMAGES, 23 igninfo, 24.. embed file.
func FakeISO(t testing.TB, ignInfo string, embedSectors int) []byte {
	t.Helper()
	img := make([]byte, (24+embedSectors+1)*sector)
	rec := func(lba, size int, dir bool, name string) []byte {
		r := make([]byte, 33+len(name)+(1-len(name)%2))
		r[0] = byte(len(r))
		binary.LittleEndian.PutUint32(r[2:], uint32(lba))
		binary.LittleEndian.PutUint32(r[10:], uint32(size))
		if dir {
			r[25] = 0x02
		}
		r[32] = byte(len(name))
		copy(r[33:], name)
		return r
	}
	put := func(lba int, recs ...[]byte) {
		pos := lba * sector
		for _, r := range recs {
			copy(img[pos:], r)
			pos += len(r)
		}
	}
	img[16*sector] = 1
	copy(img[16*sector+1:], "CD001")
	copy(img[16*sector+156:], rec(20, sector, true, "\x00"))
	put(20, rec(20, sector, true, "\x00"), rec(20, sector, true, "\x01"),
		rec(21, sector, true, "COREOS"), rec(22, sector, true, "IMAGES"))
	if ignInfo != "" {
		put(21, rec(23, len(ignInfo), false, "IGNINFO.JSO;1"))
		copy(img[23*sector:], ignInfo)
	}
	put(22, rec(24, embedSectors*sector, false, "IGNITION.IMG;1"))
	// Non-zero bytes around the embed area must be preserved.
	for i := range img[(24+embedSectors)*sector:] {
		img[(24+embedSectors)*sector+i] = 0xAA
	}
	return img
}
