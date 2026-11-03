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

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/bmc"
)

// BMCClient is what the controllers need from a BMC (see package bmc).
type BMCClient interface {
	Info() (bmc.Info, error)
	PowerOn() (bool, error)
	BootFromISO(url string) error
	BootFromNetwork() error
	BootFromDisk() error
	ForceOff() error
	EjectAll() error
	InsertedMedia() (image string, inserted bool, err error)
	Close()
}

// BMCConnector opens a BMC session.
type BMCConnector func(ctx context.Context, cfg bmc.Config) (BMCClient, error)

// DefaultBMCConnector uses Redfish.
func DefaultBMCConnector(ctx context.Context, cfg bmc.Config) (BMCClient, error) {
	return bmc.Connect(ctx, cfg)
}

// connectBMC opens a session to a source's BMC with its credentials Secret,
// read uncached through reader.
func connectBMC(ctx context.Context, reader client.Reader, connect BMCConnector, src *migrationv1alpha1.P2VSource) (BMCClient, error) {
	b := src.Spec.BMC
	if b == nil {
		return nil, fmt.Errorf("P2VSource %s has no spec.bmc", src.Name)
	}
	var sec corev1.Secret
	if err := reader.Get(ctx, types.NamespacedName{Namespace: src.Namespace, Name: b.CredentialsSecretRef.Name}, &sec); err != nil {
		return nil, fmt.Errorf("reading BMC credentials %s: %w", b.CredentialsSecretRef.Name, err)
	}
	if connect == nil {
		connect = DefaultBMCConnector
	}
	return connect(ctx, bmc.Config{
		Address: b.Address, SystemID: b.SystemID, Insecure: b.InsecureSkipVerify,
		Username: string(sec.Data["username"]), Password: string(sec.Data["password"]),
	})
}
