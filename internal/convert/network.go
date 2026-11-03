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

package convert

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/metal2vm/metal2vm/internal/netfix"
)

// FixNetwork applies netfix to a Linux guest's disks with guestfish. It runs
// before virt-v2v so that virt-v2v's SELinux relabel covers the edited files.
// Guests without the files (e.g. Windows) are left untouched.
func FixNetwork(ctx context.Context, p Plan, guestfish, workDir string, log io.Writer) ([]string, error) {
	if len(p.Network.NICs) == 0 {
		return nil, nil
	}
	in := filepath.Join(workDir, "netfix-in")
	if err := os.MkdirAll(in, 0o700); err != nil {
		return nil, err
	}
	// guestfish exits 1 when inspection finds no OS; treat as "nothing to fix".
	if err := runGuestfish(ctx, guestfish, p.Disks, true, ReadScript(in), log); err != nil {
		return []string{"network config not inspected: " + err.Error()}, nil
	}
	files, err := ReadFetched(in)
	if err != nil {
		return nil, err
	}
	res := netfix.Apply(files, p.Network)
	if len(res.Files) == 0 {
		return res.Notes, nil
	}
	out := filepath.Join(workDir, "netfix-out")
	script, err := WriteScript(out, res.Files, files)
	if err != nil {
		return nil, err
	}
	if err := runGuestfish(ctx, guestfish, p.Disks, false, script, log); err != nil {
		return res.Notes, fmt.Errorf("writing network config: %w", err)
	}
	return res.Notes, nil
}

func runGuestfish(ctx context.Context, guestfish string, disks []Disk, readOnly bool, script string, log io.Writer) error {
	args := []string{"--format=raw", "-i"}
	if readOnly {
		args = append(args, "--ro")
	} else {
		args = append(args, "--rw")
	}
	for _, d := range disks {
		args = append(args, "-a", d.Path)
	}
	cmd := exec.CommandContext(ctx, guestfish, args...)
	cmd.Stdin = strings.NewReader(script)
	var tail tailBuffer
	w := io.MultiWriter(log, &tail)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("guestfish: %w: %s", err, tail.lastError())
	}
	return nil
}

// ReadScript fetches netfix.Paths into dir: directories as <i>.tar, files as
// <i>.file. Missing paths are ignored ("-" prefix).
func ReadScript(dir string) string {
	var b strings.Builder
	for i, gp := range netfix.Paths {
		if gp.Dir {
			fmt.Fprintf(&b, "-tar-out %s %s\n", gp.Path, filepath.Join(dir, fmt.Sprintf("%d.tar", i)))
		} else {
			fmt.Fprintf(&b, "-download %s %s\n", gp.Path, filepath.Join(dir, fmt.Sprintf("%d.file", i)))
		}
	}
	return b.String()
}

// ReadFetched loads what ReadScript fetched as guest path → content.
func ReadFetched(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for i, gp := range netfix.Paths {
		if !gp.Dir {
			b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%d.file", i)))
			if err == nil {
				files[gp.Path] = b
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		f, err := os.Open(filepath.Join(dir, fmt.Sprintf("%d.tar", i)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		tr := tar.NewReader(f)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = f.Close()
				return nil, err
			}
			// Only top-level regular files; config dirs are flat.
			name := strings.TrimPrefix(h.Name, "./")
			if h.Typeflag != tar.TypeReg || strings.Contains(name, "/") || h.Size > 1<<20 {
				continue
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				_ = f.Close()
				return nil, err
			}
			files[path.Join(gp.Path, name)] = b
		}
		_ = f.Close()
	}
	return files, nil
}

// WriteScript stages changed files under dir and returns the guestfish
// commands uploading them. New files get mode 0644; existing files keep theirs.
func WriteScript(dir string, changed, existing map[string][]byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	paths := make([]string, 0, len(changed))
	for p := range changed {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	var b strings.Builder
	for i, p := range paths {
		local := filepath.Join(dir, fmt.Sprintf("%d", i))
		if err := os.WriteFile(local, changed[p], 0o600); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "mkdir-p %s\nupload %s %s\n", path.Dir(p), local, p)
		if _, ok := existing[p]; !ok {
			fmt.Fprintf(&b, "chmod 0644 %s\n", p)
		}
	}
	return b.String(), nil
}
