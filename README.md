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
- A CertForge account at [certgovernance.app](https://certgovernance.app)

### CertForge setup required before installation

The issuer will reject certificate requests if CertForge is not configured for your domains.
Complete these steps first:

1. **Create an account** at [certgovernance.app](https://certgovernance.app) and set up your organization.

2. **Add your domains** — in CertForge, create a Domain Trust Profile (DTP) that covers the
   domains your Kubernetes workloads will request certificates for. The DTP defines which CA to
   use, whether wildcards are permitted, and whether requests require manual approval.

   Example: if your workloads will request certs for `*.internal.example.com`, your DTP must
   include that pattern (or `*.example.com`). Requests for domains not covered by any DTP will
   be rejected with an `InvalidRequest` condition on the `CertificateRequest`.

3. **Generate an API token** — in CertForge, go to Settings → API Keys and create a token
   scoped to your organization. This token is what you supply during Helm installation.

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
3. CertForge checks the request against your Domain Trust Profiles
   - If no DTP covers the requested domains, the `CertificateRequest` is marked `InvalidRequest` and no retry occurs — add the domain to a DTP in CertForge to resolve
4. If auto-approval is configured, the certificate is issued immediately
5. If manual approval is required, the request waits in CertForge's approval queue
6. The controller polls every 15 seconds until issued or denied
7. On issuance, the signed certificate is written back to the `CertificateRequest`
8. cert-manager stores the key + certificate as a Kubernetes Secret

### Troubleshooting

If a `Certificate` stays in a pending state, check the underlying `CertificateRequest`:

```bash
kubectl describe certificaterequest <name> -n <namespace>
```

| Condition | Reason | Cause |
|-----------|--------|-------|
| `InvalidRequest=True` | `PolicyViolation` | Domain not covered by any CertForge DTP, or wildcard not permitted |
| `Denied=True` | `Denied` | Request was manually denied in the CertForge approval queue |
| `Ready=False` | `Pending` | Waiting for approval in CertForge, or transient connectivity issue |

## Building

```bash
go build ./...
docker build -t certforge-issuer:dev .
```

## License

Apache 2.0
