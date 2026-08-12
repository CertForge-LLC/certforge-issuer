package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

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
	token   string
	http    *http.Client
}

func newClient(baseURL, token string) *certforgeClient {
	return &certforgeClient{
		baseURL: baseURL,
		token:   token,
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
	req.Header.Set("Authorization", "Bearer "+c.token)

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

// Poll checks the status of a previously submitted request.
func (c *certforgeClient) Poll(ctx context.Context, id string) (certResponse, error) {
	if id == "" {
		return certResponse{}, fmt.Errorf("empty request ID")
	}
	req, err := http.NewRequestWithContext(ctx, "GET",
		c.baseURL+"/api/v1/certificate-requests/"+id, nil)
	if err != nil {
		return certResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

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
