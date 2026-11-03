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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/names"
	"github.com/metal2vm/metal2vm/internal/protocol"
	"github.com/metal2vm/metal2vm/internal/transfer"
)

var _ = Describe("P2VMigration Controller", func() {
	ctx := context.Background()
	const (
		host = "metal2vm-gateway.apps.example.com"
		ns   = "default"
		gib  = int64(1) << 30
	)

	newSourceWithInventory := func(name string, connected bool) *migrationv1alpha1.P2VSource {
		src := &migrationv1alpha1.P2VSource{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, src) })
		now := metav1.NewTime(time.Now().Truncate(time.Second))
		src.Status.Inventory = &migrationv1alpha1.Inventory{
			CollectedAt: now, CollectedBy: migrationv1alpha1.InventoryFromLiveAgent,
			Disks: []migrationv1alpha1.Disk{
				{ID: "root", Path: "/dev/vda", SizeBytes: 30 * gib},
				{ID: "data", Path: "/dev/sda", SizeBytes: 5*gib + 123},
				{ID: "luks", Path: "/dev/sdb", SizeBytes: gib, Encrypted: true},
			},
		}
		src.Status.Agent = &migrationv1alpha1.AgentStatus{Connected: connected, LastHeartbeat: &now}
		Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())
		return src
	}
	newMigration := func(name, source string, mutate func(*migrationv1alpha1.P2VMigrationSpec)) *migrationv1alpha1.P2VMigration {
		m := &migrationv1alpha1.P2VMigration{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: migrationv1alpha1.P2VMigrationSpec{
				SourceRef: corev1.LocalObjectReference{Name: source},
				Disks: []migrationv1alpha1.DiskMapping{
					{SourceDiskID: "root", Action: migrationv1alpha1.DiskCopy},
					{SourceDiskID: "data", Action: migrationv1alpha1.DiskCopy},
					{SourceDiskID: "luks", Action: migrationv1alpha1.DiskSkip},
				},
			},
		}
		if mutate != nil {
			mutate(&m.Spec)
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, m) })
		return m
	}
	reconciler := func() *P2VMigrationReconciler {
		return &P2VMigrationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Namespace: operatorNS,
			TransferImage: "registry.example.com/metal2vm/transfer@sha256:abc"}
	}
	reconcileN := func(m *migrationv1alpha1.P2VMigration, n int) {
		r := reconciler()
		for range n {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(m)})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), m)).To(Succeed())
	}

	BeforeEach(func() {
		reconcileGatewayConfig(ctx)
		sc := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "fast", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}},
			Provisioner: "example.com/csi",
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, sc))).To(Succeed())
	})

	It("copies disks through a transfer pod and reaches Converting", func() {
		src := newSourceWithInventory("srv-a", false)
		m := newMigration("mig-a", "srv-a", nil)

		By("passing pre-flight with a warning for the skipped encrypted disk excluded")
		reconcileN(m, 2)
		Expect(apimeta.IsStatusConditionTrue(m.Status.Conditions, ConditionPreflightPassed)).To(BeTrue(), m.Status.Message)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseBooting))
		Expect(m.Status.Message).To(ContainSubstring("srv-a-boot"))

		By("waiting in Booting until the agent connects")
		reconcileN(m, 1)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseBooting))
		src.Status.Agent.Connected = true
		Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())
		reconcileN(m, 3)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseCopying))

		By("creating block PVCs sized from the inventory")
		var pvc corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.DiskPVC("mig-a", 1)}, &pvc)).To(Succeed())
		Expect(*pvc.Spec.VolumeMode).To(Equal(corev1.PersistentVolumeBlock))
		Expect(*pvc.Spec.StorageClassName).To(Equal("fast"))
		want := resource.MustParse("5121Mi") // 5 GiB + 123 bytes, rounded up to whole MiB
		Expect(pvc.Spec.Resources.Requests.Storage().Cmp(want)).To(Equal(0), pvc.Spec.Resources.Requests.Storage().String())
		Expect(pvc.Annotations).To(HaveKeyWithValue(AnnotationSourceDisk, "data"))

		By("locking the tunnel down to the agent's reverse forwards")
		var sec corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.Transfer("mig-a")}, &sec)).To(Succeed())
		var users map[string][]string
		Expect(json.Unmarshal(sec.Data[names.TransferKeyUsers], &users)).To(Succeed())
		Expect(users).To(HaveKeyWithValue(string(sec.Data[names.TransferKeyAuth]),
			[]string{"^R:127\\.0\\.0\\.1:10809$", "^R:127\\.0\\.0\\.1:10810$"}))
		var exports []protocol.Export
		Expect(json.Unmarshal(sec.Data[names.TransferKeyExports], &exports)).To(Succeed())
		Expect(exports).To(Equal([]protocol.Export{{DiskID: "root", Port: 10809}, {DiskID: "data", Port: 10810}}))

		var route routev1.Route
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.Transfer("mig-a")}, &route)).To(Succeed())
		Expect(route.Spec.Host).To(HavePrefix("m2v-"))
		Expect(route.Spec.Host).To(HaveSuffix(".apps.example.com"))
		Expect(string(sec.Data[names.TransferKeyTunnelURL])).To(Equal("https://" + route.Spec.Host))

		By("running a hardened transfer pod with the PVCs as block devices")
		var pod corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.Transfer("mig-a")}, &pod)).To(Succeed())
		c := pod.Spec.Containers[0]
		Expect(c.Image).To(Equal("registry.example.com/metal2vm/transfer@sha256:abc"))
		Expect(c.VolumeDevices).To(HaveLen(2))
		Expect(*c.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		Expect(*pod.Spec.AutomountServiceAccountToken).To(BeFalse())
		Expect(c.Env[0].Value).To(ContainSubstring("/dev/m2v/disk1"))

		Expect(m.Status.Disks).To(HaveLen(3))
		Expect(m.Status.Disks[2].Phase).To(Equal("Skipped"))

		By("finishing when the transfer pod succeeds")
		final, _ := json.Marshal(transfer.Status{Done: true, Disks: []transfer.DiskStatus{
			{DiskID: "root", Phase: transfer.DiskCopied, BytesTotal: 30 * gib, BytesCopied: 30 * gib},
			{DiskID: "data", Phase: transfer.DiskCopied, BytesTotal: 5*gib + 123, BytesCopied: 5*gib + 123},
		}})
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "transfer", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: string(final)},
		}}}
		Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
		reconcileN(m, 1)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseConverting))
		Expect(apimeta.IsStatusConditionTrue(m.Status.Conditions, ConditionCopied)).To(BeTrue())
		Expect(m.Status.Disks[1].BytesCopied).To(Equal(5*gib + 123))
		Expect(m.Status.Disks[1].Phase).To(Equal("Copied"))
	})

	It("reports every pre-flight blocker at once", func() {
		newSourceWithInventory("srv-b", true)
		m := newMigration("mig-b", "srv-b", func(s *migrationv1alpha1.P2VMigrationSpec) {
			s.Mode = migrationv1alpha1.ModeWarm
			s.Disks = []migrationv1alpha1.DiskMapping{
				{SourceDiskID: "nope", Action: migrationv1alpha1.DiskCopy, StorageClassName: "missing"},
				{SourceDiskID: "luks", Action: migrationv1alpha1.DiskCopy},
			}
		})
		reconcileN(m, 3)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhasePreflight))
		failed := map[string]migrationv1alpha1.PreflightSeverity{}
		for _, p := range m.Status.Preflight {
			if !p.Passed {
				failed[p.Name] = p.Severity
			}
		}
		Expect(failed).To(Equal(map[string]migrationv1alpha1.PreflightSeverity{
			"ModeSupported":         migrationv1alpha1.SeverityBlocker,
			"DisksKnown":            migrationv1alpha1.SeverityBlocker,
			"StorageClassAvailable": migrationv1alpha1.SeverityBlocker,
			"NoEncryptedDisks":      migrationv1alpha1.SeverityWarning,
		}))
		Expect(strings.HasPrefix(m.Status.Message, "pre-flight check failed")).To(BeTrue())
	})

	It("fails the migration when the transfer pod fails", func() {
		newSourceWithInventory("srv-c", true)
		m := newMigration("mig-c", "srv-c", func(s *migrationv1alpha1.P2VMigrationSpec) {
			s.Disks = s.Disks[:1]
		})
		reconcileN(m, 6)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseCopying))
		var pod corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.Transfer("mig-c")}, &pod)).To(Succeed())
		final, _ := json.Marshal(transfer.Status{Done: true, Error: "disk root: nbdcopy: connection reset"})
		pod.Status.Phase = corev1.PodFailed
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "transfer", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: string(final)},
		}}}
		Expect(k8sClient.Status().Update(ctx, &pod)).To(Succeed())
		reconcileN(m, 1)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseFailed))
		Expect(m.Status.Message).To(ContainSubstring("connection reset"))
	})
})
