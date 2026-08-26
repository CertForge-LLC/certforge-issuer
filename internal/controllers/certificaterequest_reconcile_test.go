package controllers

// Integration-style tests for CertificateRequestReconciler.Reconcile().
//
// These tests use an httptest.Server to simulate the CertForge REST API so the
// full Submit → Poll lifecycle runs through real HTTP rather than mocks. The k8s
// layer still uses the controller-runtime fake client.
//
// Helper functions (testScheme, credentialsSecret, issuerCR, issuerReadyConditions)
// are declared in issuer_controller_test.go (same package).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	certforgev1alpha1 "github.com/certforge/certforge-issuer/api/v1alpha1"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// readyClusterIssuerAt returns a CertForgeClusterIssuer with Ready=True pointing
// at the given URL and using a static-token Secret named "certforge-credentials".
func readyClusterIssuerAt(url string) *certforgev1alpha1.CertForgeClusterIssuer {
	return &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL:           url,
			AuthSecretRef: &corev1.LocalObjectReference{Name: "certforge-credentials"},
		},
		Status: certforgev1alpha1.CertForgeIssuerStatus{Conditions: issuerReadyConditions()},
	}
}

// newCRForIssuer returns a minimal CertificateRequest for the "certforge"
// ClusterIssuer in the given namespace with optional labels.
func newCRForIssuer(name, ns string, labels map[string]string) *cmapi.CertificateRequest {
	return &cmapi.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: cmapi.CertificateRequestSpec{
			Request: []byte("---CSR PEM---"),
			IssuerRef: cmmeta.ObjectReference{
				Name:  "certforge",
				Kind:  "CertForgeClusterIssuer",
				Group: certforgev1alpha1.GroupVersion.Group,
			},
		},
	}
}

// newCertForgeServer builds an httptest.Server that routes POST /certificate-requests
// to onSubmit, GET /certificate-requests/* to onPoll, and always answers /ping with 200.
func newCertForgeServer(t *testing.T, onSubmit, onPoll http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/ping":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/certificate-requests":
			onSubmit(w, r)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/certificate-requests/"):
			onPoll(w, r)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// createTempTokenFile writes content to a temp file, registers cleanup, and
// returns the path.
func createTempTokenFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "certforge-sa-token-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

// ── Submit → Poll: happy path ─────────────────────────────────────────────────

// TestReconcile_SubmitAndIssued: a brand-new CR is submitted to CertForge and
// the same reconcile pass polls to find it already issued. Verifies that the CR
// gets certificate bytes, Ready=True, and Approved=True with no requeue.
func TestReconcile_SubmitAndIssued(t *testing.T) {
	srv := newCertForgeServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(certResponse{ID: "req-001", Status: "pending"})
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(certResponse{ID: "req-001", Status: "issued", Certificate: "---CERT PEM---"})
		},
	)
	defer srv.Close()

	s := testScheme()
	issuer := readyClusterIssuerAt(srv.URL)
	secret := credentialsSecret("certforge-credentials", "certforge-system", "tok")
	cr := newCRForIssuer("my-cr", "default", nil)

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(issuer, secret, cr).
		WithStatusSubresource(&cmapi.CertificateRequest{}).
		Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-cr", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile() unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("issued CR should not requeue, got %+v", result)
	}

	updated := &cmapi.CertificateRequest{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-cr", Namespace: "default"}, updated); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if string(updated.Status.Certificate) != "---CERT PEM---" {
		t.Errorf("Certificate = %q, want '---CERT PEM---'", updated.Status.Certificate)
	}
	if !isConditionTrue(updated, cmapi.CertificateRequestConditionReady) {
		t.Error("expected Ready=True after issued")
	}
	if !isConditionTrue(updated, cmapi.CertificateRequestConditionApproved) {
		t.Error("expected Approved=True — issuer sets it when no external approver has run")
	}
}

// TestReconcile_SubmitAndPending: the first poll returns "pending". The CR shows
// Ready=False and the reconciler requeues.
func TestReconcile_SubmitAndPending(t *testing.T) {
	srv := newCertForgeServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(certResponse{ID: "req-002", Status: "pending"})
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(certResponse{ID: "req-002", Status: "pending", Reason: "awaiting approval"})
		},
	)
	defer srv.Close()

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(readyClusterIssuerAt(srv.URL),
			credentialsSecret("certforge-credentials", "certforge-system", "tok"),
			newCRForIssuer("my-cr", "default", nil)).
		WithStatusSubresource(&cmapi.CertificateRequest{}).
		Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-cr", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0 while pending")
	}

	updated := &cmapi.CertificateRequest{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-cr", Namespace: "default"}, updated); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if isConditionTrue(updated, cmapi.CertificateRequestConditionReady) {
		t.Error("Ready should be False while pending")
	}
}

// TestReconcile_SubmitAndRejected: the first poll returns "rejected". The CR gets
// Denied=True and stampCertificateDenied annotates the parent Certificate.
func TestReconcile_SubmitAndRejected(t *testing.T) {
	srv := newCertForgeServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(certResponse{ID: "req-003", Status: "pending"})
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(certResponse{ID: "req-003", Status: "rejected", Reason: "domain not permitted"})
		},
	)
	defer srv.Close()

	parentCert := &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cert", Namespace: "default"},
	}
	cr := newCRForIssuer("my-cr", "default", map[string]string{
		"cert-manager.io/certificate-name": "my-cert",
	})

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(readyClusterIssuerAt(srv.URL),
			credentialsSecret("certforge-credentials", "certforge-system", "tok"),
			cr, parentCert).
		WithStatusSubresource(&cmapi.CertificateRequest{}).
		Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-cr", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("rejected CR should not requeue, got %+v", result)
	}

	updated := &cmapi.CertificateRequest{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-cr", Namespace: "default"}, updated); err != nil {
		t.Fatalf("re-fetch CR: %v", err)
	}
	if !isConditionTrue(updated, cmapi.CertificateRequestConditionDenied) {
		t.Error("expected Denied=True after CertForge rejection")
	}

	// stampCertificateDenied should have annotated the parent Certificate.
	cert := &cmapi.Certificate{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-cert", Namespace: "default"}, cert); err != nil {
		t.Fatalf("re-fetch Certificate: %v", err)
	}
	if cert.Annotations[annotationDeniedAt] == "" {
		t.Error("expected certforge.io/denied-at on parent Certificate after rejection")
	}
}

// ── Submit errors ─────────────────────────────────────────────────────────────

// TestReconcile_PolicyError: CertForge returns 422 on submit. The CR gets the
// "rejected" annotation sentinel, Denied=True, and the parent Certificate is stamped.
func TestReconcile_PolicyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]string{"error": "domain not covered by any DTP"})
	}))
	defer srv.Close()

	parentCert := &cmapi.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cert", Namespace: "default"},
	}
	cr := newCRForIssuer("my-cr", "default", map[string]string{
		"cert-manager.io/certificate-name": "my-cert",
	})

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(readyClusterIssuerAt(srv.URL),
			credentialsSecret("certforge-credentials", "certforge-system", "tok"),
			cr, parentCert).
		WithStatusSubresource(&cmapi.CertificateRequest{}).
		Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-cr", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("policy rejection is terminal — should not requeue, got %+v", result)
	}

	// CR must have the "rejected" sentinel annotation.
	afterPatch := &cmapi.CertificateRequest{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-cr", Namespace: "default"}, afterPatch); err != nil {
		t.Fatalf("re-fetch CR: %v", err)
	}
	if afterPatch.Annotations[annotationRequestID] != "rejected" {
		t.Errorf("expected 'rejected' sentinel annotation, got %q", afterPatch.Annotations[annotationRequestID])
	}
	if !isConditionTrue(afterPatch, cmapi.CertificateRequestConditionDenied) {
		t.Error("expected Denied=True after policy error")
	}

	cert := &cmapi.Certificate{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-cert", Namespace: "default"}, cert); err != nil {
		t.Fatalf("re-fetch Certificate: %v", err)
	}
	if cert.Annotations[annotationDeniedAt] == "" {
		t.Error("expected certforge.io/denied-at on parent Certificate after policy rejection")
	}
}

// TestReconcile_SubmitServerError: a 500 from CertForge is transient; the CR shows
// Ready=False/Pending and requeues.
func TestReconcile_SubmitServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "db down"})
	}))
	defer srv.Close()

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(readyClusterIssuerAt(srv.URL),
			credentialsSecret("certforge-credentials", "certforge-system", "tok"),
			newCRForIssuer("my-cr", "default", nil)).
		WithStatusSubresource(&cmapi.CertificateRequest{}).
		Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-cr", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after transient submit error")
	}
}

// ── requestID annotation paths ────────────────────────────────────────────────

// TestReconcile_RejectedAnnotation: a CR whose requestID annotation is already
// "rejected" is denied immediately — no HTTP calls, no re-submit.
func TestReconcile_RejectedAnnotation(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			w.WriteHeader(http.StatusOK)
			return
		}
		callCount++
		t.Errorf("unexpected HTTP call #%d to %s %s", callCount, r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cr := newCRForIssuer("my-cr", "default", nil)
	cr.Annotations = map[string]string{
		annotationRequestID: "rejected",
	}

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(readyClusterIssuerAt(srv.URL),
			credentialsSecret("certforge-credentials", "certforge-system", "tok"),
			cr).
		WithStatusSubresource(&cmapi.CertificateRequest{}).
		Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-cr", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &cmapi.CertificateRequest{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-cr", Namespace: "default"}, updated); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if !isConditionTrue(updated, cmapi.CertificateRequestConditionDenied) {
		t.Error("expected Denied=True from existing 'rejected' sentinel annotation")
	}
	if callCount > 0 {
		t.Errorf("expected zero CertForge API calls, got %d", callCount)
	}
}

// TestReconcile_ExistingRequestID_Issued: requestID annotation already set from a
// prior submit → Reconcile skips submit and goes straight to poll.
func TestReconcile_ExistingRequestID_Issued(t *testing.T) {
	var submitCalled bool
	srv := newCertForgeServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			submitCalled = true
			t.Error("Submit should not be called when requestID annotation already exists")
			w.WriteHeader(http.StatusInternalServerError)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(certResponse{ID: "req-existing", Status: "issued", Certificate: "---CERT---"})
		},
	)
	defer srv.Close()

	cr := newCRForIssuer("my-cr", "default", nil)
	cr.Annotations = map[string]string{
		annotationRequestID: "req-existing",
	}

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(readyClusterIssuerAt(srv.URL),
			credentialsSecret("certforge-credentials", "certforge-system", "tok"),
			cr).
		WithStatusSubresource(&cmapi.CertificateRequest{}).
		Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-cr", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if submitCalled {
		t.Error("Submit was called despite requestID annotation already being present")
	}

	updated := &cmapi.CertificateRequest{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-cr", Namespace: "default"}, updated); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if string(updated.Status.Certificate) != "---CERT---" {
		t.Errorf("Certificate = %q, want '---CERT---'", updated.Status.Certificate)
	}
}

// ── resolveIssuer: workload identity path ─────────────────────────────────────

// TestResolveIssuer_ClusterIssuer_WorkloadIdentity: a ClusterIssuer using
// workloadIdentity should resolve to a FileTokenSource (not StaticTokenSource).
func TestResolveIssuer_ClusterIssuer_WorkloadIdentity(t *testing.T) {
	tokenPath := createTempTokenFile(t, "eyJhbGciOiJSUzI1NiJ9.wi-token")

	s := testScheme()
	clusterIssuer := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL: "https://app.certgovernance.app",
			WorkloadIdentity: &certforgev1alpha1.WorkloadIdentitySpec{
				Audience:  "https://app.certgovernance.app",
				TokenFile: tokenPath,
			},
		},
		Status: certforgev1alpha1.CertForgeIssuerStatus{Conditions: issuerReadyConditions()},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(clusterIssuer).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	cf, _, err := r.resolveIssuer(context.Background(),
		issuerCR("certforge", "CertForgeClusterIssuer", "default"))
	if err != nil {
		t.Fatalf("resolveIssuer() unexpected error: %v", err)
	}
	if cf == nil {
		t.Fatal("resolveIssuer returned nil client for workload identity")
	}
}

// ── stampCertificateDenied edge cases ─────────────────────────────────────────

// TestReconcile_StampDenied_NoCertObject: when CertForge rejects a CR whose
// parent Certificate has already been GC'd (not present in the store),
// stampCertificateDenied must silently log and return — it must NOT propagate
// the not-found error. The CR's Denied=True condition is the authoritative signal.
func TestReconcile_StampDenied_NoCertObject(t *testing.T) {
	srv := newCertForgeServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(certResponse{ID: "req-004", Status: "pending"})
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(certResponse{ID: "req-004", Status: "rejected", Reason: "policy"})
		},
	)
	defer srv.Close()

	// CR has the certificate-name label, but the parent Certificate is absent.
	cr := newCRForIssuer("my-cr", "default", map[string]string{
		"cert-manager.io/certificate-name": "my-cert",
	})

	s := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(readyClusterIssuerAt(srv.URL),
			credentialsSecret("certforge-credentials", "certforge-system", "tok"),
			cr).
		// Parent Certificate intentionally NOT in the store.
		WithStatusSubresource(&cmapi.CertificateRequest{}).
		Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	// Must succeed — the missing Certificate is a durability no-op, not a failure.
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-cr", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error when parent Certificate is missing: %v", err)
	}

	updated := &cmapi.CertificateRequest{}
	if err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-cr", Namespace: "default"}, updated); err != nil {
		t.Fatalf("re-fetch CR: %v", err)
	}
	// The Denied condition must still be set — stamp failure is not fatal.
	if !isConditionTrue(updated, cmapi.CertificateRequestConditionDenied) {
		t.Error("expected Denied=True even when stampCertificateDenied cannot fetch parent Certificate")
	}
}

// TestResolveIssuer_NoCredentialSource: issuer has neither authSecretRef nor
// workloadIdentity → error mentioning credential source.
func TestResolveIssuer_NoCredentialSource(t *testing.T) {
	s := testScheme()
	clusterIssuer := &certforgev1alpha1.CertForgeClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{Name: "certforge"},
		Spec: certforgev1alpha1.CertForgeIssuerSpec{
			URL: "https://app.certgovernance.app",
		},
		Status: certforgev1alpha1.CertForgeIssuerStatus{Conditions: issuerReadyConditions()},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(clusterIssuer).Build()
	r := &CertificateRequestReconciler{Client: fakeClient}

	_, _, err := r.resolveIssuer(context.Background(),
		issuerCR("certforge", "CertForgeClusterIssuer", "default"))
	if err == nil {
		t.Fatal("expected error when issuer has no credential source")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("error %q should mention credential source requirement", err.Error())
	}
}
