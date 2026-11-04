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
	"encoding/json"
	"fmt"
	"slices"
	"time"

	consolev1 "github.com/openshift/api/console/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	securityv1 "github.com/openshift/api/security/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// ConversionSCC lets conversion pods use CRI-O's "unshare" seccomp profile.
	ConversionSCC = "metal2vm-conversion"
	// ConsolePluginName is the ConsolePlugin and its Service in the operator namespace.
	ConsolePluginName = "metal2vm-console-plugin"
	consolePluginPort = 9443
	// capabilityAll drops every capability (restricted pods and the SCC).
	capabilityAll corev1.Capability = "ALL"
)

// The operator creates these cluster-scoped objects itself: OLM bundles cannot
// carry SCCs or ConsolePlugins, and one mechanism serves OLM, Helm and kustomize.
// delete is needed to set an ownerReference (OwnerReferencesPermissionEnforcement).
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,verbs=create
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=metal2vm-conversion,verbs=get;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames=metal2vm-conversion-scc,verbs=get
// +kubebuilder:rbac:groups=console.openshift.io,resources=consoleplugins,verbs=create
// +kubebuilder:rbac:groups=console.openshift.io,resources=consoleplugins,resourceNames=metal2vm-console-plugin,verbs=get;update;patch;delete
// +kubebuilder:rbac:groups=operator.openshift.io,resources=consoles,resourceNames=cluster,verbs=get;patch

// PlatformSetup keeps the operator's cluster-scoped objects in place. Both are
// owned by the ClusterRole metal2vm-conversion-scc, which the installer (OLM,
// Helm, kustomize) manages, so uninstalling metal2vm removes them as well.
type PlatformSetup struct {
	Client client.Client
	// Reader reads without the cache (single named objects, no cluster-wide watch).
	Reader    client.Reader
	Namespace string
	// SCC creates the conversion SCC (OpenShift).
	SCC bool
	// ConsolePlugin registers the console plugin (OpenShift console present).
	ConsolePlugin bool
	// EnableConsolePlugin also adds the plugin to the console operator config.
	EnableConsolePlugin bool
	// Interval re-checks the objects (repairs drift); default 10 minutes.
	Interval time.Duration
}

// Start implements manager.Runnable; it runs on the leader only.
func (p *PlatformSetup) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("platform")
	interval := p.Interval
	if interval == 0 {
		interval = 10 * time.Minute
	}
	for {
		wait := interval
		if err := p.Ensure(ctx); err != nil {
			log.Error(err, "Could not set up cluster-scoped objects; retrying")
			wait = 30 * time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// Ensure creates or repairs the SCC and the console plugin registration.
func (p *PlatformSetup) Ensure(ctx context.Context) error {
	owner, err := p.anchor(ctx)
	if err != nil {
		return err
	}
	if p.SCC {
		if err := p.ensureSCC(ctx, owner); err != nil {
			return fmt.Errorf("SecurityContextConstraints %s: %w", ConversionSCC, err)
		}
	}
	if p.ConsolePlugin {
		if err := p.ensureConsolePlugin(ctx, owner); err != nil {
			return fmt.Errorf("ConsolePlugin %s: %w", ConsolePluginName, err)
		}
		if p.EnableConsolePlugin {
			if err := p.enableConsolePlugin(ctx); err != nil {
				return fmt.Errorf("enabling the console plugin: %w", err)
			}
		}
	}
	return nil
}

// anchor returns the owner reference to the installer-managed ClusterRole, or
// nil if it does not exist (the objects are then left without an owner).
func (p *PlatformSetup) anchor(ctx context.Context) ([]metav1.OwnerReference, error) {
	var cr rbacv1.ClusterRole
	err := p.Reader.Get(ctx, types.NamespacedName{Name: ConversionSCCRole}, &cr)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []metav1.OwnerReference{{
		APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole", Name: cr.Name, UID: cr.UID,
	}}, nil
}

// DesiredConversionSCC is restricted-v2 plus CRI-O's stock "unshare" seccomp
// profile (/var/lib/kubelet/seccomp/profiles/unshare.json, shipped with the
// node OS): virt-v2v's network helper passt sandboxes itself with unshare(2),
// which the default profile blocks. Same approach as Forklift/MTV.
func DesiredConversionSCC() *securityv1.SecurityContextConstraints {
	return &securityv1.SecurityContextConstraints{
		ObjectMeta: metav1.ObjectMeta{
			Name:   ConversionSCC,
			Labels: map[string]string{LabelPartOf: PartOfValue},
			Annotations: map[string]string{
				"kubernetes.io/description": "metal2vm conversion pods: restricted-v2 plus the unshare seccomp profile for virt-v2v's passt.",
			},
		},
		AllowPrivilegeEscalation: new(false),
		RequiredDropCapabilities: []corev1.Capability{capabilityAll},
		RunAsUser:                securityv1.RunAsUserStrategyOptions{Type: securityv1.RunAsUserStrategyMustRunAsRange},
		SELinuxContext:           securityv1.SELinuxContextStrategyOptions{Type: securityv1.SELinuxStrategyMustRunAs},
		FSGroup:                  securityv1.FSGroupStrategyOptions{Type: securityv1.FSGroupStrategyMustRunAs},
		SupplementalGroups:       securityv1.SupplementalGroupsStrategyOptions{Type: securityv1.SupplementalGroupsStrategyRunAsAny},
		SeccompProfiles:          []string{"runtime/default", "localhost/profiles/unshare.json"},
		Volumes: []securityv1.FSType{
			securityv1.FSTypeConfigMap, securityv1.FSTypeCSI, securityv1.FSTypeDownwardAPI, securityv1.FSTypeEmptyDir,
			securityv1.FSTypeEphemeral, securityv1.FSTypePersistentVolumeClaim, securityv1.FSProjected, securityv1.FSTypeSecret,
		},
		Users:  []string{},
		Groups: []string{},
	}
}

func (p *PlatformSetup) ensureSCC(ctx context.Context, owner []metav1.OwnerReference) error {
	want := DesiredConversionSCC()
	want.OwnerReferences = owner
	var cur securityv1.SecurityContextConstraints
	err := p.Reader.Get(ctx, types.NamespacedName{Name: ConversionSCC}, &cur)
	if apierrors.IsNotFound(err) {
		return p.Client.Create(ctx, want)
	}
	if err != nil {
		return err
	}
	next := cur.DeepCopy()
	meta := next.ObjectMeta
	*next = *want
	next.ObjectMeta = meta
	next.Labels, next.Annotations, next.OwnerReferences = want.Labels, want.Annotations, owner
	if equality.Semantic.DeepEqual(&cur, next) {
		return nil
	}
	return p.Client.Update(ctx, next)
}

func (p *PlatformSetup) ensureConsolePlugin(ctx context.Context, owner []metav1.OwnerReference) error {
	spec := consolev1.ConsolePluginSpec{
		DisplayName: "metal2vm",
		I18n:        consolev1.ConsolePluginI18n{LoadType: consolev1.Preload},
		Backend: consolev1.ConsolePluginBackend{
			Type: consolev1.Service,
			Service: &consolev1.ConsolePluginService{
				Name: ConsolePluginName, Namespace: p.Namespace, Port: consolePluginPort, BasePath: "/",
			},
		},
	}
	var cur consolev1.ConsolePlugin
	err := p.Reader.Get(ctx, types.NamespacedName{Name: ConsolePluginName}, &cur)
	if apierrors.IsNotFound(err) {
		return p.Client.Create(ctx, &consolev1.ConsolePlugin{
			ObjectMeta: metav1.ObjectMeta{Name: ConsolePluginName, Labels: map[string]string{LabelPartOf: PartOfValue},
				OwnerReferences: owner},
			Spec: spec,
		})
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(cur.Spec, spec) && equality.Semantic.DeepEqual(cur.OwnerReferences, owner) {
		return nil
	}
	cur.Spec, cur.OwnerReferences = spec, owner
	return p.Client.Update(ctx, &cur)
}

// enableConsolePlugin adds the plugin to consoles.operator.openshift.io/cluster
// (what the "Console plugin: Enable" switch in the web console does).
func (p *PlatformSetup) enableConsolePlugin(ctx context.Context) error {
	var c operatorv1.Console
	if err := p.Reader.Get(ctx, types.NamespacedName{Name: "cluster"}, &c); err != nil {
		return err
	}
	if slices.Contains(c.Spec.Plugins, ConsolePluginName) {
		return nil
	}
	// The resourceVersion precondition avoids dropping a plugin added concurrently.
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": c.ResourceVersion},
		"spec":     map[string]any{"plugins": append(c.Spec.Plugins, ConsolePluginName)},
	})
	if err != nil {
		return err
	}
	return p.Client.Patch(ctx, &c, client.RawPatch(types.MergePatchType, body))
}
