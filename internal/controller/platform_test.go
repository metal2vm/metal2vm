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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	consolev1 "github.com/openshift/api/console/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	securityv1 "github.com/openshift/api/security/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("PlatformSetup", Ordered, func() {
	var p *PlatformSetup
	var anchor rbacv1.ClusterRole

	BeforeAll(func() {
		anchor = rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: ConversionSCCRole}}
		Expect(k8sClient.Create(ctx, &anchor)).To(Succeed())
		Expect(k8sClient.Create(ctx, &operatorv1.Console{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Spec:       operatorv1.ConsoleSpec{Plugins: []string{"other-plugin"}},
		})).To(Succeed())
		p = &PlatformSetup{Client: k8sClient, Reader: k8sClient, Namespace: "metal2vm-system",
			SCC: true, ConsolePlugin: true}
	})

	It("creates the conversion SCC owned by the installer's ClusterRole", func() {
		Expect(p.Ensure(ctx)).To(Succeed())
		var scc securityv1.SecurityContextConstraints
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ConversionSCC}, &scc)).To(Succeed())
		Expect(scc.SeccompProfiles).To(ContainElement("localhost/profiles/unshare.json"))
		Expect(scc.AllowPrivilegedContainer).To(BeFalse())
		Expect(scc.OwnerReferences).To(HaveLen(1))
		Expect(scc.OwnerReferences[0].UID).To(Equal(anchor.UID))
	})

	It("repairs an SCC that was changed", func() {
		var scc securityv1.SecurityContextConstraints
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ConversionSCC}, &scc)).To(Succeed())
		scc.AllowPrivilegedContainer = true
		scc.SeccompProfiles = []string{"*"}
		Expect(k8sClient.Update(ctx, &scc)).To(Succeed())

		Expect(p.Ensure(ctx)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ConversionSCC}, &scc)).To(Succeed())
		Expect(scc.AllowPrivilegedContainer).To(BeFalse())
		Expect(scc.SeccompProfiles).To(Equal(DesiredConversionSCC().SeccompProfiles))
	})

	It("registers the console plugin backed by the operator namespace's Service", func() {
		var cp consolev1.ConsolePlugin
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ConsolePluginName}, &cp)).To(Succeed())
		Expect(cp.Spec.Backend.Service).NotTo(BeNil())
		Expect(cp.Spec.Backend.Service.Namespace).To(Equal("metal2vm-system"))
		Expect(cp.Spec.Backend.Service.Port).To(BeEquivalentTo(9443))
		Expect(cp.OwnerReferences).To(HaveLen(1))
	})

	It("leaves the console config alone unless asked to enable the plugin", func() {
		var c operatorv1.Console
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cluster"}, &c)).To(Succeed())
		Expect(c.Spec.Plugins).To(Equal([]string{"other-plugin"}))

		p.EnableConsolePlugin = true
		Expect(p.Ensure(ctx)).To(Succeed())
		Expect(p.Ensure(ctx)).To(Succeed()) // idempotent
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cluster"}, &c)).To(Succeed())
		Expect(c.Spec.Plugins).To(Equal([]string{"other-plugin", ConsolePluginName}))
	})
})
