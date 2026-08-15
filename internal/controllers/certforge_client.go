package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ─── TokenSource ──────────────────────────────────────────────────────────────

// TokenSource provides a bearer token for each CertForge API request.
// The interface allows both static tokens (Secret-backed) and dynamic tokens
// (file-backed projected ServiceAccount tokens that rotate hourly).
type TokenSource interface {
	// Token returns the current bearer token. For file-backed sources the file
	// is re-read on every call so rotation is transparent to callers.
	Token() (string, error)
}

// StaticTokenSource returns the same token on every call.
type StaticTokenSource struct{ token string }

func (s StaticTokenSource) Token() (string, error) { return s.token, nil }

// FileTokenSource reads the token from a file on every call.
// Kubernetes projected ServiceAccount tokens are written to a file by the
// kubelet and replaced in-place when they rotate (typically every hour).
type FileTokenSource struct{ path string }

func (f FileTokenSource) Token() (string, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", f.path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// PolicyError is returned when CertForge rejects a CSR due to policy (HTTP 422).
// It is a terminal error — retrying will not help until the policy is updated.
type PolicyError struct{ Message string }

func (e *PolicyError) Error() string { return e.Message }

// apiErrMessage extracts the human-readable message from a CertForge error
// body. The API returns {"error":"..."} JSON; if parsing fails the raw body
// is returned so no information is lost.
func apiErrMessage(b []byte) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &envelope) == nil && envelope.Error != "" {
		return envelope.Error
	}
	return string(b)
}

// certforgeClient talks to the CertForge REST API.
type certforgeClient struct {
	baseURL string
	ts      TokenSource
	http    *http.Client
}

// newClient creates a client with a static token (Secret-backed credential path).
func newClient(baseURL, token string) *certforgeClient {
	return &certforgeClient{
		baseURL: baseURL,
		ts:      StaticTokenSource{token: token},
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// newClientWithTokenSource creates a client with a dynamic token source
// (e.g. a projected ServiceAccount token file that rotates hourly).
func newClientWithTokenSource(baseURL string, ts TokenSource) *certforgeClient {
	return &certforgeClient{
		baseURL: baseURL,
		ts:      ts,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

type submitRequest struct {
	CSR               string `json:"csr"`
	Source            string `json:"source"`
	Namespace         string `json:"namespace,omitempty"`
	Name              string `json:"name,omitempty"`
	IssuanceProfileID string `json:"issuance_profile_id,omitempty"`
}

type certResponse struct {
	ID          string `json:"id"`
	Status      string `json:"status"`               // pending | issued | rejected (human) | denied (policy)
	Certificate string `json:"certificate,omitempty"` // PEM when issued
	Reason      string `json:"reason,omitempty"`
}

// Submit posts a CSR to CertForge and returns the request ID.
// issuanceProfileID is optional; pass "" to use the DTP default.
func (c *certforgeClient) Submit(ctx context.Context, csrPEM, namespace, name, issuanceProfileID string) (string, error) {
	token, err := c.ts.Token()
	if err != nil {
		return "", fmt.Errorf("get token: %w", err)
	}
	body, _ := json.Marshal(submitRequest{
		CSR:               csrPEM,
		Source:            "cert-manager",
		Namespace:         namespace,
		Name:              name,
		IssuanceProfileID: issuanceProfileID,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.baseURL+"/api/v1/certificate-requests", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST certificate-requests: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return "", &PolicyError{Message: apiErrMessage(b)}
	}
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return "", fmt.Errorf("certforge returned %d: %s", resp.StatusCode, apiErrMessage(b))
	}

	var out certResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return out.ID, nil
}

// Ping makes a lightweight authenticated GET to /api/v1/ping to verify that the
// token is valid and the CertForge server is reachable. It returns nil on success,
// or a descriptive error that distinguishes auth failures from connectivity failures.
func (c *certforgeClient) Ping(ctx context.Context) error {
	token, err := c.ts.Token()
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/ping", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach CertForge at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain to allow connection reuse

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("token rejected by CertForge (401 Unauthorized) — check the token in your credentials Secret")
	case http.StatusForbidden:
		return fmt.Errorf("token lacks required scope (403 Forbidden) — token needs read and enroll scopes")
	default:
		return fmt.Errorf("unexpected response from CertForge ping: HTTP %d", resp.StatusCode)
	}
}

// Poll checks the status of a previously submitted request.
func (c *certforgeClient) Poll(ctx context.Context, id string) (certResponse, error) {
	if id == "" {
		return certResponse{}, fmt.Errorf("empty request ID")
	}
	token, err := c.ts.Token()
	if err != nil {
		return certResponse{}, fmt.Errorf("get token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET",
		c.baseURL+"/api/v1/certificate-requests/"+id, nil)
	if err != nil {
		return certResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return certResponse{}, fmt.Errorf("GET certificate-requests/%s: %w", id, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return certResponse{}, fmt.Errorf("certforge returned %d: %s", resp.StatusCode, apiErrMessage(b))
	}

	var out certResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return certResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}
