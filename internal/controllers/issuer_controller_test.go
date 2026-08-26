package controllers

import (
	"context"
	"strings"
	"testing"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	certforgev1alpha1 "github.com/certforge/certforge-issuer/api/v1alpha1"
)

// ── test helpers ──────────────────────────────────────────────────────────────

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(cmapi.AddToScheme(s))
	utilruntime.Must(certforgev1alpha1.AddToScheme(s))
	return s
}

func issuerReadyConditions() []metav1.Condition {
	return []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Verified",
		Message:            "ok",
		LastTransitionTime: metav1.Now(),
	}}
}

func issuerNotReadyConditions() []metav1.Condition {
	return []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "PingFailed",
		Message:            "bad token",
		LastTransitionTime: metav1.Now(),
	}}
}

func credentialsSecret(name, ns, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{"token": []byte(token)},
	}
}

func issuerCR(issuerName, issuerKind, namespace string) *cmapi.CertificateRequest {
	return &cmapi.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cr", Namespace: namespace},
		Spec: cmapi.CertificateRequestSpec{
			IssuerRef: cmmeta.ObjectReference{
				Name:  issuerName,
				Kind:  issuerKind,
				Group: certforgev1alpha1.GroupVersion.Group,
			},
		},
	}
}

// ── resolveIssuer: ClusterIssuer ──────────────────────────────────────────────

func TestResolveIssuer_ClusterIssuer_HappyPath(t *testing.T) {
	s := testScheme()
	clusterIssuer := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:               "https://app.certgovernance.app",
			AuthSecretRef:     &corev1.LocalObjectReference{Name: "certforge-credentials"},
			IssuanceProfileID: "profile-abc",
		},
		Status: certforgev1alpha1.CertForgeIssuerStatus{Conditions: issuerReadyConditions()},
	}
	secret := credentialsSecret("certforge-credentials", "certforge-system", "my-api-token")

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(clusterIssuer, secret).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	cf, profileID, err := r.resolveIssuer(context.Background(), issuerCR("certforge", "CertForgeClusterIssuer", "default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cf == nil {
		t.Fatal("resolveIssuer returned nil client")
	}
	if profileID != "profile-abc" {
		t.Errorf("profileID = %q, want profile-abc", profileID)
	}
}

func TestResolveIssuer_ClusterIssuer_NotReady(t *testing.T) {
	s := testScheme()
	clusterIssuer := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:           "https://app.certgovernance.app",
			AuthSecretRef: &corev1.LocalObjectReference{Name: "certforge-credentials"},
		},
		Status: certforgev1alpha1.CertForgeIssuerStatus{Conditions: issuerNotReadyConditions()},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(clusterIssuer).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	_, _, err := r.resolveIssuer(context.Background(), issuerCR("certforge", "CertForgeClusterIssuer", "default"))
	if err == nil {
		t.Fatal("expected error for not-ready ClusterIssuer")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("error %q should mention 'not ready'", err.Error())
	}
}

func TestResolveIssuer_ClusterIssuer_NotFound(t *testing.T) {
	s := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	_, _, err := r.resolveIssuer(context.Background(), issuerCR("certforge", "CertForgeClusterIssuer", "default"))
	if err == nil {
		t.Fatal("expected error for missing ClusterIssuer")
	}
	if !strings.Contains(err.Error(), "certforge") {
		t.Errorf("error %q should name the missing issuer", err.Error())
	}
}

func TestResolveIssuer_ClusterIssuer_SecretMissing(t *testing.T) {
	s := testScheme()
	clusterIssuer := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:           "https://app.certgovernance.app",
			AuthSecretRef: &corev1.LocalObjectReference{Name: "certforge-credentials"},
		},
		Status: certforgev1alpha1.CertForgeIssuerStatus{Conditions: issuerReadyConditions()},
	}
	// No secret in the fake store

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(clusterIssuer).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	_, _, err := r.resolveIssuer(context.Background(), issuerCR("certforge", "CertForgeClusterIssuer", "default"))
	if err == nil {
		t.Fatal("expected error for missing credentials Secret")
	}
	if !strings.Contains(err.Error(), "certforge-credentials") {
		t.Errorf("error %q should mention the secret name", err.Error())
	}
}

func TestResolveIssuer_ClusterIssuer_EmptyToken(t *testing.T) {
	s := testScheme()
	clusterIssuer := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:           "https://app.certgovernance.app",
			AuthSecretRef: &corev1.LocalObjectReference{Name: "certforge-credentials"},
		},
		Status: certforgev1alpha1.CertForgeIssuerStatus{Conditions: issuerReadyConditions()},
	}
	emptySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge-credentials", Namespace: "certforge-system"},
		Data:       map[string][]byte{}, // token key absent
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(clusterIssuer, emptySecret).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	_, _, err := r.resolveIssuer(context.Background(), issuerCR("certforge", "CertForgeClusterIssuer", "default"))
	if err == nil {
		t.Fatal("expected error for empty/absent token key")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error %q should mention 'token'", err.Error())
	}
}

func TestResolveIssuer_ClusterIssuer_CustomSecretNamespace(t *testing.T) {
	s := testScheme()
	clusterIssuer := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:             "https://app.certgovernance.app",
			AuthSecretRef:   &corev1.LocalObjectReference{Name: "certforge-credentials"},
			SecretNamespace: "vault-ns", // non-default namespace
		},
		Status: certforgev1alpha1.CertForgeIssuerStatus{Conditions: issuerReadyConditions()},
	}
	// Secret is in vault-ns, not certforge-system
	secret := credentialsSecret("certforge-credentials", "vault-ns", "vault-token")

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(clusterIssuer, secret).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	cf, _, err := r.resolveIssuer(context.Background(), issuerCR("certforge", "CertForgeClusterIssuer", "default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cf == nil {
		t.Fatal("resolveIssuer returned nil client")
	}
}

// ── resolveIssuer: namespace-scoped Issuer ───────────────────────────────────

func TestResolveIssuer_NamespacedIssuer_HappyPath(t *testing.T) {
	s := testScheme()
	nsIssuer := &certforgev1alpha1.CertForgeIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge", Namespace: "team-ns"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:           "https://app.certgovernance.app",
			AuthSecretRef: &corev1.LocalObjectReference{Name: "certforge-credentials"},
		},
		Status: certforgev1alpha1.CertForgeIssuerStatus{Conditions: issuerReadyConditions()},
	}
	// For namespaced Issuer, secret is in the same namespace as the CR
	secret := credentialsSecret("certforge-credentials", "team-ns", "team-token")

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(nsIssuer, secret).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	cf, _, err := r.resolveIssuer(context.Background(), issuerCR("certforge", "CertForgeIssuer", "team-ns"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cf == nil {
		t.Fatal("resolveIssuer returned nil client")
	}
}

func TestResolveIssuer_UnknownKind(t *testing.T) {
	s := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	cr := issuerCR("certforge", "UnknownIssuerKind", "default")
	_, _, err := r.resolveIssuer(context.Background(), cr)
	if err == nil {
		t.Fatal("expected error for unknown issuer kind")
	}
	if !strings.Contains(err.Error(), "UnknownIssuerKind") {
		t.Errorf("error %q should mention the unknown kind", err.Error())
	}
}

// ── Reconcile: denial propagation ────────────────────────────────────────────

// TestReconcile_DenialPropagation_ParentAnnotation verifies that a CR whose parent
// Certificate has certforge.io/denied-at set is immediately denied without re-submitting
// to CertForge — even after cert-manager GCs the original denied CR.
func TestReconcile_DenialPropagation_ParentAnnotation(t *testing.T) {
	s := testScheme()

	parentCert := &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cert",
			Namespace: "default",
			Annotations: map[string]string{
				annotationDeniedAt: "2026-01-01T00:00:00Z",
			},
		},
	}
	cr := &cmapi.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cert-retry",
			Namespace: "default",
			Labels: map[string]string{
				"cert-manager.io/certificate-name": "my-cert",
			},
		},
		Spec: cmapi.CertificateRequestSpec{
			IssuerRef: cmmeta.ObjectReference{
				Name:  "certforge",
				Kind:  "CertForgeClusterIssuer",
				Group: certforgev1alpha1.GroupVersion.Group,
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(parentCert, cr).
		WithStatusSubresource(&cmapi.CertificateRequest{}).
		Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-cert-retry", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile() unexpected error: %v", err)
	}
	// Denial is terminal — should not requeue
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("should not requeue after terminal denial, got %+v", result)
	}

	// Re-fetch the CR and verify Denied=True was written to its status.
	updated := &cmapi.CertificateRequest{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-cert-retry", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to re-fetch CR: %v", err)
	}
	if !isConditionTrue(updated, cmapi.CertificateRequestConditionDenied) {
		t.Error("expected Denied=True on CR whose parent Certificate has denied-at annotation")
	}
}

// TestReconcile_DenialPropagation_SiblingCR verifies that when cert-manager creates a new
// CertificateRequest as a backoff retry, and a sibling CR for the same Certificate already
// has Denied=True, the new CR is denied immediately without re-submitting to CertForge.
func TestReconcile_DenialPropagation_SiblingCR(t *testing.T) {
	s := testScheme()

	parentCert := &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cert", Namespace: "default"},
	}

	// Original CR that was denied by a CertForge approver
	deniedSibling := &cmapi.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cert-1",
			Namespace: "default",
			Labels:    map[string]string{"cert-manager.io/certificate-name": "my-cert"},
		},
		Spec: cmapi.CertificateRequestSpec{
			IssuerRef: cmmeta.ObjectReference{
				Name:  "certforge",
				Kind:  "CertForgeClusterIssuer",
				Group: certforgev1alpha1.GroupVersion.Group,
			},
		},
		Status: cmapi.CertificateRequestStatus{
			Conditions: []cmapi.CertificateRequestCondition{
				{
					Type:    cmapi.CertificateRequestConditionDenied,
					Status:  cmmeta.ConditionTrue,
					Reason:  "Denied",
					Message: "rejected by CertForge approver",
				},
			},
		},
	}

	// New retry CR created by cert-manager's backoff timer
	retryCR := &cmapi.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cert-2",
			Namespace: "default",
			Labels:    map[string]string{"cert-manager.io/certificate-name": "my-cert"},
		},
		Spec: cmapi.CertificateRequestSpec{
			IssuerRef: cmmeta.ObjectReference{
				Name:  "certforge",
				Kind:  "CertForgeClusterIssuer",
				Group: certforgev1alpha1.GroupVersion.Group,
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(parentCert, deniedSibling, retryCR).
		WithStatusSubresource(&cmapi.CertificateRequest{}).
		Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-cert-2", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile() unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("should not requeue after terminal denial, got %+v", result)
	}

	// The retry CR should now also be denied
	updated := &cmapi.CertificateRequest{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-cert-2", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to re-fetch retry CR: %v", err)
	}
	if !isConditionTrue(updated, cmapi.CertificateRequestConditionDenied) {
		t.Error("expected Denied=True on retry CR when a sibling CR is already denied")
	}
}

// TestReconcile_AlreadySigned verifies that a CertificateRequest with a certificate
// already written to its status is a no-op (idempotent reconcile).
func TestReconcile_AlreadySigned(t *testing.T) {
	s := testScheme()
	cr := &cmapi.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "done-cr", Namespace: "default"},
		Spec: cmapi.CertificateRequestSpec{
			IssuerRef: cmmeta.ObjectReference{
				Name:  "certforge",
				Kind:  "CertForgeClusterIssuer",
				Group: certforgev1alpha1.GroupVersion.Group,
			},
		},
		Status: cmapi.CertificateRequestStatus{
			Certificate: []byte("---CERT PEM---"),
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cr).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "done-cr", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile() unexpected error: %v", err)
	}
	// Should return immediately with zero result — no requeue, no work done
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("already-signed CR should not requeue, got %+v", result)
	}
}

// TestReconcile_WrongIssuerGroup verifies that CertificateRequests for other issuer
// groups (e.g. cert-manager's built-in issuers) are silently ignored.
func TestReconcile_WrongIssuerGroup(t *testing.T) {
	s := testScheme()
	cr := &cmapi.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-cr", Namespace: "default"},
		Spec: cmapi.CertificateRequestSpec{
			IssuerRef: cmmeta.ObjectReference{
				Name:  "letsencrypt",
				Kind:  "ClusterIssuer",
				Group: "cert-manager.io", // not certforge.io
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cr).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "acme-cr", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("foreign-group CR should be ignored with zero result, got %+v", result)
	}
}
