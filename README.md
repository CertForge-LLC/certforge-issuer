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

## Prerequisites

- Kubernetes 1.24+
- cert-manager v1.14+
- A CertForge account — [free tier](https://certgovernance.app) includes 100 certificates,
  25 domains, full approval workflows, and audit log export. No credit card required.

### CertForge setup (required before installation)

The issuer will reject certificate requests if CertForge is not configured for your domains.
Complete these steps first — they take about five minutes.

1. **Create an account** at [certgovernance.app](https://certgovernance.app) and set up your organization.

2. **Add your domains** — in CertForge, create a Domain Trust Profile (DTP) that covers the
   domains your Kubernetes workloads will request certificates for. The DTP defines which CA to
   use, whether wildcards are permitted, and whether requests require manual approval.

   Example: if your workloads will request certs for `*.internal.example.com`, your DTP must
   include that pattern (or `*.example.com`). Requests for domains not covered by any DTP are
   rejected with an `InvalidRequest` condition on the `CertificateRequest`.

3. **Generate an API token** — go to Settings → API Keys and create a token scoped to your
   organization. You'll supply this token during Helm installation in the next step.

## Quick Start

**Install the issuer** (the Helm chart creates a `certforge-credentials` Secret in
`certforge-system` automatically from the token you provide):

```bash
helm install certforge-issuer oci://ghcr.io/certforge-llc/charts/certforge-issuer \
  --namespace certforge-system \
  --create-namespace \
  --set certforge.url=https://app.certgovernance.app \
  --set certforge.token=<your-api-token>
```

**Create an issuer resource** in each namespace that needs certificates (or use
`CertForgeClusterIssuer` for cluster-wide access — see [Usage](#usage)):

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

> The `certforge-credentials` Secret was created by the Helm chart above.
> For `CertForgeClusterIssuer`, the Secret must be in the `certforge-system` namespace
> (or the namespace set in `secretNamespace`).

**Reference it from your Certificate:**

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

### Verify before deploying to a cluster

CertForge includes a built-in integration test that simulates the full cert-manager flow — policy
evaluation, approval, and CA issuance — without needing a live Kubernetes cluster.

Go to **Integrations → cert-manager** in the CertForge UI and click **Deploy & Watch**. It
generates a real CSR, submits it through your Trust Profile and approval configuration, and shows
the result in real time. The issued certificate appears in Certificate Requests exactly as it would
from a live cluster.

Use this to confirm your Trust Profile covers the domains you intend to use and that your CA
configuration is working before installing certforge-issuer.

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

The Secret is read from the `certforge-system` namespace by default. Use `secretNamespace` to
override this if your credentials live elsewhere.

### Data Residency

CertForge operates in **US East** and **EU West**. Your organization's certificates, keys, and
audit data are pinned to a single region by the API token — no cross-border data flows occur.

Set `url` to the appropriate endpoint for your org's region:

| Region | URL |
|--------|-----|
| US East (default) | `https://app.certgovernance.app` |
| EU West (GDPR-aligned) | `https://eu.certgovernance.app` |

The `url` and API token together route every certificate request to the correct region. No other
configuration is needed.

**EU West example:**

```yaml
apiVersion: certforge.io/v1alpha1
kind: CertForgeClusterIssuer
metadata:
  name: certforge-eu
spec:
  url: https://eu.certgovernance.app
  authSecretRef:
    name: certforge-eu-credentials
```

```bash
kubectl create secret generic certforge-eu-credentials \
  --namespace certforge-system \
  --from-literal=token=<your-eu-org-api-token>
```

### Issuance Profiles

If your Domain Trust Profile has multiple CA configurations (for example, Let's Encrypt for
external certs and an internal CA for service-mesh certs), you can pin an issuance profile at the
issuer level or override it per Certificate.

**Issuer-level default** — all certs from this issuer use the specified profile:

```yaml
apiVersion: certforge.io/v1alpha1
kind: CertForgeClusterIssuer
metadata:
  name: certforge-internal
spec:
  url: https://app.certgovernance.app
  authSecretRef:
    name: certforge-credentials
  issuanceProfileID: "your-internal-ca-profile-id"
```

**Per-Certificate override** — set the `certforge.io/issuance-profile` annotation on a
`Certificate` resource to override the issuer default for that certificate only:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: external-cert
  namespace: default
  annotations:
    certforge.io/issuance-profile: "your-letsencrypt-profile-id"
spec:
  secretName: external-cert-tls
  dnsNames:
    - api.example.com
  issuerRef:
    name: certforge-internal
    kind: CertForgeClusterIssuer
    group: certforge.io
```

The annotation takes precedence over `issuanceProfileID` in the issuer spec. If neither is set,
CertForge uses the default profile configured on the matching Domain Trust Profile.

### Custom Secret Namespace (ClusterIssuer)

By default `CertForgeClusterIssuer` reads credentials from `certforge-system`. Override this with
`secretNamespace`:

```yaml
apiVersion: certforge.io/v1alpha1
kind: CertForgeClusterIssuer
metadata:
  name: certforge
spec:
  url: https://app.certgovernance.app
  authSecretRef:
    name: certforge-credentials
  secretNamespace: my-secrets-namespace
```

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

## Spec Reference

### CertForgeIssuerSpec (shared by `CertForgeIssuer` and `CertForgeClusterIssuer`)

| Field | Required | Description |
|-------|----------|-------------|
| `url` | Yes | Base URL of the CertForge server. Determines the data region. |
| `authSecretRef.name` | Yes | Name of the Secret containing a `token` key with the API bearer token. |
| `issuanceProfileID` | No | Default issuance profile ID for all certs from this issuer. Overrides the DTP default. Can be overridden per Certificate via the `certforge.io/issuance-profile` annotation. |
| `secretNamespace` | No | Namespace to read the credentials Secret from (`CertForgeClusterIssuer` only). Defaults to `certforge-system`. |

### Annotations

| Annotation | Resource | Description |
|------------|----------|-------------|
| `certforge.io/issuance-profile` | `Certificate` | Overrides `issuanceProfileID` from the issuer spec for this certificate only. |

## Building

```bash
go build ./...
docker build -t certforge-issuer:dev .
```

## Get Started Free

[certgovernance.app](https://certgovernance.app) — 100 certificates, 25 domains, full approval
workflows, audit log and export. No credit card required.

## License

Apache 2.0
