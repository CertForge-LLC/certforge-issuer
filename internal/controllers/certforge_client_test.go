package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func newTestServer(statusCode int, body interface{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
		if body != nil {
			json.NewEncoder(w).Encode(body)
		}
	}))
}

// ── Ping ─────────────────────────────────────────────────────────────────────

func TestPing_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/v1/ping" {
			t.Errorf("unexpected path %s, want /api/v1/ping", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("missing Authorization header")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	if err := newClient(srv.URL, "test-token").Ping(context.Background()); err != nil {
		t.Fatalf("Ping() unexpected error: %v", err)
	}
}

func TestPing_Unauthorized(t *testing.T) {
	srv := newTestServer(http.StatusUnauthorized, nil)
	defer srv.Close()

	err := newClient(srv.URL, "bad-token").Ping(context.Background())
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Errorf("error %q should mention '401 Unauthorized'", err.Error())
	}
}

func TestPing_Forbidden(t *testing.T) {
	srv := newTestServer(http.StatusForbidden, nil)
	defer srv.Close()

	err := newClient(srv.URL, "no-scope-token").Ping(context.Background())
	if err == nil {
		t.Fatal("expected error for 403")
	}
	if !strings.Contains(err.Error(), "403 Forbidden") {
		t.Errorf("error %q should mention '403 Forbidden'", err.Error())
	}
}

func TestPing_UnexpectedStatus(t *testing.T) {
	srv := newTestServer(http.StatusInternalServerError, nil)
	defer srv.Close()

	err := newClient(srv.URL, "tok").Ping(context.Background())
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error %q should mention 'HTTP 500'", err.Error())
	}
}

func TestPing_ConnectivityError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // close before making the request

	err := newClient(srv.URL, "tok").Ping(context.Background())
	if err == nil {
		t.Fatal("expected error for closed server")
	}
	if !strings.Contains(err.Error(), "cannot reach CertForge") {
		t.Errorf("error %q should mention 'cannot reach CertForge'", err.Error())
	}
}

// ── Submit ────────────────────────────────────────────────────────────────────

func TestSubmit_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/certificate-requests" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("missing Authorization header")
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(certResponse{ID: "req-abc123", Status: "pending"})
	}))
	defer srv.Close()

	id, err := newClient(srv.URL, "tok").Submit(context.Background(), "---CSR---", "default", "my-cert", "")
	if err != nil {
		t.Fatalf("Submit() unexpected error: %v", err)
	}
	if id != "req-abc123" {
		t.Errorf("got id %q, want %q", id, "req-abc123")
	}
}

func TestSubmit_WithIssuanceProfile(t *testing.T) {
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(certResponse{ID: "req-xyz", Status: "pending"})
	}))
	defer srv.Close()

	_, err := newClient(srv.URL, "tok").Submit(context.Background(), "---CSR---", "ns", "name", "profile-99")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["issuance_profile_id"] != "profile-99" {
		t.Errorf("issuance_profile_id = %v, want profile-99", gotBody["issuance_profile_id"])
	}
}

func TestSubmit_PolicyError422(t *testing.T) {
	srv := newTestServer(http.StatusUnprocessableEntity, map[string]string{"error": "domain not covered by any DTP"})
	defer srv.Close()

	_, err := newClient(srv.URL, "tok").Submit(context.Background(), "---CSR---", "ns", "name", "")
	if err == nil {
		t.Fatal("expected error for 422")
	}
	var policyErr *PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("expected *PolicyError, got %T: %v", err, err)
	}
	if !strings.Contains(policyErr.Message, "domain not covered by any DTP") {
		t.Errorf("PolicyError.Message = %q, want it to contain server message", policyErr.Message)
	}
}

func TestSubmit_PolicyError_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := newClient(srv.URL, "tok").Submit(context.Background(), "---CSR---", "ns", "name", "")
	var policyErr *PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("expected *PolicyError even for non-JSON body, got %T: %v", err, err)
	}
	// Falls back to raw body
	if policyErr.Message != "not json" {
		t.Errorf("PolicyError.Message = %q, want raw body", policyErr.Message)
	}
}

func TestSubmit_ServerError(t *testing.T) {
	srv := newTestServer(http.StatusInternalServerError, map[string]string{"error": "internal server error"})
	defer srv.Close()

	_, err := newClient(srv.URL, "tok").Submit(context.Background(), "---CSR---", "ns", "name", "")
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if strings.Contains(err.Error(), "PolicyError") {
		t.Error("500 should not produce a PolicyError")
	}
}

// ── Poll ──────────────────────────────────────────────────────────────────────

func TestPoll_EmptyID(t *testing.T) {
	_, err := newClient("http://unused", "tok").Poll(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty ID")
	}
	if !strings.Contains(err.Error(), "empty request ID") {
		t.Errorf("error %q should mention 'empty request ID'", err.Error())
	}
}

func TestPoll_Issued(t *testing.T) {
	want := certResponse{ID: "r1", Status: "issued", Certificate: "---CERT PEM---"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/certificate-requests/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	got, err := newClient(srv.URL, "tok").Poll(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Poll() unexpected error: %v", err)
	}
	if got.Status != "issued" {
		t.Errorf("Status = %q, want issued", got.Status)
	}
	if got.Certificate != "---CERT PEM---" {
		t.Errorf("Certificate = %q, want '---CERT PEM---'", got.Certificate)
	}
}

func TestPoll_Pending(t *testing.T) {
	srv := newTestServer(http.StatusOK, certResponse{ID: "r1", Status: "pending", Reason: "awaiting approval"})
	defer srv.Close()

	got, err := newClient(srv.URL, "tok").Poll(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Poll() unexpected error: %v", err)
	}
	if got.Status != "pending" {
		t.Errorf("Status = %q, want pending", got.Status)
	}
}

func TestPoll_Rejected(t *testing.T) {
	srv := newTestServer(http.StatusOK, certResponse{ID: "r1", Status: "rejected", Reason: "policy violation"})
	defer srv.Close()

	got, err := newClient(srv.URL, "tok").Poll(context.Background(), "r1")
	if err != nil {
		t.Fatalf("Poll() unexpected error: %v", err)
	}
	if got.Status != "rejected" {
		t.Errorf("Status = %q, want rejected", got.Status)
	}
	if got.Reason != "policy violation" {
		t.Errorf("Reason = %q, want 'policy violation'", got.Reason)
	}
}

func TestPoll_ServerError(t *testing.T) {
	srv := newTestServer(http.StatusInternalServerError, map[string]string{"error": "db unavailable"})
	defer srv.Close()

	_, err := newClient(srv.URL, "tok").Poll(context.Background(), "r1")
	if err == nil {
		t.Fatal("expected error for 500")
	}
}

// ── apiErrMessage ─────────────────────────────────────────────────────────────

func TestAPIErrMessage_ValidJSON(t *testing.T) {
	got := apiErrMessage([]byte(`{"error":"domain not in DTP"}`))
	if got != "domain not in DTP" {
		t.Errorf("got %q, want 'domain not in DTP'", got)
	}
}

func TestAPIErrMessage_FallsBackToRawBody(t *testing.T) {
	raw := "plain text error"
	got := apiErrMessage([]byte(raw))
	if got != raw {
		t.Errorf("got %q, want %q", got, raw)
	}
}

// ── FileTokenSource ───────────────────────────────────────────────────────────

// TestFileTokenSource_HappyPath: reads a token from a file and trims whitespace.
// Simulates a projected ServiceAccount token file written by the kubelet.
func TestFileTokenSource_HappyPath(t *testing.T) {
	f, err := os.CreateTemp("", "certforge-token-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("my-sa-token\n"); err != nil { // kubelet appends a newline
		t.Fatal(err)
	}
	f.Close()

	ts := FileTokenSource{path: f.Name()}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token() unexpected error: %v", err)
	}
	if tok != "my-sa-token" {
		t.Errorf("token = %q, want %q (should strip trailing newline)", tok, "my-sa-token")
	}
}

// TestPolicyError_Error: the Error() method returns the message — needed by
// errors.As callers and the %v formatting in log output.
func TestPolicyError_Error(t *testing.T) {
	e := &PolicyError{Message: "domain not covered by any DTP"}
	if got := e.Error(); got != "domain not covered by any DTP" {
		t.Errorf("Error() = %q, want message string", got)
	}
}

// TestFileTokenSource_MissingFile: a missing token file returns a descriptive
// error — the controller should surface this as PingFailed/token-read-error.
func TestFileTokenSource_MissingFile(t *testing.T) {
	ts := FileTokenSource{path: "/nonexistent/certforge/token"}
	_, err := ts.Token()
	if err == nil {
		t.Fatal("expected error for missing token file")
	}
	if !strings.Contains(err.Error(), "read token file") {
		t.Errorf("error %q should mention 'read token file'", err.Error())
	}
}
