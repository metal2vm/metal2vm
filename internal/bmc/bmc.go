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

// Package bmc controls a server through its Redfish BMC: virtual media, one-
// time boot override and power. It uses gofish and tolerates the common
// vendor differences (virtual media under Systems or Managers, BMCs that
// require or reject TransferProtocolType).
package bmc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"
)

// Info describes the managed system.
type Info struct {
	Manufacturer string
	Model        string
	SerialNumber string
	PowerState   string
	// VirtualMedia is false when no CD/DVD virtual media device was found.
	VirtualMedia bool
}

// Client is a connection to one system behind a BMC.
type Client struct {
	api    *gofish.APIClient
	system *schemas.ComputerSystem
}

// Config selects the BMC and system.
type Config struct {
	// Address is the Redfish base URL, e.g. https://10.0.0.10.
	Address  string
	SystemID string
	Username string
	Password string
	Insecure bool
}

// Connect opens a session (basic auth) and selects the system: SystemID if
// set, otherwise the only system.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	api, err := gofish.ConnectContext(ctx, gofish.ClientConfig{
		Endpoint: strings.TrimSuffix(cfg.Address, "/"), Username: cfg.Username, Password: cfg.Password,
		Insecure: cfg.Insecure, BasicAuth: true, TLSHandshakeTimeout: 15,
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to the BMC: %w", err)
	}
	systems, err := api.Service.Systems()
	if err != nil {
		api.Logout()
		return nil, fmt.Errorf("listing systems: %w", err)
	}
	c := &Client{api: api}
	for _, s := range systems {
		if cfg.SystemID == "" || s.ID == cfg.SystemID {
			if c.system != nil && cfg.SystemID == "" {
				api.Logout()
				return nil, errors.New("the BMC manages several systems: set spec.bmc.systemID")
			}
			c.system = s
		}
	}
	if c.system == nil {
		api.Logout()
		return nil, fmt.Errorf("system %q not found on the BMC", cfg.SystemID)
	}
	return c, nil
}

// Close ends the session.
func (c *Client) Close() { c.api.Logout() }

// Info reads the system's identity and power state.
func (c *Client) Info() (Info, error) {
	if err := c.refresh(); err != nil {
		return Info{}, err
	}
	vm, _ := c.cdMedia()
	return Info{
		Manufacturer: c.system.Manufacturer, Model: c.system.Model, SerialNumber: c.system.SerialNumber,
		PowerState: string(c.system.PowerState), VirtualMedia: vm != nil,
	}, nil
}

func (c *Client) refresh() error {
	s, err := schemas.GetObject[schemas.ComputerSystem](c.api, c.system.ODataID)
	if err != nil {
		return fmt.Errorf("reading the system: %w", err)
	}
	c.system = s
	return nil
}

// PowerOn reports whether the system is powered on.
func (c *Client) PowerOn() (bool, error) {
	if err := c.refresh(); err != nil {
		return false, err
	}
	return c.system.PowerState == schemas.OnPowerState, nil
}

// cdMedia finds a CD/DVD virtual media device: under the system (newer
// Redfish), else under the managers.
func (c *Client) cdMedia() (*schemas.VirtualMedia, error) {
	var all []*schemas.VirtualMedia
	if media, err := c.system.VirtualMedia(); err == nil {
		all = append(all, media...)
	}
	if len(all) == 0 {
		managers, err := c.api.Service.Managers()
		if err != nil {
			return nil, err
		}
		for _, m := range managers {
			if media, err := m.VirtualMedia(); err == nil {
				all = append(all, media...)
			}
		}
	}
	for _, m := range all {
		if slices.Contains(m.MediaTypes, schemas.CDVirtualMediaType) || slices.Contains(m.MediaTypes, schemas.DVDVirtualMediaType) {
			return m, nil
		}
	}
	return nil, errors.New("no CD/DVD virtual media device found on the BMC")
}

// InsertISO mounts url as virtual CD, replacing any inserted media.
func (c *Client) InsertISO(url string) error {
	m, err := c.cdMedia()
	if err != nil {
		return err
	}
	if m.Inserted != nil && *m.Inserted {
		if _, err := m.EjectMedia(); err != nil {
			return fmt.Errorf("ejecting the current media: %w", err)
		}
	}
	inserted, readOnly := true, true
	params := &schemas.VirtualMediaInsertMediaParameters{Image: url, Inserted: &inserted, WriteProtected: &readOnly}
	if _, err := m.InsertMedia(params); err == nil {
		return nil
	}
	// Some BMCs (e.g. Supermicro, older iDRAC) require the protocol to be explicit.
	proto := schemas.HTTPSTransferProtocolType
	if strings.HasPrefix(url, "http://") {
		proto = schemas.HTTPTransferProtocolType
	}
	params.TransferProtocolType = &proto
	if _, err := m.InsertMedia(params); err != nil {
		return fmt.Errorf("inserting the ISO: %w", err)
	}
	return nil
}

// InsertedMedia returns the image mounted on the CD/DVD virtual media.
func (c *Client) InsertedMedia() (string, bool, error) {
	m, err := c.cdMedia()
	if err != nil {
		return "", false, err
	}
	return m.Image, m.Inserted == nil || *m.Inserted, nil
}

// EjectAll ejects the CD/DVD virtual media, if any is inserted.
func (c *Client) EjectAll() error {
	m, err := c.cdMedia()
	if err != nil {
		return nil // nothing to eject
	}
	if m.Inserted != nil && !*m.Inserted {
		return nil
	}
	_, err = m.EjectMedia()
	return err
}

// BootOnce sets a one-time boot override (Cd or Hdd).
func (c *Client) BootOnce(target schemas.BootSource) error {
	if err := c.system.SetBoot(&schemas.Boot{
		BootSourceOverrideTarget: target, BootSourceOverrideEnabled: schemas.OnceBootSourceOverrideEnabled,
	}); err != nil {
		return fmt.Errorf("setting the boot override to %s: %w", target, err)
	}
	return nil
}

// PowerCycle powers the system on, or force-restarts it when it is on.
func (c *Client) PowerCycle() error {
	on, err := c.PowerOn()
	if err != nil {
		return err
	}
	reset := schemas.OnResetType
	if on {
		reset = schemas.ForceRestartResetType
	}
	if _, err := c.system.Reset(reset); err != nil {
		return fmt.Errorf("resetting the system (%s): %w", reset, err)
	}
	return nil
}

// ForceOff cuts power.
func (c *Client) ForceOff() error {
	if _, err := c.system.Reset(schemas.ForceOffResetType); err != nil {
		return fmt.Errorf("powering off: %w", err)
	}
	return nil
}

// BootFromISO inserts url, sets a one-time CD boot and power-cycles: the
// server boots the live environment.
func (c *Client) BootFromISO(url string) error {
	if err := c.InsertISO(url); err != nil {
		return err
	}
	if err := c.BootOnce(schemas.CdBootSource); err != nil {
		return err
	}
	return c.PowerCycle()
}

// BootFromNetwork sets a one-time PXE boot and power-cycles.
func (c *Client) BootFromNetwork() error {
	if err := c.BootOnce(schemas.PxeBootSource); err != nil {
		return err
	}
	return c.PowerCycle()
}

// BootFromDisk ejects the live media and boots the server's own disks (rollback).
func (c *Client) BootFromDisk() error {
	if err := c.EjectAll(); err != nil {
		return fmt.Errorf("ejecting the live media: %w", err)
	}
	if err := c.BootOnce(schemas.HddBootSource); err != nil {
		return err
	}
	return c.PowerCycle()
}

// WaitPowerOff polls until the system is off or ctx ends.
func (c *Client) WaitPowerOff(ctx context.Context, every time.Duration) error {
	for {
		on, err := c.PowerOn()
		if err == nil && !on {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}
