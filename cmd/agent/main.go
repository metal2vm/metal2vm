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

// metal2vm-agent runs in the live environment booted on a physical server.
// It reports the hardware inventory to the metal2vm gateway and keeps a
// heartbeat. When a migration starts, it exports the source disks read-only
// to the migration's transfer pod.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/metal2vm/metal2vm/api/v1alpha1"

	"github.com/metal2vm/metal2vm/internal/agent"
	"github.com/metal2vm/metal2vm/internal/agent/client"
	"github.com/metal2vm/metal2vm/internal/agent/inventory"
	"github.com/metal2vm/metal2vm/internal/agent/transfer"
	"github.com/metal2vm/metal2vm/internal/boot"
	"github.com/metal2vm/metal2vm/internal/protocol"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const heartbeatInterval = 20 * time.Second

func main() {
	configPath := flag.String("config", boot.AgentConfigPath, "agent configuration file")
	once := flag.Bool("inventory", false, "print the inventory as JSON and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	collector := inventory.Collector{}

	if *once {
		inv, err := collector.Collect()
		if err != nil {
			log.Error("collecting inventory", "error", err)
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(inv)
		return
	}

	for _, err := range agent.SetDisksReadOnly() {
		log.Warn("could not mark device read-only", "error", err)
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Error("loading config", "error", err)
		os.Exit(1)
	}
	c, err := client.New(cfg)
	if err != nil {
		log.Error("creating gateway client", "error", err)
		os.Exit(1)
	}
	target := fmt.Sprintf("%s/%s", cfg.Namespace, cfg.Source)
	agent.ShowStatus("Source: "+target, "Gateway: "+cfg.GatewayURL, "Status: connecting...")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	disks := &diskMap{}
	xfer := &transfer.Manager{
		GatewayURL: cfg.GatewayURL,
		HTTP:       c.HTTP(),
		CAFile:     cfg.CAFile,
		ToolsDir:   "/run/metal2vm/tools",
		DevicePath: disks.path,
		Log:        log,
	}
	defer xfer.Stop()
	a := &agentLoop{log: log, client: c, collector: collector, target: target, gateway: cfg.GatewayURL,
		disks: disks, xfer: xfer}
	if err := a.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (boot.AgentConfig, error) {
	var cfg boot.AgentConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	if cfg.GatewayURL == "" || cfg.Token == "" || cfg.CAFile == "" {
		return cfg, errors.New("gatewayURL, token and caFile are required")
	}
	return cfg, nil
}

// diskMap remembers where each inventoried disk lives.
type diskMap struct {
	mu    sync.Mutex
	paths map[string]string
}

func (d *diskMap) set(disks []v1alpha1.Disk) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paths = map[string]string{}
	for _, disk := range disks {
		d.paths[disk.ID] = disk.Path
	}
}

func (d *diskMap) path(id string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.paths[id]
	return p, ok
}

type agentLoop struct {
	log       *slog.Logger
	client    *client.Client
	collector inventory.Collector
	target    string
	gateway   string
	disks     *diskMap
	xfer      *transfer.Manager
}

func (a *agentLoop) status(s string) {
	lines := []string{"Source: " + a.target, "Gateway: " + a.gateway, "Status: " + s}
	if m := a.xfer.Active(); m != "" {
		lines = append(lines, "Migration: "+m+" (disks are being copied)")
	}
	agent.ShowStatus(lines...)
}

func (a *agentLoop) sendInventory(ctx context.Context) error {
	inv, err := a.collector.Collect()
	if err != nil {
		return err
	}
	a.disks.set(inv.Disks)
	return retry(ctx, a.log, "inventory", func() error {
		return a.client.SendInventory(ctx, protocol.InventoryRequest{AgentVersion: version, Inventory: *inv})
	})
}

// run reports the inventory (retrying with backoff), then heartbeats until ctx
// ends, starting and stopping disk exports as the gateway instructs.
func (a *agentLoop) run(ctx context.Context) error {
	if err := a.sendInventory(ctx); err != nil {
		a.status("ERROR " + err.Error())
		return err
	}
	a.log.Info("inventory reported")
	a.status("connected, inventory reported")

	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		resp, err := a.client.Heartbeat(ctx, protocol.HeartbeatRequest{AgentVersion: version})
		var se *client.StatusError
		switch {
		case errors.As(err, &se) && se.Permanent():
			a.status("ERROR token rejected (expired?)")
			return err
		case err != nil:
			a.log.Warn("heartbeat failed", "error", err)
		default:
			if resp.ResendInventory {
				if err := a.sendInventory(ctx); err != nil {
					return err
				}
			}
			if resp.PowerOff {
				a.log.Info("cutover: powering off the server")
				a.xfer.Stop()
				a.status("cutover: powering off (the server now runs as a VM)")
				return agent.PowerOff()
			}
			before := a.xfer.Active()
			if err := a.xfer.Ensure(ctx, resp.Transfer); err != nil {
				a.log.Error("starting disk export", "error", err)
				a.status("ERROR disk export: " + err.Error())
			} else if a.xfer.Active() != before {
				a.status("connected")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// retry calls fn until it succeeds, the error is permanent, or ctx ends.
func retry(ctx context.Context, log *slog.Logger, what string, fn func() error) error {
	delay := 2 * time.Second
	for {
		err := fn()
		if err == nil {
			return nil
		}
		var se *client.StatusError
		if errors.As(err, &se) && se.Permanent() {
			return err
		}
		log.Warn("retrying", "what", what, "error", err, "in", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Minute)
	}
}
