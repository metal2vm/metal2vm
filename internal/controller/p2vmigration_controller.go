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
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	routev1 "github.com/openshift/api/route/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/tokens"
	"github.com/metal2vm/metal2vm/internal/vmbuilder"
)

// Condition types set on P2VMigration.
const (
	ConditionPreflightPassed = "PreflightPassed"
	ConditionAgentConnected  = "AgentConnected"
	ConditionCopied          = "Copied"
	// ConditionDegraded is true while steps fail and are being retried.
	ConditionDegraded = "Degraded"
)

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// requeueSoon moves to the next phase promptly after a phase change.
var requeueSoon = ctrl.Result{RequeueAfter: 200 * time.Millisecond}

// P2VMigrationReconciler drives a migration through its phases.
type P2VMigrationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Namespace is the operator namespace (gateway CA lives there).
	Namespace string
	// TransferImage is the default transfer pod image; P2VConfig can override it.
	TransferImage string
	// OpenShift enables the conversion SCC and its seccomp profile.
	OpenShift bool
	// WithoutRoutes and WithoutKubeVirt are set on clusters that lack these APIs;
	// the controller then neither watches them nor starts migrations.
	WithoutRoutes   bool
	WithoutKubeVirt bool
	// APIReader reads BMC credentials Secrets, which are not cached.
	APIReader client.Reader
	// BMC opens BMC sessions; defaults to Redfish.
	BMC BMCConnector
	// HTTP polls transfer pods; nil uses a client with a short timeout.
	HTTP *http.Client
	Now  func() time.Time
}

// +kubebuilder:rbac:groups=migration.metal2vm.io,resources=p2vmigrations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=migration.metal2vm.io,resources=p2vmigrations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=migration.metal2vm.io,resources=p2vmigrations/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=storageprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch

func (r *P2VMigrationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile advances the migration by at most one phase per call.
func (r *P2VMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var m migrationv1alpha1.P2VMigration
	if err := r.Get(ctx, req.NamespacedName, &m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := m.DeepCopy()
	res, err := r.step(ctx, &m)
	// Surface errors in the status: a click-ops admin never sees operator logs.
	if err != nil {
		setCondition(&m, ConditionDegraded, metav1.ConditionTrue, "ReconcileError", truncate(err.Error(), 1000))
	} else if apimeta.IsStatusConditionTrue(m.Status.Conditions, ConditionDegraded) {
		setCondition(&m, ConditionDegraded, metav1.ConditionFalse, "Recovered", "the last step succeeded")
	}
	if !equality.Semantic.DeepEqual(orig.Status, m.Status) {
		if perr := r.Status().Patch(ctx, &m, client.MergeFrom(orig)); perr != nil && err == nil {
			err = perr
		}
	}
	return res, err
}

func (r *P2VMigrationReconciler) step(ctx context.Context, m *migrationv1alpha1.P2VMigration) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if m.Spec.Archived && terminalPhases[m.Status.Phase] {
		if !apimeta.IsStatusConditionTrue(m.Status.Conditions, ConditionArchived) {
			return ctrl.Result{}, r.reconcileArchive(ctx, m)
		}
		return ctrl.Result{}, nil
	}
	if m.Spec.Rollback && rollbackPhases[m.Status.Phase] {
		return r.reconcileRollback(ctx, m)
	}
	switch m.Status.Phase {
	case "", migrationv1alpha1.PhasePending:
		now := metav1.NewTime(r.now())
		m.Status.StartedAt = &now
		r.setPhase(m, migrationv1alpha1.PhasePreflight, "running pre-flight checks")
		return requeueSoon, nil

	case migrationv1alpha1.PhasePreflight:
		src, results, err := r.preflight(ctx, m)
		if err != nil {
			return ctrl.Result{}, err
		}
		m.Status.Preflight = results
		if blocked := firstBlocker(results); blocked != nil {
			setCondition(m, ConditionPreflightPassed, metav1.ConditionFalse, "Blocked", blocked.Message)
			m.Status.Message = "pre-flight check failed: " + blocked.Message
			// Discovery by boot: a server never inventoried can be booted into
			// the live environment by its BMC; pre-flight then re-runs against
			// the real hardware. (PXE servers are served during pre-flight too.)
			if onlyInventoryMissing(results) && src != nil && src.Spec.BMC != nil &&
				(m.Spec.BootMethod == migrationv1alpha1.BootRedfish || m.Spec.BootMethod == migrationv1alpha1.BootPXE) {
				if ok, _ := bootMethodReady(m, src); ok {
					if err := r.bootViaBMC(ctx, m, src); err != nil {
						return ctrl.Result{}, err
					}
					m.Status.Message = "booting " + src.Name + " through its BMC to discover its hardware"
				}
			}
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		setCondition(m, ConditionPreflightPassed, metav1.ConditionTrue, "Passed", "all pre-flight checks passed")
		r.setPhase(m, migrationv1alpha1.PhaseBooting, bootMessage(m, src))
		return requeueSoon, nil

	case migrationv1alpha1.PhaseBooting:
		return r.reconcileBooting(ctx, m)

	case migrationv1alpha1.PhaseConnected:
		src, err := r.source(ctx, m)
		if err != nil {
			return ctrl.Result{}, err
		}
		if src == nil || src.Status.Inventory == nil {
			r.fail(m, "the P2VSource or its inventory disappeared")
			return ctrl.Result{}, nil
		}
		// The live agent's inventory is authoritative for sizes and device paths.
		for _, d := range m.Spec.Disks {
			if d.Action != migrationv1alpha1.DiskSkip && findDisk(src.Status.Inventory, d.SourceDiskID) == nil {
				r.fail(m, fmt.Sprintf("disk %s is not present on the booted server", d.SourceDiskID))
				return ctrl.Result{}, nil
			}
		}
		r.setPhase(m, migrationv1alpha1.PhaseCopying, "preparing disk copy")
		return requeueSoon, nil

	case migrationv1alpha1.PhaseCopying:
		res, err := r.reconcileCopy(ctx, m)
		if err != nil {
			log.Error(err, "copy step failed")
		}
		return res, err

	case migrationv1alpha1.PhaseConverting:
		return r.reconcileConvert(ctx, m)

	case migrationv1alpha1.PhaseTesting:
		return r.reconcileTesting(ctx, m)

	case migrationv1alpha1.PhaseCuttingOver:
		return r.reconcileCutover(ctx, m)
	}
	return ctrl.Result{}, nil
}

func (r *P2VMigrationReconciler) setPhase(m *migrationv1alpha1.P2VMigration, p migrationv1alpha1.MigrationPhase, msg string) {
	m.Status.Phase = p
	m.Status.Message = msg
}

func (r *P2VMigrationReconciler) fail(m *migrationv1alpha1.P2VMigration, msg string) {
	now := metav1.NewTime(r.now())
	m.Status.CompletedAt = &now
	r.setPhase(m, migrationv1alpha1.PhaseFailed, msg)
	setCondition(m, "Ready", metav1.ConditionFalse, "Failed", msg)
}

func setCondition(m *migrationv1alpha1.P2VMigration, t string, s metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: m.Generation,
	})
}

// reconcileBooting starts the boot (BMC) when needed and waits for the agent.
func (r *P2VMigrationReconciler) reconcileBooting(ctx context.Context, m *migrationv1alpha1.P2VMigration) (ctrl.Result, error) {
	src, err := r.source(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if src != nil && !agentConnected(src) && (m.Spec.BootMethod == migrationv1alpha1.BootRedfish ||
		(m.Spec.BootMethod == migrationv1alpha1.BootPXE && src.Spec.BMC != nil)) {
		if err := r.bootViaBMC(ctx, m, src); err != nil {
			return ctrl.Result{}, err
		}
	}
	if src == nil || !agentConnected(src) {
		m.Status.Message = bootMessage(m, src)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	setCondition(m, ConditionAgentConnected, metav1.ConditionTrue, "Connected", "the live agent is connected")
	m.Status.Agent = src.Status.Agent.DeepCopy()
	r.setPhase(m, migrationv1alpha1.PhaseConnected, "live agent connected")
	return requeueSoon, nil
}

// bootMethodReady checks that the planned boot method can work for src.
func bootMethodReady(m *migrationv1alpha1.P2VMigration, src *migrationv1alpha1.P2VSource) (bool, string) {
	switch m.Spec.BootMethod {
	case "", migrationv1alpha1.BootManual:
		return true, ""
	case migrationv1alpha1.BootRedfish:
		if src == nil || src.Spec.BMC == nil {
			return false, "bootMethod redfish needs spec.bmc on the P2VSource"
		}
		if !apimeta.IsStatusConditionTrue(src.Status.Conditions, ConditionBMCReachable) {
			msg := "the BMC has not been checked yet"
			if c := apimeta.FindStatusCondition(src.Status.Conditions, ConditionBMCReachable); c != nil {
				msg = c.Message
			}
			return false, "the source's BMC is not usable: " + msg
		}
		return true, ""
	case migrationv1alpha1.BootPXE:
		if src == nil || src.Spec.PXE == nil {
			return false, "bootMethod pxe needs spec.pxe.bootMACAddress on the P2VSource"
		}
		return true, ""
	default:
		return false, fmt.Sprintf("unknown bootMethod %s", m.Spec.BootMethod)
	}
}

// ConditionBootMediaInserted records that the BMC was told to boot the live ISO.
const ConditionBootMediaInserted = "BootMediaInserted"

// bootViaBMC inserts the live ISO and power-cycles the server, once.
func (r *P2VMigrationReconciler) bootViaBMC(ctx context.Context, m *migrationv1alpha1.P2VMigration, src *migrationv1alpha1.P2VSource) error {
	if apimeta.IsStatusConditionTrue(m.Status.Conditions, ConditionBootMediaInserted) {
		return nil
	}
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: src.Namespace, Name: BootSecretName(src.Name)}, &sec); err != nil {
		return fmt.Errorf("boot media: %w", err)
	}
	c, err := connectBMC(ctx, r.APIReader, r.BMC, src)
	if err != nil {
		return err
	}
	defer c.Close()
	if m.Spec.BootMethod == migrationv1alpha1.BootPXE {
		if err := c.BootFromNetwork(); err != nil {
			return err
		}
		setCondition(m, ConditionBootMediaInserted, metav1.ConditionTrue, "BMC", "the BMC is network-booting the server (PXE)")
		return nil
	}
	url := string(sec.Data[tokens.KeyISOURL])
	if src.Spec.BMC.MediaProtocol == "HTTP" {
		url = string(sec.Data[tokens.KeyISOHTTPURL])
		if url == "" {
			return fmt.Errorf("mediaProtocol HTTP needs P2VConfig spec.gateway.bootMediaHTTP")
		}
	}
	// The cached migration may be stale: trust the BMC. If our ISO is already
	// inserted, the boot is under way; do not power-cycle again.
	if img, inserted, err := c.InsertedMedia(); err == nil && inserted && img == url {
		setCondition(m, ConditionBootMediaInserted, metav1.ConditionTrue, "BMC",
			"the BMC is booting the server from the metal2vm live ISO")
		return nil
	}
	if err := c.BootFromISO(url); err != nil {
		return err
	}
	setCondition(m, ConditionBootMediaInserted, metav1.ConditionTrue, "BMC",
		"the BMC is booting the server from the metal2vm live ISO")
	return nil
}

func bootMessage(m *migrationv1alpha1.P2VMigration, src *migrationv1alpha1.P2VSource) string {
	switch {
	case m.Spec.BootMethod == migrationv1alpha1.BootRedfish:
		return fmt.Sprintf("waiting for the live agent: %s is booting the metal2vm ISO through its BMC", m.Spec.SourceRef.Name)
	case m.Spec.BootMethod == migrationv1alpha1.BootPXE && src != nil && src.Spec.BMC != nil:
		return fmt.Sprintf("waiting for the live agent: %s is network-booting (PXE) through its BMC", m.Spec.SourceRef.Name)
	case m.Spec.BootMethod == migrationv1alpha1.BootPXE:
		return fmt.Sprintf("waiting for the live agent: power on or reboot %s with network boot (PXE); "+
			"iPXE URL in Secret %s (key ipxeURL / ipxeHTTPURL)", m.Spec.SourceRef.Name, BootSecretName(m.Spec.SourceRef.Name))
	}
	secret := BootSecretName(m.Spec.SourceRef.Name)
	if src != nil && src.Status.BootMedia != nil {
		secret = src.Status.BootMedia.SecretName
	}
	return fmt.Sprintf("waiting for the live agent: boot %s from the ISO in Secret %s (key isoURL)",
		m.Spec.SourceRef.Name, secret)
}

func agentConnected(src *migrationv1alpha1.P2VSource) bool {
	return src.Status.Agent != nil && src.Status.Agent.Connected &&
		src.Status.Inventory != nil && src.Status.Inventory.CollectedBy == migrationv1alpha1.InventoryFromLiveAgent
}

func (r *P2VMigrationReconciler) source(ctx context.Context, m *migrationv1alpha1.P2VMigration) (*migrationv1alpha1.P2VSource, error) {
	var src migrationv1alpha1.P2VSource
	err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: m.Spec.SourceRef.Name}, &src)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return &src, err
}

func findDisk(inv *migrationv1alpha1.Inventory, id string) *migrationv1alpha1.Disk {
	if inv == nil {
		return nil
	}
	for i := range inv.Disks {
		if inv.Disks[i].ID == id {
			return &inv.Disks[i]
		}
	}
	return nil
}

// onlyInventoryMissing reports whether the missing inventory is the only blocker.
func onlyInventoryMissing(results []migrationv1alpha1.PreflightResult) bool {
	missing := false
	for _, r := range results {
		if r.Passed || r.Severity != migrationv1alpha1.SeverityBlocker {
			continue
		}
		if r.Name != "InventoryAvailable" {
			return false
		}
		missing = true
	}
	return missing
}

func firstBlocker(results []migrationv1alpha1.PreflightResult) *migrationv1alpha1.PreflightResult {
	for i := range results {
		if !results[i].Passed && results[i].Severity == migrationv1alpha1.SeverityBlocker {
			return &results[i]
		}
	}
	return nil
}

// terminalPhases end a migration; only archiving (and rollback from
// Completed) act on them.
var terminalPhases = map[migrationv1alpha1.MigrationPhase]bool{
	migrationv1alpha1.PhaseCompleted: true, migrationv1alpha1.PhaseRolledBack: true, migrationv1alpha1.PhaseFailed: true,
}

// activePhases are phases in which a migration holds its source.
var activePhases = map[migrationv1alpha1.MigrationPhase]bool{
	migrationv1alpha1.PhaseBooting: true, migrationv1alpha1.PhaseConnected: true,
	migrationv1alpha1.PhaseCopying: true, migrationv1alpha1.PhaseConverting: true,
	migrationv1alpha1.PhaseReadyForTest: true, migrationv1alpha1.PhaseTesting: true,
	migrationv1alpha1.PhaseCuttingOver: true, migrationv1alpha1.PhaseRollingBack: true,
}

// preflight evaluates every check; it never stops at the first failure so the
// admin sees all problems at once.
func (r *P2VMigrationReconciler) preflight(ctx context.Context, m *migrationv1alpha1.P2VMigration) (
	*migrationv1alpha1.P2VSource, []migrationv1alpha1.PreflightResult, error) {
	results := make(checks, 0, 20)
	add := results.add
	B := migrationv1alpha1.SeverityBlocker

	src, err := r.source(ctx, m)
	if err != nil {
		return nil, nil, err
	}
	add("SourceFound", src != nil, B, "P2VSource "+m.Spec.SourceRef.Name+" must exist in this namespace")
	add("ModeSupported", m.Spec.Mode != migrationv1alpha1.ModeWarm, B, "warm migration is not available yet; use cold")
	bootOK, bootMsg := bootMethodReady(m, src)
	add("BootMethodSupported", bootOK, B, bootMsg)

	var cfg migrationv1alpha1.P2VConfig
	if err := r.Get(ctx, types.NamespacedName{Name: migrationv1alpha1.P2VConfigSingletonName}, &cfg); client.IgnoreNotFound(err) != nil {
		return nil, nil, err
	}
	add("GatewayAvailable", cfg.Status.GatewayURL != "", B, "the metal2vm gateway must be available (P2VConfig \"cluster\")")
	add("KubeVirtAvailable", !r.WithoutKubeVirt, B, "OpenShift Virtualization (KubeVirt) must be installed on this cluster")

	var others migrationv1alpha1.P2VMigrationList
	if err := r.List(ctx, &others, client.InNamespace(m.Namespace)); err != nil {
		return nil, nil, err
	}
	busy := ""
	for _, o := range others.Items {
		if o.Name != m.Name && o.Spec.SourceRef.Name == m.Spec.SourceRef.Name && activePhases[o.Status.Phase] {
			busy = o.Name
		}
	}
	add("SourceNotBusy", busy == "", B, "P2VMigration "+busy+" is already migrating this source")

	var inv *migrationv1alpha1.Inventory
	if src != nil {
		inv = src.Status.Inventory
	}
	add("InventoryAvailable", inv != nil, B,
		"the source has no inventory yet: boot it from its ISO once, or run the OS pre-check")

	results = append(results, diskChecks(m, inv)...)
	results = append(results, nicChecks(m, src, inv)...)
	ipam, err := r.keepIPOnIPAMNetwork(ctx, m)
	if err != nil {
		return nil, nil, err
	}
	add("KeepIPWithoutIPAM", ipam == "", migrationv1alpha1.SeverityWarning, "network "+ipam+" assigns IP addresses itself "+
		"(IPAM/port security): a kept static IP may be blocked there; use a bridged or localnet network without IPAM to keep IPs")

	missingSC := ""
	for _, d := range m.Spec.Disks {
		if d.Action != migrationv1alpha1.DiskCopy {
			continue
		}
		sc, err := r.storageClassFor(ctx, d, &cfg)
		if err != nil {
			return nil, nil, err
		}
		if sc == "" {
			missingSC = d.SourceDiskID
		}
	}
	add("StorageClassAvailable", missingSC == "", B,
		"no usable StorageClass for disk "+missingSC+": set storageClassName, P2VConfig defaultStorageClassName, or a cluster default")
	return src, results, nil
}

// checks collects pre-flight results.
type checks []migrationv1alpha1.PreflightResult

func (c *checks) add(name string, ok bool, sev migrationv1alpha1.PreflightSeverity, msg string) {
	*c = append(*c, migrationv1alpha1.PreflightResult{Name: name, Passed: ok, Severity: sev, Message: msg})
}

// diskChecks validates the per-disk plan against the inventory.
func diskChecks(m *migrationv1alpha1.P2VMigration, inv *migrationv1alpha1.Inventory) checks {
	var results checks
	add := results.add
	B, W := migrationv1alpha1.SeverityBlocker, migrationv1alpha1.SeverityWarning
	copies, unknown, encrypted, attach := 0, "", "", ""
	for _, d := range m.Spec.Disks {
		disk := findDisk(inv, d.SourceDiskID)
		if inv != nil && disk == nil && d.Action != migrationv1alpha1.DiskSkip {
			unknown = d.SourceDiskID
		}
		if disk != nil && disk.Encrypted && d.Action != migrationv1alpha1.DiskSkip {
			encrypted = d.SourceDiskID
		}
		switch d.Action {
		case migrationv1alpha1.DiskCopy:
			copies++
		case migrationv1alpha1.DiskAttach:
			attach = d.SourceDiskID
		}
	}
	memKnown := (inv != nil && inv.MemoryMiB > 0) || (m.Spec.TargetVM.Memory != nil && !m.Spec.TargetVM.Memory.IsZero())
	add("VMSizeKnown", inv == nil || memKnown, B, "the source memory size is unknown: set targetVM.memory")
	add("DisksKnown", unknown == "", B, "disk "+unknown+" is not in the source inventory")
	add("DiskActionsSupported", attach == "", B, "re-attaching SAN LUNs (disk "+attach+") is not available yet")
	add("DisksToCopy", copies > 0, B, "at least one disk must be copied")
	add("NoEncryptedDisks", encrypted == "", W,
		"disk "+encrypted+" holds an encrypted volume; conversion will need it unlocked or its key")

	return results
}

// nicChecks validates the per-NIC plan against the inventory and OS pre-check.
func nicChecks(m *migrationv1alpha1.P2VMigration, src *migrationv1alpha1.P2VSource,
	inv *migrationv1alpha1.Inventory) checks {
	var results checks
	add := results.add
	B, W := migrationv1alpha1.SeverityBlocker, migrationv1alpha1.SeverityWarning
	unknownNIC, keepIPNoPrecheck := "", ""
	for _, n := range m.Spec.NICs {
		known := false
		if inv != nil {
			for _, nic := range inv.NICs {
				known = known || strings.EqualFold(nic.MAC, n.SourceMAC)
			}
		}
		if src != nil && findOSNIC(src.Status.OSConfig, n.SourceMAC) != nil {
			known = true
		}
		if inv != nil && !known {
			unknownNIC = n.SourceMAC
		}
		if n.KeepIP && (src == nil || findOSNIC(src.Status.OSConfig, n.SourceMAC) == nil) {
			keepIPNoPrecheck = n.SourceMAC
		}
	}
	add("NICsKnown", unknownNIC == "", B, "NIC "+unknownNIC+" is not in the source inventory")
	add("KeepIPHasOSConfig", keepIPNoPrecheck == "", W, "NIC "+keepIPNoPrecheck+": without the OS pre-check (spec.osAccess), "+
		"static IPs are kept on Linux (from the guest's own files) but not on Windows")
	return results
}

// keepIPOnIPAMNetwork returns the first network a keepIP NIC is mapped to
// that does its own IP address management, or "".
func (r *P2VMigrationReconciler) keepIPOnIPAMNetwork(ctx context.Context, m *migrationv1alpha1.P2VMigration) (string, error) {
	for _, n := range m.Spec.NICs {
		if !n.KeepIP || n.Type != migrationv1alpha1.NetworkMultus {
			continue
		}
		ns, name := m.Namespace, n.NetworkName
		if before, after, ok := strings.Cut(n.NetworkName, "/"); ok {
			ns, name = before, after
		}
		nad := &unstructured.Unstructured{}
		nad.SetGroupVersionKind(schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"})
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, nad); err != nil {
			if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
				continue
			}
			return "", err
		}
		cfg, _, _ := unstructured.NestedString(nad.Object, "spec", "config")
		if networkAssignsIPs(cfg) {
			return n.NetworkName, nil
		}
	}
	return "", nil
}

// networkAssignsIPs reports whether a CNI config allocates addresses: OVN-K
// "subnets", or an "ipam" section with a type.
func networkAssignsIPs(config string) bool {
	var c struct {
		Subnets string `json:"subnets"`
		IPAM    struct {
			Type string `json:"type"`
		} `json:"ipam"`
	}
	if json.Unmarshal([]byte(config), &c) != nil {
		return false
	}
	return c.Subnets != "" || c.IPAM.Type != ""
}

// storageClassFor resolves a disk's StorageClass: plan, then P2VConfig, then
// the cluster default. It returns "" when none exists.
func (r *P2VMigrationReconciler) storageClassFor(ctx context.Context, d migrationv1alpha1.DiskMapping,
	cfg *migrationv1alpha1.P2VConfig) (string, error) {
	for _, name := range []string{d.StorageClassName, cfg.Spec.DefaultStorageClassName} {
		if name == "" {
			continue
		}
		var sc storagev1.StorageClass
		err := r.Get(ctx, types.NamespacedName{Name: name}, &sc)
		if err == nil {
			return name, nil
		}
		if !apierrors.IsNotFound(err) {
			return "", err
		}
		return "", nil
	}
	var list storagev1.StorageClassList
	if err := r.List(ctx, &list); err != nil {
		return "", err
	}
	for _, sc := range list.Items {
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return sc.Name, nil
		}
	}
	return "", nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *P2VMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Agent connection changes on the P2VSource move migrations out of Booting.
	bySource := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var list migrationv1alpha1.P2VMigrationList
		if err := mgr.GetClient().List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for _, m := range list.Items {
			if m.Spec.SourceRef.Name == o.GetName() {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&m)})
			}
		}
		return reqs
	})
	b := ctrl.NewControllerManagedBy(mgr).
		For(&migrationv1alpha1.P2VMigration{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{})
	if !r.WithoutRoutes {
		b = b.Owns(&routev1.Route{})
	}
	if !r.WithoutKubeVirt {
		b = b.Watches(&kubevirtv1.VirtualMachine{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				if name := o.GetLabels()[vmbuilder.LabelMigration]; name != "" {
					return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: name}}}
				}
				return nil
			}))
	}
	return b.
		Watches(&migrationv1alpha1.P2VSource{}, bySource).
		Named("p2vmigration").
		Complete(r)
}
