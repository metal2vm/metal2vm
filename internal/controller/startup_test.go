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
	"path/filepath"
	"testing"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
)

// TestStartsWithoutOptionalAPIs starts the controllers against an API server
// that has only metal2vm's CRDs: no OpenShift Routes, no KubeVirt (plain
// Kubernetes, e.g. the Kind cluster of the e2e tests). The manager must start
// and sync its caches instead of failing on watches of missing kinds.
func TestStartsWithoutOptionalAPIs(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()

	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, migrationv1alpha1.AddToScheme, routev1.AddToScheme, kubevirtv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 s,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{CacheSyncTimeout: 20 * time.Second, SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := SetupControllers(mgr, Options{Namespace: "metal2vm-system", TransferImage: "transfer:test"}); err != nil {
		t.Fatalf("setting up controllers: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- mgr.Start(ctx) }()
	select {
	case err := <-errc:
		t.Fatalf("manager stopped on a cluster without Routes/KubeVirt: %v", err)
	case <-time.After(25 * time.Second):
		// Still running after the cache sync timeout: all watches started.
	}

	// What the admin sees: the gateway explains the missing Route API, and a
	// migration is blocked in pre-flight because KubeVirt is missing.
	c := mgr.GetClient()
	for _, ns := range []string{"metal2vm-system", "apps"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Create(ctx, &migrationv1alpha1.P2VConfig{
		ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.P2VConfigSingletonName}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &migrationv1alpha1.P2VMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "web01", Namespace: "apps"},
		Spec: migrationv1alpha1.P2VMigrationSpec{
			SourceRef: corev1.LocalObjectReference{Name: "web01"},
			Disks:     []migrationv1alpha1.DiskMapping{{SourceDiskID: "d1", Action: migrationv1alpha1.DiskCopy}},
		}}); err != nil {
		t.Fatal(err)
	}
	var reason string
	kubeVirtBlocked := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		var cfg migrationv1alpha1.P2VConfig
		if err := c.Get(ctx, types.NamespacedName{Name: migrationv1alpha1.P2VConfigSingletonName}, &cfg); err == nil {
			if cond := apimeta.FindStatusCondition(cfg.Status.Conditions, ConditionAvailable); cond != nil {
				reason = cond.Reason
			}
		}
		var m migrationv1alpha1.P2VMigration
		if err := c.Get(ctx, types.NamespacedName{Namespace: "apps", Name: "web01"}, &m); err == nil {
			for _, p := range m.Status.Preflight {
				if p.Name == "KubeVirtAvailable" && !p.Passed {
					kubeVirtBlocked = true
				}
			}
		}
		if reason == "RouteAPIUnavailable" && kubeVirtBlocked {
			break
		}
	}
	if reason != "RouteAPIUnavailable" {
		t.Errorf("P2VConfig Available reason = %q, want RouteAPIUnavailable", reason)
	}
	if !kubeVirtBlocked {
		t.Error("pre-flight did not block the migration on the missing KubeVirt API")
	}
	select {
	case err := <-errc:
		t.Fatalf("manager stopped: %v", err)
	default:
	}
}
