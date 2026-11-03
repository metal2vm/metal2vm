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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
)

func TestNetworkAssignsIPs(t *testing.T) {
	for cfg, want := range map[string]bool{
		`{"type":"ovn-k8s-cni-overlay","topology":"layer2","subnets":"192.168.77.0/24"}`:       true,
		`{"type":"ovn-k8s-cni-overlay","topology":"localnet","physicalNetworkName":"physnet"}`: false,
		`{"type":"bridge","bridge":"br1","ipam":{"type":"whereabouts","range":"10.0.0.0/24"}}`: true,
		`{"type":"bridge","bridge":"br1","ipam":{}}`:                                           false,
		`not json`: false,
	} {
		if got := networkAssignsIPs(cfg); got != want {
			t.Errorf("%s: got %v, want %v", cfg, got, want)
		}
	}
}

func TestNetworkPlan(t *testing.T) {
	m := &migrationv1alpha1.P2VMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "app01", Namespace: "apps"},
		Spec: migrationv1alpha1.P2VMigrationSpec{NICs: []migrationv1alpha1.NICMapping{
			{SourceMAC: "3C:EC:EF:00:00:01", Type: migrationv1alpha1.NetworkMultus, NetworkName: "vlan10", KeepMAC: true, KeepIP: true},
			{SourceMAC: "3c:ec:ef:00:00:02", Type: migrationv1alpha1.NetworkPod},
			{SourceMAC: "3c:ec:ef:00:00:03", Type: migrationv1alpha1.NetworkMultus, NetworkName: "vlan20"},
		}},
	}
	osc := &migrationv1alpha1.OSConfig{NICs: []migrationv1alpha1.NIC{
		{MAC: "3c:ec:ef:00:00:01", Name: "Ethernet0", IPAddresses: []string{"fe80::1/64", "10.5.0.20/24"},
			Gateway: "10.5.0.1", DNSServers: []string{"10.5.0.10", "10.5.0.11"}},
		{MAC: "3c:ec:ef:00:00:02", Name: "eno2", DHCP: true},
	}}
	plan, static := networkPlan(m, osc)
	if len(plan.NICs) != 3 {
		t.Fatalf("plan %+v", plan)
	}
	keep, pod, newNet := plan.NICs[0], plan.NICs[1], plan.NICs[2]
	if keep.DHCP || keep.OldName != "Ethernet0" || keep.TargetMAC != "3c:ec:ef:00:00:01" {
		t.Errorf("bridged keepIP NIC %+v", keep)
	}
	if !pod.DHCP || pod.OldName != "eno2" || pod.TargetMAC != "3c:ec:ef:00:00:02" {
		t.Errorf("pod NIC %+v", pod)
	}
	if !newNet.DHCP || newNet.OldName != "" || newNet.TargetMAC == "3c:ec:ef:00:00:03" {
		t.Errorf("new-network NIC %+v", newNet)
	}
	if len(static) != 1 || static[0] != "3c:ec:ef:00:00:01:ip:10.5.0.20,10.5.0.1,24,10.5.0.10,10.5.0.11" {
		t.Errorf("static IPs %v", static)
	}

	// Windows: no Linux fix-up, static IPs still go to virt-v2v.
	osc.OS.Family = migrationv1alpha1.OSWindows
	plan, static = networkPlan(m, osc)
	if len(plan.NICs) != 0 || len(static) != 1 {
		t.Errorf("windows plan %+v, static %v", plan, static)
	}
}

func TestOnlyInventoryMissing(t *testing.T) {
	b := migrationv1alpha1.SeverityBlocker
	inv := migrationv1alpha1.PreflightResult{Name: "InventoryAvailable", Severity: b}
	other := migrationv1alpha1.PreflightResult{Name: "SourceNotBusy", Severity: b}
	warn := migrationv1alpha1.PreflightResult{Name: "NoEncryptedDisks", Severity: migrationv1alpha1.SeverityWarning}
	if !onlyInventoryMissing([]migrationv1alpha1.PreflightResult{inv, warn}) {
		t.Error("inventory plus a warning is a discovery case")
	}
	if onlyInventoryMissing([]migrationv1alpha1.PreflightResult{inv, other}) {
		t.Error("another blocker must stop the discovery boot")
	}
	if onlyInventoryMissing([]migrationv1alpha1.PreflightResult{warn}) {
		t.Error("nothing missing")
	}
}
