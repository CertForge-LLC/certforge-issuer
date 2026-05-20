# certforge-issuer

A [cert-manager](https://cert-manager.io) external issuer for [CertForge](https://certgovernance.app) certificate governance.

## Overview

`certforge-issuer` bridges Kubernetes workloads using cert-manager with CertForge's policy engine.
When cert-manager needs to issue a certificate, this controller intercepts the request, submits the
CSR to CertForge for policy evaluation and approval, then returns the signed certificate back to
cert-manager — which stores it as a Kubernetes Secret as usual.

```
Pod → cert-manager → CertForge Issuer → CertForge API → CA
                                      ← signed cert   ←
```

## Prerequisites

- Kubernetes 1.24+
- cert-manager v1.14+
- A CertForge account and API token

## Installation

### Helm

```bash
helm install certforge-issuer oci://ghcr.io/certforge/charts/certforge-issuer \
  --namespace certforge-system \
  --create-namespace \
  --set certforge.url=https://app.certgovernance.app \
  --set certforge.token=<your-api-token>
```

### Manual

```bash
kubectl apply -f https://raw.githubusercontent.com/CertForge-LLC/certforge-issuer/main/config/crd/certforge-issuer.yaml
kubectl apply -f https://raw.githubusercontent.com/CertForge-LLC/certforge-issuer/main/config/rbac/rbac.yaml

# Create credentials secret
kubectl create secret generic certforge-credentials \
  --namespace certforge-system \
  --from-literal=token=<your-api-token>

kubectl apply -f https://raw.githubusercontent.com/CertForge-LLC/certforge-issuer/main/config/manager/deployment.yaml
```

## Usage

### Namespaced Issuer

```yaml
apiVersion: certforge.io/v1alpha1
kind: CertForgeIssuer
metadata:
  name: certforge
  namespace: default
spec:
  url: https://app.certgovernance.app
  authSecretRef:
    name: certforge-credentials
```

### Cluster-scoped Issuer

```yaml
apiVersion: certforge.io/v1alpha1
kind: CertForgeClusterIssuer
metadata:
  name: certforge
spec:
  url: https://app.certgovernance.app
  authSecretRef:
    name: certforge-credentials
```

The secret must exist in the `certforge-system` namespace for `CertForgeClusterIssuer`.

### Certificate

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: my-cert
  namespace: default
spec:
  secretName: my-cert-tls
  dnsNames:
    - my-service.example.com
  issuerRef:
    name: certforge
    kind: CertForgeIssuer
    group: certforge.io
```

cert-manager creates a `CertificateRequest`, the controller submits it to CertForge for policy
evaluation, and the resulting certificate is returned once approved and issued.

## How It Works

1. cert-manager creates a `CertificateRequest` with `issuerRef.group: certforge.io`
2. The controller intercepts and POSTs the CSR to `POST /api/v1/certificate-requests`
3. CertForge evaluates the request against your domain trust policies
4. If auto-approval is configured, the certificate is issued immediately
5. If manual approval is required, the request waits in CertForge's approval queue
6. The controller polls every 15 seconds until issued or denied
7. On issuance, the signed certificate is written back to the `CertificateRequest`
8. cert-manager stores the key + certificate as a Kubernetes Secret

## Building

```bash
go build ./...
docker build -t certforge-issuer:dev .
```

## License

Apache 2.0
