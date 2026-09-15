#!/bin/bash
# ai-gateway-controller Konflux / Prow e2e orchestrator.
#
# TEMP (remove before merge): defaults PRAXIS_EXTPROC_IMAGE to quay.io/maas/odh-praxis-extproc:pr699-76cb977,
# built with praxis-proxy/ai#699 (llmisvc_model_provider_resolver @ 76cb977). Revert to odh-stable / Konflux
# odh-praxis-extproc-ci snapshot once that filter is on a published odh-praxis-extproc tag.
#
# USAGE:
#   AI_GATEWAY_CONTROLLER_IMAGE=quay.io/opendatahub/odh-ai-gateway-controller:odh-pr \
#     ./test/e2e/scripts/prow_run_ai_gateway_controller_test.sh

set -euo pipefail

_find_project_root_bootstrap() {
  local dir="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
  while [[ "$dir" != "/" && ! -e "$dir/.git" ]]; do dir="$(dirname "$dir")"; done
  [[ -e "$dir/.git" ]] && printf '%s\n' "$dir" || return 1
}
PROJECT_ROOT="$(_find_project_root_bootstrap)"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ ! -f "${PROJECT_ROOT}/scripts/deployment-helpers.sh" ]]; then
  echo "ERROR: MaaS deploy scripts missing. Run: ./hack/scripts/sync-maas-e2e-tests.sh" >&2
  exit 1
fi

POD_TIMEOUT=${POD_TIMEOUT:-600}
export POD_TIMEOUT

source "$PROJECT_ROOT/scripts/deployment-helpers.sh"
source "$PROJECT_ROOT/test/e2e/scripts/auth_utils.sh"
# shellcheck disable=SC1091
source "$PROJECT_ROOT/test/e2e/scripts/maas-image-defaults.sh"

SKIP_DEPLOYMENT=${SKIP_DEPLOYMENT:-false}
SKIP_VALIDATION=${SKIP_VALIDATION:-false}
SKIP_AUTH_CHECK=${SKIP_AUTH_CHECK:-true}
INSECURE_HTTP=${INSECURE_HTTP:-false}
EXTERNAL_OIDC=false

export MAAS_API_IMAGE
export MAAS_CONTROLLER_IMAGE
export AI_GATEWAY_OPERATOR_IMAGE=${AI_GATEWAY_OPERATOR_IMAGE:-}
export AI_GATEWAY_CONTROLLER_IMAGE
export OPERATOR_CATALOG=${OPERATOR_CATALOG:-}
export OPERATOR_IMAGE=${OPERATOR_IMAGE:-}
DEPLOY_MODE=${DEPLOY_MODE:-kustomize}
export POLICY_ENGINE="${POLICY_ENGINE:-rhcl}"
export RHCL_NAMESPACE="${RHCL_NAMESPACE:-kuadrant-system}"
export RHCL_STARTING_CSV="${RHCL_STARTING_CSV:-}"

AUTHORINO_NAMESPACE="${AUTHORINO_NAMESPACE:-$(resolve_authorino_namespace "$POLICY_ENGINE")}"
export AUTHORINO_NAMESPACE

DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
MAAS_SUBSCRIPTION_NAMESPACE="${MAAS_SUBSCRIPTION_NAMESPACE:-models-as-a-service}"
MODEL_NAMESPACE="${MODEL_NAMESPACE:-llm}"
MODEL_NAME="${MODEL_NAME:-facebook-opt-125m-simulated}"
export MODEL_NAME
export E2E_MODEL_PATH="${E2E_MODEL_PATH:-/llm/${MODEL_NAME}}"
export E2E_MODEL_REF="${E2E_MODEL_REF:-${MODEL_NAME}}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
INGRESS_MODE="${INGRESS_MODE:-ocproute}"
export INGRESS_MODE
ENABLE_TENANT_NAMESPACE_DISCOVERY="${ENABLE_TENANT_NAMESPACE_DISCOVERY:-true}"
AITENANT_NAMESPACE="${AITENANT_NAMESPACE:-ai-tenants}"

ARTIFACTS_DIR="${ARTIFACT_DIR:-${ARTIFACTS:-${LOG_DIR:-$PROJECT_ROOT/test/e2e/reports}}}"
mkdir -p "$ARTIFACTS_DIR"

print_header() {
    echo ""
    echo "----------------------------------------"
    echo "$1"
    echo "----------------------------------------"
    echo ""
}

phase_mark() {
    local name="${1:?phase_mark requires a phase name}"
    local event="${2:?phase_mark requires start or end}"
    mkdir -p "$ARTIFACTS_DIR"
    echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) ${name} ${event}" | tee -a "${ARTIFACTS_DIR}/phase-timings.txt"
}

check_prerequisites() {
    echo "Checking prerequisites..."
    local current_user
    if ! current_user=$(oc whoami 2>/dev/null); then
        echo "ERROR: Not logged into OpenShift. Run 'oc login' first"
        exit 1
    fi
    if ! oc auth can-i '*' '*' --all-namespaces >/dev/null 2>&1; then
        echo "ERROR: User '$current_user' does not have admin privileges"
        exit 1
    elif ! kubectl get --raw /apis/config.openshift.io/v1/clusterversions >/dev/null 2>&1; then
        echo "ERROR: This script requires OpenShift"
        exit 1
    fi
    if [[ -z "${AI_GATEWAY_CONTROLLER_IMAGE}" ]]; then
        echo "ERROR: AI_GATEWAY_CONTROLLER_IMAGE is required" >&2
        exit 1
    fi
    echo "Prerequisites met — logged in as: $current_user"
    echo "DEPLOY_MODE: ${DEPLOY_MODE}"
    echo "MAAS_API_IMAGE: ${MAAS_API_IMAGE}"
    echo "MAAS_CONTROLLER_IMAGE: ${MAAS_CONTROLLER_IMAGE}"
    echo "AI_GATEWAY_CONTROLLER_IMAGE: ${AI_GATEWAY_CONTROLLER_IMAGE}"
    echo "PRAXIS_EXTPROC_IMAGE: ${PRAXIS_EXTPROC_IMAGE}"
}

enable_tenant_namespace_discovery_for_e2e() {
    [[ "${ENABLE_TENANT_NAMESPACE_DISCOVERY}" == "true" ]] || return 0

    echo "Enabling --enable-tenant-namespace-discovery on maas-controller..."
    if ! oc get deployment maas-controller -n "$DEPLOYMENT_NAMESPACE" &>/dev/null; then
        echo "ERROR: maas-controller not found in ${DEPLOYMENT_NAMESPACE}"
        return 1
    fi

    local args_json
    args_json="$(oc get deployment maas-controller -n "$DEPLOYMENT_NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || echo '[]')"
    if echo "$args_json" | grep -q 'enable-tenant-namespace-discovery'; then
        echo "maas-controller already has tenant namespace discovery enabled"
    elif [[ -z "$args_json" || "$args_json" == "<no value>" ]]; then
        oc patch deployment maas-controller -n "$DEPLOYMENT_NAMESPACE" --type=json -p='[
          {"op": "add", "path": "/spec/template/spec/containers/0/args", "value": ["--enable-tenant-namespace-discovery=true"]}
        ]' || { echo "ERROR: failed to initialize args"; return 1; }
    else
        oc patch deployment maas-controller -n "$DEPLOYMENT_NAMESPACE" --type=json -p='[
          {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--enable-tenant-namespace-discovery=true"}
        ]' || { echo "ERROR: failed to patch args"; return 1; }
    fi

    if ! echo "$args_json" | grep -q 'enable-tenant-namespace-discovery'; then
        oc rollout status deployment/maas-controller -n "$DEPLOYMENT_NAMESPACE" --timeout=180s \
          || { echo "ERROR: rollout failed"; return 1; }
    fi
}

setup_vars_for_tests() {
    echo "-- Setting up variables for tests --"
    K8S_CLUSTER_URL=$(oc whoami --show-server)
    export K8S_CLUSTER_URL
    [[ -z "$K8S_CLUSTER_URL" ]] && { echo "ERROR: Failed to retrieve cluster URL"; exit 1; }

    export INSECURE_HTTP
    export CLUSTER_DOMAIN="$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
    [[ -z "$CLUSTER_DOMAIN" ]] && { echo "ERROR: Failed to detect cluster domain"; exit 1; }
    export HOST="maas.${CLUSTER_DOMAIN}"
    export EXTERNAL_OIDC=false

    if [[ "$INSECURE_HTTP" == "true" ]]; then
        export MAAS_API_BASE_URL="http://${HOST}/maas-api"
    else
        export MAAS_API_BASE_URL="https://${HOST}/maas-api"
    fi

    echo "HOST: ${HOST}"
    echo "MAAS_API_BASE_URL: ${MAAS_API_BASE_URL}"
}

validate_deployment() {
    echo "Deployment Validation"
    if [[ "$SKIP_VALIDATION" == "false" ]]; then
        if ! E2E_MODEL_PATH="$E2E_MODEL_PATH" E2E_MODEL_REF="$E2E_MODEL_REF" \
            "$PROJECT_ROOT/scripts/validate-deployment.sh"; then
            echo "First validation failed; retrying after short wait..."
            wait_for_gateway_programmed "$GATEWAY_NAME" "$GATEWAY_NAMESPACE" 60 || true
            kubectl wait --for=condition=Available --timeout=60s \
                "deployment/maas-controller" -n "$DEPLOYMENT_NAMESPACE" 2>/dev/null || true
            if [[ -n "${MAAS_API_DEPLOYMENT_NAMESPACE:-}" ]]; then
                kubectl wait --for=condition=Available --timeout=60s \
                    "deployment/maas-api" -n "$MAAS_API_DEPLOYMENT_NAMESPACE" 2>/dev/null || true
            fi
            if ! E2E_MODEL_PATH="$E2E_MODEL_PATH" E2E_MODEL_REF="$E2E_MODEL_REF" \
            "$PROJECT_ROOT/scripts/validate-deployment.sh"; then
                echo "ERROR: Deployment validation failed after retry"
                exit 1
            fi
        fi
        echo "Deployment validation completed"
    else
        echo "Skipping validation (SKIP_VALIDATION=true)"
    fi
}

run_e2e_tests() {
    echo "-- E2E Tests (ai-gateway-controller, no external-model tests) --"

    export GATEWAY_HOST="${HOST}"
    export DEPLOYMENT_NAMESPACE
    export MAAS_SUBSCRIPTION_NAMESPACE
    export GATEWAY_NAMESPACE
    export GATEWAY_NAME
    export AITENANT_NAMESPACE
    export ENABLE_TENANT_NAMESPACE_DISCOVERY
    enable_tenant_namespace_discovery_for_e2e || exit 1
    export E2E_SKIP_TLS_VERIFY=true
    export MODEL_NAME
    export E2E_MODEL_NAMESPACE="$MODEL_NAMESPACE"

    local scheme="https"
    [[ "$INSECURE_HTTP" == "true" ]] && scheme="http"
    local gw_url="${scheme}://${GATEWAY_HOST}/maas-api/health"
    local gw_timeout=120
    local gw_deadline=$((SECONDS + gw_timeout))
    echo "Waiting for gateway: ${gw_url} ..."
    while [[ $SECONDS -lt $gw_deadline ]]; do
        local http_code
        http_code=$(curl -sk -o /dev/null -w '%{http_code}' -m 5 "$gw_url" 2>/dev/null || echo "000")
        if [[ "$http_code" =~ ^2 ]]; then
            echo "Gateway reachable (HTTP $http_code)"
            break
        fi
        sleep 1
    done

    local api_base="${scheme}://${GATEWAY_HOST}/maas-api"
    local auth_timeout=180
    local auth_deadline=$((SECONDS + auth_timeout))
    echo "Waiting for authenticated gateway access ..."
    while [[ $SECONDS -lt $auth_deadline ]]; do
        local auth_code
        auth_code=$(curl -sk -o /dev/null -w '%{http_code}' -m 5 \
            -H "Authorization: Bearer ${TOKEN}" \
            "${api_base}/v1/subscriptions" 2>/dev/null || echo "000")
        if [[ "$auth_code" == "200" ]]; then
            echo "Authenticated gateway access working (HTTP $auth_code)"
            break
        fi
        sleep 5
    done
    if [[ $SECONDS -ge $auth_deadline ]]; then
        echo "ERROR: Authenticated gateway access not working after ${auth_timeout}s"
        exit 1
    fi

    export ARTIFACTS_DIR
    export E2E_PARALLEL_WORKERS="${E2E_PARALLEL_WORKERS:-7}"
    export E2E_RECONCILE_WAIT="${E2E_RECONCILE_WAIT:-4}"
    "${SCRIPT_DIR}/run_e2e_tests.sh"
}

_run_exit_artifacts() {
    local exit_code=$?
    set +e
    DEPLOYMENT_NAMESPACE="$DEPLOYMENT_NAMESPACE" MAAS_SUBSCRIPTION_NAMESPACE="$MAAS_SUBSCRIPTION_NAMESPACE" \
      AUTHORINO_NAMESPACE="$AUTHORINO_NAMESPACE" ARTIFACTS_DIR="$ARTIFACTS_DIR" \
        collect_e2e_artifacts
    mkdir -p "$ARTIFACTS_DIR"
    DEPLOYMENT_NAMESPACE="$DEPLOYMENT_NAMESPACE" MAAS_SUBSCRIPTION_NAMESPACE="$MAAS_SUBSCRIPTION_NAMESPACE" \
      AUTHORINO_NAMESPACE="$AUTHORINO_NAMESPACE" \
        run_auth_debug_report 2>&1 | tee "$ARTIFACTS_DIR/auth-debug.log"
    exit $exit_code
}
trap '_run_exit_artifacts' EXIT

print_header "ai-gateway-controller E2E on OpenShift"
check_prerequisites

if [[ "$SKIP_DEPLOYMENT" == "true" ]]; then
    echo "Skipping deployment (SKIP_DEPLOYMENT=true)"
else
    phase_mark deploy_platform start
    # shellcheck disable=SC1091
    source "${SCRIPT_DIR}/deploy-platform.sh"
    phase_mark deploy_platform end

    print_header "Deploying Models"
    phase_mark deploy_models start
    # shellcheck disable=SC1091
    source "${SCRIPT_DIR}/deploy-models.sh"
    phase_mark deploy_models end
    patch_authorino_debug

    print_header "Deploying ai-gateway-controller"
    phase_mark deploy_ai_gateway_controller start
    # shellcheck disable=SC1091
    source "${SCRIPT_DIR}/deploy-ai-gateway-controller.sh"
    phase_mark deploy_ai_gateway_controller end
fi

print_header "Setting up variables for tests"
setup_vars_for_tests

print_header "Setting up test tokens"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/setup-test-tokens.sh"

print_header "Validating Deployment"
phase_mark validate start
validate_deployment
phase_mark validate end

print_header "Running E2E Tests"
run_e2e_tests

echo "ai-gateway-controller e2e completed successfully"
