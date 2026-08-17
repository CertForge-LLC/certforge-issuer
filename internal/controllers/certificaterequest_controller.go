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
	annotationRequestID       = "certforge.io/request-id"
	annotationSubmittedAt     = "certforge.io/submitted-at"
	annotationIssuanceProfile = "certforge.io/issuance-profile"
	// annotationDeniedAt is written on the parent Certificate when a CR is permanently
	// denied. It persists across CertificateRequest GC so retry CRs created by
	// cert-manager's backoff timer are denied immediately without re-submitting to CertForge.
	// Operators clear the cycle by deleting the Certificate object.
	annotationDeniedAt = "certforge.io/denied-at"
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

	// Propagate denial from the parent Certificate annotation or sibling CRs.
	//
	// cert-manager's Certificate controller retries failed requests on an exponential
	// backoff (≈1 h, 2 h, …) even when Denied=True — it creates a brand-new
	// CertificateRequest for the same Certificate object. Without this check the issuer
	// would re-submit the new CR to CertForge, generating another approval notification
	// on every retry cycle.
	//
	// Two layers of detection (most durable first):
	//  1. Parent Certificate annotation certforge.io/denied-at — written when we first
	//     set Denied=True and survives CertificateRequest GC.
	//  2. Sibling CertificateRequest with Denied=True — catches the case where the old
	//     CR is still present when the retry CR appears.
	//
	// Operators break the cycle by deleting the Certificate object.
	const deniedMsg = "A previous request for this Certificate was rejected by a CertForge approver. " +
		"Delete the Certificate object to submit a new certificate request."

	// cert-manager v1.15+ stores certificate-name as an annotation; older
	// releases used a label. Check both for compatibility.
	certName := cr.Labels["cert-manager.io/certificate-name"]
	if certName == "" {
		certName = cr.Annotations["cert-manager.io/certificate-name"]
	}
	if certName != "" {
		// Layer 1: check parent Certificate annotation.
		parentCert := &cmapi.Certificate{}
		if err := r.Get(ctx, types.NamespacedName{Name: certName, Namespace: cr.Namespace}, parentCert); err == nil {
			if parentCert.Annotations[annotationDeniedAt] != "" {
				logger.Info("parent Certificate has denied-at annotation — propagating denial without re-submitting",
					"certificate", certName, "deniedAt", parentCert.Annotations[annotationDeniedAt])
				setRejectedCondition(cr, "PreviouslyDenied", deniedMsg)
				return ctrl.Result{}, r.Status().Update(ctx, cr)
			}
		}

		// Layer 2: check sibling CertificateRequests.
		siblingList := &cmapi.CertificateRequestList{}
		if err := r.List(ctx, siblingList,
			client.InNamespace(cr.Namespace),
			client.MatchingLabels{"cert-manager.io/certificate-name": certName},
		); err == nil {
			for i := range siblingList.Items {
				sibling := &siblingList.Items[i]
				if sibling.Name == cr.Name {
					continue
				}
				if isConditionTrue(sibling, cmapi.CertificateRequestConditionDenied) {
					logger.Info("propagating denial from sibling CertificateRequest — not re-submitting",
						"sibling", sibling.Name)
					setRejectedCondition(cr, "PreviouslyDenied", deniedMsg)
					return ctrl.Result{}, r.Status().Update(ctx, cr)
				}
			}
		}
	}

	// Resolve the issuer and load credentials.
	cfURL, ts, issuerProfileID, err := r.resolveIssuer(ctx, cr)
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

	cf := newClientWithTokenSource(cfURL, ts)

	// Check if we already submitted this request (stored in annotation).
	requestID := cr.Annotations[annotationRequestID]

	// "rejected" is a sentinel written on PolicyError to prevent re-submission races.
	if requestID == "rejected" {
		setRejectedCondition(cr, "PolicyViolation", "Request rejected by CertForge policy")
		r.stampCertificateDenied(ctx, cr)
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
				r.stampCertificateDenied(ctx, cr)
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
		r.stampCertificateDenied(ctx, cr)
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

// resolveIssuer finds the Issuer or ClusterIssuer, checks it is Ready, and
// returns the CertForge URL, a TokenSource, the default issuance profile ID,
// and any error. The TokenSource is either Secret-backed (static token) or
// file-backed (projected ServiceAccount token that rotates hourly).
func (r *CertificateRequestReconciler) resolveIssuer(ctx context.Context, cr *cmapi.CertificateRequest) (string, TokenSource, string, error) {
	var spec certforgev1alpha1.CertForgeIssuerSpec

	switch cr.Spec.IssuerRef.Kind {
	case "CertForgeClusterIssuer", "":
		obj := &certforgev1alpha1.CertForgeClusterIssuer{}
		if err := r.Get(ctx, types.NamespacedName{Name: cr.Spec.IssuerRef.Name}, obj); err != nil {
			return "", nil, "", fmt.Errorf("ClusterIssuer %q not found: %w", cr.Spec.IssuerRef.Name, err)
		}
		if !apimeta.IsStatusConditionTrue(obj.Status.Conditions, "Ready") {
			return "", nil, "", fmt.Errorf("ClusterIssuer %q is not ready", cr.Spec.IssuerRef.Name)
		}
		spec = obj.Spec
	case "CertForgeIssuer":
		obj := &certforgev1alpha1.CertForgeIssuer{}
		if err := r.Get(ctx, types.NamespacedName{Name: cr.Spec.IssuerRef.Name, Namespace: cr.Namespace}, obj); err != nil {
			return "", nil, "", fmt.Errorf("Issuer %q not found: %w", cr.Spec.IssuerRef.Name, err)
		}
		if !apimeta.IsStatusConditionTrue(obj.Status.Conditions, "Ready") {
			return "", nil, "", fmt.Errorf("Issuer %q is not ready", cr.Spec.IssuerRef.Name)
		}
		spec = obj.Spec
	default:
		return "", nil, "", fmt.Errorf("unknown issuer kind %q", cr.Spec.IssuerRef.Kind)
	}

	// Resolve the token source: Secret-backed (static) or file-backed (workload identity).
	var ts TokenSource
	switch {
	case spec.WorkloadIdentity != nil:
		tokenFile := spec.WorkloadIdentity.TokenFile
		if tokenFile == "" {
			tokenFile = "/var/run/secrets/certforge/token"
		}
		ts = FileTokenSource{path: tokenFile}

	case spec.AuthSecretRef != nil:
		// Resolve the namespace for the credentials Secret.
		// For ClusterIssuer: use SecretNamespace if set, else default to "certforge-system".
		// For namespace-scoped Issuer: use the CertificateRequest's own namespace.
		secretNS := cr.Namespace
		if cr.Spec.IssuerRef.Kind == "CertForgeClusterIssuer" || cr.Spec.IssuerRef.Kind == "" {
			secretNS = "certforge-system"
			if spec.SecretNamespace != "" {
				secretNS = spec.SecretNamespace
			}
		}
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: spec.AuthSecretRef.Name, Namespace: secretNS}, secret); err != nil {
			return "", nil, "", fmt.Errorf("credentials secret %q not found: %w", spec.AuthSecretRef.Name, err)
		}
		token := string(secret.Data["token"])
		if token == "" {
			return "", nil, "", fmt.Errorf("secret %q has no 'token' key", spec.AuthSecretRef.Name)
		}
		ts = StaticTokenSource{token: token}

	default:
		return "", nil, "", fmt.Errorf("issuer has no credential source (set authSecretRef or workloadIdentity)")
	}

	return spec.URL, ts, spec.IssuanceProfileID, nil
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

// stampCertificateDenied writes annotationDeniedAt on the parent Certificate so that
// future retry CertificateRequests (created by cert-manager's backoff timer) are denied
// immediately without re-submitting to CertForge, even after the denied CR is GC'd.
// Errors are logged and silently ignored — the CR's Denied condition is already set and
// that is the authoritative signal; the annotation is a durability optimisation.
func (r *CertificateRequestReconciler) stampCertificateDenied(ctx context.Context, cr *cmapi.CertificateRequest) {
	// cert-manager v1.15+ stores certificate-name as an annotation; older releases used a label.
	certName := cr.Labels["cert-manager.io/certificate-name"]
	if certName == "" {
		certName = cr.Annotations["cert-manager.io/certificate-name"]
	}
	if certName == "" {
		return
	}
	logger := log.FromContext(ctx)
	cert := &cmapi.Certificate{}
	if err := r.Get(ctx, types.NamespacedName{Name: certName, Namespace: cr.Namespace}, cert); err != nil {
		logger.V(1).Info("could not fetch parent Certificate to stamp denied-at", "error", err)
		return
	}
	patch := client.MergeFrom(cert.DeepCopy())
	if cert.Annotations == nil {
		cert.Annotations = map[string]string{}
	}
	cert.Annotations[annotationDeniedAt] = time.Now().UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, cert, patch); err != nil {
		logger.V(1).Info("could not stamp denied-at on parent Certificate", "error", err)
	}
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
