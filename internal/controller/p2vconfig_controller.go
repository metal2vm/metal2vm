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
	"maps"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	migrationv1alpha1 "github.com/metal2vm/metal2vm/api/v1alpha1"
	"github.com/metal2vm/metal2vm/internal/gateway"
	"github.com/metal2vm/metal2vm/internal/pki"
	"github.com/metal2vm/metal2vm/internal/tokens"
)

// ConditionAvailable is set on P2VConfig when the gateway is reachable.
const ConditionAvailable = "Available"

// Common labels on objects metal2vm creates.
const (
	LabelPartOf = "app.kubernetes.io/part-of"
	PartOfValue = "metal2vm"
)

// OperatorPodLabels select the operator pods, which also serve the gateway.
var OperatorPodLabels = map[string]string{
	"control-plane":          "controller-manager",
	"app.kubernetes.io/name": PartOfValue,
}

// P2VConfigReconciler exposes the gateway and manages its certificates.
type P2VConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Namespace is the operator namespace.
	Namespace string
	Now       func() time.Time
}

// +kubebuilder:rbac:groups=migration.metal2vm.io,resources=p2vconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=migration.metal2vm.io,resources=p2vconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=migration.metal2vm.io,resources=p2vconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes/custom-host,verbs=create;update;patch

func (r *P2VConfigReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile ensures the gateway Service, Route, CA and serving certificate.
func (r *P2VConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var cfg migrationv1alpha1.P2VConfig
	if err := r.Get(ctx, req.NamespacedName, &cfg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if cfg.Name != migrationv1alpha1.P2VConfigSingletonName {
		return ctrl.Result{}, nil
	}

	ca, err := r.ensureCA(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("gateway CA: %w", err)
	}
	if err := r.ensureService(ctx, &cfg); err != nil {
		return ctrl.Result{}, fmt.Errorf("gateway service: %w", err)
	}

	exposure := cfg.Spec.Gateway.Exposure
	if exposure == "" {
		exposure = migrationv1alpha1.ExposureRoute
	}
	if exposure != migrationv1alpha1.ExposureRoute {
		return ctrl.Result{}, r.setAvailable(ctx, &cfg, "", metav1.ConditionFalse, "UnsupportedExposure",
			fmt.Sprintf("exposure %q is not implemented yet; use Route", exposure))
	}
	host, err := r.ensureRoute(ctx, &cfg)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("gateway route: %w", err)
	}
	if host == "" {
		log.Info("waiting for the gateway route host")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err := r.ensureServingCert(ctx, ca, host); err != nil {
		return ctrl.Result{}, fmt.Errorf("gateway certificate: %w", err)
	}
	if err := r.setAvailable(ctx, &cfg, "https://"+host, metav1.ConditionTrue, "GatewayReady",
		"live agents connect to https://"+host+" (port 443)"); err != nil {
		return ctrl.Result{}, err
	}
	// Re-check daily so serving certificates renew before they expire.
	return ctrl.Result{RequeueAfter: 24 * time.Hour}, nil
}

func (r *P2VConfigReconciler) setAvailable(ctx context.Context, cfg *migrationv1alpha1.P2VConfig, url string,
	status metav1.ConditionStatus, reason, msg string) error {
	patch := client.MergeFrom(cfg.DeepCopy())
	cfg.Status.GatewayURL = url
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type: ConditionAvailable, Status: status, Reason: reason, Message: msg, ObservedGeneration: cfg.Generation,
	})
	return r.Status().Patch(ctx, cfg, patch)
}

func labelled(extra map[string]string) map[string]string {
	l := map[string]string{tokens.LabelManagedBy: tokens.ManagedByValue, LabelPartOf: PartOfValue}
	maps.Copy(l, extra)
	return l
}

// ensureCA creates the gateway CA once. It is deliberately not owned by the
// P2VConfig: rotating it would invalidate every boot ISO already handed out.
func (r *P2VConfigReconciler) ensureCA(ctx context.Context) (pki.KeyPair, error) {
	var sec corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: gateway.CASecretName}, &sec)
	if err == nil {
		return pki.KeyPair{CertPEM: sec.Data[gateway.CACertKey], KeyPEM: sec.Data[corev1.TLSPrivateKeyKey]}, nil
	}
	if !apierrors.IsNotFound(err) {
		return pki.KeyPair{}, err
	}
	ca, err := pki.NewCA("metal2vm-gateway-ca", r.now())
	if err != nil {
		return pki.KeyPair{}, err
	}
	sec = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gateway.CASecretName, Namespace: r.Namespace, Labels: labelled(nil)},
		Data:       map[string][]byte{gateway.CACertKey: ca.CertPEM, corev1.TLSPrivateKeyKey: ca.KeyPEM},
	}
	return ca, r.Create(ctx, &sec)
}

func (r *P2VConfigReconciler) ensureService(ctx context.Context, cfg *migrationv1alpha1.P2VConfig) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: gateway.ServiceName, Namespace: r.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labelled(nil)
		svc.Spec.Selector = OperatorPodLabels
		svc.Spec.Ports = []corev1.ServicePort{{
			Name: "https", Port: 443, TargetPort: intstr.FromInt32(gateway.Port), Protocol: corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(cfg, svc, r.Scheme)
	})
	return err
}

func (r *P2VConfigReconciler) ensureRoute(ctx context.Context, cfg *migrationv1alpha1.P2VConfig) (string, error) {
	route := &routev1.Route{ObjectMeta: metav1.ObjectMeta{Name: gateway.RouteName, Namespace: r.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		route.Labels = labelled(nil)
		if cfg.Spec.Gateway.Hostname != "" {
			route.Spec.Host = cfg.Spec.Gateway.Hostname
		}
		route.Spec.To = routev1.RouteTargetReference{Kind: "Service", Name: gateway.ServiceName}
		route.Spec.Port = &routev1.RoutePort{TargetPort: intstr.FromString("https")}
		route.Spec.TLS = &routev1.TLSConfig{
			Termination:                   routev1.TLSTerminationPassthrough,
			InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyNone,
		}
		return controllerutil.SetControllerReference(cfg, route, r.Scheme)
	})
	if err != nil {
		return "", err
	}
	return route.Spec.Host, nil
}

func (r *P2VConfigReconciler) ensureServingCert(ctx context.Context, ca pki.KeyPair, host string) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: gateway.TLSSecretName, Namespace: r.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		sec.Labels = labelled(nil)
		sec.Type = corev1.SecretTypeTLS
		if !pki.NeedsRenewal(sec.Data[corev1.TLSCertKey], host, r.now()) {
			return nil
		}
		kp, err := pki.NewServing(ca, []string{host}, r.now())
		if err != nil {
			return err
		}
		sec.Data = map[string][]byte{corev1.TLSCertKey: kp.CertPEM, corev1.TLSPrivateKeyKey: kp.KeyPEM}
		return nil
	})
	return err
}

// EnsureDefaultConfig creates the P2VConfig singleton if it does not exist,
// so a fresh install works without any manual step.
func EnsureDefaultConfig(ctx context.Context, c client.Client) error {
	cfg := &migrationv1alpha1.P2VConfig{ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.P2VConfigSingletonName}}
	err := c.Create(ctx, cfg)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *P2VConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toSingleton := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: migrationv1alpha1.P2VConfigSingletonName}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&migrationv1alpha1.P2VConfig{}).
		Owns(&corev1.Service{}).
		Owns(&routev1.Route{}).
		Watches(&corev1.Secret{}, toSingleton, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetNamespace() == r.Namespace &&
				(o.GetName() == gateway.CASecretName || o.GetName() == gateway.TLSSecretName)
		}))).
		Named("p2vconfig").
		Complete(r)
}
