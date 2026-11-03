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

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// SetDisksReadOnly marks every disk and partition read-only in the kernel
// (BLKROSET), so nothing in the live environment can write to source disks.
func SetDisksReadOnly() []error {
	var errs []error
	devs, _ := filepath.Glob("/sys/class/block/*")
	for _, d := range devs {
		name := filepath.Base(d)
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "zram") || strings.HasPrefix(name, "sr") {
			continue
		}
		if err := setReadOnly("/dev/" + name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errs
}

func setReadOnly(dev string) error {
	f, err := os.OpenFile(dev, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return unix.IoctlSetPointerInt(int(f.Fd()), unix.BLKROSET, 1)
}
