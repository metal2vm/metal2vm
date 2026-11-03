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

// Package gateway serves boot media to servers and the API live agents call.
// It is exposed outside the cluster (Route, TLS passthrough) and authenticates
// every non-public request with per-source credentials from package tokens.
package gateway

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/boot"
	"github.com/metal2vm/metal2vm/internal/names"
	"github.com/metal2vm/metal2vm/internal/protocol"
	"github.com/metal2vm/metal2vm/internal/tokens"
)

// Names of the gateway's objects in the operator namespace.
const (
	ServiceName   = "metal2vm-gateway"
	RouteName     = "metal2vm-gateway"
	CASecretName  = "metal2vm-gateway-ca"
	TLSSecretName = "metal2vm-gateway-tls"
	// CACertKey holds the CA certificate in CASecretName.
	CACertKey = "ca.crt"
	// Port is the container port the gateway listens on.
	Port = 9444
)

// Condition types set by the gateway on P2VSource.
const (
	ConditionInventoryCollected = "InventoryCollected"
)

// Server is the gateway. It implements manager.Runnable.
type Server struct {
	// Client must cache P2VSource, P2VConfig and metal2vm-labelled Secrets.
	Client    client.Client
	Namespace string
	Addr      string
	ISOPath   string
	AgentPath string
	// ToolsPath is the live-tools rootfs (nbdkit) tarball served to agents.
	ToolsPath string
	Log       logr.Logger
	// Now is overridable for tests.
	Now func() time.Time

	certMu  sync.Mutex
	certRV  string
	certVal *tls.Certificate
}

// NeedLeaderElection is false: every replica serves.
func (s *Server) NeedLeaderElection() bool { return false }

// Start serves until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.getCertificate},
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServeTLS("", "") }()
	s.Log.Info("gateway listening", "addr", s.Addr)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET "+boot.AgentDownloadPath, s.serveAgent)
	mux.HandleFunc("GET /boot/v1/iso/{file}", s.serveISO)
	mux.HandleFunc("GET "+protocol.ToolsDownloadPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeFile(w, r, s.ToolsPath)
	})
	mux.HandleFunc("POST "+protocol.InventoryPath, s.handleInventory)
	mux.HandleFunc("POST "+protocol.HeartbeatPath, s.handleHeartbeat)
	return mux
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	var sec corev1.Secret
	if err := s.Client.Get(context.Background(), types.NamespacedName{Namespace: s.Namespace, Name: TLSSecretName}, &sec); err != nil {
		return nil, fmt.Errorf("gateway certificate not ready: %w", err)
	}
	s.certMu.Lock()
	defer s.certMu.Unlock()
	if s.certVal != nil && s.certRV == sec.ResourceVersion {
		return s.certVal, nil
	}
	cert, err := tls.X509KeyPair(sec.Data[corev1.TLSCertKey], sec.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, err
	}
	s.certVal, s.certRV = &cert, sec.ResourceVersion
	return &cert, nil
}

func (s *Server) serveAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, s.AgentPath)
}

// bootSecret finds the boot Secret whose key (bootKey or token) matches secret.
func (s *Server) bootSecret(ctx context.Context, hashLabel, dataKey, secret string) (*corev1.Secret, error) {
	if secret == "" {
		return nil, errUnauthorized
	}
	var list corev1.SecretList
	if err := s.Client.List(ctx, &list, client.MatchingLabels{
		tokens.LabelManagedBy: tokens.ManagedByValue,
		hashLabel:             tokens.Hash(secret),
	}); err != nil {
		return nil, err
	}
	for i := range list.Items {
		sec := &list.Items[i]
		if subtle.ConstantTimeCompare(sec.Data[dataKey], []byte(secret)) == 1 {
			exp, err := time.Parse(time.RFC3339, string(sec.Data[tokens.KeyExpiresAt]))
			if err != nil || s.now().After(exp) {
				return nil, errExpired
			}
			return sec, nil
		}
	}
	return nil, errUnauthorized
}

var (
	errUnauthorized = errors.New("unauthorized")
	errExpired      = errors.New("credentials expired")
)

func (s *Server) serveISO(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutSuffix(r.PathValue("file"), ".iso")
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	sec, err := s.bootSecret(ctx, tokens.LabelBootKeyHash, tokens.KeyBootKey, key)
	switch {
	case errors.Is(err, errExpired):
		http.Error(w, "boot media expired; download a new ISO", http.StatusGone)
		return
	case errors.Is(err, errUnauthorized):
		http.NotFound(w, r)
		return
	case err != nil:
		s.fail(w, "looking up boot media", err)
		return
	}
	source := sec.Labels[tokens.LabelSource]

	ign, err := s.liveIgnition(ctx, sec.Namespace, source, string(sec.Data[tokens.KeyToken]))
	if err != nil {
		s.fail(w, "rendering ignition", err)
		return
	}
	f, err := os.Open(s.ISOPath)
	if err != nil {
		s.fail(w, "opening base ISO", err)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		s.fail(w, "opening base ISO", err)
		return
	}
	iso, err := boot.NewEmbeddedISO(f, st.Size(), ign)
	if err != nil {
		s.fail(w, "embedding ignition", err)
		return
	}
	if r.Header.Get("Range") == "" {
		s.Log.Info("serving boot ISO", "namespace", sec.Namespace, "source", source, "remote", r.RemoteAddr)
	}
	w.Header().Set("Content-Type", "application/x-iso9660-image")
	http.ServeContent(w, r, fmt.Sprintf("metal2vm-%s-%s.iso", sec.Namespace, source), st.ModTime(),
		io.NewSectionReader(iso, 0, iso.Size()))
}

func (s *Server) liveIgnition(ctx context.Context, namespace, source, token string) ([]byte, error) {
	var cfg v1alpha1.P2VConfig
	if err := s.Client.Get(ctx, types.NamespacedName{Name: v1alpha1.P2VConfigSingletonName}, &cfg); err != nil {
		return nil, err
	}
	if cfg.Status.GatewayURL == "" {
		return nil, errors.New("gateway URL not known yet")
	}
	var ca corev1.Secret
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: CASecretName}, &ca); err != nil {
		return nil, err
	}
	return boot.LiveIgnition(boot.LiveConfig{
		Agent: boot.AgentConfig{
			GatewayURL: cfg.Status.GatewayURL, Token: token, Namespace: namespace, Source: source,
		},
		CACertPEM: ca.Data[CACertKey],
	})
}

// agentSource authenticates an agent request and returns its boot Secret and P2VSource.
func (s *Server) agentSource(w http.ResponseWriter, r *http.Request) (*corev1.Secret, *v1alpha1.P2VSource, bool) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	sec, err := s.bootSecret(r.Context(), tokens.LabelTokenHash, tokens.KeyToken, token)
	if errors.Is(err, errUnauthorized) || errors.Is(err, errExpired) {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return nil, nil, false
	}
	if err != nil {
		s.fail(w, "authenticating agent", err)
		return nil, nil, false
	}
	var src v1alpha1.P2VSource
	if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: sec.Namespace, Name: sec.Labels[tokens.LabelSource]}, &src); err != nil {
		s.fail(w, "loading P2VSource", err)
		return nil, nil, false
	}
	return sec, &src, true
}

func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	sec, src, ok := s.agentSource(w, r)
	if !ok {
		return
	}
	var req protocol.InventoryRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid inventory: "+err.Error(), http.StatusBadRequest)
		return
	}
	now := metav1.NewTime(s.now())
	inv := req.Inventory
	inv.CollectedBy = v1alpha1.InventoryFromLiveAgent
	inv.CollectedAt = now

	patch := client.MergeFrom(src.DeepCopy())
	src.Status.Inventory = &inv
	src.Status.Agent = &v1alpha1.AgentStatus{Connected: true, LastHeartbeat: &now, Version: req.AgentVersion}
	apimeta.SetStatusCondition(&src.Status.Conditions, metav1.Condition{
		Type: ConditionInventoryCollected, Status: metav1.ConditionTrue, Reason: "LiveAgent",
		Message:            fmt.Sprintf("%d disks, %d NICs reported by the live agent", len(inv.Disks), len(inv.NICs)),
		ObservedGeneration: src.Generation,
	})
	if err := s.Client.Status().Patch(r.Context(), src, patch); err != nil {
		s.fail(w, "updating P2VSource status", err)
		return
	}
	s.Log.Info("inventory received", "namespace", src.Namespace, "source", src.Name,
		"disks", len(inv.Disks), "nics", len(inv.NICs), "agentVersion", req.AgentVersion)
	s.extendToken(r.Context(), sec)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	sec, src, ok := s.agentSource(w, r)
	if !ok {
		return
	}
	var req protocol.HeartbeatRequest
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	now := metav1.NewTime(s.now())
	patch := client.MergeFrom(src.DeepCopy())
	src.Status.Agent = &v1alpha1.AgentStatus{Connected: true, LastHeartbeat: &now, Version: req.AgentVersion}
	if err := s.Client.Status().Patch(r.Context(), src, patch); err != nil {
		s.fail(w, "updating P2VSource status", err)
		return
	}
	s.extendToken(r.Context(), sec)
	transfer, err := s.transferFor(r.Context(), src)
	if err != nil {
		s.Log.Error(err, "looking up transfer", "namespace", src.Namespace, "source", src.Name)
	}
	writeJSON(w, protocol.HeartbeatResponse{
		ResendInventory: src.Status.Inventory == nil || src.Status.Inventory.CollectedBy != v1alpha1.InventoryFromLiveAgent,
		Transfer:        transfer,
	})
}

// transferFor returns the disk export instruction for a source whose
// migration is copying, or nil.
func (s *Server) transferFor(ctx context.Context, src *v1alpha1.P2VSource) (*protocol.TransferInstruction, error) {
	var list v1alpha1.P2VMigrationList
	if err := s.Client.List(ctx, &list, client.InNamespace(src.Namespace)); err != nil {
		return nil, err
	}
	for _, m := range list.Items {
		if m.Spec.SourceRef.Name != src.Name || m.Status.Phase != v1alpha1.PhaseCopying {
			continue
		}
		var sec corev1.Secret
		err := s.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: names.Transfer(m.Name)}, &sec)
		if err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		var exports []protocol.Export
		if err := json.Unmarshal(sec.Data[names.TransferKeyExports], &exports); err != nil || len(exports) == 0 {
			return nil, nil // not ready yet
		}
		return &protocol.TransferInstruction{
			Migration: m.Namespace + "/" + m.Name,
			TunnelURL: string(sec.Data[names.TransferKeyTunnelURL]),
			Auth:      string(sec.Data[names.TransferKeyAuth]),
			Exports:   exports,
		}, nil
	}
	return nil, nil
}

// TokenTTLAnnotation on a boot Secret records the TTL it was issued with.
const TokenTTLAnnotation = "metal2vm.io/token-ttl"

// extendToken keeps a connected agent's credentials valid: once less than half
// of the TTL remains, the expiry is moved to now+TTL.
func (s *Server) extendToken(ctx context.Context, sec *corev1.Secret) {
	ttl, err := time.ParseDuration(sec.Annotations[TokenTTLAnnotation])
	if err != nil || ttl <= 0 {
		return
	}
	exp, err := time.Parse(time.RFC3339, string(sec.Data[tokens.KeyExpiresAt]))
	if err != nil || exp.Sub(s.now()) > ttl/2 {
		return
	}
	patch := client.MergeFrom(sec.DeepCopy())
	sec.Data[tokens.KeyExpiresAt] = []byte(s.now().Add(ttl).UTC().Format(time.RFC3339))
	if err := s.Client.Patch(ctx, sec, patch); err != nil {
		s.Log.Error(err, "extending agent token", "namespace", sec.Namespace, "secret", sec.Name)
	}
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.Log.Error(err, what)
	http.Error(w, what+" failed", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
