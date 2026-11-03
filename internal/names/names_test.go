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

package names

import (
	"strings"
	"testing"
)

func TestShort(t *testing.T) {
	if got := Short("web01", "-transfer"); got != "web01-transfer" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("a", 80)
	a, b := Short(long, "-transfer"), Short(long+"b", "-transfer")
	if len(a) > MaxDNSLabel || !strings.HasSuffix(a, "-transfer") || a == b {
		t.Fatalf("got %q / %q", a, b)
	}
	if h := TunnelHost("ns", "m", "apps.example.com"); !strings.HasPrefix(h, "m2v-") || len(strings.Split(h, ".")[0]) > MaxDNSLabel {
		t.Fatalf("host %q", h)
	}
}
