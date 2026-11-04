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
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Options configure the metal2vm controllers.
type Options struct {
	// Namespace of the operator (gateway, transfer services).
	Namespace string
	// TransferImage runs transfer and conversion pods.
	TransferImage string
}

// SetupControllers registers the metal2vm controllers with mgr.
func SetupControllers(mgr ctrl.Manager, o Options) error {
	// Optional APIs: watching a kind the cluster does not serve would stop the
	// manager, so only watch what exists and report the rest in status.
	has := func(group, kind string) bool {
		_, err := mgr.GetRESTMapper().RESTMapping(schema.GroupKind{Group: group, Kind: kind}, "v1")
		return err == nil
	}
	// On OpenShift, conversion pods use the metal2vm-conversion SCC.
	openShift := has("security.openshift.io", "SecurityContextConstraints")
	routes := has("route.openshift.io", "Route")
	kubeVirt := has("kubevirt.io", "VirtualMachine")
	ctrl.Log.WithName("setup").Info("Detected platform", "openshift", openShift, "routes", routes, "kubevirt", kubeVirt)

	if err := (&P2VSourceReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
		BMC:       DefaultBMCConnector,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("p2vsource controller: %w", err)
	}
	if err := (&P2VMigrationReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Namespace:       o.Namespace,
		TransferImage:   o.TransferImage,
		OpenShift:       openShift,
		WithoutRoutes:   !routes,
		WithoutKubeVirt: !kubeVirt,
		APIReader:       mgr.GetAPIReader(),
		BMC:             DefaultBMCConnector,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("p2vmigration controller: %w", err)
	}
	if err := (&P2VConfigReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Namespace:     o.Namespace,
		WithoutRoutes: !routes,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("p2vconfig controller: %w", err)
	}
	return nil
}
