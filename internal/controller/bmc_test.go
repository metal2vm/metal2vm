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
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/bmc/redfishsim"
)

var _ = Describe("Redfish BMC integration", func() {
	ctx := context.Background()
	var (
		mem  *redfishsim.Memory
		srv  *httptest.Server
		src  *migrationv1alpha1.P2VSource
		now  time.Time
		clk  = func() time.Time { return now }
		name = "srv-bmc"
	)

	BeforeEach(func() {
		reconcileGatewayConfig(ctx)
		now = time.Now()
		mem = redfishsim.NewMemory("1")
		srv = httptest.NewTLSServer(&redfishsim.Server{Backend: mem, Username: "root", Password: "calvin",
			Manufacturer: "HPE", Model: "ProLiant DL380 Gen11"})
		DeferCleanup(srv.Close)
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "ilo", Namespace: "default"},
			Data:       map[string][]byte{"username": []byte("root"), "password": []byte("calvin")},
		}))).To(Succeed())
		src = &migrationv1alpha1.P2VSource{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: migrationv1alpha1.P2VSourceSpec{BMC: &migrationv1alpha1.BMCAccess{
				Address: srv.URL, InsecureSkipVerify: true, CredentialsSecretRef: corev1.LocalObjectReference{Name: "ilo"},
			}},
		}
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, src) })
		sr := &P2VSourceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient, Now: clk}
		_, err := sr.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(src)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(src), src)).To(Succeed())
	})

	It("checks the BMC, boots the ISO, confirms power-off and rolls back", func() {
		Expect(apimeta.IsStatusConditionTrue(src.Status.Conditions, ConditionBMCReachable)).To(BeTrue())
		Expect(src.Status.BMC.Model).To(Equal("ProLiant DL380 Gen11"))
		Expect(src.Status.BMC.PowerState).To(Equal("On"))

		m := &migrationv1alpha1.P2VMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig-bmc", Namespace: "default"},
			Spec: migrationv1alpha1.P2VMigrationSpec{
				SourceRef: corev1.LocalObjectReference{Name: name}, BootMethod: migrationv1alpha1.BootRedfish,
				Disks: []migrationv1alpha1.DiskMapping{{SourceDiskID: "d0", Action: migrationv1alpha1.DiskCopy}},
			},
		}
		Expect(bootMethodReady(m, src)).To(BeTrue())

		r := &P2VMigrationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient, Now: clk}

		By("booting the live ISO through the BMC, once, even with a stale status")
		Expect(r.bootViaBMC(ctx, m, src)).To(Succeed())
		apimeta.RemoveStatusCondition(&m.Status.Conditions, ConditionBootMediaInserted) // simulate a stale cache
		Expect(r.bootViaBMC(ctx, m, src)).To(Succeed())
		img, inserted := mem.Media("1")
		Expect(inserted).To(BeTrue())
		Expect(img).To(ContainSubstring("/boot/v1/iso/"))
		Expect(mem.Boot("1")).To(Equal("Cd"))
		Expect(mem.Calls).To(Equal([]string{"insert 1 " + img, "boot 1 Cd", "reset 1 ForceRestart"}))

		By("waiting while the source is still on")
		off, _, err := r.sourceOff(ctx, m, src)
		Expect(err).NotTo(HaveOccurred())
		Expect(off).To(BeFalse())

		By("confirming the OS shutdown through the BMC and ejecting the media")
		mem.SetPower("1", false)
		off, how, err := r.sourceOff(ctx, m, src)
		Expect(err).NotTo(HaveOccurred())
		Expect(off).To(BeTrue())
		Expect(how).To(Equal("confirmed by the BMC"))
		_, inserted = mem.Media("1")
		Expect(inserted).To(BeFalse(), "live media must be ejected so a power-on boots the disks")

		By("rolling back: booting the server's own disks")
		m.Status.Phase = migrationv1alpha1.PhaseCompleted
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, m) })
		mem.Calls = nil
		_, err = r.reconcileRollback(ctx, m)
		Expect(err).NotTo(HaveOccurred())
		Expect(mem.Boot("1")).To(Equal("Hdd"))
		on, _ := mem.PowerOn("1")
		Expect(on).To(BeTrue())

		By("not restarting the server when a stale cache runs the rollback again")
		calls := len(mem.Calls)
		_, err = r.reconcileRollback(ctx, m)
		Expect(err).NotTo(HaveOccurred())
		Expect(mem.Calls[calls:]).NotTo(ContainElement(ContainSubstring("reset")))
		Expect(m.Status.Phase).To(Equal(migrationv1alpha1.PhaseRolledBack))
		Expect(m.Status.Message).To(ContainSubstring("through its BMC"))
	})

	It("rolls back from Testing by rebooting the live environment onto the disks", func() {
		m := &migrationv1alpha1.P2VMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig-rb-testing", Namespace: "default"},
			Spec: migrationv1alpha1.P2VMigrationSpec{SourceRef: corev1.LocalObjectReference{Name: name},
				Disks: []migrationv1alpha1.DiskMapping{{SourceDiskID: "d0", Action: migrationv1alpha1.DiskCopy}}},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, m) })
		r := &P2VMigrationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient, Now: clk}
		Expect(mem.InsertMedia("1", "https://gw/boot/v1/iso/k.iso")).To(Succeed()) // server runs the live ISO
		mem.Calls = nil
		m.Status.Phase = migrationv1alpha1.PhaseTesting
		_, err := r.reconcileRollback(ctx, m)
		Expect(err).NotTo(HaveOccurred())
		Expect(mem.Calls).To(Equal([]string{"eject 1", "boot 1 Hdd", "reset 1 ForceRestart"}))
		_, inserted := mem.Media("1")
		Expect(inserted).To(BeFalse())
	})

	It("forces the source off when the OS does not shut down", func() {
		m := &migrationv1alpha1.P2VMigration{ObjectMeta: metav1.ObjectMeta{Name: "mig-force"}}
		r := &P2VMigrationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient, Now: clk}
		off, _, err := r.sourceOff(ctx, m, src)
		Expect(err).NotTo(HaveOccurred())
		Expect(off).To(BeFalse())
		now = now.Add(3 * time.Minute)
		off, how, err := r.sourceOff(ctx, m, src)
		Expect(err).NotTo(HaveOccurred())
		Expect(off).To(BeTrue())
		Expect(how).To(ContainSubstring("forced off"))
		Expect(mem.Calls).To(ContainElement("reset 1 ForceOff"))
	})

	It("blocks redfish boot when the BMC is unusable", func() {
		bad := src.DeepCopy()
		apimeta.SetStatusCondition(&bad.Status.Conditions, metav1.Condition{Type: ConditionBMCReachable,
			Status: metav1.ConditionFalse, Reason: "Unreachable", Message: "connection refused"})
		m := &migrationv1alpha1.P2VMigration{Spec: migrationv1alpha1.P2VMigrationSpec{BootMethod: migrationv1alpha1.BootRedfish}}
		ok, msg := bootMethodReady(m, bad)
		Expect(ok).To(BeFalse())
		Expect(msg).To(ContainSubstring("connection refused"))
		bad.Spec.BMC = nil
		ok, _ = bootMethodReady(m, bad)
		Expect(ok).To(BeFalse())
	})
})
