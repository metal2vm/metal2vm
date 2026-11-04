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

// csv-related-images adds every image used by a ClusterServiceVersion's
// deployments (containers and init containers) to spec.relatedImages, which
// oc-mirror uses to mirror an operator for disconnected clusters.
//
// Usage: go run ./hack/csv-related-images bundle/manifests/metal2vm.clusterserviceversion.yaml
package main

import (
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: csv-related-images <csv.yaml>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(file string) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var csv map[string]any
	if err := yaml.Unmarshal(raw, &csv); err != nil {
		return err
	}
	spec, _ := csv["spec"].(map[string]any)
	related, _ := spec["relatedImages"].([]any)
	have := map[string]bool{}
	for _, r := range related {
		if m, ok := r.(map[string]any); ok {
			have[fmt.Sprint(m["image"])] = true
		}
	}
	var images []string
	for _, d := range dig(spec, "install", "spec", "deployments") {
		pod, _ := digMap(d, "spec", "template", "spec")
		for _, key := range []string{"initContainers", "containers"} {
			list, _ := pod[key].([]any)
			for _, c := range list {
				if m, ok := c.(map[string]any); ok {
					images = append(images, fmt.Sprint(m["image"]))
				}
			}
		}
	}
	slices.Sort(images)
	for _, img := range slices.Compact(images) {
		if !have[img] {
			related = append(related, map[string]any{"name": imageName(img), "image": img})
		}
	}
	spec["relatedImages"] = related
	out, err := yaml.Marshal(csv)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o644)
}

// imageName turns ghcr.io/metal2vm/metal2vm-console-plugin:v1 into console-plugin.
func imageName(img string) string {
	n := path.Base(img)
	n, _, _ = strings.Cut(n, "@")
	n, _, _ = strings.Cut(n, ":")
	return strings.TrimPrefix(n, "metal2vm-")
}

func dig(m map[string]any, keys ...string) []map[string]any {
	last := keys[len(keys)-1]
	parent, ok := digMap(m, keys[:len(keys)-1]...)
	if !ok {
		return nil
	}
	list, _ := parent[last].([]any)
	var out []map[string]any
	for _, x := range list {
		if v, ok := x.(map[string]any); ok {
			out = append(out, v)
		}
	}
	return out
}

func digMap(m map[string]any, keys ...string) (map[string]any, bool) {
	for _, k := range keys {
		next, ok := m[k].(map[string]any)
		if !ok {
			return nil, false
		}
		m = next
	}
	return m, true
}
