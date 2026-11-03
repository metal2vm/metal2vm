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
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
)

const testMAC = "aa:bb:cc:dd:ee:ff"

// These tests exercise the CRD schema and CEL rules against a real API server.
var _ = Describe("API validation", func() {
	ctx := context.Background()

	migration := func(name string, disk migrationv1alpha1.DiskMapping, nics ...migrationv1alpha1.NICMapping) *migrationv1alpha1.P2VMigration {
		return &migrationv1alpha1.P2VMigration{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: migrationv1alpha1.P2VMigrationSpec{
				SourceRef: corev1.LocalObjectReference{Name: "server01"},
				Disks:     []migrationv1alpha1.DiskMapping{disk},
				NICs:      nics,
			},
		}
	}
	iscsi := &migrationv1alpha1.LUNAttachment{ISCSI: &migrationv1alpha1.ISCSILUN{
		TargetPortal: "10.0.0.5:3260", IQN: "iqn.2001-05.com.example:data", LUN: 1,
	}}

	DescribeTable("P2VMigration",
		func(obj *migrationv1alpha1.P2VMigration, wantErr string) {
			err := k8sClient.Create(ctx, obj)
			if wantErr == "" {
				Expect(err).NotTo(HaveOccurred())
				Expect(obj.Spec.Mode).To(Equal(migrationv1alpha1.ModeCold), "defaults applied")
				Expect(obj.Spec.TestBoot).To(Equal(new(true)))
				Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
				return
			}
			Expect(err).To(MatchError(ContainSubstring(wantErr)))
		},
		Entry("copy disk with defaults",
			migration("v-copy", migrationv1alpha1.DiskMapping{SourceDiskID: "d0", Action: migrationv1alpha1.DiskCopy}), ""),
		Entry("attach requires lun",
			migration("v-attach-nolun", migrationv1alpha1.DiskMapping{SourceDiskID: "d0", Action: migrationv1alpha1.DiskAttach}),
			"lun is required when action is attach"),
		Entry("attach iSCSI LUN",
			migration("v-attach", migrationv1alpha1.DiskMapping{SourceDiskID: "d0", Action: migrationv1alpha1.DiskAttach, LUN: iscsi}), ""),
		Entry("copy rejects lun",
			migration("v-copy-lun", migrationv1alpha1.DiskMapping{SourceDiskID: "d0", Action: migrationv1alpha1.DiskCopy, LUN: iscsi}),
			"lun is only valid when action is attach"),
		Entry("lun needs exactly one transport",
			migration("v-lun-empty", migrationv1alpha1.DiskMapping{SourceDiskID: "d0", Action: migrationv1alpha1.DiskAttach, LUN: &migrationv1alpha1.LUNAttachment{}}),
			"exactly one of fc or iscsi must be set"),
		Entry("multus needs networkName",
			migration("v-multus", migrationv1alpha1.DiskMapping{SourceDiskID: "d0", Action: migrationv1alpha1.DiskCopy},
				migrationv1alpha1.NICMapping{SourceMAC: testMAC, Type: migrationv1alpha1.NetworkMultus}),
			"networkName is required for multus networks"),
		Entry("keepIP requires multus",
			migration("v-keepip", migrationv1alpha1.DiskMapping{SourceDiskID: "d0", Action: migrationv1alpha1.DiskCopy},
				migrationv1alpha1.NICMapping{SourceMAC: testMAC, Type: migrationv1alpha1.NetworkPod, KeepIP: true}),
			"keepIP requires a multus"),
		Entry("bridged NIC keeping IP and MAC",
			migration("v-bridged", migrationv1alpha1.DiskMapping{SourceDiskID: "d0", Action: migrationv1alpha1.DiskCopy},
				migrationv1alpha1.NICMapping{SourceMAC: testMAC, Type: migrationv1alpha1.NetworkMultus,
					NetworkName: "vlan100", KeepMAC: true, KeepIP: true}), ""),
	)

	It("rejects a P2VConfig not named cluster", func() {
		cfg := &migrationv1alpha1.P2VConfig{ObjectMeta: metav1.ObjectMeta{Name: "other"}}
		Expect(k8sClient.Create(ctx, cfg)).To(MatchError(ContainSubstring("P2VConfig must be named 'cluster'")))
	})

	It("rejects a malformed PXE MAC address", func() {
		src := &migrationv1alpha1.P2VSource{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-mac", Namespace: "default"},
			Spec:       migrationv1alpha1.P2VSourceSpec{PXE: &migrationv1alpha1.PXEAccess{BootMACAddress: "not-a-mac"}},
		}
		Expect(k8sClient.Create(ctx, src)).To(MatchError(ContainSubstring("bootMACAddress")))
	})

	It("accepts every sample in config/samples", func() {
		files, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "migration_*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(files).NotTo(BeEmpty())
		for _, f := range files {
			data, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			obj := &unstructured.Unstructured{}
			Expect(yaml.Unmarshal(data, &obj.Object)).To(Succeed(), f)
			if obj.GetKind() != "P2VConfig" {
				obj.SetNamespace("default")
			}
			// Server-side apply validates whether or not the object already exists.
			Expect(k8sClient.Patch(ctx, obj, client.Apply, client.DryRunAll, //nolint:staticcheck // unstructured apply
				client.FieldOwner("samples-test"), client.ForceOwnership)).To(Succeed(), f)
		}
	})
})
