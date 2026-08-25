// certforge-issuer-enroll enrolls the certforge-issuer controller with CertForge
// and stores the resulting mTLS credentials in a Kubernetes Secret.
//
// Usage:
//
//	certforge-issuer-enroll \
//	  --token  <one-time enrollment token from CertForge UI>  \
//	  --url    https://app.certgov.app                        \
//	  --label  prod-cluster                                   \
//	  --secret certforge-mtls                                 \
//	  [--namespace certforge-system]                          \
//	  [--kubeconfig ~/.kube/config]
//
// After enrollment the secret can be referenced via mtlsSecretRef in the
// CertForgeIssuer or CertForgeClusterIssuer CR:
//
//	spec:
//	  url: https://app.certgov.app
//	  mtlsSecretRef:
//	    name: certforge-mtls
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	token := flag.String("token", "", "one-time enrollment token from CertForge UI (required)")
	certforgeURL := flag.String("url", "https://app.certgov.app", "CertForge dashboard URL")
	label := flag.String("label", "issuer", "label for this agent in CertForge")
	secretName := flag.String("secret", "certforge-mtls", "Kubernetes Secret name to create/update")
	namespace := flag.String("namespace", "certforge-system", "Kubernetes namespace for the Secret")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig (default: in-cluster or KUBECONFIG env)")
	flag.Parse()

	if *token == "" {
		fmt.Fprintln(os.Stderr, "error: --token is required")
		fmt.Fprintln(os.Stderr, "  Generate one in CertForge → Settings → Agent Tokens (type: issuer)")
		os.Exit(1)
	}

	// 1. Generate ECDSA P-384 key pair.
	fmt.Println("Generating ECDSA P-384 key pair...")
	privateKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		log.Fatalf("generate key: %v", err)
	}
	csrTmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("issuer:%s", *label)},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, privateKey)
	if err != nil {
		log.Fatalf("create CSR: %v", err)
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))

	// 2. POST to /v1/agent/enroll on the dashboard port.
	enrollURL := strings.TrimRight(*certforgeURL, "/") + "/v1/agent/enroll"
	fmt.Printf("Enrolling with CertForge at %s...\n", enrollURL)
	payload, _ := json.Marshal(map[string]string{
		"token":      *token,
		"csr_pem":    csrPEM,
		"label":      *label,
		"agent_type": "issuer",
	})
	hc := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec — bootstrap only, CA pinned after
		},
	}
	resp, err := hc.Post(enrollURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Fatalf("enroll request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		log.Fatalf("enroll failed (%s): %s", resp.Status, b)
	}

	var result struct {
		CertPEM        string `json:"cert_pem"`
		CAPEM          string `json:"ca_pem"`
		MTLSEndpoint   string `json:"mtls_endpoint"`    // host:port
		MTLSServerCert string `json:"mtls_server_cert"` // pinned server cert PEM
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Fatalf("decode response: %v", err)
	}
	if result.CertPEM == "" || result.CAPEM == "" {
		log.Fatalf("enrollment response missing cert_pem or ca_pem")
	}

	// 3. Encode client key.
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		log.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// 4. Parse host and port from the returned mTLS endpoint.
	mtlsHost := ""
	mtlsPort := "8443"
	if result.MTLSEndpoint != "" {
		ep := result.MTLSEndpoint
		if i := strings.LastIndex(ep, ":"); i > strings.LastIndex(ep, "]") {
			mtlsPort = ep[i+1:]
			mtlsHost = ep[:i]
		} else {
			mtlsHost = ep
		}
	}

	// 5. Build the K8s Secret.
	secretData := map[string][]byte{
		"client.crt": []byte(result.CertPEM),
		"client.key": keyPEM,
		"ca.crt":     []byte(result.CAPEM),
		"server.crt": []byte(result.MTLSServerCert),
		"mtls_host":  []byte(mtlsHost),
		"mtls_port":  []byte(mtlsPort),
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      *secretName,
			Namespace: *namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "certforge-issuer-enroll",
				"certforge.io/agent-type":      "issuer",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: secretData,
	}

	// 6. Write the Secret to Kubernetes.
	cfg, err := clientcmd.BuildConfigFromFlags("", resolveKubeconfig(*kubeconfig))
	if err != nil {
		log.Fatalf("build kubeconfig: %v", err)
	}
	k8s, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("build k8s client: %v", err)
	}
	ctx := context.Background()
	existing, getErr := k8s.CoreV1().Secrets(*namespace).Get(ctx, *secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) {
		if _, err := k8s.CoreV1().Secrets(*namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			log.Fatalf("create secret %s/%s: %v", *namespace, *secretName, err)
		}
		fmt.Printf("✅ Created Secret %s/%s\n", *namespace, *secretName)
	} else if getErr != nil {
		log.Fatalf("get secret %s/%s: %v", *namespace, *secretName, getErr)
	} else {
		existing.Data = secretData
		if _, err := k8s.CoreV1().Secrets(*namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			log.Fatalf("update secret %s/%s: %v", *namespace, *secretName, err)
		}
		fmt.Printf("✅ Updated Secret %s/%s\n", *namespace, *secretName)
	}

	// 7. Print summary.
	certBlock, _ := pem.Decode([]byte(result.CertPEM))
	cert, _ := x509.ParseCertificate(certBlock.Bytes)
	fmt.Println()
	fmt.Println("Enrollment successful!")
	if cert != nil {
		fmt.Printf("  CN:          %s\n", cert.Subject.CommonName)
		fmt.Printf("  Valid until: %s\n", cert.NotAfter.Format("2006-01-02"))
	}
	if result.MTLSEndpoint != "" {
		fmt.Printf("  mTLS endpoint: %s\n", result.MTLSEndpoint)
	}
	fmt.Println()
	fmt.Println("Next: reference the Secret in your CertForgeIssuer or CertForgeClusterIssuer:")
	fmt.Println()
	fmt.Printf("  spec:\n")
	fmt.Printf("    url: %s\n", *certforgeURL)
	fmt.Printf("    mtlsSecretRef:\n")
	fmt.Printf("      name: %s\n", *secretName)
}

func resolveKubeconfig(path string) string {
	if path != "" {
		return path
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return env
	}
	return clientcmd.RecommendedHomeFile
}
