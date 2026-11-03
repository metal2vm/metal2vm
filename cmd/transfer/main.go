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

// metal2vm-transfer runs in a migration's transfer pod. It accepts the live
// agent's chisel tunnel (reverse port forwards only) and copies each disk
// into its PVC, serving progress on /status for the operator.
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

	"github.com/metal2vm/metal2vm/internal/transfer"
)

func main() {
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
