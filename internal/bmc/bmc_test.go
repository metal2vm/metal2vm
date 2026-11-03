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

package bmc

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/metal2vm/metal2vm/internal/bmc/redfishsim"
)

func fakeBMC(t *testing.T, systems ...string) (*redfishsim.Memory, string) {
	t.Helper()
	mem := redfishsim.NewMemory(systems...)
	srv := httptest.NewTLSServer(&redfishsim.Server{Backend: mem, Username: "root", Password: "calvin",
		Manufacturer: "Dell Inc.", Model: "PowerEdge R750"})
	t.Cleanup(srv.Close)
	return mem, srv.URL
}

func TestBootFromISOAndDisk(t *testing.T) {
	mem, url := fakeBMC(t, "System.Embedded.1")
	c, err := Connect(context.Background(), Config{Address: url, Username: "root", Password: "calvin", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	info, err := c.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Model != "PowerEdge R750" || info.SerialNumber != "SIM-System.Embedded.1" || info.PowerState != "On" || !info.VirtualMedia {
		t.Fatalf("info %+v", info)
	}

	iso := "https://gw.apps.example.com/boot/v1/iso/key.iso"
	if err := c.BootFromISO(iso); err != nil {
		t.Fatal(err)
	}
	if img, ok := mem.Media("System.Embedded.1"); !ok || img != iso {
		t.Fatalf("media %q %v", img, ok)
	}
	want := []string{"insert System.Embedded.1 " + iso, "boot System.Embedded.1 Cd", "reset System.Embedded.1 ForceRestart"}
	if !slices.Equal(mem.Calls, want) {
		t.Fatalf("calls %v, want %v", mem.Calls, want)
	}

	// Cutover: power off; rollback: eject, boot from disk, power on.
	if err := c.ForceOff(); err != nil {
		t.Fatal(err)
	}
	if on, _ := c.PowerOn(); on {
		t.Fatal("still on after ForceOff")
	}
	mem.Calls = nil
	if err := c.BootFromDisk(); err != nil {
		t.Fatal(err)
	}
	want = []string{"eject System.Embedded.1", "boot System.Embedded.1 Hdd", "reset System.Embedded.1 On"}
	if !slices.Equal(mem.Calls, want) {
		t.Fatalf("rollback calls %v, want %v", mem.Calls, want)
	}
}

func TestInsertRetriesWithProtocol(t *testing.T) {
	mem, url := fakeBMC(t, "1")
	c, err := Connect(context.Background(), Config{Address: url, Username: "root", Password: "calvin", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	mem.FailNext = errors.New("TransferProtocolType required")
	if err := c.InsertISO("http://gw/boot.iso"); err != nil {
		t.Fatal(err)
	}
	if len(mem.Calls) != 2 {
		t.Fatalf("expected a retry, calls %v", mem.Calls)
	}
}

func TestConnectErrors(t *testing.T) {
	_, url := fakeBMC(t, "a", "b")
	ctx := context.Background()
	if _, err := Connect(ctx, Config{Address: url, Username: "root", Password: "wrong", Insecure: true}); err == nil {
		t.Error("wrong password must fail")
	}
	if _, err := Connect(ctx, Config{Address: url, Username: "root", Password: "calvin", Insecure: true}); err == nil ||
		!strings.Contains(err.Error(), "systemID") {
		t.Errorf("several systems without systemID: %v", err)
	}
	if _, err := Connect(ctx, Config{Address: url, Username: "root", Password: "calvin", Insecure: true, SystemID: "b"}); err != nil {
		t.Errorf("explicit systemID: %v", err)
	}
	if _, err := Connect(ctx, Config{Address: url, Username: "root", Password: "calvin"}); err == nil {
		t.Error("self-signed BMC certificate must be rejected unless insecureSkipVerify")
	}
}
