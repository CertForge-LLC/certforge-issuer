package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	certforgev1alpha1 "github.com/certforge/certforge-issuer/api/v1alpha1"
)

const (
	annotationRequestID      = "certforge.io/request-id"
	annotationSubmittedAt    = "certforge.io/submitted-at"
	annotationIssuanceProfile = "certforge.io/issuance-profile"
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

	// certforge-issuer acts as its own approver: it submits the CSR to CertForge
	// immediately (without waiting for an external Approved condition) and then sets
	// Denied or Approved+cert based on CertForge's decision.
	//
	// Why: cert-manager's webhook forbids Denied=True when Approved=True is already set
	// on the same CertificateRequest. If an external approver-policy runs first and sets
	// Approved=True, we can never set Denied=True on a human rejection — so cert-manager
	// treats InvalidRequest as a failure and retries indefinitely.
	//
	// By submitting before any external approver sets Approved, we can set Denied=True
	// on rejection, which is the only truly terminal condition in cert-manager (no retry).
	// If an older certforge-auto-approve policy happens to race and set Approved=True
	// first, we fall back to InvalidRequest for graceful degradation.

	// Resolve the issuer and load credentials.
	cfURL, token, issuerProfileID, err := r.resolveIssuer(ctx, cr)
	if err != nil {
		logger.Error(err, "failed to resolve issuer")
		setCondition(cr, cmapi.CertificateRequestConditionReady,
			cmmeta.ConditionFalse, "IssuerNotReady", err.Error())
		if serr := r.Status().Update(ctx, cr); serr != nil {
			return ctrl.Result{}, serr
		}
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	// Per-Certificate annotation overrides the issuer-level default.
	issuanceProfileID := cr.Annotations[annotationIssuanceProfile]
	if issuanceProfileID == "" {
		issuanceProfileID = issuerProfileID
	}

	cf := newClient(cfURL, token)

	// Check if we already submitted this request (stored in annotation).
	requestID := cr.Annotations[annotationRequestID]

	// "rejected" is a sentinel written on PolicyError to prevent re-submission races.
	if requestID == "rejected" {
		setRejectedCondition(cr, "PolicyViolation", "Request rejected by CertForge policy")
		return ctrl.Result{}, r.Status().Update(ctx, cr)
	}

	if requestID == "" {
		// First time — submit the CSR.
		id, err := cf.Submit(ctx, string(cr.Spec.Request), cr.Namespace, cr.Name, issuanceProfileID)
		if err != nil {
			var policyErr *PolicyError
			if errors.As(err, &policyErr) {
				// Write the sentinel annotation so concurrent reconciles don't re-submit.
				patch := client.MergeFrom(cr.DeepCopy())
				if cr.Annotations == nil {
					cr.Annotations = map[string]string{}
				}
				cr.Annotations[annotationRequestID] = "rejected"
				if err := r.Patch(ctx, cr, patch); err != nil {
					return ctrl.Result{}, fmt.Errorf("writing rejected annotation: %w", err)
				}
				logger.Info("CSR rejected by CertForge policy", "reason", policyErr.Message)
				setRejectedCondition(cr, "PolicyViolation", policyErr.Message)
				return ctrl.Result{}, r.Status().Update(ctx, cr)
			}
			logger.Error(err, "failed to submit CSR to CertForge")
			setCondition(cr, cmapi.CertificateRequestConditionReady,
				cmmeta.ConditionFalse, "Pending", fmt.Sprintf("Submitting to CertForge: %v", err))
			if err := r.Status().Update(ctx, cr); err != nil {
				return ctrl.Result{}, err
			}
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
		// Take the patch baseline before mutating status so all fields are
		// written in a single Patch call.
		patch := client.MergeFrom(cr.DeepCopy())
		cr.Status.Certificate = []byte(result.Certificate)
		setCondition(cr, cmapi.CertificateRequestConditionReady,
			cmmeta.ConditionTrue, "Issued", "Certificate issued by CertForge")
		// If no external approver has run yet, set Approved=True ourselves so
		// cert-manager's issuing controller accepts the certificate bytes.
		if !isConditionTrue(cr, cmapi.CertificateRequestConditionApproved) {
			setCondition(cr, cmapi.CertificateRequestConditionApproved,
				cmmeta.ConditionTrue, "Approved", "Approved by CertForge")
		}
		if err := r.Status().Patch(ctx, cr, patch); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("certificate issued", "requestID", requestID)
		return ctrl.Result{}, nil

	case "rejected":
		// Use Denied=True — the only condition cert-manager treats as truly
		// terminal (no new CertificateRequest is created).
		// We can set Denied here because we no longer wait for an external
		// approver to set Approved=True first. If an older certforge-auto-approve
		// policy races and sets Approved=True before we get here, fall back to
		// InvalidRequest (webhook blocks Denied+Approved coexisting).
		patch := client.MergeFrom(cr.DeepCopy())
		msg := "Request rejected by CertForge approver"
		if result.Reason != "" {
			msg = fmt.Sprintf("Request rejected by CertForge approver: %s", result.Reason)
		}
		setRejectedCondition(cr, "Rejected", msg)
		logger.Info("certificate request denied by CertForge approver", "requestID", requestID, "reason", result.Reason)
		return ctrl.Result{}, r.Status().Patch(ctx, cr, patch)

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
		setCondition(cr, cmapi.CertificateRequestConditionReady,
			cmmeta.ConditionFalse, "Pending", msg)
		logger.Info("request pending approval", "requestID", requestID)
		if err := r.Status().Update(ctx, cr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
}

// resolveIssuer finds the Issuer or ClusterIssuer and returns the CertForge URL, token,
// default issuance profile ID, and any error.
func (r *CertificateRequestReconciler) resolveIssuer(ctx context.Context, cr *cmapi.CertificateRequest) (string, string, string, error) {
	var spec certforgev1alpha1.CertForgeIssuerSpec

	switch cr.Spec.IssuerRef.Kind {
	case "CertForgeClusterIssuer", "":
		obj := &certforgev1alpha1.CertForgeClusterIssuer{}
		if err := r.Get(ctx, types.NamespacedName{Name: cr.Spec.IssuerRef.Name}, obj); err != nil {
			return "", "", "", fmt.Errorf("ClusterIssuer %q not found: %w", cr.Spec.IssuerRef.Name, err)
		}
		if !apimeta.IsStatusConditionTrue(obj.Status.Conditions, "Ready") {
			return "", "", "", fmt.Errorf("ClusterIssuer %q is not ready", cr.Spec.IssuerRef.Name)
		}
		spec = obj.Spec
	case "CertForgeIssuer":
		obj := &certforgev1alpha1.CertForgeIssuer{}
		if err := r.Get(ctx, types.NamespacedName{Name: cr.Spec.IssuerRef.Name, Namespace: cr.Namespace}, obj); err != nil {
			return "", "", "", fmt.Errorf("Issuer %q not found: %w", cr.Spec.IssuerRef.Name, err)
		}
		if !apimeta.IsStatusConditionTrue(obj.Status.Conditions, "Ready") {
			return "", "", "", fmt.Errorf("Issuer %q is not ready", cr.Spec.IssuerRef.Name)
		}
		spec = obj.Spec
	default:
		return "", "", "", fmt.Errorf("unknown issuer kind %q", cr.Spec.IssuerRef.Kind)
	}

	// Resolve the namespace for the credentials Secret.
	// For ClusterIssuer: use SecretNamespace if set, else default to "certforge-system".
	// For namespace-scoped Issuer: use the issuer's own namespace.
	secretNS := cr.Namespace
	if cr.Spec.IssuerRef.Kind == "CertForgeClusterIssuer" || cr.Spec.IssuerRef.Kind == "" {
		secretNS = "certforge-system"
		if spec.SecretNamespace != "" {
			secretNS = spec.SecretNamespace
		}
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: spec.AuthSecretRef.Name, Namespace: secretNS}, secret); err != nil {
		return "", "", "", fmt.Errorf("credentials secret %q not found: %w", spec.AuthSecretRef.Name, err)
	}
	token := string(secret.Data["token"])
	if token == "" {
		return "", "", "", fmt.Errorf("secret %q has no 'token' key", spec.AuthSecretRef.Name)
	}
	return spec.URL, token, spec.IssuanceProfileID, nil
}

// setCondition mutates cr.Status.Conditions in memory — callers own the status write.
// LastTransitionTime is updated only when the condition's Status changes.
func setCondition(
	cr *cmapi.CertificateRequest,
	condType cmapi.CertificateRequestConditionType,
	status cmmeta.ConditionStatus,
	reason, message string,
) {
	now := metav1.Now()
	for i, c := range cr.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				cr.Status.Conditions[i].LastTransitionTime = &now
			}
			cr.Status.Conditions[i].Status = status
			cr.Status.Conditions[i].Reason = reason
			cr.Status.Conditions[i].Message = message
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
}

func isConditionTrue(cr *cmapi.CertificateRequest, t cmapi.CertificateRequestConditionType) bool {
	for _, c := range cr.Status.Conditions {
		if c.Type == t && c.Status == cmmeta.ConditionTrue {
			return true
		}
	}
	return false
}

// setRejectedCondition marks a CertificateRequest as permanently rejected.
//
// Preferred: Denied=True — cert-manager will not create a new CertificateRequest.
// Fallback:  InvalidRequest=True — used when Approved=True is already set, because
//            cert-manager's webhook forbids Denied and Approved coexisting on the same
//            request. InvalidRequest is less terminal (cert-manager v1.21 retries), but
//            it is the best we can do in that situation. Operators should remove any
//            certforge-auto-approve CertificateRequestPolicy so we can always use Denied.
func setRejectedCondition(cr *cmapi.CertificateRequest, reason, message string) {
	if isConditionTrue(cr, cmapi.CertificateRequestConditionApproved) {
		// Approved already set — can't use Denied; fall back to InvalidRequest.
		setCondition(cr, cmapi.CertificateRequestConditionInvalidRequest,
			cmmeta.ConditionTrue, reason, message)
		return
	}
	setCondition(cr, cmapi.CertificateRequestConditionDenied,
		cmmeta.ConditionTrue, reason, message)
}

func (r *CertificateRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cmapi.CertificateRequest{}).
		Complete(r)
}
