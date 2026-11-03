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

// Package redfishsim is a minimal Redfish BMC: service root, systems, boot
// override, reset and CD virtual media, backed by a pluggable Backend. It
// serves the unit tests (in-memory backend) and the lab emulator for
// KubeVirt VMs. It implements only what metal2vm's BMC client uses.
package redfishsim

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// Backend performs the BMC operations on a system.
type Backend interface {
	Systems() []string
	PowerOn(system string) (bool, error)
	// Reset handles On, ForceOff, GracefulShutdown, ForceRestart, GracefulRestart.
	Reset(system, resetType string) error
	// SetBootOnce handles Cd, Hdd and None.
	SetBootOnce(system, target string) error
	InsertMedia(system, image string) error
	EjectMedia(system string) error
	Media(system string) (image string, inserted bool)
}

// Server is an http.Handler implementing the Redfish subset.
type Server struct {
	Backend      Backend
	Username     string
	Password     string
	Manufacturer string
	Model        string
}

const (
	root      = "/redfish/v1"
	odataID   = "@odata.id"
	odataType = "@odata.type"
	target    = "target"
)

var resetTypes = []string{"On", "ForceOff", "GracefulShutdown", "ForceRestart", "GracefulRestart"}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(s.Username)) != 1 ||
		subtle.ConstantTimeCompare([]byte(pass), []byte(s.Password)) != 1 {
		if r.URL.Path != root && r.URL.Path != root+"/" {
			w.Header().Set("WWW-Authenticate", `Basic realm="redfish"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	p := strings.TrimSuffix(r.URL.Path, "/")
	parts := strings.Split(strings.TrimPrefix(p, root), "/")[1:]
	switch {
	case p == root:
		writeJSON(w, map[string]any{
			odataID: root + "/", odataType: "#ServiceRoot.v1_15_0.ServiceRoot", "Id": "RootService",
			"RedfishVersion": "1.15.0", "Systems": ref(root + "/Systems"), "Managers": ref(root + "/Managers"),
		})
	case p == root+"/Systems":
		var members []any
		for _, id := range s.Backend.Systems() {
			members = append(members, ref(root+"/Systems/"+id))
		}
		writeJSON(w, collection(p, "#ComputerSystemCollection.ComputerSystemCollection", members))
	case p == root+"/Managers":
		writeJSON(w, collection(p, "#ManagerCollection.ManagerCollection", nil))
	case len(parts) >= 2 && parts[0] == "Systems":
		s.system(w, r, parts[1], parts[2:])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) known(id string) bool { return slices.Contains(s.Backend.Systems(), id) }

func (s *Server) system(w http.ResponseWriter, r *http.Request, id string, rest []string) {
	if !s.known(id) {
		http.NotFound(w, r)
		return
	}
	base := root + "/Systems/" + id
	sub := strings.Join(rest, "/")
	switch {
	case sub == "" && r.Method == http.MethodGet:
		on, err := s.Backend.PowerOn(id)
		if err != nil {
			fail(w, err)
			return
		}
		state := "Off"
		if on {
			state = "On"
		}
		writeJSON(w, map[string]any{
			odataID: base, odataType: "#ComputerSystem.v1_20_0.ComputerSystem", "Id": id, "Name": id,
			"Manufacturer": s.Manufacturer, "Model": s.Model, "SerialNumber": "SIM-" + id, "PowerState": state,
			"Boot": map[string]any{
				"BootSourceOverrideEnabled":                        "Disabled",
				"BootSourceOverrideTarget":                         "None",
				"BootSourceOverrideTarget@Redfish.AllowableValues": []string{"None", "Cd", "Hdd", "Pxe"},
			},
			"VirtualMedia": ref(base + "/VirtualMedia"),
			"Actions": map[string]any{"#ComputerSystem.Reset": map[string]any{
				target: base + "/Actions/ComputerSystem.Reset", "ResetType@Redfish.AllowableValues": resetTypes,
			}},
		})
	case sub == "" && r.Method == http.MethodPatch:
		var body struct {
			Boot struct {
				BootSourceOverrideTarget string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.Backend.SetBootOnce(id, body.Boot.BootSourceOverrideTarget); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "Actions/ComputerSystem.Reset" && r.Method == http.MethodPost:
		var body struct{ ResetType string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.Backend.Reset(id, body.ResetType); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "VirtualMedia":
		writeJSON(w, collection(base+"/VirtualMedia", "#VirtualMediaCollection.VirtualMediaCollection",
			[]any{ref(base + "/VirtualMedia/Cd")}))
	case sub == "VirtualMedia/Cd":
		image, inserted := s.Backend.Media(id)
		via := "NotConnected"
		if inserted {
			via = "URI"
		}
		writeJSON(w, map[string]any{
			odataID: base + "/VirtualMedia/Cd", odataType: "#VirtualMedia.v1_6_0.VirtualMedia", "Id": "Cd", "Name": "Virtual CD",
			"MediaTypes": []string{"CD", "DVD"}, "Image": image, "Inserted": inserted, "WriteProtected": true, "ConnectedVia": via,
			"Actions": map[string]any{
				"#VirtualMedia.InsertMedia": map[string]any{target: base + "/VirtualMedia/Cd/Actions/VirtualMedia.InsertMedia"},
				"#VirtualMedia.EjectMedia":  map[string]any{target: base + "/VirtualMedia/Cd/Actions/VirtualMedia.EjectMedia"},
			},
		})
	case sub == "VirtualMedia/Cd/Actions/VirtualMedia.InsertMedia" && r.Method == http.MethodPost:
		var body struct{ Image string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Image == "" {
			http.Error(w, "Image is required", http.StatusBadRequest)
			return
		}
		if err := s.Backend.InsertMedia(id, body.Image); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "VirtualMedia/Cd/Actions/VirtualMedia.EjectMedia" && r.Method == http.MethodPost:
		if err := s.Backend.EjectMedia(id); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func ref(id string) map[string]string { return map[string]string{odataID: id} }

func collection(id, typ string, members []any) map[string]any {
	if members == nil {
		members = []any{}
	}
	return map[string]any{odataID: id, odataType: typ, "Members": members, "Members@odata.count": len(members)}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), http.StatusInternalServerError)
}

// Memory is an in-memory Backend for tests. It records every call.
type Memory struct {
	mu       sync.Mutex
	power    map[string]bool
	boot     map[string]string
	image    map[string]string
	Calls    []string
	FailNext error
}

// NewMemory returns a Memory backend with the given systems, all powered on.
func NewMemory(systems ...string) *Memory {
	m := &Memory{power: map[string]bool{}, boot: map[string]string{}, image: map[string]string{}}
	for _, s := range systems {
		m.power[s] = true
	}
	return m
}

func (m *Memory) record(format string, a ...any) error {
	m.Calls = append(m.Calls, fmt.Sprintf(format, a...))
	if err := m.FailNext; err != nil {
		m.FailNext = nil
		return err
	}
	return nil
}

func (m *Memory) Systems() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.power))
	for s := range m.power {
		out = append(out, s)
	}
	return out
}

func (m *Memory) PowerOn(s string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.power[s], nil
}

// SetPower changes the power state (tests simulate an OS shutting down).
func (m *Memory) SetPower(s string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.power[s] = on
}

func (m *Memory) Reset(s, t string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.record("reset %s %s", s, t); err != nil {
		return err
	}
	switch t {
	case "On", "ForceRestart", "GracefulRestart":
		m.power[s] = true
	case "ForceOff", "GracefulShutdown":
		m.power[s] = false
	default:
		return fmt.Errorf("unsupported ResetType %q", t)
	}
	return nil
}

func (m *Memory) SetBootOnce(s, t string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.boot[s] = t
	return m.record("boot %s %s", s, t)
}

func (m *Memory) InsertMedia(s, image string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.record("insert %s %s", s, image); err != nil {
		return err
	}
	m.image[s] = image
	return nil
}

func (m *Memory) EjectMedia(s string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.image, s)
	return m.record("eject %s", s)
}

func (m *Memory) Media(s string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	img, ok := m.image[s]
	return img, ok
}

// Boot returns the last boot override of a system.
func (m *Memory) Boot(s string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.boot[s]
}
