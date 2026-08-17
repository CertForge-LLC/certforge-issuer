package controllers

// Tests for IssuerReconciler — the controller that owns CertForgeIssuer and
// CertForgeClusterIssuer objects. Each test drives Reconcile() directly using a
// fake k8s client and an httptest.Server for the CertForge ping endpoint.
//
// Helper functions (testScheme, credentialsSecret, newTestServer) are declared
// in the sibling test files in this package.

import (
	"context"
	"net/http"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	certforgev1alpha1 "github.com/certforge/certforge-issuer/api/v1alpha1"
)

// findCondition returns the named condition from a []metav1.Condition slice, or nil.
func findCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// ── invalid spec ─────────────────────────────────────────────────────────────

// TestIssuerReconcile_BothAuthMethods: setting both authSecretRef and
// workloadIdentity is mutually exclusive → Ready=False/InvalidSpec without
// touching the Secret or pinging CertForge.
func TestIssuerReconcile_BothAuthMethods(t *testing.T) {
	s := testScheme()
	obj := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:              "https://app.certgovernance.app",
			AuthSecretRef:    &corev1.LocalObjectReference{Name: "certforge-credentials"},
			WorkloadIdentity: &certforgev1alpha1.WorkloadIdentitySpec{Audience: "certforge"},
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(obj).
		WithStatusSubresource(&certforgev1alpha1.CertForgeClusterIssuer{}).
		Build()
	r := &IssuerReconciler{Client: fakeClient, Kind: "CertForgeClusterIssuer"}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "certforge"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := &certforgev1alpha1.CertForgeClusterIssuer{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "certforge"}, got); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	assertCond(t, got.Status.Conditions, metav1.ConditionFalse, "InvalidSpec")
}

// TestIssuerReconcile_NeitherAuthMethod: omitting both authSecretRef and
// workloadIdentity → Ready=False/InvalidSpec.
func TestIssuerReconcile_NeitherAuthMethod(t *testing.T) {
	s := testScheme()
	obj := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL: "https://app.certgovernance.app",
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(obj).
		WithStatusSubresource(&certforgev1alpha1.CertForgeClusterIssuer{}).
		Build()
	r := &IssuerReconciler{Client: fakeClient, Kind: "CertForgeClusterIssuer"}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "certforge"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := &certforgev1alpha1.CertForgeClusterIssuer{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "certforge"}, got); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	assertCond(t, got.Status.Conditions, metav1.ConditionFalse, "InvalidSpec")
}

// ── secret resolution ─────────────────────────────────────────────────────────

// TestIssuerReconcile_SecretNotFound: referenced credentials Secret is missing
// → Ready=False/SecretNotFound.
func TestIssuerReconcile_SecretNotFound(t *testing.T) {
	s := testScheme()
	obj := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:           "https://app.certgovernance.app",
			AuthSecretRef: &corev1.LocalObjectReference{Name: "certforge-credentials"},
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(obj).
		WithStatusSubresource(&certforgev1alpha1.CertForgeClusterIssuer{}).
		Build()
	r := &IssuerReconciler{Client: fakeClient, Kind: "CertForgeClusterIssuer"}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "certforge"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := &certforgev1alpha1.CertForgeClusterIssuer{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "certforge"}, got); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	assertCond(t, got.Status.Conditions, metav1.ConditionFalse, "SecretNotFound")
}

// TestIssuerReconcile_InvalidSecret: Secret exists but has no "token" key
// → Ready=False/InvalidSecret.
func TestIssuerReconcile_InvalidSecret(t *testing.T) {
	s := testScheme()
	obj := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:           "https://app.certgovernance.app",
			AuthSecretRef: &corev1.LocalObjectReference{Name: "certforge-credentials"},
		},
	}
	emptySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge-credentials", Namespace: "certforge-system"},
		Data:       map[string][]byte{}, // "token" key absent
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(obj, emptySecret).
		WithStatusSubresource(&certforgev1alpha1.CertForgeClusterIssuer{}).
		Build()
	r := &IssuerReconciler{Client: fakeClient, Kind: "CertForgeClusterIssuer"}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "certforge"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := &certforgev1alpha1.CertForgeClusterIssuer{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "certforge"}, got); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	assertCond(t, got.Status.Conditions, metav1.ConditionFalse, "InvalidSecret")
}

// ── ping ─────────────────────────────────────────────────────────────────────

// TestIssuerReconcile_PingFailed: CertForge returns 401 → Ready=False/PingFailed
// and the reconciler requeues after 30s.
func TestIssuerReconcile_PingFailed(t *testing.T) {
	srv := newTestServer(http.StatusUnauthorized, nil)
	defer srv.Close()

	s := testScheme()
	obj := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:           srv.URL,
			AuthSecretRef: &corev1.LocalObjectReference{Name: "certforge-credentials"},
		},
	}
	secret := credentialsSecret("certforge-credentials", "certforge-system", "bad-token")
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(obj, secret).
		WithStatusSubresource(&certforgev1alpha1.CertForgeClusterIssuer{}).
		Build()
	r := &IssuerReconciler{Client: fakeClient, Kind: "CertForgeClusterIssuer"}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "certforge"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0 so the issuer retries after PingFailed")
	}

	got := &certforgev1alpha1.CertForgeClusterIssuer{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "certforge"}, got); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	assertCond(t, got.Status.Conditions, metav1.ConditionFalse, "PingFailed")
}

// TestIssuerReconcile_AuthSecretRef_HappyPath: valid Secret + reachable server
// → Ready=True/Verified and a periodic re-ping requeue.
func TestIssuerReconcile_AuthSecretRef_HappyPath(t *testing.T) {
	srv := newTestServer(http.StatusOK, map[string]string{"ok": "true"})
	defer srv.Close()

	s := testScheme()
	obj := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:           srv.URL,
			AuthSecretRef: &corev1.LocalObjectReference{Name: "certforge-credentials"},
		},
	}
	secret := credentialsSecret("certforge-credentials", "certforge-system", "good-token")
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(obj, secret).
		WithStatusSubresource(&certforgev1alpha1.CertForgeClusterIssuer{}).
		Build()
	r := &IssuerReconciler{Client: fakeClient, Kind: "CertForgeClusterIssuer"}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "certforge"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Must requeue periodically for live health checks even when nothing changes.
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0 for periodic live-health re-ping")
	}

	got := &certforgev1alpha1.CertForgeClusterIssuer{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "certforge"}, got); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	assertCond(t, got.Status.Conditions, metav1.ConditionTrue, "Verified")
}

// TestIssuerReconcile_WorkloadIdentity_HappyPath: token read from a projected
// ServiceAccount token file → Ready=True/Verified. No Secret required.
func TestIssuerReconcile_WorkloadIdentity_HappyPath(t *testing.T) {
	srv := newTestServer(http.StatusOK, map[string]string{"ok": "true"})
	defer srv.Close()

	// Write a temporary token file simulating a kubelet-managed projected SA token.
	f, err := os.CreateTemp("", "certforge-sa-token-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("eyJhbGciOiJSUzI1NiJ9.sa-oidc-token"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s := testScheme()
	obj := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL: srv.URL,
			WorkloadIdentity: &certforgev1alpha1.WorkloadIdentitySpec{
				Audience:  srv.URL,
				TokenFile: f.Name(),
			},
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(obj).
		WithStatusSubresource(&certforgev1alpha1.CertForgeClusterIssuer{}).
		Build()
	r := &IssuerReconciler{Client: fakeClient, Kind: "CertForgeClusterIssuer"}

	_, err = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "certforge"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := &certforgev1alpha1.CertForgeClusterIssuer{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "certforge"}, got); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	assertCond(t, got.Status.Conditions, metav1.ConditionTrue, "Verified")
}

// TestIssuerReconcile_NotFound: reconciling a missing object is a no-op
// (client.IgnoreNotFound) — no error returned, no panic.
func TestIssuerReconcile_NotFound(t *testing.T) {
	s := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	r := &IssuerReconciler{Client: fakeClient, Kind: "CertForgeClusterIssuer"}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "missing"},
	})
	if err != nil {
		t.Fatalf("expected no error for NotFound, got: %v", err)
	}
}

// TestIssuerReconcile_NamespacedIssuer_HappyPath: namespace-scoped CertForgeIssuer
// reads its credentials Secret from req.Namespace, not "certforge-system".
func TestIssuerReconcile_NamespacedIssuer_HappyPath(t *testing.T) {
	srv := newTestServer(http.StatusOK, map[string]string{"ok": "true"})
	defer srv.Close()

	s := testScheme()
	obj := &certforgev1alpha1.CertForgeIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge", Namespace: "team-ns"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:           srv.URL,
			AuthSecretRef: &corev1.LocalObjectReference{Name: "certforge-credentials"},
		},
	}
	secret := credentialsSecret("certforge-credentials", "team-ns", "team-token")
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(obj, secret).
		WithStatusSubresource(&certforgev1alpha1.CertForgeIssuer{}).
		Build()
	r := &IssuerReconciler{Client: fakeClient, Kind: "CertForgeIssuer"}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "certforge", Namespace: "team-ns"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := &certforgev1alpha1.CertForgeIssuer{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "certforge", Namespace: "team-ns"}, got); err != nil {
		t.Fatalf("re-fetch CertForgeIssuer: %v", err)
	}
	assertCond(t, got.Status.Conditions, metav1.ConditionTrue, "Verified")
}

// ── assertion helper ──────────────────────────────────────────────────────────

// assertCond asserts the Ready condition in the given slice. Extracted as a
// helper so tests stay focused on what makes each case unique.
func assertCond(t *testing.T, conds []metav1.Condition, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	cond := findCondition(conds, "Ready")
	if cond == nil {
		t.Fatalf("Ready condition not set")
	}
	if cond.Status != wantStatus {
		t.Errorf("Ready.Status = %q, want %q (reason: %q, msg: %q)", cond.Status, wantStatus, cond.Reason, cond.Message)
	}
	if cond.Reason != wantReason {
		t.Errorf("Ready.Reason = %q, want %q", cond.Reason, wantReason)
	}
}

