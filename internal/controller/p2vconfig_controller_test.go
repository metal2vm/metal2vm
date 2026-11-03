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
	"crypto/x509"
	"encoding/pem"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/gateway"
)

const (
	operatorNS  = "metal2vm-system"
	gatewayHost = "metal2vm-gateway.apps.example.com"
)

func ensureNamespace(ctx context.Context, name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).To(Succeed())
}

// reconcileGatewayConfig creates the P2VConfig singleton with a fixed route host
// (envtest has no OpenShift router to assign one) and reconciles it.
func reconcileGatewayConfig(ctx context.Context) *migrationv1alpha1.P2VConfig {
	ensureNamespace(ctx, operatorNS)
	cfg := &migrationv1alpha1.P2VConfig{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: migrationv1alpha1.P2VConfigSingletonName}, cfg)
	if err != nil {
		cfg = &migrationv1alpha1.P2VConfig{ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.P2VConfigSingletonName}}
		cfg.Spec.Gateway.Hostname = gatewayHost
		Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
	}
	r := &P2VConfigReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Namespace: operatorNS}
	_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cfg)})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cfg), cfg)).To(Succeed())
	return cfg
}

var _ = Describe("P2VConfig Controller", func() {
	ctx := context.Background()
	const host = "metal2vm-gateway.apps.example.com"

	It("exposes the gateway and issues a certificate for the route host", func() {
		cfg := reconcileGatewayConfig(ctx)

		Expect(cfg.Status.GatewayURL).To(Equal("https://" + host))
		Expect(apimeta.IsStatusConditionTrue(cfg.Status.Conditions, ConditionAvailable)).To(BeTrue())

		var route routev1.Route
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: gateway.RouteName}, &route)).To(Succeed())
		Expect(route.Spec.TLS.Termination).To(Equal(routev1.TLSTerminationPassthrough))
		Expect(route.Spec.Host).To(Equal(host))

		var svc corev1.Service
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: gateway.ServiceName}, &svc)).To(Succeed())
		Expect(svc.Spec.Selector).To(Equal(OperatorPodLabels))
		Expect(svc.Spec.Ports[0].TargetPort.IntValue()).To(Equal(gateway.Port))

		var ca, tlsSec corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: gateway.CASecretName}, &ca)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operatorNS, Name: gateway.TLSSecretName}, &tlsSec)).To(Succeed())
		pool := x509.NewCertPool()
		Expect(pool.AppendCertsFromPEM(ca.Data[gateway.CACertKey])).To(BeTrue())
		block, _ := pem.Decode(tlsSec.Data[corev1.TLSCertKey])
		cert, err := x509.ParseCertificate(block.Bytes)
		Expect(err).NotTo(HaveOccurred())
		_, err = cert.Verify(x509.VerifyOptions{DNSName: host, Roots: pool})
		Expect(err).NotTo(HaveOccurred())

		By("keeping the CA and certificate stable across reconciles")
		caBefore, certBefore := string(ca.Data[gateway.CACertKey]), string(tlsSec.Data[corev1.TLSCertKey])
		reconcileGatewayConfig(ctx)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&ca), &ca)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&tlsSec), &tlsSec)).To(Succeed())
		Expect(string(ca.Data[gateway.CACertKey])).To(Equal(caBefore))
		Expect(string(tlsSec.Data[corev1.TLSCertKey])).To(Equal(certBefore))

		By("renewing the certificate when it is about to expire")
		r := &P2VConfigReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Namespace: operatorNS,
			Now: func() time.Time { return time.Now().Add(2*365*24*time.Hour - 24*time.Hour) }}
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cfg)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&tlsSec), &tlsSec)).To(Succeed())
		Expect(string(tlsSec.Data[corev1.TLSCertKey])).NotTo(Equal(certBefore))
	})

	It("creates the default P2VConfig idempotently", func() {
		Expect(EnsureDefaultConfig(ctx, k8sClient)).To(Succeed())
		Expect(EnsureDefaultConfig(ctx, k8sClient)).To(Succeed())
	})
})
