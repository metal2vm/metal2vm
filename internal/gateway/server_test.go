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

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/metal2vm/metal2vm/api/v1alpha1"
	agentclient "github.com/metal2vm/metal2vm/internal/agent/client"
	"github.com/metal2vm/metal2vm/internal/boot"
	"github.com/metal2vm/metal2vm/internal/boot/boottest"
	"github.com/metal2vm/metal2vm/internal/names"
	"github.com/metal2vm/metal2vm/internal/protocol"
	"github.com/metal2vm/metal2vm/internal/tokens"
)

const (
	opNS       = "metal2vm-system"
	srcNS      = "apps"
	srcName    = "web01"
	bootKey    = "boot-key-123"
	agentTok   = "agent-token-456"
	gatewayURL = "https://gw.apps.example.com"
)

type fixture struct {
	t      *testing.T
	srv    *Server
	http   *httptest.Server
	kube   client.Client
	now    time.Time
	expiry time.Time
}

func newFixture(t *testing.T, ttl time.Duration, expiresIn time.Duration) *fixture {
	t.Helper()
	dir := t.TempDir()
	isoPath := filepath.Join(dir, "live.iso")
	if err := os.WriteFile(isoPath, boottest.FakeISO(t, "", 4), 0o644); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(dir, "agent")
	if err := os.WriteFile(agentPath, []byte("#!agent-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(expiresIn)
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.P2VSource{}, &v1alpha1.P2VConfig{}, &v1alpha1.P2VMigration{}).
		WithObjects(
			&v1alpha1.P2VConfig{
				ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.P2VConfigSingletonName},
				Status:     v1alpha1.P2VConfigStatus{GatewayURL: gatewayURL},
			},
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: CASecretName, Namespace: opNS},
				Data:       map[string][]byte{CACertKey: []byte("-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n")},
			},
			&v1alpha1.P2VSource{ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: srcNS}},
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: srcName + "-boot", Namespace: srcNS,
					Labels: map[string]string{
						tokens.LabelManagedBy:   tokens.ManagedByValue,
						tokens.LabelSource:      srcName,
						tokens.LabelBootKeyHash: tokens.Hash(bootKey),
						tokens.LabelTokenHash:   tokens.Hash(agentTok),
					},
					Annotations: map[string]string{TokenTTLAnnotation: ttl.String()},
				},
				Data: map[string][]byte{
					tokens.KeyBootKey:   []byte(bootKey),
					tokens.KeyToken:     []byte(agentTok),
					tokens.KeyExpiresAt: []byte(expiry.Format(time.RFC3339)),
				},
			},
		).Build()

	srv := &Server{Client: kube, Namespace: opNS, ISOPath: isoPath, AgentPath: agentPath, Log: logr.Discard(),
		Now: func() time.Time { return now }}
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(h.Close)
	return &fixture{t: t, srv: srv, http: h, kube: kube, now: now, expiry: expiry}
}

func (f *fixture) agent(token string) *agentclient.Client {
	return agentclient.NewWithTransport(f.http.URL, token, http.DefaultTransport)
}

func (f *fixture) source() *v1alpha1.P2VSource {
	var s v1alpha1.P2VSource
	if err := f.kube.Get(context.Background(), types.NamespacedName{Namespace: srcNS, Name: srcName}, &s); err != nil {
		f.t.Fatal(err)
	}
	return &s
}

func TestServeISO(t *testing.T) {
	f := newFixture(t, 24*time.Hour, 24*time.Hour)

	resp, err := http.Get(f.http.URL + "/boot/v1/iso/" + bootKey + ".iso")
	if err != nil {
		t.Fatal(err)
	}
	iso, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, iso)
	}
	area, err := boot.FindEmbedArea(bytes.NewReader(iso))
	if err != nil {
		t.Fatal(err)
	}
	region := iso[area.Offset : area.Offset+area.Length]
	if !bytes.Contains(region, []byte("config.ign")) {
		t.Fatal("embed area has no config.ign")
	}
	if !bytes.Contains(region, []byte(gatewayURL+boot.AgentDownloadPath)) {
		t.Fatal("ignition does not reference the agent download URL")
	}

	// BMC virtual media fetches ISOs with Range requests.
	req, _ := http.NewRequest(http.MethodGet, f.http.URL+"/boot/v1/iso/"+bootKey+".iso", nil)
	req.Header.Set("Range", "bytes=32768-32773")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(part[1:6]) != "CD001" {
		t.Fatalf("range request: status %d body %q", resp.StatusCode, part)
	}

	for key, want := range map[string]int{"wrong-key.iso": 404, bootKey: 404} {
		resp, _ := http.Get(f.http.URL + "/boot/v1/iso/" + key)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: status %d, want %d", key, resp.StatusCode, want)
		}
	}
}

func TestServeISOExpired(t *testing.T) {
	f := newFixture(t, 24*time.Hour, -time.Minute)
	resp, err := http.Get(f.http.URL + "/boot/v1/iso/" + bootKey + ".iso")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status %d, want 410", resp.StatusCode)
	}
}

func TestServeAgent(t *testing.T) {
	f := newFixture(t, time.Hour, time.Hour)
	resp, err := http.Get(f.http.URL + boot.AgentDownloadPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "#!agent-binary" {
		t.Fatalf("agent body %q", body)
	}
}

func TestAgentFlow(t *testing.T) {
	f := newFixture(t, 24*time.Hour, 24*time.Hour)
	ctx := context.Background()
	a := f.agent(agentTok)

	// Before any inventory, the gateway asks for one.
	resp, err := a.Heartbeat(ctx, protocol.HeartbeatRequest{AgentVersion: "v0.1.0"})
	if err != nil || !resp.ResendInventory {
		t.Fatalf("heartbeat before inventory: %+v %v", resp, err)
	}

	inv := v1alpha1.Inventory{
		Firmware: v1alpha1.Firmware{Type: v1alpha1.FirmwareUEFI, SecureBoot: true},
		Disks:    []v1alpha1.Disk{{ID: "wwn-naa.1", SizeBytes: 1 << 30}},
		NICs:     []v1alpha1.NIC{{MAC: "aa:bb:cc:dd:ee:ff"}},
		// Agent-supplied provenance must be overridden by the gateway.
		CollectedBy: v1alpha1.InventoryFromPreCheck,
	}
	if err := a.SendInventory(ctx, protocol.InventoryRequest{AgentVersion: "v0.1.0", Inventory: inv}); err != nil {
		t.Fatal(err)
	}
	src := f.source()
	if src.Status.Inventory == nil || src.Status.Inventory.CollectedBy != v1alpha1.InventoryFromLiveAgent ||
		len(src.Status.Inventory.Disks) != 1 || !src.Status.Inventory.Firmware.SecureBoot {
		t.Fatalf("inventory not stored: %+v", src.Status.Inventory)
	}
	if src.Status.Agent == nil || !src.Status.Agent.Connected || src.Status.Agent.Version != "v0.1.0" {
		t.Fatalf("agent status %+v", src.Status.Agent)
	}

	resp, err = a.Heartbeat(ctx, protocol.HeartbeatRequest{AgentVersion: "v0.1.0"})
	if err != nil || resp.ResendInventory {
		t.Fatalf("heartbeat after inventory: %+v %v", resp, err)
	}
}

func TestAgentRejectsBadToken(t *testing.T) {
	f := newFixture(t, time.Hour, time.Hour)
	for _, tok := range []string{"", "wrong", bootKey} {
		_, err := f.agent(tok).Heartbeat(context.Background(), protocol.HeartbeatRequest{})
		var se *agentclient.StatusError
		if !errors.As(err, &se) || !se.Permanent() {
			t.Errorf("token %q: got %v, want 401", tok, err)
		}
	}
}

func TestAgentRejectsExpiredToken(t *testing.T) {
	f := newFixture(t, time.Hour, -time.Second)
	_, err := f.agent(agentTok).Heartbeat(context.Background(), protocol.HeartbeatRequest{})
	var se *agentclient.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("got %v, want 401", err)
	}
}

func TestHeartbeatExtendsToken(t *testing.T) {
	// 10 minutes left of a 1 hour TTL: less than half, so it is extended to now+1h.
	f := newFixture(t, time.Hour, 10*time.Minute)
	if _, err := f.agent(agentTok).Heartbeat(context.Background(), protocol.HeartbeatRequest{}); err != nil {
		t.Fatal(err)
	}
	var sec corev1.Secret
	if err := f.kube.Get(context.Background(), types.NamespacedName{Namespace: srcNS, Name: srcName + "-boot"}, &sec); err != nil {
		t.Fatal(err)
	}
	if got, want := string(sec.Data[tokens.KeyExpiresAt]), f.now.Add(time.Hour).Format(time.RFC3339); got != want {
		t.Fatalf("expiresAt %s, want %s", got, want)
	}
}

func TestInventoryRejectsGarbage(t *testing.T) {
	f := newFixture(t, time.Hour, time.Hour)
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+protocol.InventoryPath, strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+agentTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestHeartbeatCarriesTransfer(t *testing.T) {
	f := newFixture(t, 24*time.Hour, 24*time.Hour)
	ctx := context.Background()
	mig := &v1alpha1.P2VMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "web01-mig", Namespace: srcNS},
		Spec: v1alpha1.P2VMigrationSpec{
			SourceRef: corev1.LocalObjectReference{Name: srcName},
			Disks:     []v1alpha1.DiskMapping{{SourceDiskID: "d0", Action: v1alpha1.DiskCopy}},
		},
	}
	if err := f.kube.Create(ctx, mig); err != nil {
		t.Fatal(err)
	}
	exports, _ := json.Marshal([]protocol.Export{{DiskID: "d0", Port: protocol.FirstExportPort}})
	if err := f.kube.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: names.Transfer(mig.Name), Namespace: srcNS},
		Data: map[string][]byte{
			names.TransferKeyTunnelURL: []byte("https://m2v-abc.apps.example.com"),
			names.TransferKeyAuth:      []byte("agent:pw"),
			names.TransferKeyExports:   exports,
		},
	}); err != nil {
		t.Fatal(err)
	}
	a := f.agent(agentTok)

	// Not copying yet: no instruction.
	resp, err := a.Heartbeat(ctx, protocol.HeartbeatRequest{})
	if err != nil || resp.Transfer != nil {
		t.Fatalf("before copying: %+v %v", resp, err)
	}

	mig.Status.Phase = v1alpha1.PhaseCopying
	if err := f.kube.Status().Update(ctx, mig); err != nil {
		t.Fatal(err)
	}
	resp, err = a.Heartbeat(ctx, protocol.HeartbeatRequest{})
	if err != nil || resp.Transfer == nil {
		t.Fatalf("while copying: %+v %v", resp, err)
	}
	tr := resp.Transfer
	if tr.TunnelURL != "https://m2v-abc.apps.example.com" || tr.Auth != "agent:pw" || tr.Migration != srcNS+"/web01-mig" ||
		len(tr.Exports) != 1 || tr.Exports[0].Port != protocol.FirstExportPort {
		t.Fatalf("instruction %+v", tr)
	}
}

func TestServeTools(t *testing.T) {
	f := newFixture(t, time.Hour, time.Hour)
	tools := filepath.Join(t.TempDir(), "live-tools.tar.gz")
	_ = os.WriteFile(tools, []byte("tgz"), 0o644)
	f.srv.ToolsPath = tools
	resp, err := http.Get(f.http.URL + protocol.ToolsDownloadPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "tgz" {
		t.Fatalf("tools body %q", body)
	}
}

func TestPXE(t *testing.T) {
	f := newFixture(t, 24*time.Hour, 24*time.Hour)
	ctx := context.Background()
	pxe := t.TempDir()
	for _, n := range []string{"kernel", "initramfs.img", "rootfs.img"} {
		_ = os.WriteFile(filepath.Join(pxe, n), []byte(n+"-bytes"), 0o644)
	}
	f.srv.PXEDir = pxe
	get := func(path string) (int, string) {
		resp, err := http.Get(f.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	code, body := get("/boot/v1/pxe/" + bootKey + "/boot.ipxe")
	if code != 200 || !strings.HasPrefix(body, "#!ipxe") ||
		!strings.Contains(body, f.http.URL+"/boot/v1/pxe/"+bootKey+"/ignition.img") ||
		!strings.Contains(body, "coreos.live.rootfs_url="+f.http.URL+"/boot/v1/pxe/rootfs.img") {
		t.Fatalf("script %d:\n%s", code, body)
	}
	if code, _ := get("/boot/v1/pxe/wrong/boot.ipxe"); code != 404 {
		t.Errorf("unknown key: %d", code)
	}
	code, body = get("/boot/v1/pxe/" + bootKey + "/ignition.img")
	if code != 200 || !strings.HasPrefix(body, "070701") || !strings.Contains(body, "config.ign") ||
		!strings.Contains(body, gatewayURL+boot.AgentDownloadPath) {
		t.Fatalf("ignition initrd %d", code)
	}
	if code, body := get("/boot/v1/pxe/kernel"); code != 200 || body != "kernel-bytes" {
		t.Errorf("kernel %d %q", code, body)
	}

	// by-mac: exit unless a PXE migration is booting.
	var src v1alpha1.P2VSource
	_ = f.kube.Get(ctx, types.NamespacedName{Namespace: srcNS, Name: srcName}, &src)
	src.Spec.PXE = &v1alpha1.PXEAccess{BootMACAddress: "3C:EC:EF:12:34:56"}
	if err := f.kube.Update(ctx, &src); err != nil {
		t.Fatal(err)
	}
	if _, body := get("/boot/v1/pxe-mac/3c-ec-ef-12-34-56"); body != IPXEExit {
		t.Fatalf("no migration: %q", body)
	}
	mig := &v1alpha1.P2VMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "pxe-mig", Namespace: srcNS},
		Spec: v1alpha1.P2VMigrationSpec{SourceRef: corev1.LocalObjectReference{Name: srcName}, BootMethod: v1alpha1.BootPXE,
			Disks: []v1alpha1.DiskMapping{{SourceDiskID: "d0", Action: v1alpha1.DiskCopy}}},
	}
	_ = f.kube.Create(ctx, mig)
	mig.Status.Phase = v1alpha1.PhaseBooting
	_ = f.kube.Status().Update(ctx, mig)
	if _, body := get("/boot/v1/pxe-mac/3c:ec:ef:12:34:56"); !strings.Contains(body, "/boot/v1/pxe/"+bootKey+"/ignition.img") {
		t.Fatalf("booting migration: %q", body)
	}
	mig.Status.Phase = v1alpha1.PhaseCopying
	_ = f.kube.Status().Update(ctx, mig)
	if _, body := get("/boot/v1/pxe-mac/3c:ec:ef:12:34:56"); body != IPXEExit {
		t.Fatal("after the boot window the script must fall through to the local disk")
	}
}

func TestBootHandlerExcludesAgentAPI(t *testing.T) {
	f := newFixture(t, time.Hour, time.Hour)
	h := httptest.NewServer(f.srv.BootHandler())
	defer h.Close()
	for _, path := range []string{boot.AgentDownloadPath, protocol.ToolsDownloadPath} {
		resp, _ := http.Get(h.URL + path)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s served on the plain HTTP listener: %d", path, resp.StatusCode)
		}
	}
	resp, _ := http.Post(h.URL+protocol.HeartbeatPath, "application/json", strings.NewReader("{}"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("agent API on the plain HTTP listener: %d", resp.StatusCode)
	}
}
