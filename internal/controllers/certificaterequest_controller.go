package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	certforgev1alpha1 "github.com/certforge/certforge-issuer/api/v1alpha1"
)

const (
	annotationRequestID   = "certforge.io/request-id"
	annotationSubmittedAt = "certforge.io/submitted-at"
)

// CertificateRequestReconciler watches CertificateRequest objects and
// delegates issuance to CertForge when the issuerRef group is certforge.io.
type CertificateRequestReconciler struct {
	client.Client
}

func (r *CertificateRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cr := &cmapi.CertificateRequest{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only handle requests for our issuer group.
	if cr.Spec.IssuerRef.Group != certforgev1alpha1.GroupVersion.Group {
		return ctrl.Result{}, nil
	}

	// Already signed — nothing to do.
	if len(cr.Status.Certificate) > 0 {
		return ctrl.Result{}, nil
	}

	// Already terminal — nothing to do.
	if isConditionTrue(cr, cmapi.CertificateRequestConditionDenied) ||
		isConditionTrue(cr, cmapi.CertificateRequestConditionInvalidRequest) {
		return ctrl.Result{}, nil
	}

	// Resolve the issuer and load credentials.
	cfURL, token, err := r.resolveIssuer(ctx, cr)
	if err != nil {
		logger.Error(err, "failed to resolve issuer")
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	cf := newClient(cfURL, token)

	// Check if we already submitted this request (stored in annotation).
	requestID := cr.Annotations[annotationRequestID]

	// "rejected" is a sentinel written on PolicyError to prevent re-submission races.
	if requestID == "rejected" {
		r.setCondition(ctx, cr, cmapi.CertificateRequestConditionInvalidRequest,
			cmmeta.ConditionTrue, "PolicyViolation", "Request rejected by CertForge policy")
		return ctrl.Result{}, nil
	}

	if requestID == "" {
		// First time — submit the CSR.
		id, err := cf.Submit(ctx, string(cr.Spec.Request), cr.Namespace, cr.Name)
		if err != nil {
			var policyErr *PolicyError
			if errors.As(err, &policyErr) {
				// Write the sentinel annotation first so concurrent reconciles don't re-submit.
				patch := client.MergeFrom(cr.DeepCopy())
				if cr.Annotations == nil {
					cr.Annotations = map[string]string{}
				}
				cr.Annotations[annotationRequestID] = "rejected"
				_ = r.Patch(ctx, cr, patch)
				logger.Info("CSR rejected by CertForge policy", "reason", policyErr.Message)
				r.setCondition(ctx, cr, cmapi.CertificateRequestConditionInvalidRequest,
					cmmeta.ConditionTrue, "PolicyViolation", policyErr.Message)
				return ctrl.Result{}, nil
			}
			logger.Error(err, "failed to submit CSR to CertForge")
			r.setCondition(ctx, cr, cmapi.CertificateRequestConditionReady,
				cmmeta.ConditionFalse, "Pending", fmt.Sprintf("Submitting to CertForge: %v", err))
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}

		// Store the request ID and submission time so we can poll and show elapsed time.
		patch := client.MergeFrom(cr.DeepCopy())
		if cr.Annotations == nil {
			cr.Annotations = map[string]string{}
		}
		cr.Annotations[annotationRequestID] = id
		cr.Annotations[annotationSubmittedAt] = time.Now().UTC().Format(time.RFC3339)
		if err := r.Patch(ctx, cr, patch); err != nil {
			return ctrl.Result{}, err
		}
		requestID = id
		logger.Info("submitted to CertForge", "requestID", id)
	}

	// Poll for the result.
	result, err := cf.Poll(ctx, requestID)
	if err != nil {
		logger.Error(err, "poll failed", "requestID", requestID)
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	switch result.Status {
	case "issued":
		patch := client.MergeFrom(cr.DeepCopy())
		cr.Status.Certificate = []byte(result.Certificate)
		r.setCondition(ctx, cr, cmapi.CertificateRequestConditionReady,
			cmmeta.ConditionTrue, "Issued", "Certificate issued by CertForge")
		if err := r.Status().Patch(ctx, cr, patch); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("certificate issued", "requestID", requestID)
		return ctrl.Result{}, nil

	case "denied":
		r.setCondition(ctx, cr, cmapi.CertificateRequestConditionDenied,
			cmmeta.ConditionTrue, "Denied",
			fmt.Sprintf("Request denied by CertForge: %s", result.Reason))
		logger.Info("certificate request denied", "requestID", requestID, "reason", result.Reason)
		return ctrl.Result{}, nil

	default: // pending
		msg := "Waiting for CertForge to issue certificate"
		if result.Reason != "" {
			msg = result.Reason
		}
		if ts := cr.Annotations[annotationSubmittedAt]; ts != "" {
			if submitted, err := time.Parse(time.RFC3339, ts); err == nil {
				msg = fmt.Sprintf("%s (submitted %s ago)", msg, formatElapsed(time.Since(submitted)))
			}
		}
		r.setCondition(ctx, cr, cmapi.CertificateRequestConditionReady,
			cmmeta.ConditionFalse, "Pending", msg)
		logger.Info("request pending approval", "requestID", requestID)
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
}

// resolveIssuer finds the Issuer or ClusterIssuer and returns the CertForge URL and token.
func (r *CertificateRequestReconciler) resolveIssuer(ctx context.Context, cr *cmapi.CertificateRequest) (string, string, error) {
	var spec certforgev1alpha1.CertForgeIssuerSpec

	switch cr.Spec.IssuerRef.Kind {
	case "CertForgeClusterIssuer", "":
		obj := &certforgev1alpha1.CertForgeClusterIssuer{}
		if err := r.Get(ctx, types.NamespacedName{Name: cr.Spec.IssuerRef.Name}, obj); err != nil {
			return "", "", fmt.Errorf("ClusterIssuer %q not found: %w", cr.Spec.IssuerRef.Name, err)
		}
		spec = obj.Spec
	case "CertForgeIssuer":
		obj := &certforgev1alpha1.CertForgeIssuer{}
		if err := r.Get(ctx, types.NamespacedName{Name: cr.Spec.IssuerRef.Name, Namespace: cr.Namespace}, obj); err != nil {
			return "", "", fmt.Errorf("Issuer %q not found: %w", cr.Spec.IssuerRef.Name, err)
		}
		spec = obj.Spec
	default:
		return "", "", fmt.Errorf("unknown issuer kind %q", cr.Spec.IssuerRef.Kind)
	}

	// Resolve the token from the Secret.
	secretNS := cr.Namespace
	if cr.Spec.IssuerRef.Kind == "CertForgeClusterIssuer" || cr.Spec.IssuerRef.Kind == "" {
		secretNS = "certforge-system"
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: spec.AuthSecretRef.Name, Namespace: secretNS}, secret); err != nil {
		return "", "", fmt.Errorf("credentials secret %q not found: %w", spec.AuthSecretRef.Name, err)
	}
	token := string(secret.Data["token"])
	if token == "" {
		return "", "", fmt.Errorf("secret %q has no 'token' key", spec.AuthSecretRef.Name)
	}
	return spec.URL, token, nil
}

func (r *CertificateRequestReconciler) setCondition(
	ctx context.Context,
	cr *cmapi.CertificateRequest,
	condType cmapi.CertificateRequestConditionType,
	status cmmeta.ConditionStatus,
	reason, message string,
) {
	now := metav1.Now()
	for i, c := range cr.Status.Conditions {
		if c.Type == condType {
			cr.Status.Conditions[i].Status = status
			cr.Status.Conditions[i].Reason = reason
			cr.Status.Conditions[i].Message = message
			cr.Status.Conditions[i].LastTransitionTime = &now
			r.Status().Update(ctx, cr) //nolint:errcheck
			return
		}
	}
	cr.Status.Conditions = append(cr.Status.Conditions, cmapi.CertificateRequestCondition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: &now,
	})
	r.Status().Update(ctx, cr) //nolint:errcheck
}

func isConditionTrue(cr *cmapi.CertificateRequest, t cmapi.CertificateRequestConditionType) bool {
	for _, c := range cr.Status.Conditions {
		if c.Type == t && c.Status == cmmeta.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *CertificateRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cmapi.CertificateRequest{}).
		Complete(r)
}
