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
	"net"
	"strconv"
	"strings"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/convert"
	"github.com/metal2vm/metal2vm/internal/names"
	"github.com/metal2vm/metal2vm/internal/netfix"
	"github.com/metal2vm/metal2vm/internal/vmbuilder"
)

// Additional condition types for conversion and cutover.
const (
	ConditionConverted        = "Converted"
	ConditionSourcePoweredOff = "SourcePoweredOff"
	ConditionVMReady          = "VMReady"
	ConditionArchived         = "Archived"

	// ConversionServiceAccount runs conversion pods; on OpenShift it is bound
	// to the metal2vm-conversion SCC (unshare seccomp profile for passt).
	ConversionServiceAccount = "metal2vm-conversion"
	// ConversionSCCRole is the ClusterRole granting "use" on that SCC.
	ConversionSCCRole = "metal2vm-conversion-scc"
	// unshareProfile is CRI-O's stock profile on OpenShift nodes.
	unshareProfile = "profiles/unshare.json"
	// annotationCutoverRestart marks a VM restarted onto production networking.
	annotationCutoverRestart = "metal2vm.io/cutover-restarted"
	// componentConvert names the conversion pod's component, container and subcommand.
	componentConvert = "convert"
	// sourceOffAfter: without heartbeats for this long, the source counts as off.
	sourceOffAfter = 60 * time.Second
)

// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch
// Needed to make VMs owners (blockOwnerDeletion) of the migrated disks' PVCs:
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=metal2vm-conversion,verbs=use

// reconcileConvert runs the conversion pod and then creates the VM (test boot)
// or moves straight to cutover.
func (r *P2VMigrationReconciler) reconcileConvert(ctx context.Context, m *migrationv1alpha1.P2VMigration) (ctrl.Result, error) {
	src, err := r.source(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if src == nil || src.Status.Inventory == nil {
		r.fail(m, "the P2VSource or its inventory disappeared")
		return ctrl.Result{}, nil
	}
	var cfg migrationv1alpha1.P2VConfig
	if err := r.Get(ctx, types.NamespacedName{Name: migrationv1alpha1.P2VConfigSingletonName}, &cfg); err != nil {
		return ctrl.Result{}, err
	}
	plan, err := r.ensurePVCs(ctx, m, src.Status.Inventory, &cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if r.OpenShift {
		if err := r.ensureConversionIdentity(ctx, m.Namespace); err != nil {
			return ctrl.Result{}, fmt.Errorf("conversion service account: %w", err)
		}
	}
	pod, err := r.ensureConvertPod(ctx, m, &cfg, src.Status.Inventory, src.Status.OSConfig, plan)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		res := convertResult(pod)
		if res == nil {
			r.fail(m, "the conversion pod did not report a result")
			return ctrl.Result{}, nil
		}
		r.recordGuest(m, plan, res)
		setCondition(m, ConditionConverted, metav1.ConditionTrue, "Converted",
			"guest converted: "+firstNonEmpty(res.OS.ProductName, res.OS.OSInfo, res.OS.Name))
		if ptr.Deref(m.Spec.TestBoot, true) {
			if err := r.ensureVM(ctx, m, src.Status.Inventory, plan, true); err != nil {
				return ctrl.Result{}, err
			}
			r.setPhase(m, migrationv1alpha1.PhaseTesting, testingMessage(m))
		} else {
			r.setPhase(m, migrationv1alpha1.PhaseCuttingOver, "powering off the source before starting the VM")
		}
		return requeueSoon, nil
	case corev1.PodFailed:
		msg := "guest conversion failed"
		if res := convertResult(pod); res != nil && res.Error != "" {
			msg += ": " + res.Error
		}
		setCondition(m, ConditionConverted, metav1.ConditionFalse, "Failed", msg)
		r.fail(m, msg)
		return ctrl.Result{}, nil
	default:
		m.Status.Message = "converting the guest (installing virtio drivers, fixing the bootloader)"
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

// networkPlan maps the planned NICs for guest conversion. A NIC keeps its IP
// configuration only on a bridged network with keepIP; everywhere else (pod
// network, new subnet) it is switched to DHCP. Interface names and static
// IPs come from the OS pre-check when available.
func networkPlan(m *migrationv1alpha1.P2VMigration, osc *migrationv1alpha1.OSConfig) (netfix.Plan, []string) {
	var p netfix.Plan
	var staticIPs []string
	// netfix edits Linux files only; Windows gets static IPs from virt-v2v.
	windows := osc != nil && osc.OS.Family == migrationv1alpha1.OSWindows
	for _, n := range m.Spec.NICs {
		src := strings.ToLower(n.SourceMAC)
		target := vmbuilder.TargetMAC(m, n)
		keepIP := n.Type == migrationv1alpha1.NetworkMultus && n.KeepIP
		nic := netfix.NIC{SourceMAC: src, TargetMAC: target, DHCP: !keepIP}
		if osNIC := findOSNIC(osc, src); osNIC != nil {
			nic.OldName = osNIC.Name
			// Windows forgets static IPs when the NIC changes; virt-v2v re-applies
			// them (ignored for Linux, whose files netfix keeps).
			if keepIP && !osNIC.DHCP {
				if arg := staticIPArg(target, osNIC); arg != "" {
					staticIPs = append(staticIPs, arg)
				}
			}
		}
		if !windows {
			p.NICs = append(p.NICs, nic)
		}
	}
	return p, staticIPs
}

func findOSNIC(osc *migrationv1alpha1.OSConfig, mac string) *migrationv1alpha1.NIC {
	if osc == nil {
		return nil
	}
	for i := range osc.NICs {
		if strings.EqualFold(osc.NICs[i].MAC, mac) {
			return &osc.NICs[i]
		}
	}
	return nil
}

// staticIPArg renders virt-v2v's --mac MAC:ip:IP,GW,LEN,DNS... for the first
// IPv4 address of nic.
func staticIPArg(mac string, nic *migrationv1alpha1.NIC) string {
	for _, cidr := range nic.IPAddresses {
		ip, ipnet, err := net.ParseCIDR(cidr)
		if err != nil || ip.To4() == nil {
			continue
		}
		ones, _ := ipnet.Mask.Size()
		parts := make([]string, 0, 3+len(nic.DNSServers))
		parts = append(parts, ip.String(), nic.Gateway, strconv.Itoa(ones))
		parts = append(parts, nic.DNSServers...)
		return mac + ":ip:" + strings.Join(parts, ",")
	}
	return ""
}

func testingMessage(m *migrationv1alpha1.P2VMigration) string {
	return fmt.Sprintf("VM %s is running WITHOUT network for testing. Open its console to check it, "+
		"then cut over (set spec.cutover) or roll back (set spec.rollback).", vmbuilder.Name(m))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (r *P2VMigrationReconciler) recordGuest(m *migrationv1alpha1.P2VMigration, plan copyPlan, res *convert.Result) {
	family := migrationv1alpha1.OSUnknown
	switch res.OS.Name {
	case "linux":
		family = migrationv1alpha1.OSLinux
	case "windows":
		family = migrationv1alpha1.OSWindows
	}
	m.Status.Guest = &migrationv1alpha1.GuestInfo{
		Family: family, Distro: res.OS.Distro, OSInfo: res.OS.OSInfo,
		ProductName: res.OS.ProductName, Firmware: migrationv1alpha1.FirmwareType(res.Firmware),
		NetworkNotes: res.NetworkNotes,
	}
	boot := map[string]int32{}
	for _, d := range res.Disks {
		if d.Index < len(plan.Disks) {
			boot[plan.Disks[d.Index].DiskID] = int32(d.BootOrder)
		}
	}
	for i := range m.Status.Disks {
		m.Status.Disks[i].BootOrder = boot[m.Status.Disks[i].SourceDiskID]
	}
}

// ensureConversionIdentity creates the namespace's conversion ServiceAccount
// and binds it to the conversion SCC.
func (r *P2VMigrationReconciler) ensureConversionIdentity(ctx context.Context, ns string) error {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: ConversionServiceAccount, Namespace: ns,
		Labels: map[string]string{LabelPartOf: PartOfValue}}}
	if err := r.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: ConversionServiceAccount, Namespace: ns, Labels: map[string]string{LabelPartOf: PartOfValue}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: ConversionSCCRole},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: ConversionServiceAccount, Namespace: ns}},
	}
	if err := r.Create(ctx, rb); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *P2VMigrationReconciler) ensureConvertPod(ctx context.Context, m *migrationv1alpha1.P2VMigration,
	cfg *migrationv1alpha1.P2VConfig, inv *migrationv1alpha1.Inventory, osConfig *migrationv1alpha1.OSConfig,
	plan copyPlan) (*corev1.Pod, error) {
	name := names.Short(m.Name, "-convert")
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, &pod)
	if err == nil || !apierrors.IsNotFound(err) {
		return &pod, err
	}

	cp := convert.Plan{
		Name:      vmbuilder.Name(m),
		MemoryMiB: inv.MemoryMiB,
		VCPUs:     max(inv.CPU.Sockets, 1) * max(inv.CPU.CoresPerSocket, 1) * max(inv.CPU.ThreadsPerCore, 1),
		Firmware:  string(inv.Firmware.Type),
	}
	cp.Network, cp.StaticIPs = networkPlan(m, osConfig)
	var volumes []corev1.Volume
	var devices []corev1.VolumeDevice
	var mounts []corev1.VolumeMount
	for i, d := range plan.Disks {
		cp.Disks = append(cp.Disks, convert.Disk{Path: d.Target, File: d.File})
		vol := fmt.Sprintf("disk%d", i)
		volumes = append(volumes, corev1.Volume{Name: vol, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: d.PVC},
		}})
		if d.Mode == corev1.PersistentVolumeBlock {
			devices = append(devices, corev1.VolumeDevice{Name: vol, DevicePath: d.Target})
		} else {
			mounts = append(mounts, corev1.VolumeMount{Name: vol, MountPath: dirOf(d.Target)})
		}
	}
	planJSON, err := json.Marshal(cp)
	if err != nil {
		return nil, err
	}
	// libguestfs needs scratch space for its appliance and overlays.
	for _, dir := range []string{"/var/tmp", "/tmp"} {
		vol := "scratch" + map[string]string{"/var/tmp": "-vartmp", "/tmp": "-tmp"}[dir]
		volumes = append(volumes, corev1.Volume{Name: vol, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resource.MustParse("10Gi"))},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: vol, MountPath: dir})
	}

	seccomp := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	sa := ""
	if r.OpenShift {
		seccomp = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeLocalhost, LocalhostProfile: new(unshareProfile)}
		sa = ConversionServiceAccount
	}
	pod = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: m.Namespace,
			Labels: map[string]string{
				names.LabelMigration: names.Short(m.Name, ""), labelComponent: componentConvert, LabelPartOf: PartOfValue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ServiceAccountName:           sa,
			AutomountServiceAccountToken: new(false),
			SecurityContext:              &corev1.PodSecurityContext{RunAsNonRoot: new(true), SeccompProfile: seccomp},
			Containers: []corev1.Container{{
				Name:  componentConvert,
				Image: r.transferImage(cfg),
				Args:  []string{componentConvert},
				Env:   []corev1.EnvVar{{Name: "CONVERT_PLAN", Value: string(planJSON)}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("3Gi"),
						"devices.kubevirt.io/kvm": resource.MustParse("1"),
					},
				},
				VolumeDevices: devices,
				VolumeMounts:  mounts,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: new(false),
					ReadOnlyRootFilesystem:   new(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capabilityAll}},
				},
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			}},
			Volumes: volumes,
		},
	}
	if err := ctrl.SetControllerReference(m, &pod, r.Scheme); err != nil {
		return nil, err
	}
	return &pod, r.Create(ctx, &pod)
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return p
}

func convertResult(pod *corev1.Pod) *convert.Result {
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.Message != "" {
			var r convert.Result
			if json.Unmarshal([]byte(t.Message), &r) == nil {
				return &r
			}
		}
	}
	return nil
}

// guestResult rebuilds the conversion result needed by the VM builder from status.
func guestResult(m *migrationv1alpha1.P2VMigration, plan copyPlan) convert.Result {
	res := convert.Result{}
	if g := m.Status.Guest; g != nil {
		res.Firmware = string(g.Firmware)
		res.OS = convert.OS{Name: map[migrationv1alpha1.OSFamily]string{
			migrationv1alpha1.OSLinux: "linux", migrationv1alpha1.OSWindows: "windows"}[g.Family],
			Distro: g.Distro, OSInfo: g.OSInfo, ProductName: g.ProductName}
	}
	boot := map[string]int32{}
	for _, d := range m.Status.Disks {
		boot[d.SourceDiskID] = d.BootOrder
	}
	for i, d := range plan.Disks {
		res.Disks = append(res.Disks, convert.DiskInfo{Index: i, BootOrder: int(boot[d.DiskID])})
	}
	return res
}

// ensureVM creates the VM if missing and hands the disk PVCs over to it, so
// deleting the migration never deletes a migrated server's disks.
func (r *P2VMigrationReconciler) ensureVM(ctx context.Context, m *migrationv1alpha1.P2VMigration,
	inv *migrationv1alpha1.Inventory, plan copyPlan, testBoot bool) error {
	vols := make([]vmbuilder.Volume, len(plan.Disks))
	for i, d := range plan.Disks {
		vols[i] = vmbuilder.Volume{PVC: d.PVC, Index: i}
	}
	desired, err := vmbuilder.Build(vmbuilder.Input{
		Migration: m, Inventory: inv, Guest: guestResult(m, plan), Volumes: vols, TestBoot: testBoot,
	})
	if err != nil {
		return err
	}
	var vm kubevirtv1.VirtualMachine
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), &vm)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("creating VM: %w", err)
		}
		vm = *desired
	case err != nil:
		return err
	case vm.Labels[vmbuilder.LabelMigration] != m.Name:
		return fmt.Errorf("a VirtualMachine named %s already exists and was not created by this migration", vm.Name)
	}
	m.Status.VMName = vm.Name
	for _, d := range plan.Disks {
		if err := r.adoptPVC(ctx, m, &vm, d.PVC); err != nil {
			return fmt.Errorf("handing PVC %s to the VM: %w", d.PVC, err)
		}
	}
	return nil
}

func (r *P2VMigrationReconciler) adoptPVC(ctx context.Context, m *migrationv1alpha1.P2VMigration,
	vm *kubevirtv1.VirtualMachine, name string) error {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, &pvc); err != nil {
		return err
	}
	patch := client.MergeFrom(pvc.DeepCopy())
	owners := []metav1.OwnerReference{{
		APIVersion: kubevirtv1.SchemeGroupVersion.String(), Kind: "VirtualMachine",
		Name: vm.Name, UID: vm.UID, Controller: new(true), BlockOwnerDeletion: new(true),
	}}
	for _, o := range pvc.OwnerReferences {
		if o.UID != m.UID && o.UID != vm.UID {
			owners = append(owners, o)
		}
	}
	pvc.OwnerReferences = owners
	return r.Patch(ctx, &pvc, patch)
}

// reconcileTesting waits for the admin's cutover or rollback decision.
func (r *P2VMigrationReconciler) reconcileTesting(ctx context.Context, m *migrationv1alpha1.P2VMigration) (ctrl.Result, error) {
	r.observeVM(ctx, m)
	if m.Spec.Cutover != nil {
		if wait := m.Spec.Cutover.Sub(r.now()); wait > 0 {
			m.Status.Message = "cutover scheduled for " + m.Spec.Cutover.UTC().Format(time.RFC3339)
			return ctrl.Result{RequeueAfter: wait}, nil
		}
		r.setPhase(m, migrationv1alpha1.PhaseCuttingOver, "powering off the source")
		return requeueSoon, nil
	}
	m.Status.Message = testingMessage(m)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// reconcileCutover powers off the source (if allowed), then starts the VM on
// its production networks.
func (r *P2VMigrationReconciler) reconcileCutover(ctx context.Context, m *migrationv1alpha1.P2VMigration) (ctrl.Result, error) {
	src, err := r.source(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if src == nil || src.Status.Inventory == nil {
		r.fail(m, "the P2VSource or its inventory disappeared")
		return ctrl.Result{}, nil
	}

	if ptr.Deref(m.Spec.AutoShutdownSource, true) {
		off, how, err := r.sourceOff(ctx, m, src)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !off {
			m.Status.Message = "waiting for the source server to power off"
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		setCondition(m, ConditionSourcePoweredOff, metav1.ConditionTrue, "PoweredOff", "the source server is off ("+how+")")
	} else {
		setCondition(m, ConditionSourcePoweredOff, metav1.ConditionUnknown, "NotManaged",
			"autoShutdownSource is false: make sure the source is off to avoid duplicate IPs")
	}

	var cfg migrationv1alpha1.P2VConfig
	if err := r.Get(ctx, types.NamespacedName{Name: migrationv1alpha1.P2VConfigSingletonName}, &cfg); err != nil {
		return ctrl.Result{}, err
	}
	plan, err := r.ensurePVCs(ctx, m, src.Status.Inventory, &cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureVM(ctx, m, src.Status.Inventory, plan, false); err != nil {
		return ctrl.Result{}, err
	}
	done, err := r.ensureProductionNetworking(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		m.Status.Message = "restarting the VM on its production networks"
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	// The VM's Ready flag may still describe the test instance; only an
	// instance started after the network switch counts.
	ready, err := r.restartedInstanceReady(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.observeVM(ctx, m)
	if !ready {
		m.Status.Message = "waiting for the VM to start on its production networks"
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	now := metav1.NewTime(r.now())
	m.Status.CompletedAt = &now
	setCondition(m, "Ready", metav1.ConditionTrue, "Completed", "migration completed")
	r.setPhase(m, migrationv1alpha1.PhaseCompleted, fmt.Sprintf("VM %s is running on its production networks. "+
		"Archive the migration to clean up; until then it can be rolled back.", m.Status.VMName))
	return ctrl.Result{}, nil
}

// forceOffAfter: with a BMC, a source still on this long after the power-off
// request is powered off hard.
const forceOffAfter = 2 * time.Minute

// sourceOff asks the agent to power the source off (through the gateway),
// and with a BMC confirms it, forces it after forceOffAfter and ejects the
// live media so a later power-on boots the server's own disks.
func (r *P2VMigrationReconciler) sourceOff(ctx context.Context, m *migrationv1alpha1.P2VMigration,
	src *migrationv1alpha1.P2VSource) (bool, string, error) {
	cond := apimeta.FindStatusCondition(m.Status.Conditions, ConditionSourcePoweredOff)
	if cond == nil || cond.Reason != "Requested" {
		setCondition(m, ConditionSourcePoweredOff, metav1.ConditionFalse, "Requested", "asked the live agent to power off the source")
		cond = apimeta.FindStatusCondition(m.Status.Conditions, ConditionSourcePoweredOff)
	}
	if src.Spec.BMC == nil {
		return !sourceStillUp(src, r.now()), "no heartbeat from the live agent", nil
	}
	c, err := connectBMC(ctx, r.APIReader, r.BMC, src)
	if err != nil {
		return false, "", err
	}
	defer c.Close()
	on, err := c.PowerOn()
	if err != nil {
		return false, "", err
	}
	how := "confirmed by the BMC"
	if on {
		if r.now().Sub(cond.LastTransitionTime.Time) < forceOffAfter {
			return false, "", nil
		}
		if err := c.ForceOff(); err != nil {
			return false, "", err
		}
		how = "forced off through the BMC after the OS did not shut down"
	}
	if err := c.EjectAll(); err != nil {
		return false, "", fmt.Errorf("ejecting the live media: %w", err)
	}
	return true, how, nil
}

// sourceStillUp reports whether the live agent is still heartbeating.
func sourceStillUp(src *migrationv1alpha1.P2VSource, now time.Time) bool {
	a := src.Status.Agent
	return a != nil && a.Connected && a.LastHeartbeat != nil && now.Sub(a.LastHeartbeat.Time) < sourceOffAfter
}

// ensureProductionNetworking switches a test-boot VM to the planned NICs and
// restarts it once. It returns true when the VM runs with production networking.
func (r *P2VMigrationReconciler) ensureProductionNetworking(ctx context.Context, m *migrationv1alpha1.P2VMigration) (bool, error) {
	var vm kubevirtv1.VirtualMachine
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: m.Status.VMName}, &vm); err != nil {
		return false, err
	}
	ifaces, nets := vmbuilder.Networking(m)
	if vm.Annotations[annotationCutoverRestart] != "" {
		return true, nil
	}
	patch := client.MergeFrom(vm.DeepCopy())
	spec := &vm.Spec.Template.Spec
	spec.Domain.Devices.Interfaces, spec.Networks = ifaces, nets
	spec.Domain.Devices.AutoattachPodInterface = nil
	if len(nets) == 0 {
		spec.Domain.Devices.AutoattachPodInterface = new(false)
	}
	vm.Spec.RunStrategy = new(kubevirtv1.RunStrategyAlways)
	if vm.Annotations == nil {
		vm.Annotations = map[string]string{}
	}
	vm.Annotations[annotationCutoverRestart] = r.now().UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, &vm, patch); err != nil {
		return false, err
	}
	// Restart onto the new template: with runStrategy Always, KubeVirt
	// recreates the deleted instance from the updated spec.
	vmi := &kubevirtv1.VirtualMachineInstance{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: vm.Name}}
	if err := r.Delete(ctx, vmi); client.IgnoreNotFound(err) != nil {
		return false, err
	}
	return false, nil
}

// restartedInstanceReady reports whether the VM instance was created after the
// cutover restart and is ready.
func (r *P2VMigrationReconciler) restartedInstanceReady(ctx context.Context, m *migrationv1alpha1.P2VMigration) (bool, error) {
	var vm kubevirtv1.VirtualMachine
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: m.Status.VMName}, &vm); err != nil {
		return false, err
	}
	restarted, err := time.Parse(time.RFC3339, vm.Annotations[annotationCutoverRestart])
	if err != nil {
		return false, nil
	}
	var vmi kubevirtv1.VirtualMachineInstance
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: vm.Name}, &vmi); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if vmi.DeletionTimestamp != nil || vmi.CreationTimestamp.Time.Before(restarted) || vmi.Status.Phase != kubevirtv1.Running {
		return false, nil
	}
	for _, c := range vmi.Status.Conditions {
		if c.Type == kubevirtv1.VirtualMachineInstanceReady {
			return c.Status == corev1.ConditionTrue, nil
		}
	}
	return false, nil
}

// observeVM mirrors the VM's readiness into a condition.
func (r *P2VMigrationReconciler) observeVM(ctx context.Context, m *migrationv1alpha1.P2VMigration) {
	if m.Status.VMName == "" {
		return
	}
	var vm kubevirtv1.VirtualMachine
	if err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: m.Status.VMName}, &vm); err != nil {
		return
	}
	if vm.Status.Ready {
		setCondition(m, ConditionVMReady, metav1.ConditionTrue, "Ready", "the VM is running")
	} else {
		// PrintableStatus is empty until KubeVirt has looked at the VM; a
		// condition reason must not be empty.
		reason := string(vm.Status.PrintableStatus)
		if reason == "" {
			reason = "NotReady"
		}
		setCondition(m, ConditionVMReady, metav1.ConditionFalse, reason, "the VM is not ready: "+reason)
	}
}

// rollbackPhases are phases from which spec.rollback is honoured.
var rollbackPhases = map[migrationv1alpha1.MigrationPhase]bool{
	migrationv1alpha1.PhaseTesting: true, migrationv1alpha1.PhaseCuttingOver: true, migrationv1alpha1.PhaseCompleted: true,
}

// reconcileRollback stops the VM. The source disks were never modified, so
// the server can simply be booted from them again.
func (r *P2VMigrationReconciler) reconcileRollback(ctx context.Context, m *migrationv1alpha1.P2VMigration) (ctrl.Result, error) {
	if m.Status.VMName != "" {
		var vm kubevirtv1.VirtualMachine
		err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: m.Status.VMName}, &vm)
		if err == nil && ptr.Deref(vm.Spec.RunStrategy, "") != kubevirtv1.RunStrategyHalted {
			patch := client.MergeFrom(vm.DeepCopy())
			vm.Spec.RunStrategy = new(kubevirtv1.RunStrategyHalted)
			if err := r.Patch(ctx, &vm, patch); err != nil {
				return ctrl.Result{}, err
			}
		} else if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
	}
	msg := fmt.Sprintf("VM %s is stopped. Power the source server on and boot it from its own disks "+
		"(remove the metal2vm boot media); its disks were never modified.", m.Status.VMName)
	src, err := r.source(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if src != nil && src.Spec.BMC != nil {
		c, err := connectBMC(ctx, r.APIReader, r.BMC, src)
		if err != nil {
			return ctrl.Result{}, err
		}
		defer c.Close()
		// Decide from the BMC, not the (possibly stale) cache, so a repeated
		// rollback never restarts a server that already runs its own OS:
		//   off                -> boot from disk
		//   on, live ISO in    -> still in the live environment: eject and reboot from disk
		//   on, no live ISO    -> already back on its own OS: leave it alone
		on, err := c.PowerOn()
		if err != nil {
			return ctrl.Result{}, err
		}
		_, inserted, err := c.InsertedMedia()
		if err != nil {
			inserted = false // no virtual media device: nothing can be inserted
		}
		if !on || inserted {
			if err := c.BootFromDisk(); err != nil {
				return ctrl.Result{}, err
			}
		}
		msg = fmt.Sprintf("VM %s is stopped and %s is booting from its own disks through its BMC; "+
			"its disks were never modified.", m.Status.VMName, src.Name)
	}
	now := metav1.NewTime(r.now())
	m.Status.CompletedAt = &now
	setCondition(m, "Ready", metav1.ConditionFalse, "RolledBack", "migration rolled back")
	r.setPhase(m, migrationv1alpha1.PhaseRolledBack, msg)
	return ctrl.Result{}, nil
}

// reconcileArchive removes temporary resources once the migration is finished.
func (r *P2VMigrationReconciler) reconcileArchive(ctx context.Context, m *migrationv1alpha1.P2VMigration) error {
	key := func(n string) types.NamespacedName { return types.NamespacedName{Namespace: m.Namespace, Name: n} }
	for _, obj := range []client.Object{
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.Transfer(m.Name)}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: names.Short(m.Name, "-convert")}},
		&routev1.Route{ObjectMeta: metav1.ObjectMeta{Name: names.Transfer(m.Name)}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: names.Transfer(m.Name)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.Transfer(m.Name)}},
	} {
		obj.SetNamespace(m.Namespace)
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil && !apimeta.IsNoMatchError(err) {
			return fmt.Errorf("deleting %s: %w", key(obj.GetName()), err)
		}
	}
	setCondition(m, ConditionArchived, metav1.ConditionTrue, "Archived", "temporary migration resources were removed")
	return nil
}
