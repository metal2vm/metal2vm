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

// metal2vm-transfer runs in a migration's pods:
//
//	metal2vm-transfer          accept the live agent's chisel tunnel (reverse
//	                           port forwards only) and copy each disk into its
//	                           PVC, serving progress on /status
//	metal2vm-transfer convert  convert the copied guest in place with
//	                           virt-v2v-in-place and report what it found
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	chserver "github.com/jpillora/chisel/server"

	"github.com/metal2vm/metal2vm/internal/convert"
	"github.com/metal2vm/metal2vm/internal/transfer"
)

func runConvert(args []string) int {
	fs := flag.NewFlagSet("convert", flag.ExitOnError)
	v2v := fs.String("virt-v2v-in-place", "/usr/libexec/virt-v2v-in-place", "virt-v2v-in-place binary")
	guestfish := fs.String("guestfish", "guestfish", "guestfish binary (network config fix-up)")
	workDir := fs.String("work-dir", "/var/tmp", "writable directory for domain and inspection XML")
	termLog := fs.String("termination-log", "/dev/termination-log", "where the result is written")
	_ = fs.Parse(args)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var plan convert.Plan
	if err := json.Unmarshal([]byte(os.Getenv("CONVERT_PLAN")), &plan); err != nil || len(plan.Disks) == 0 {
		log.Error("CONVERT_PLAN must hold a plan with at least one disk", "error", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("converting guest", "name", plan.Name, "disks", len(plan.Disks), "firmware", plan.Firmware)
	res, err := convert.Run(ctx, plan, *v2v, *guestfish, *workDir, os.Stdout)
	if err != nil {
		res.Error = err.Error()
		convert.WriteTermination(*termLog, res)
		log.Error("conversion failed", "error", err)
		return 1
	}
	convert.WriteTermination(*termLog, res)
	log.Info("conversion finished", "os", res.OS.OSInfo, "firmware", res.Firmware)
	return 0
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "convert" {
		os.Exit(runConvert(os.Args[2:]))
	}
	tunnelAddr := flag.String("tunnel-port", "8443", "port of the TLS chisel server")
	statusAddr := flag.String("status-address", ":8081", "address of the plain HTTP status endpoint")
	certFile := flag.String("tls-cert", "/etc/metal2vm-transfer/tls.crt", "tunnel TLS certificate")
	keyFile := flag.String("tls-key", "/etc/metal2vm-transfer/tls.key", "tunnel TLS key")
	authFile := flag.String("authfile", "/etc/metal2vm-transfer/users.json",
		"chisel authfile restricting the agent to its reverse forwards")
	termLog := flag.String("termination-log", "/dev/termination-log", "where the final status is written")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var plan transfer.Plan
	if err := json.Unmarshal([]byte(os.Getenv("TRANSFER_PLAN")), &plan); err != nil || len(plan.Disks) == 0 {
		log.Error("TRANSFER_PLAN must hold a plan with at least one disk", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := chserver.NewServer(&chserver.Config{
		AuthFile:  *authFile,
		Reverse:   true,
		KeepAlive: 25 * time.Second,
		TLS:       chserver.TLSConfig{Cert: *certFile, Key: *keyFile},
	})
	if err != nil {
		log.Error("creating tunnel server", "error", err)
		os.Exit(1)
	}
	if err := srv.StartContext(ctx, "0.0.0.0", *tunnelAddr); err != nil {
		log.Error("starting tunnel server", "error", err)
		os.Exit(1)
	}

	copier := transfer.NewCopier(plan, log)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(copier.Status())
	})
	go func() {
		s := &http.Server{Addr: *statusAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		if err := s.ListenAndServe(); err != nil {
			log.Error("status server", "error", err)
		}
	}()

	log.Info("waiting for the agent", "disks", len(plan.Disks))
	err = copier.Run(ctx)
	transfer.WriteTermination(*termLog, copier.Status())
	if err != nil {
		log.Error("transfer failed", "error", err)
		os.Exit(1)
	}
	log.Info("all disks copied")
}
