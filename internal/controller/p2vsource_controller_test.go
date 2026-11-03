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
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/precheck"
	"github.com/metal2vm/metal2vm/internal/tokens"
)

var _ = Describe("P2VSource Controller", func() {
	ctx := context.Background()
	const host = "metal2vm-gateway.apps.example.com"

	newSource := func(name string) *migrationv1alpha1.P2VSource {
		src := &migrationv1alpha1.P2VSource{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, src) })
		return src
	}
	reconcileAt := func(src *migrationv1alpha1.P2VSource, now time.Time) reconcile.Result {
		r := &P2VSourceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Now: func() time.Time { return now }}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(src)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(src), src)).To(Succeed())
		return res
	}
	bootSecret := func(src *migrationv1alpha1.P2VSource) *corev1.Secret {
		sec := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: src.Namespace, Name: BootSecretName(src.Name)}, sec)).To(Succeed())
		return sec
	}

	BeforeEach(func() { reconcileGatewayConfig(ctx) })

	It("issues boot media with hashed lookup labels", func() {
		src := newSource("web01")
		now := time.Now()
		res := reconcileAt(src, now)

		Expect(apimeta.IsStatusConditionTrue(src.Status.Conditions, ConditionBootMediaReady)).To(BeTrue())
		Expect(src.Status.BootMedia).NotTo(BeNil())
		Expect(src.Status.BootMedia.SecretName).To(Equal("web01-boot"))
		Expect(res.RequeueAfter).To(BeNumerically("~", 24*time.Hour, time.Minute))

		sec := bootSecret(src)
		key, token := string(sec.Data[tokens.KeyBootKey]), string(sec.Data[tokens.KeyToken])
		Expect(key).NotTo(BeEmpty())
		Expect(token).NotTo(Equal(key))
		Expect(string(sec.Data[tokens.KeyISOURL])).To(Equal("https://" + host + "/boot/v1/iso/" + key + ".iso"))
		Expect(sec.Labels).To(HaveKeyWithValue(tokens.LabelBootKeyHash, tokens.Hash(key)))
		Expect(sec.Labels).To(HaveKeyWithValue(tokens.LabelTokenHash, tokens.Hash(token)))
		Expect(sec.Labels).To(HaveKeyWithValue(tokens.LabelSource, "web01"))
		Expect(sec.OwnerReferences).To(HaveLen(1))
		for _, v := range sec.Labels {
			Expect(strings.Contains(v, key) || strings.Contains(v, token)).To(BeFalse(), "labels must not leak secrets")
		}

		By("keeping credentials until they expire")
		reconcileAt(src, now.Add(time.Hour))
		Expect(string(bootSecret(src).Data[tokens.KeyToken])).To(Equal(token))

		By("rotating credentials after expiry")
		reconcileAt(src, now.Add(25*time.Hour))
		rotated := bootSecret(src)
		Expect(string(rotated.Data[tokens.KeyToken])).NotTo(Equal(token))
		Expect(string(rotated.Data[tokens.KeyBootKey])).NotTo(Equal(key))
	})

	It("marks the agent disconnected after missed heartbeats", func() {
		src := newSource("db01")
		now := time.Now()
		last := metav1.NewTime(now.Add(-30 * time.Second).Truncate(time.Second))
		src.Status.Agent = &migrationv1alpha1.AgentStatus{Connected: true, LastHeartbeat: &last}
		Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())

		res := reconcileAt(src, now)
		Expect(src.Status.Agent.Connected).To(BeTrue())
		Expect(res.RequeueAfter).To(BeNumerically("<=", agentTimeout))

		reconcileAt(src, now.Add(2*time.Minute))
		Expect(src.Status.Agent.Connected).To(BeFalse())
	})
})

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 1536: "1.5 KiB", 322122547: "307.2 MiB", 35 << 30: "35.0 GiB", 3 << 40: "3.0 TiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

var _ = Describe("P2VSource OS pre-check", func() {
	ctx := context.Background()
	BeforeEach(func() { reconcileGatewayConfig(ctx) })

	It("records the OS config, backs off after failures and pins the host key", func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "web02-ssh", Namespace: "default"},
			Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("pw")},
		}))).To(Succeed())
		src := &migrationv1alpha1.P2VSource{
			ObjectMeta: metav1.ObjectMeta{Name: "web02", Namespace: "default"},
			Spec: migrationv1alpha1.P2VSourceSpec{OSAccess: &migrationv1alpha1.OSAccess{
				Type: migrationv1alpha1.OSAccessSSH, Address: "web02.example.com",
				CredentialsSecretRef: corev1.LocalObjectReference{Name: "web02-ssh"},
			}},
		}
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, src) })

		var calls []string
		fail := true
		now := time.Now()
		r := &P2VSourceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Now: func() time.Time { return now },
			PreCheck: func(_ context.Context, a migrationv1alpha1.OSAccess, c precheck.Credentials, _ []byte, known string) (*migrationv1alpha1.OSConfig, error) {
				calls = append(calls, c.Username+"@"+a.Address+" known="+known)
				if fail {
					return nil, fmt.Errorf("connection refused")
				}
				return &migrationv1alpha1.OSConfig{HostKeyFingerprint: "SHA256:abc",
					OS:   migrationv1alpha1.OSInfo{Hostname: "web02", Distribution: "rhel", Version: "9.4"},
					NICs: []migrationv1alpha1.NIC{{MAC: "3c:ec:ef:00:00:01", Name: "eno1", IPAddresses: []string{"10.0.0.5/24"}}}}, nil
			}}
		run := func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(src)})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(src), src)).To(Succeed())
		}

		run()
		Expect(apimeta.FindStatusCondition(src.Status.Conditions, ConditionOSReachable).Reason).To(Equal("PreCheckFailed"))
		By("not retrying immediately")
		run()
		Expect(calls).To(HaveLen(1))

		By("succeeding after the back-off")
		fail = false
		now = now.Add(3 * time.Minute)
		run()
		Expect(calls).To(HaveLen(2))
		Expect(calls[1]).To(Equal("admin@web02.example.com known="))
		Expect(apimeta.IsStatusConditionTrue(src.Status.Conditions, ConditionOSReachable)).To(BeTrue())
		Expect(src.Status.OSConfig.NICs[0].Name).To(Equal("eno1"))

		By("not re-running once it succeeded")
		run()
		Expect(calls).To(HaveLen(2))

		By("re-running on request with the pinned host key")
		src.Annotations = map[string]string{AnnotationPreCheck: now.Add(time.Minute).UTC().Format(time.RFC3339)}
		Expect(k8sClient.Update(ctx, src)).To(Succeed())
		now = now.Add(2 * time.Minute)
		run()
		Expect(calls).To(HaveLen(3))
		Expect(calls[2]).To(HaveSuffix("known=SHA256:abc"))
	})
})
