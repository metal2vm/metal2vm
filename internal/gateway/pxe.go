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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/boot"
	"github.com/metal2vm/metal2vm/internal/tokens"
)

// IPXEExit makes iPXE give up so the firmware boots the next device (the
// server's own disk). Served whenever no migration is waiting for a PXE boot.
const IPXEExit = "#!ipxe\necho metal2vm: no migration is waiting for this server; booting the next device\nexit\n"

// IPXEScript is the iPXE script booting the CoreOS live environment with the
// source's Ignition config appended as a second initrd (like
// `coreos-installer pxe ignition wrap`).
func IPXEScript(base, key string) string {
	return fmt.Sprintf(`#!ipxe
echo metal2vm: booting the migration environment
kernel %[1]s/boot/v1/pxe/kernel initrd=initramfs.img initrd=ignition.img coreos.live.rootfs_url=%[1]s/boot/v1/pxe/rootfs.img ignition.firstboot ignition.platform.id=metal
initrd --name initramfs.img %[1]s/boot/v1/pxe/initramfs.img
initrd --name ignition.img %[1]s/boot/v1/pxe/%[2]s/ignition.img
boot
`, base, key)
}

// baseURL is the scheme and host the request arrived on, so scripts work on
// both the HTTPS and the plain HTTP listener.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) serveIPXE(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if _, err := s.bootSecret(r.Context(), tokens.LabelBootKeyHash, tokens.KeyBootKey, key); err != nil {
		s.bootError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, IPXEScript(baseURL(r), key))
}

func (s *Server) servePXEIgnition(w http.ResponseWriter, r *http.Request) {
	sec, err := s.bootSecret(r.Context(), tokens.LabelBootKeyHash, tokens.KeyBootKey, r.PathValue("key"))
	if err != nil {
		s.bootError(w, r, err)
		return
	}
	ign, err := s.liveIgnition(r.Context(), sec.Namespace, sec.Labels[tokens.LabelSource], string(sec.Data[tokens.KeyToken]))
	if err != nil {
		s.fail(w, "rendering ignition", err)
		return
	}
	s.Log.Info("serving PXE ignition", "namespace", sec.Namespace, "source", sec.Labels[tokens.LabelSource], "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(boot.NewCPIO([]boot.CPIOFile{{Name: "config.ign", Data: ign}}))
}

// serveIPXEByMAC lets one DHCP setting serve every server:
//
//	filename "http://<boot host>/boot/v1/pxe-mac/${net0/mac}"
//
// It only answers with a boot script while the server's migration waits for
// a PXE boot; otherwise the firmware falls through to the local disk.
func (s *Server) serveIPXEByMAC(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(strings.ReplaceAll(r.PathValue("mac"), "-", ":"))
	w.Header().Set("Content-Type", "text/plain")
	src, err := s.sourceWaitingForPXE(r, mac)
	if err != nil {
		s.Log.Error(err, "looking up PXE boot", "mac", mac)
	}
	if src == nil {
		_, _ = io.WriteString(w, IPXEExit)
		return
	}
	var sec corev1.Secret
	if err := s.Client.Get(r.Context(), client.ObjectKey{Namespace: src.Namespace, Name: src.Name + "-boot"}, &sec); err != nil {
		_, _ = io.WriteString(w, IPXEExit)
		return
	}
	s.Log.Info("serving PXE boot script", "namespace", src.Namespace, "source", src.Name, "mac", mac)
	_, _ = io.WriteString(w, IPXEScript(baseURL(r), string(sec.Data[tokens.KeyBootKey])))
}

func (s *Server) sourceWaitingForPXE(r *http.Request, mac string) (*v1alpha1.P2VSource, error) {
	var sources v1alpha1.P2VSourceList
	if err := s.Client.List(r.Context(), &sources); err != nil {
		return nil, err
	}
	for i := range sources.Items {
		src := &sources.Items[i]
		if src.Spec.PXE == nil || !strings.EqualFold(src.Spec.PXE.BootMACAddress, mac) {
			continue
		}
		var migs v1alpha1.P2VMigrationList
		if err := s.Client.List(r.Context(), &migs, client.InNamespace(src.Namespace)); err != nil {
			return nil, err
		}
		for _, m := range migs.Items {
			if m.Spec.SourceRef.Name == src.Name && m.Spec.BootMethod == v1alpha1.BootPXE &&
				(m.Status.Phase == v1alpha1.PhaseBooting || m.Status.Phase == v1alpha1.PhasePreflight) {
				return src, nil
			}
		}
	}
	return nil, nil
}

func (s *Server) bootError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errExpired):
		http.Error(w, "boot media expired", http.StatusGone)
	case errors.Is(err, errUnauthorized):
		http.NotFound(w, r)
	default:
		s.fail(w, "looking up boot media", err)
	}
}
