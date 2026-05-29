# certforge-issuer

cert-manager external issuer that adds policy enforcement, approval workflows, and a full
audit trail to every certificate request in your cluster.

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

---

cert-manager automates certificate renewal. It doesn't control *who* can request *what*.
**certforge-issuer** bridges cert-manager to CertForge's policy engine so every certificate
request is evaluated against your Domain Trust Profiles — and your security team gets an
immutable audit trail of what was issued, when, and who approved it.

```
Pod → cert-manager → certforge-issuer → CertForge API → CA
                                      ← signed cert   ←
```

## Why

Without a policy layer, any workload with a cert-manager `Certificate` resource can request a
certificate for any domain in your cluster — `*.production.example.com`, internal CA subjects,
anything. There is nothing to stop it.

**certforge-issuer adds:**

- **Domain Trust Profiles** — define which CAs, SANs, and wildcard patterns are valid per domain
- **Approval workflows** — route certificate requests to a human approver before issuance
- **Policy enforcement** — requests that don't match a Trust Profile are denied before reaching a CA
- **Audit trail** — every request, approval, and renewal is logged with actor, timestamp, and outcome

Your cert-manager setup stays exactly as-is. Add certforge-issuer as the external issuer and
governance is in place without changing a single workload manifest.

## Quick Start

**1. Create a free account** at [certgovernance.app](https://certgovernance.app), add your domains,
and generate an API token under Settings → API Keys.

**2. Install the issuer:**

```bash
helm install certforge-issuer oci://ghcr.io/certforge/charts/certforge-issuer \
  --namespace certforge-system \
  --create-namespace \
  --set certforge.url=https://app.certgovernance.app \
  --set certforge.token=<your-api-token>
```

**3. Create an issuer resource:**

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

**4. Reference it from your Certificate:**

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
evaluation, and the signed certificate is returned once approved and issued.

## Prerequisites

- Kubernetes 1.24+
- cert-manager v1.14+
- A CertForge account — [free tier](https://certgovernance.app) includes 100 certificates,
  full approval workflows, and audit log export

### CertForge setup required before installation

The issuer will reject certificate requests if CertForge is not configured for your domains.
Complete these steps first:

1. **Create an account** at [certgovernance.app](https://certgovernance.app) and set up your organization.

2. **Add your domains** — in CertForge, create a Domain Trust Profile (DTP) that covers the
   domains your Kubernetes workloads will request certificates for. The DTP defines which CA to
   use, whether wildcards are permitted, and whether requests require manual approval.

   Example: if your workloads will request certs for `*.internal.example.com`, your DTP must
   include that pattern (or `*.example.com`). Requests for domains not covered by any DTP are
   rejected with an `InvalidRequest` condition on the `CertificateRequest`.

3. **Generate an API token** — in CertForge, go to Settings → API Keys and create a token
   scoped to your organization. This token is what you supply during Helm installation.

## Usage

### Cluster-scoped Issuer

For issuing certificates across all namespaces:

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

### Manual Installation (without Helm)

```bash
kubectl apply -f https://raw.githubusercontent.com/CertForge-LLC/certforge-issuer/main/config/crd/certforge-issuer.yaml
kubectl apply -f https://raw.githubusercontent.com/CertForge-LLC/certforge-issuer/main/config/rbac/rbac.yaml

kubectl create secret generic certforge-credentials \
  --namespace certforge-system \
  --from-literal=token=<your-api-token>

kubectl apply -f https://raw.githubusercontent.com/CertForge-LLC/certforge-issuer/main/config/manager/deployment.yaml
```

## How It Works

1. cert-manager creates a `CertificateRequest` with `issuerRef.group: certforge.io`
2. The controller POSTs the CSR to `POST /api/v1/certificate-requests`
3. CertForge checks the request against your Domain Trust Profiles
   - If no DTP covers the requested domains, the `CertificateRequest` is marked `InvalidRequest`
     and no retry occurs — add the domain to a DTP in CertForge to resolve
4. If auto-approval is configured, the certificate is issued immediately
5. If manual approval is required, the request waits in CertForge's approval queue
6. The controller polls every 15 seconds until issued or denied
7. On issuance, the signed certificate is written back to the `CertificateRequest`
8. cert-manager stores the key + certificate as a Kubernetes Secret

### Troubleshooting

If a `Certificate` stays pending, check the underlying `CertificateRequest`:

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

## Get Started Free

[certgovernance.app](https://certgovernance.app) — 100 certificates, full approval workflows,
audit log and export. No credit card required.

## License

Apache 2.0
