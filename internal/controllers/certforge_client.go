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
	CSR       string `json:"csr"`
	Source    string `json:"source"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

type certResponse struct {
	ID          string `json:"id"`
	Status      string `json:"status"`               // pending | issued | denied
	Certificate string `json:"certificate,omitempty"` // PEM when issued
	Reason      string `json:"reason,omitempty"`
}

// Submit posts a CSR to CertForge and returns the request ID.
func (c *certforgeClient) Submit(ctx context.Context, csrPEM, namespace, name string) (string, error) {
	body, _ := json.Marshal(submitRequest{
		CSR:       csrPEM,
		Source:    "cert-manager",
		Namespace: namespace,
		Name:      name,
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
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("policy violation: %s", string(b))
	}
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("certforge returned %d: %s", resp.StatusCode, string(b))
	}

	var out certResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return out.ID, nil
}

// Poll checks the status of a previously submitted request.
func (c *certforgeClient) Poll(ctx context.Context, id string) (certResponse, error) {
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
		b, _ := io.ReadAll(resp.Body)
		return certResponse{}, fmt.Errorf("certforge returned %d: %s", resp.StatusCode, string(b))
	}

	var out certResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return certResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}
