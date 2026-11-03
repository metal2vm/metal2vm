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
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kubevirtv1 "kubevirt.io/api/core/v1"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/convert"
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
			CollectedAt: now, CollectedBy: migrationv1alpha1.InventoryFromLiveAgent, MemoryMiB: 4096,
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
			TransferImage: "registry.example.com/metal2vm/transfer@sha256:abc", OpenShift: true}
	}
	succeed := func(pod *corev1.Pod, message string) {
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: pod.Spec.Containers[0].Name, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: message},
		}}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
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

		By("moving to Converting when the transfer pod succeeds")
		final, _ := json.Marshal(transfer.Status{Done: true, Disks: []transfer.DiskStatus{
			{DiskID: "root", Phase: transfer.DiskCopied, BytesTotal: 30 * gib, BytesCopied: 30 * gib},
			{DiskID: "data", Phase: transfer.DiskCopied, BytesTotal: 5*gib + 123, BytesCopied: 5*gib + 123},
		}})
		succeed(&pod, string(final))
		reconcileN(m, 1)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseConverting))
		Expect(apimeta.IsStatusConditionTrue(m.Status.Conditions, ConditionCopied)).To(BeTrue())
		Expect(m.Status.Disks[1].BytesCopied).To(Equal(5*gib + 123))
		Expect(m.Status.Disks[1].Phase).To(Equal("Copied"))

		By("running a conversion pod with /dev/kvm and the unshare seccomp profile")
		reconcileN(m, 1)
		var conv corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "mig-a-convert"}, &conv)).To(Succeed())
		Expect(conv.Spec.ServiceAccountName).To(Equal(ConversionServiceAccount))
		Expect(*conv.Spec.SecurityContext.SeccompProfile.LocalhostProfile).To(Equal("profiles/unshare.json"))
		Expect(conv.Spec.Containers[0].Args).To(Equal([]string{"convert"}))
		Expect(conv.Spec.Containers[0].Resources.Limits).To(HaveKey(corev1.ResourceName("devices.kubevirt.io/kvm")))
		var rb rbacv1.RoleBinding
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: ConversionServiceAccount}, &rb)).To(Succeed())
		Expect(rb.RoleRef.Name).To(Equal(ConversionSCCRole))

		By("creating a network-less test VM that owns the disks")
		res, _ := json.Marshal(convert.Result{Firmware: "uefi",
			OS:    convert.OS{Name: "linux", Distro: "rhel", OSInfo: "rhel9.8", ProductName: "Red Hat Enterprise Linux 9.8"},
			Disks: []convert.DiskInfo{{Index: 0, BootOrder: 1}, {Index: 1, BootOrder: 3}}})
		succeed(&conv, string(res))
		reconcileN(m, 1)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseTesting))
		Expect(m.Status.Guest.OSInfo).To(Equal("rhel9.8"))
		Expect(m.Status.Disks[0].BootOrder).To(Equal(int32(1)))
		var vm kubevirtv1.VirtualMachine
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "srv-a"}, &vm)).To(Succeed())
		Expect(vm.Spec.Template.Spec.Networks).To(BeEmpty())
		Expect(*vm.Spec.Template.Spec.Domain.Devices.Disks[0].BootOrder).To(Equal(uint(1)))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.DiskPVC("mig-a", 0)}, &pvc)).To(Succeed())
		Expect(pvc.OwnerReferences).To(HaveLen(1))
		Expect(pvc.OwnerReferences[0].Kind).To(Equal("VirtualMachine"))

		By("waiting for the admin's decision while testing")
		reconcileN(m, 2)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseTesting))
		Expect(m.Status.Message).To(ContainSubstring("WITHOUT network"))

		By("powering off the source at cutover before touching the VM")
		now := metav1.Now()
		m.Spec.Cutover = &now
		Expect(k8sClient.Update(ctx, m)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(src), src)).To(Succeed())
		fresh := metav1.NewTime(time.Now().Truncate(time.Second))
		src.Status.Agent = &migrationv1alpha1.AgentStatus{Connected: true, LastHeartbeat: &fresh}
		Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())
		reconcileN(m, 2)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseCuttingOver))
		Expect(m.Status.Message).To(ContainSubstring("power off"))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&vm), &vm)).To(Succeed())
		Expect(vm.Spec.Template.Spec.Networks).To(BeEmpty(), "the VM must stay offline while the source is up")

		By("switching the VM to production networking once the source is off")
		src.Status.Agent.Connected = false
		Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())
		reconcileN(m, 1)
		Expect(apimeta.IsStatusConditionTrue(m.Status.Conditions, ConditionSourcePoweredOff)).To(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&vm), &vm)).To(Succeed())
		Expect(vm.Annotations).To(HaveKey(annotationCutoverRestart))

		By("not completing on the stale Ready flag of the test instance")
		vm.Status.Ready = true
		Expect(k8sClient.Status().Update(ctx, &vm)).To(Succeed())
		reconcileN(m, 1)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseCuttingOver))

		By("completing once a new instance is running and ready")
		vmi := &kubevirtv1.VirtualMachineInstance{ObjectMeta: metav1.ObjectMeta{Name: vm.Name, Namespace: ns}}
		Expect(k8sClient.Create(ctx, vmi)).To(Succeed())
		vmi.Status.Phase = kubevirtv1.Running
		vmi.Status.Conditions = []kubevirtv1.VirtualMachineInstanceCondition{
			{Type: kubevirtv1.VirtualMachineInstanceReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, vmi)).To(Succeed())
		reconcileN(m, 1)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseCompleted))

		By("cleaning up temporary resources when archived")
		m.Spec.Archived = true
		Expect(k8sClient.Update(ctx, m)).To(Succeed())
		reconcileN(m, 1)
		Expect(apimeta.IsStatusConditionTrue(m.Status.Conditions, ConditionArchived)).To(BeTrue())
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.Transfer("mig-a")}, &routev1.Route{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.DiskPVC("mig-a", 0)}, &pvc)).To(Succeed(),
			"archiving must never delete the VM's disks")
	})

	It("rolls back by stopping the VM", func() {
		newSourceWithInventory("srv-d", true)
		m := newMigration("mig-d", "srv-d", func(s *migrationv1alpha1.P2VMigrationSpec) { s.Disks = s.Disks[:1] })
		reconcileN(m, 6)
		var pod corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: names.Transfer("mig-d")}, &pod)).To(Succeed())
		succeed(&pod, `{"done":true}`)
		reconcileN(m, 2)
		var conv corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "mig-d-convert"}, &conv)).To(Succeed())
		succeed(&conv, `{"firmware":"bios","os":{"name":"linux","osinfo":"ubuntu24.04"}}`)
		reconcileN(m, 1)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseTesting))

		m.Spec.Rollback = true
		Expect(k8sClient.Update(ctx, m)).To(Succeed())
		reconcileN(m, 1)
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseRolledBack))
		var vm kubevirtv1.VirtualMachine
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "srv-d"}, &vm)).To(Succeed())
		Expect(*vm.Spec.RunStrategy).To(Equal(kubevirtv1.RunStrategyHalted))
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
