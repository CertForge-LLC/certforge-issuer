package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	certforgev1alpha1 "github.com/certforge/certforge-issuer/api/v1alpha1"
)

// IssuerReconciler validates CertForgeIssuer and CertForgeClusterIssuer objects,
// confirming that the referenced credentials Secret exists and the token is non-empty.
type IssuerReconciler struct {
	client.Client
	Kind string // "CertForgeIssuer" or "CertForgeClusterIssuer"
}

func (r *IssuerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	spec, statusConditions, updateStatus, err := r.loadIssuer(ctx, req)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve credentials from the referenced Secret.
	secretNS := req.Namespace
	if r.Kind == "CertForgeClusterIssuer" {
		secretNS = "certforge-system" // ClusterIssuers reference secrets in a fixed namespace
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: spec.AuthSecretRef.Name, Namespace: secretNS}, secret); err != nil {
		logger.Error(err, "credentials secret not found", "secret", spec.AuthSecretRef.Name)
		meta.SetStatusCondition(statusConditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "SecretNotFound",
			Message: fmt.Sprintf("Secret %s/%s not found: %v", secretNS, spec.AuthSecretRef.Name, err),
		})
		return ctrl.Result{}, updateStatus(ctx)
	}

	token := string(secret.Data["token"])
	if token == "" {
		meta.SetStatusCondition(statusConditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "InvalidSecret",
			Message: fmt.Sprintf("Secret %s/%s missing 'token' key", secretNS, spec.AuthSecretRef.Name),
		})
		return ctrl.Result{}, updateStatus(ctx)
	}

	meta.SetStatusCondition(statusConditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "Verified",
		Message: fmt.Sprintf("Credentials verified, connected to %s", spec.URL),
	})

	logger.Info("issuer ready", "url", spec.URL)
	return ctrl.Result{}, updateStatus(ctx)
}

// loadIssuer loads either kind and returns a uniform view plus a status update closure.
func (r *IssuerReconciler) loadIssuer(ctx context.Context, req ctrl.Request) (
	spec *certforgev1alpha1.CertForgeIssuerSpec,
	conditions *[]metav1.Condition,
	update func(context.Context) error,
	err error,
) {
	if r.Kind == "CertForgeClusterIssuer" {
		obj := &certforgev1alpha1.CertForgeClusterIssuer{}
		if err = r.Get(ctx, req.NamespacedName, obj); err != nil {
			return nil, nil, nil, err
		}
		return &obj.Spec, &obj.Status.Conditions, func(ctx context.Context) error {
			return r.Status().Update(ctx, obj)
		}, nil
	}
	obj := &certforgev1alpha1.CertForgeIssuer{}
	if err = r.Get(ctx, req.NamespacedName, obj); err != nil {
		return nil, nil, nil, err
	}
	return &obj.Spec, &obj.Status.Conditions, func(ctx context.Context) error {
		return r.Status().Update(ctx, obj)
	}, nil
}

func (r *IssuerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Kind == "CertForgeClusterIssuer" {
		return ctrl.NewControllerManagedBy(mgr).
			For(&certforgev1alpha1.CertForgeClusterIssuer{}).
			Complete(r)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&certforgev1alpha1.CertForgeIssuer{}).
		Complete(r)
}
