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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/gateway"
	"github.com/metal2vm/metal2vm/internal/names"
	"github.com/metal2vm/metal2vm/internal/pki"
	"github.com/metal2vm/metal2vm/internal/protocol"
	"github.com/metal2vm/metal2vm/internal/tokens"
	"github.com/metal2vm/metal2vm/internal/transfer"
)

const (
	labelComponent     = "metal2vm.io/component"
	componentTransfer  = "transfer"
	tunnelPortName     = "tunnel"
	transferTunnelPort = 8443
	transferStatusPort = 8081
	// fsOverhead reserves filesystem space when a disk lands as disk.img on a
	// Filesystem-mode PVC (same default as CDI).
	fsOverhead = 0.06
	// AnnotationSourceDisk records which source disk a PVC holds.
	AnnotationSourceDisk = "metal2vm.io/source-disk-id"
)

var storageProfileGVK = schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "StorageProfile"}

// volumeSpec is how a disk's PVC is provisioned.
type volumeSpec struct {
	mode   corev1.PersistentVolumeMode
	access corev1.PersistentVolumeAccessMode
}

// pickVolumeSpec prefers RWX block (live-migratable VMs), then RWO block, then
// filesystem modes, from CDI's StorageProfile for the StorageClass.
func (r *P2VMigrationReconciler) pickVolumeSpec(ctx context.Context, sc string) (volumeSpec, error) {
	def := volumeSpec{mode: corev1.PersistentVolumeBlock, access: corev1.ReadWriteOnce}
	sp := &unstructured.Unstructured{}
	sp.SetGroupVersionKind(storageProfileGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: sc}, sp); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return def, nil
		}
		return def, err
	}
	sets, _, _ := unstructured.NestedSlice(sp.Object, "status", "claimPropertySets")
	have := map[volumeSpec]bool{}
	for _, s := range sets {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		mode, _ := m["volumeMode"].(string)
		modes, _ := m["accessModes"].([]any)
		for _, a := range modes {
			if as, ok := a.(string); ok {
				have[volumeSpec{corev1.PersistentVolumeMode(mode), corev1.PersistentVolumeAccessMode(as)}] = true
			}
		}
	}
	for _, want := range []volumeSpec{
		{corev1.PersistentVolumeBlock, corev1.ReadWriteMany},
		{corev1.PersistentVolumeBlock, corev1.ReadWriteOnce},
		{corev1.PersistentVolumeFilesystem, corev1.ReadWriteMany},
		{corev1.PersistentVolumeFilesystem, corev1.ReadWriteOnce},
	} {
		if have[want] {
			return want, nil
		}
	}
	return def, nil
}

// pvcSize is the PVC request for a disk: whole MiB, plus overhead on filesystems.
func pvcSize(sizeBytes int64, mode corev1.PersistentVolumeMode) resource.Quantity {
	const mib = 1 << 20
	size := (sizeBytes + mib - 1) / mib * mib
	if mode == corev1.PersistentVolumeFilesystem {
		size = int64(float64(size)/(1-fsOverhead)+mib-1) / mib * mib
	}
	return *resource.NewQuantity(size, resource.BinarySI)
}

// ingressDomain returns the cluster's apps domain. Outside OpenShift (or in
// tests) it falls back to the gateway Route host minus its first label.
func (r *P2VMigrationReconciler) ingressDomain(ctx context.Context, gatewayURL string) (string, error) {
	var ing configv1.Ingress
	err := r.Get(ctx, types.NamespacedName{Name: "cluster"}, &ing)
	if err == nil && ing.Spec.Domain != "" {
		return ing.Spec.Domain, nil
	}
	if err != nil && !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
		return "", err
	}
	host := strings.TrimPrefix(gatewayURL, "https://")
	if _, domain, ok := strings.Cut(host, "."); ok {
		return domain, nil
	}
	return "", errors.New("cannot determine the ingress domain")
}

// reconcileCopy creates the transfer resources and tracks progress.
func (r *P2VMigrationReconciler) reconcileCopy(ctx context.Context, m *migrationv1alpha1.P2VMigration) (ctrl.Result, error) {
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
		return ctrl.Result{}, fmt.Errorf("disk PVCs: %w", err)
	}
	if err := r.ensureTunnel(ctx, m, &cfg, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("tunnel: %w", err)
	}
	pod, err := r.ensureTransferPod(ctx, m, &cfg, plan)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("transfer pod: %w", err)
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		st := terminationStatus(pod)
		r.applyTransferStatus(m, plan, st, true)
		setCondition(m, ConditionCopied, metav1.ConditionTrue, "Copied", fmt.Sprintf("%d disk(s) copied", len(plan.Disks)))
		r.setPhase(m, migrationv1alpha1.PhaseConverting, "disks copied")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	case corev1.PodFailed:
		st := terminationStatus(pod)
		r.applyTransferStatus(m, plan, st, false)
		msg := "the transfer pod failed"
		if st != nil && st.Error != "" {
			msg = "disk copy failed: " + st.Error
		}
		setCondition(m, ConditionCopied, metav1.ConditionFalse, "Failed", msg)
		r.fail(m, msg)
		return ctrl.Result{}, nil
	case corev1.PodRunning:
		if st := r.pollTransfer(ctx, pod); st != nil {
			r.applyTransferStatus(m, plan, st, false)
			m.Status.Message = copyMessage(st)
		}
	default:
		r.applyTransferStatus(m, plan, nil, false)
		m.Status.Message = "starting the transfer pod"
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func copyMessage(st *transfer.Status) string {
	var total, copied int64
	waiting := true
	for _, d := range st.Disks {
		total += d.BytesTotal
		copied += d.BytesCopied
		if d.Phase != transfer.DiskWaiting {
			waiting = false
		}
	}
	if waiting {
		return "waiting for the live agent to connect to the transfer tunnel"
	}
	pct := 0
	if total > 0 {
		pct = int(copied * 100 / total)
	}
	return fmt.Sprintf("copying disks: %d%% (%s of %s)", pct, humanBytes(copied), humanBytes(total))
}

// humanBytes formats a size for messages, e.g. "12.3 GiB".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// diskPlanEntry is one copied disk with its PVC.
type diskPlanEntry struct {
	transfer.DiskPlan
	PVC  string
	Mode corev1.PersistentVolumeMode
}

type copyPlan struct {
	Disks []diskPlanEntry
}

func (r *P2VMigrationReconciler) ensurePVCs(ctx context.Context, m *migrationv1alpha1.P2VMigration,
	inv *migrationv1alpha1.Inventory, cfg *migrationv1alpha1.P2VConfig) (copyPlan, error) {
	var plan copyPlan
	idx := 0
	for _, d := range m.Spec.Disks {
		if d.Action != migrationv1alpha1.DiskCopy {
			continue
		}
		disk := findDisk(inv, d.SourceDiskID)
		if disk == nil {
			return plan, fmt.Errorf("disk %s not in inventory", d.SourceDiskID)
		}
		sc, err := r.storageClassFor(ctx, d, cfg)
		if err != nil {
			return plan, err
		}
		if sc == "" {
			return plan, fmt.Errorf("no StorageClass for disk %s", d.SourceDiskID)
		}
		name := names.DiskPVC(m.Name, idx)
		var pvc corev1.PersistentVolumeClaim
		err = r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, &pvc)
		switch {
		case apierrors.IsNotFound(err):
			vs, err := r.pickVolumeSpec(ctx, sc)
			if err != nil {
				return plan, err
			}
			pvc = corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: m.Namespace,
					Labels:      map[string]string{names.LabelMigration: names.Short(m.Name, ""), LabelPartOf: PartOfValue},
					Annotations: map[string]string{AnnotationSourceDisk: d.SourceDiskID},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: &sc,
					VolumeMode:       &vs.mode,
					AccessModes:      []corev1.PersistentVolumeAccessMode{vs.access},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: pvcSize(disk.SizeBytes, vs.mode)},
					},
				},
			}
			// Owned by the migration until the VM takes them over (M3).
			if err := ctrl.SetControllerReference(m, &pvc, r.Scheme); err != nil {
				return plan, err
			}
			if err := r.Create(ctx, &pvc); err != nil {
				return plan, err
			}
		case err != nil:
			return plan, err
		}
		mode := corev1.PersistentVolumeBlock
		if pvc.Spec.VolumeMode != nil {
			mode = *pvc.Spec.VolumeMode
		}
		entry := diskPlanEntry{PVC: name, Mode: mode, DiskPlan: transfer.DiskPlan{
			DiskID: d.SourceDiskID, Port: protocol.FirstExportPort + idx, SizeBytes: disk.SizeBytes,
		}}
		if mode == corev1.PersistentVolumeBlock {
			entry.Target = fmt.Sprintf("/dev/m2v/disk%d", idx)
		} else {
			entry.Target = fmt.Sprintf("/m2v/disk%d/disk.img", idx)
			entry.File = true
		}
		plan.Disks = append(plan.Disks, entry)
		idx++
	}
	return plan, nil
}

// ensureTunnel creates the transfer Secret (TLS, chisel auth), Service and Route.
func (r *P2VMigrationReconciler) ensureTunnel(ctx context.Context, m *migrationv1alpha1.P2VMigration,
	cfg *migrationv1alpha1.P2VConfig, plan copyPlan) error {
	name := names.Transfer(m.Name)
	domain, err := r.ingressDomain(ctx, cfg.Status.GatewayURL)
	if err != nil {
		return err
	}
	host := names.TunnelHost(m.Namespace, m.Name, domain)
	labels := map[string]string{
		names.LabelMigration: names.Short(m.Name, ""), LabelPartOf: PartOfValue,
		tokens.LabelManagedBy: tokens.ManagedByValue,
	}

	var caSec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: gateway.CASecretName}, &caSec); err != nil {
		return fmt.Errorf("gateway CA: %w", err)
	}
	ca := pki.KeyPair{CertPEM: caSec.Data[gateway.CACertKey], KeyPEM: caSec.Data[corev1.TLSPrivateKeyKey]}

	exports := make([]protocol.Export, len(plan.Disks))
	allowed := make([]string, len(plan.Disks))
	for i, d := range plan.Disks {
		exports[i] = protocol.Export{DiskID: d.DiskID, Port: d.Port}
		allowed[i] = fmt.Sprintf("^R:127\\.0\\.0\\.1:%d$", d.Port)
	}
	exportsJSON, _ := json.Marshal(exports)

	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = labels
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		if len(sec.Data[names.TransferKeyAuth]) == 0 {
			sec.Data[names.TransferKeyAuth] = []byte("agent:" + tokens.Generate())
		}
		auth := string(sec.Data[names.TransferKeyAuth])
		// The agent may only open its reverse forwards: no forward tunnels into
		// the cluster network, no SOCKS.
		users, _ := json.Marshal(map[string][]string{auth: allowed})
		sec.Data[names.TransferKeyUsers] = users
		if pki.NeedsRenewal(sec.Data[names.TransferKeyCert], host, r.now()) {
			kp, err := pki.NewServing(ca, []string{host}, r.now())
			if err != nil {
				return err
			}
			sec.Data[names.TransferKeyCert], sec.Data[names.TransferKeyKey] = kp.CertPEM, kp.KeyPEM
		}
		sec.Data[names.TransferKeyTunnelURL] = []byte("https://" + host)
		sec.Data[names.TransferKeyExports] = exportsJSON
		return ctrl.SetControllerReference(m, sec, r.Scheme)
	}); err != nil {
		return err
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = map[string]string{names.LabelMigration: names.Short(m.Name, ""), labelComponent: componentTransfer}
		svc.Spec.Ports = []corev1.ServicePort{{Name: tunnelPortName, Port: 443, TargetPort: intstr.FromInt32(transferTunnelPort)}}
		return ctrl.SetControllerReference(m, svc, r.Scheme)
	}); err != nil {
		return err
	}

	route := &routev1.Route{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		route.Labels = labels
		route.Spec.Host = host
		route.Spec.To = routev1.RouteTargetReference{Kind: kindService, Name: name}
		route.Spec.Port = &routev1.RoutePort{TargetPort: intstr.FromString(tunnelPortName)}
		route.Spec.TLS = &routev1.TLSConfig{
			Termination:                   routev1.TLSTerminationPassthrough,
			InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyNone,
		}
		return ctrl.SetControllerReference(m, route, r.Scheme)
	})
	return err
}

func (r *P2VMigrationReconciler) transferImage(cfg *migrationv1alpha1.P2VConfig) string {
	if cfg.Spec.Images.Transfer != "" {
		return cfg.Spec.Images.Transfer
	}
	return r.TransferImage
}

func (r *P2VMigrationReconciler) ensureTransferPod(ctx context.Context, m *migrationv1alpha1.P2VMigration,
	cfg *migrationv1alpha1.P2VConfig, plan copyPlan) (*corev1.Pod, error) {
	name := names.Transfer(m.Name)
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, &pod)
	if err == nil || !apierrors.IsNotFound(err) {
		return &pod, err
	}

	tp := transfer.Plan{}
	var volumes []corev1.Volume
	var devices []corev1.VolumeDevice
	var mounts []corev1.VolumeMount
	for i, d := range plan.Disks {
		tp.Disks = append(tp.Disks, d.DiskPlan)
		vol := fmt.Sprintf("disk%d", i)
		volumes = append(volumes, corev1.Volume{Name: vol, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: d.PVC},
		}})
		if d.Mode == corev1.PersistentVolumeBlock {
			devices = append(devices, corev1.VolumeDevice{Name: vol, DevicePath: d.Target})
		} else {
			mounts = append(mounts, corev1.VolumeMount{Name: vol, MountPath: strings.TrimSuffix(d.Target, "/disk.img")})
		}
	}
	planJSON, err := json.Marshal(tp)
	if err != nil {
		return nil, err
	}
	volumes = append(volumes, corev1.Volume{Name: tunnelPortName, VolumeSource: corev1.VolumeSource{
		Secret: &corev1.SecretVolumeSource{SecretName: name},
	}})
	mounts = append(mounts, corev1.VolumeMount{Name: tunnelPortName, MountPath: "/etc/metal2vm-transfer", ReadOnly: true})

	no, yes := false, true
	pod = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: m.Namespace,
			Labels: map[string]string{
				names.LabelMigration: names.Short(m.Name, ""), labelComponent: componentTransfer,
				LabelPartOf: PartOfValue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &no,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &yes,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  componentTransfer,
				Image: r.transferImage(cfg),
				Args:  []string{"--tunnel-port=8443", "--status-address=:8081"},
				Env:   []corev1.EnvVar{{Name: "TRANSFER_PLAN", Value: string(planJSON)}},
				Ports: []corev1.ContainerPort{
					{Name: tunnelPortName, ContainerPort: transferTunnelPort},
					{Name: "status", ContainerPort: transferStatusPort},
				},
				VolumeDevices: devices,
				VolumeMounts:  mounts,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &yes,
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

func (r *P2VMigrationReconciler) pollTransfer(ctx context.Context, pod *corev1.Pod) *transfer.Status {
	if pod.Status.PodIP == "" {
		return nil
	}
	hc := r.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://%s:%d/status", pod.Status.PodIP, transferStatusPort), nil)
	if err != nil {
		return nil
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var st transfer.Status
	if json.NewDecoder(resp.Body).Decode(&st) != nil {
		return nil
	}
	return &st
}

func terminationStatus(pod *corev1.Pod) *transfer.Status {
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.Message != "" {
			var st transfer.Status
			if json.Unmarshal([]byte(t.Message), &st) == nil {
				return &st
			}
		}
	}
	return nil
}

var transferPhaseToDisk = map[string]string{
	transfer.DiskWaiting: "Pending", transfer.DiskCopying: "Copying",
	transfer.DiskCopied: "Copied", transfer.DiskFailed: "Failed",
}

// applyTransferStatus mirrors transfer progress into status.disks.
func (r *P2VMigrationReconciler) applyTransferStatus(m *migrationv1alpha1.P2VMigration, plan copyPlan, st *transfer.Status, done bool) {
	byID := map[string]transfer.DiskStatus{}
	if st != nil {
		for _, d := range st.Disks {
			byID[d.DiskID] = d
		}
	}
	pvcs := map[string]diskPlanEntry{}
	for _, d := range plan.Disks {
		pvcs[d.DiskID] = d
	}
	out := make([]migrationv1alpha1.DiskStatus, 0, len(m.Spec.Disks))
	for _, d := range m.Spec.Disks {
		ds := migrationv1alpha1.DiskStatus{SourceDiskID: d.SourceDiskID}
		switch d.Action {
		case migrationv1alpha1.DiskSkip:
			ds.Phase = "Skipped"
		case migrationv1alpha1.DiskCopy:
			p := pvcs[d.SourceDiskID]
			ds.PVCName, ds.BytesTotal, ds.Phase = p.PVC, p.SizeBytes, "Pending"
			if t, ok := byID[d.SourceDiskID]; ok {
				ds.BytesCopied, ds.Phase = t.BytesCopied, transferPhaseToDisk[t.Phase]
			}
			if done {
				ds.BytesCopied, ds.Phase = p.SizeBytes, "Copied"
			}
		}
		out = append(out, ds)
	}
	m.Status.Disks = out
}
