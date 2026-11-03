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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/gateway"
	"github.com/metal2vm/metal2vm/internal/tokens"
)

const (
	// ConditionBootMediaReady is set on P2VSource when its boot ISO can be downloaded.
	ConditionBootMediaReady = "BootMediaReady"
	// defaultTokenTTL applies when P2VConfig does not set agentTokenTTL.
	defaultTokenTTL = 24 * time.Hour
	// agentTimeout marks an agent disconnected after missed heartbeats.
	agentTimeout = 90 * time.Second
)

// P2VSourceReconciler issues boot media for physical servers and tracks the agent.
type P2VSourceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Now    func() time.Time
}

// +kubebuilder:rbac:groups=migration.metal2vm.io,resources=p2vsources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=migration.metal2vm.io,resources=p2vsources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=migration.metal2vm.io,resources=p2vsources/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *P2VSourceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// BootSecretName is the name of a source's boot Secret.
func BootSecretName(source string) string {
	const suffix = "-boot"
	if len(source)+len(suffix) > 253 {
		source = source[:253-len(suffix)]
	}
	return source + suffix
}

// Reconcile keeps the boot Secret valid and the agent status fresh.
func (r *P2VSourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var src migrationv1alpha1.P2VSource
	if err := r.Get(ctx, req.NamespacedName, &src); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := src.DeepCopy()
	var requeue time.Duration

	var cfg migrationv1alpha1.P2VConfig
	err := r.Get(ctx, types.NamespacedName{Name: migrationv1alpha1.P2VConfigSingletonName}, &cfg)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if cfg.Status.GatewayURL == "" {
		apimeta.SetStatusCondition(&src.Status.Conditions, metav1.Condition{
			Type: ConditionBootMediaReady, Status: metav1.ConditionFalse, Reason: "GatewayNotReady",
			Message: "waiting for the metal2vm gateway (P2VConfig \"cluster\") to become available", ObservedGeneration: src.Generation,
		})
		requeue = 10 * time.Second
	} else {
		ttl := defaultTokenTTL
		if cfg.Spec.AgentTokenTTL != nil && cfg.Spec.AgentTokenTTL.Duration > 0 {
			ttl = cfg.Spec.AgentTokenTTL.Duration
		}
		expiresAt, err := r.ensureBootSecret(ctx, &src, cfg.Status.GatewayURL, ttl)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("boot secret: %w", err)
		}
		src.Status.BootMedia = &migrationv1alpha1.BootMediaStatus{
			SecretName: BootSecretName(src.Name), ExpiresAt: metav1.NewTime(expiresAt),
		}
		apimeta.SetStatusCondition(&src.Status.Conditions, metav1.Condition{
			Type: ConditionBootMediaReady, Status: metav1.ConditionTrue, Reason: "Issued",
			Message:            "boot ISO URL is in Secret " + BootSecretName(src.Name) + " (key " + tokens.KeyISOURL + ")",
			ObservedGeneration: src.Generation,
		})
		requeue = time.Until(expiresAt) + time.Second
	}

	// The gateway sets connected=true on every heartbeat; time it out here.
	if a := src.Status.Agent; a != nil && a.Connected && a.LastHeartbeat != nil {
		if idle := r.now().Sub(a.LastHeartbeat.Time); idle > agentTimeout {
			a.Connected = false
		} else {
			requeue = min(requeue, agentTimeout-idle+time.Second)
		}
	}

	if !equality.Semantic.DeepEqual(orig.Status, src.Status) {
		if err := r.Status().Patch(ctx, &src, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: max(requeue, time.Second)}, nil
}

// ensureBootSecret creates or rotates the source's boot credentials and keeps
// the ISO URL current. Credentials rotate only after they expire; the gateway
// extends them while an agent is connected.
func (r *P2VSourceReconciler) ensureBootSecret(ctx context.Context, src *migrationv1alpha1.P2VSource,
	gatewayURL string, ttl time.Duration) (time.Time, error) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: BootSecretName(src.Name), Namespace: src.Namespace}}
	var expiresAt time.Time
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		exp, perr := time.Parse(time.RFC3339, string(sec.Data[tokens.KeyExpiresAt]))
		if perr != nil || !r.now().Before(exp) || len(sec.Data[tokens.KeyToken]) == 0 || len(sec.Data[tokens.KeyBootKey]) == 0 {
			exp = r.now().Add(ttl).UTC().Truncate(time.Second)
			sec.Data[tokens.KeyBootKey] = []byte(tokens.Generate())
			sec.Data[tokens.KeyToken] = []byte(tokens.Generate())
			sec.Data[tokens.KeyExpiresAt] = []byte(exp.Format(time.RFC3339))
		}
		expiresAt = exp
		bootKey, token := string(sec.Data[tokens.KeyBootKey]), string(sec.Data[tokens.KeyToken])
		sec.Data[tokens.KeyISOURL] = []byte(gatewayURL + "/boot/v1/iso/" + bootKey + ".iso")
		sec.Labels = labelled(map[string]string{
			tokens.LabelSource:      src.Name,
			tokens.LabelBootKeyHash: tokens.Hash(bootKey),
			tokens.LabelTokenHash:   tokens.Hash(token),
		})
		if sec.Annotations == nil {
			sec.Annotations = map[string]string{}
		}
		sec.Annotations[gateway.TokenTTLAnnotation] = ttl.String()
		return controllerutil.SetControllerReference(src, sec, r.Scheme)
	})
	return expiresAt, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *P2VSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// A change of gateway URL (P2VConfig status) must refresh every ISO URL.
	allSources := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list migrationv1alpha1.P2VSourceList
		if err := mgr.GetClient().List(ctx, &list); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for _, s := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
		}
		return reqs
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&migrationv1alpha1.P2VSource{}).
		Owns(&corev1.Secret{}).
		Watches(&migrationv1alpha1.P2VConfig{}, allSources).
		Named("p2vsource").
		Complete(r)
}
