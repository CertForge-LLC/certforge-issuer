#!/usr/bin/env bash
# certforge-issuer integration test suite
# Usage: ./test/integration/run.sh [--keep] [--skip-build] [--cluster NAME]
#
# Required env vars:
#   CERTFORGE_URL          CertForge API base URL (e.g. https://dev.certgovernance.app)
#   CERTFORGE_TOKEN        API token with enroll+read+approve scopes
#   AUTO_APPROVE_DTP       DTP ID with auto_approve:true (e.g. faltys-approval-policy)
#   MANUAL_APPROVE_DTP     DTP ID with auto_approve:false (e.g. payment-soc2)
#   TEST_DOMAIN_AUTO       Domain for auto-approve tests (must match AUTO_APPROVE_DTP)
#   TEST_DOMAIN_MANUAL     Domain for manual-approve/reject tests (must match MANUAL_APPROVE_DTP)
#
# Optional:
#   KEEP_CLUSTER=1         Don't delete the kind cluster on exit
#   SKIP_BUILD=1           Don't rebuild the docker image (use existing certforge-issuer:dev)
#   KIND_CLUSTER_NAME      kind cluster name (default: certforge-test)
#   CERTFORGE_ISSUER_NS    namespace (default: certforge-system)
#   TIMEOUT_AUTO=120       seconds to wait for auto-approve issuance
#   TIMEOUT_MANUAL=300     seconds to wait for manual-approve issuance
#   TIMEOUT_RENEWAL=120    seconds to wait for renewal

set -euo pipefail

# ── Colours ───────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; BOLD='\033[1m'; NC='\033[0m'

pass() { echo -e "${GREEN}✓${NC} $*"; }
fail() { echo -e "${RED}✗${NC} $*"; }
info() { echo -e "${BLUE}▸${NC} $*"; }
warn() { echo -e "${YELLOW}⚠${NC} $*"; }
header() { echo -e "\n${BOLD}$*${NC}"; }

# ── Config ────────────────────────────────────────────────────────────────────
CLUSTER="${KIND_CLUSTER_NAME:-certforge-test}"
NS="${CERTFORGE_ISSUER_NS:-certforge-system}"
IMAGE="certforge-issuer:dev"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
MANIFEST_DIR="$(dirname "$0")/manifests"
TIMEOUT_AUTO="${TIMEOUT_AUTO:-120}"
TIMEOUT_MANUAL="${TIMEOUT_MANUAL:-300}"
TIMEOUT_RENEWAL="${TIMEOUT_RENEWAL:-120}"

# Test result tracking
PASS=0; FAIL=0; SKIP=0
declare -a FAILED_TESTS=()

record_pass() { PASS=$((PASS+1)); pass "PASS: $1"; }
record_fail() { FAIL=$((FAIL+1)); FAILED_TESTS+=("$1"); fail "FAIL: $1"; }
record_skip() { SKIP=$((SKIP+1)); warn "SKIP: $1 — $2"; }

# ── Prereq check ──────────────────────────────────────────────────────────────
check_prereqs() {
    header "Checking prerequisites"
    local missing=0
    for cmd in kind kubectl helm docker go curl jq; do
        if command -v "$cmd" &>/dev/null; then
            pass "$cmd found"
        else
            fail "$cmd not found"
            missing=$((missing+1))
        fi
    done
    [ $missing -eq 0 ] || { fail "Install missing tools and retry"; exit 1; }

    local required_vars=(CERTFORGE_URL CERTFORGE_TOKEN AUTO_APPROVE_DTP MANUAL_APPROVE_DTP TEST_DOMAIN_AUTO TEST_DOMAIN_MANUAL)
    for v in "${required_vars[@]}"; do
        if [ -n "${!v:-}" ]; then
            pass "$v set"
        else
            fail "$v not set"
            missing=$((missing+1))
        fi
    done
    [ $missing -eq 0 ] || { fail "Set missing env vars and retry"; exit 1; }
}

# ── CertForge API helpers ──────────────────────────────────────────────────────
cf_get() {
    curl -sf -H "Authorization: Bearer ${CERTFORGE_TOKEN}" "${CERTFORGE_URL}$1"
}

cf_post() {
    curl -sf -X POST -H "Authorization: Bearer ${CERTFORGE_TOKEN}" \
        -H "Content-Type: application/json" -d "$2" "${CERTFORGE_URL}$1"
}

# Poll CertForge for a pending approval matching the given domain, return approval_id.
# Retries for up to $1 seconds.
wait_for_approval() {
    local domain="$1" timeout="${2:-60}" elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local id
        id=$(cf_get "/api/v1/approvals" 2>/dev/null | \
            jq -r --arg d "$domain" \
            '.[] | select(.state=="pending" and (.domains[]? == $d)) | .approval_id' 2>/dev/null | head -1)
        if [ -n "$id" ]; then
            echo "$id"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed+5))
    done
    return 1
}

# Decide (approve/reject) an approval via the REST API.
decide_approval() {
    local id="$1" decision="$2" note="${3:-Automated integration test}"
    cf_post "/api/v1/approvals/${id}/decide" \
        "{\"decision\":\"${decision}\",\"note\":\"${note}\"}" >/dev/null
}

# ── kind cluster ─────────────────────────────────────────────────────────────
setup_cluster() {
    header "Setting up kind cluster: $CLUSTER"
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER}$"; then
        info "Cluster $CLUSTER already exists — reusing"
    else
        info "Creating cluster $CLUSTER …"
        kind create cluster --name "$CLUSTER" --wait 60s
        pass "Cluster created"
    fi
    kubectl config use-context "kind-${CLUSTER}" >/dev/null
}

install_cert_manager() {
    header "Installing cert-manager"
    if kubectl get ns cert-manager &>/dev/null; then
        info "cert-manager namespace exists — skipping install"
        return
    fi
    helm repo add jetstack https://charts.jetstack.io --force-update >/dev/null 2>&1
    helm upgrade --install cert-manager jetstack/cert-manager \
        --namespace cert-manager --create-namespace \
        --set crds.enabled=true \
        --wait --timeout 3m >/dev/null
    pass "cert-manager installed"
}

build_and_load_image() {
    header "Building and loading issuer image"
    if [ "${SKIP_BUILD:-0}" = "1" ]; then
        warn "SKIP_BUILD=1 — skipping docker build"
    else
        info "Building $IMAGE …"
        docker build -t "$IMAGE" "$REPO_ROOT" -q
        pass "Image built"
    fi
    info "Loading $IMAGE into kind cluster $CLUSTER …"
    kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null
    pass "Image loaded"
}

install_issuer() {
    header "Installing certforge-issuer"
    if [ -z "${CERTFORGE_TOKEN:-}" ]; then
        fail "CERTFORGE_TOKEN not set"; exit 1
    fi
    helm upgrade --install certforge-issuer "$REPO_ROOT/helm/certforge-issuer" \
        --namespace "$NS" --create-namespace \
        --set image.repository=certforge-issuer \
        --set image.tag=dev \
        --set image.pullPolicy=IfNotPresent \
        --set certforge.url="$CERTFORGE_URL" \
        --set certforge.token="$CERTFORGE_TOKEN" \
        --wait --timeout 2m >/dev/null
    pass "certforge-issuer installed"

    # Rollout restart to ensure the newly loaded image is used.
    kubectl rollout restart deployment/certforge-issuer -n "$NS" >/dev/null
    kubectl rollout status deployment/certforge-issuer -n "$NS" --timeout=60s >/dev/null
    pass "Rollout complete"
}

cleanup_test_resources() {
    kubectl delete certificate cftest-auto cftest-manual cftest-reject cftest-renewal \
        -n default --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete secret cftest-auto-tls cftest-manual-tls cftest-reject-tls cftest-renewal-tls \
        -n default --ignore-not-found >/dev/null 2>&1 || true
    sleep 2
}

# ── Test helpers ──────────────────────────────────────────────────────────────

# Wait for a CertificateRequest to have a given condition type=True.
wait_for_cr_condition() {
    local cr="$1" condition="$2" timeout="${3:-120}" elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local status
        status=$(kubectl get certificaterequest "$cr" -n default \
            -o jsonpath="{.status.conditions[?(@.type==\"${condition}\")].status}" 2>/dev/null)
        if [ "$status" = "True" ]; then return 0; fi
        sleep 5; elapsed=$((elapsed+5))
    done
    return 1
}

# Get the first CertificateRequest name for a Certificate.
get_cr_name() {
    local cert="$1" timeout="${2:-30}" elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local cr
        cr=$(kubectl get certificaterequest -n default \
            -l cert-manager.io/certificate-name="$cert" \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
        if [ -n "$cr" ] && [ "$cr" != "null" ]; then echo "$cr"; return 0; fi
        sleep 3; elapsed=$((elapsed+3))
    done
    return 1
}

# Count CertificateRequests for a Certificate.
count_crs() {
    kubectl get certificaterequest -n default \
        -l cert-manager.io/certificate-name="$1" \
        --no-headers 2>/dev/null | wc -l | tr -d ' '
}

# ── Test 1: Auto-approve issuance ─────────────────────────────────────────────
test_auto_approve() {
    header "Test 1: Auto-approve issuance"
    local name="cftest-auto"
    local start elapsed

    # Substitute domain variable and apply
    TEST_DOMAIN="$TEST_DOMAIN_AUTO" envsubst < "$MANIFEST_DIR/auto-approve.yaml" | \
        kubectl apply -f - -n default >/dev/null
    info "Certificate $name created — waiting up to ${TIMEOUT_AUTO}s for issuance …"
    start=$(date +%s)

    # Wait for Secret to be populated
    local ready=false
    local deadline=$(($(date +%s) + TIMEOUT_AUTO))
    while [ "$(date +%s)" -lt $deadline ]; do
        local secret_data
        secret_data=$(kubectl get secret "${name}-tls" -n default \
            -o jsonpath='{.data.tls\.crt}' 2>/dev/null)
        if [ -n "$secret_data" ]; then
            ready=true; break
        fi
        sleep 5
    done

    elapsed=$(( $(date +%s) - start ))

    if $ready; then
        # Verify the cert is a real X.509 cert
        local cert_pem
        cert_pem=$(kubectl get secret "${name}-tls" -n default \
            -o jsonpath='{.data.tls\.crt}' | base64 -d)
        local subject not_after
        subject=$(echo "$cert_pem" | openssl x509 -noout -subject 2>/dev/null)
        not_after=$(echo "$cert_pem" | openssl x509 -noout -enddate 2>/dev/null)
        info "Subject: $subject"
        info "Expires: $not_after"
        info "Time to issue: ${elapsed}s"
        record_pass "Auto-approve issuance (${elapsed}s)"
    else
        local cr; cr=$(get_cr_name "$name" 5) || cr="<none>"
        warn "CertificateRequest: $cr"
        kubectl describe certificaterequest "$cr" -n default 2>/dev/null | grep -A3 "Conditions:" || true
        record_fail "Auto-approve issuance (timed out after ${TIMEOUT_AUTO}s)"
    fi
}

# ── Test 2: Secret contents validation ───────────────────────────────────────
test_secret_population() {
    header "Test 2: Secret population"
    local name="cftest-auto"
    local secret="${name}-tls"

    # The secret should already be there from Test 1; if not, skip.
    if ! kubectl get secret "$secret" -n default &>/dev/null; then
        record_skip "Secret population" "cftest-auto Secret not present (Test 1 likely failed)"
        return
    fi

    local ok=true
    local crt key ca
    crt=$(kubectl get secret "$secret" -n default -o jsonpath='{.data.tls\.crt}' | base64 -d 2>/dev/null)
    key=$(kubectl get secret "$secret" -n default -o jsonpath='{.data.tls\.key}' | base64 -d 2>/dev/null)
    ca=$(kubectl get secret "$secret" -n default -o jsonpath='{.data.ca\.crt}' 2>/dev/null | base64 -d 2>/dev/null || true)

    # tls.crt must be valid PEM
    if echo "$crt" | openssl x509 -noout 2>/dev/null; then
        pass "tls.crt is valid X.509"
    else
        fail "tls.crt is not valid X.509"; ok=false
    fi

    # tls.key must be present
    if [ -n "$key" ]; then
        pass "tls.key present"
    else
        fail "tls.key missing or empty"; ok=false
    fi

    # Cert must not be expired
    if echo "$crt" | openssl x509 -noout -checkend 0 2>/dev/null; then
        local days_left
        days_left=$(( ( $(date -d "$(echo "$crt" | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2)" +%s) - $(date +%s) ) / 86400 ))
        pass "Certificate is valid — expires in ${days_left} days"
    else
        fail "Certificate is already expired"; ok=false
    fi

    # Key and cert must match (modulus comparison)
    local cert_mod key_mod
    cert_mod=$(echo "$crt" | openssl x509 -noout -modulus 2>/dev/null | md5sum)
    key_mod=$(echo "$key" | openssl rsa -noout -modulus 2>/dev/null | md5sum)
    if [ "$cert_mod" = "$key_mod" ]; then
        pass "tls.crt and tls.key are a matching pair"
    else
        fail "tls.crt and tls.key do not match"; ok=false
    fi

    if $ok; then
        record_pass "Secret population"
    else
        record_fail "Secret population"
    fi
}

# ── Test 3: Renewal (delete Secret → re-issue) ───────────────────────────────
test_renewal() {
    header "Test 3: Renewal after Secret deletion"
    local name="cftest-auto"

    if ! kubectl get certificate "$name" -n default &>/dev/null; then
        record_skip "Renewal" "cftest-auto Certificate not present (Test 1 likely failed)"
        return
    fi

    info "Deleting Secret to trigger renewal …"
    kubectl delete secret "${name}-tls" -n default --ignore-not-found >/dev/null

    info "Waiting up to ${TIMEOUT_RENEWAL}s for re-issuance …"
    local deadline=$(($(date +%s) + TIMEOUT_RENEWAL))
    local renewed=false
    while [ "$(date +%s)" -lt $deadline ]; do
        local secret_data
        secret_data=$(kubectl get secret "${name}-tls" -n default \
            -o jsonpath='{.data.tls\.crt}' 2>/dev/null)
        if [ -n "$secret_data" ]; then
            renewed=true; break
        fi
        sleep 5
    done

    if $renewed; then
        record_pass "Renewal after Secret deletion"
    else
        record_fail "Renewal after Secret deletion (Secret not re-populated within ${TIMEOUT_RENEWAL}s)"
    fi
}

# ── Test 4: Rejection → Denied=True, no retry ────────────────────────────────
test_rejection() {
    header "Test 4: Rejection sets Denied=True and stops retries"
    local name="cftest-reject"
    local domain="cftest-reject.${TEST_DOMAIN_MANUAL}"

    TEST_DOMAIN="$TEST_DOMAIN_MANUAL" envsubst < "$MANIFEST_DIR/rejection.yaml" | \
        kubectl apply -f - -n default >/dev/null
    info "Certificate $name created — waiting for approval to appear in CertForge …"

    local approval_id
    if ! approval_id=$(wait_for_approval "$domain" 60); then
        record_fail "Rejection: approval never appeared in CertForge within 60s"
        return
    fi
    info "Approval ID: ${approval_id:0:8} — rejecting via API …"

    if ! decide_approval "$approval_id" "reject" "Automated integration test — rejection path"; then
        record_fail "Rejection: API call to reject failed"
        return
    fi
    pass "Rejection recorded in CertForge"

    # Wait for Denied=True on the CertificateRequest
    local cr; cr=$(get_cr_name "$name" 30) || { record_fail "Rejection: CertificateRequest not found"; return; }
    info "CertificateRequest: $cr — waiting for Denied=True …"

    if wait_for_cr_condition "$cr" "Denied" 60; then
        pass "Denied=True set on CertificateRequest"
    else
        fail "Denied=True not set within 60s"
        kubectl describe certificaterequest "$cr" -n default | grep -A3 "Conditions:" || true
        record_fail "Rejection: Denied condition not set"
        return
    fi

    # Verify no -2 retry is created (wait 60s)
    info "Waiting 60s to confirm cert-manager does not create a retry CertificateRequest …"
    sleep 60
    local cr_count; cr_count=$(count_crs "$name")
    if [ "$cr_count" -eq 1 ]; then
        pass "No retry CertificateRequest created (count=$cr_count)"
        record_pass "Rejection: Denied=True set, no retry loop"
    else
        fail "cert-manager created ${cr_count} CertificateRequests — retry loop detected"
        kubectl get certificaterequest -n default -l cert-manager.io/certificate-name="$name"
        record_fail "Rejection: retry loop (${cr_count} CRs created)"
    fi
}

# ── Teardown ──────────────────────────────────────────────────────────────────
teardown() {
    header "Teardown"
    info "Cleaning up test resources …"
    cleanup_test_resources

    if [ "${KEEP_CLUSTER:-0}" = "1" ]; then
        warn "KEEP_CLUSTER=1 — leaving kind cluster $CLUSTER intact"
    else
        info "Deleting kind cluster $CLUSTER …"
        kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
        pass "Cluster deleted"
    fi
}

# ── Summary ───────────────────────────────────────────────────────────────────
print_summary() {
    header "═══════════════════════════════════════════"
    header " Test Results"
    header "═══════════════════════════════════════════"
    echo -e " ${GREEN}Passed${NC}: $PASS"
    echo -e " ${RED}Failed${NC}: $FAIL"
    echo -e " ${YELLOW}Skipped${NC}: $SKIP"
    if [ ${#FAILED_TESTS[@]} -gt 0 ]; then
        echo ""
        echo -e " ${RED}Failed tests:${NC}"
        for t in "${FAILED_TESTS[@]}"; do
            echo -e "   ${RED}✗${NC} $t"
        done
    fi
    echo ""
    [ $FAIL -eq 0 ]
}

# ── Main ──────────────────────────────────────────────────────────────────────
main() {
    echo -e "${BOLD}certforge-issuer Integration Tests${NC}"
    echo "Cluster: $CLUSTER | URL: ${CERTFORGE_URL:-<not set>}"
    echo ""

    check_prereqs
    setup_cluster
    install_cert_manager
    build_and_load_image
    install_issuer

    header "Cleaning up any leftover test resources"
    cleanup_test_resources

    # Run tests
    test_auto_approve
    test_secret_population
    test_renewal
    test_rejection

    teardown
    print_summary
}

trap 'echo -e "\n${RED}Interrupted${NC}"; teardown; exit 130' INT TERM

main "$@"
