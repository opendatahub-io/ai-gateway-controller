#!/usr/bin/env bash
# MaaS-specific cluster diagnostics for e2e / CI must-gather bundles.
#
# Standard `oc adm must-gather` does not include maas.opendatahub.io CRs, tenant
# readiness status, payload-processing, or worker-tenant namespaces. This script
# writes a self-contained tree under gather-maas/ (or the path you pass).
#
# Usage:
#   ./test/e2e/scripts/collect-maas-must-gather.sh [/path/to/gather-maas]
#
# Environment (defaults match prow_run_ai_gateway_controller_test.sh / auth_utils):
#   DEPLOYMENT_NAMESPACE, MAAS_SUBSCRIPTION_NAMESPACE, AITENANT_NAMESPACE,
#   GATEWAY_NAMESPACE, AUTHORINO_NAMESPACE, LLM_NAMESPACE, ISTIO_NAMESPACE,
#   MAAS_API_DEPLOYMENT_NAMESPACE (derived from DEPLOYMENT_NAMESPACE when unset)
set -uo pipefail

_dest="${1:-${ARTIFACT_DIR:-${ARTIFACTS_DIR:-./gather-maas}}}"
mkdir -p "$_dest"

DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
MAAS_SUBSCRIPTION_NAMESPACE="${MAAS_SUBSCRIPTION_NAMESPACE:-models-as-a-service}"
AITENANT_NAMESPACE="${AITENANT_NAMESPACE:-ai-tenants}"
AUTHORINO_NAMESPACE="${AUTHORINO_NAMESPACE:-kuadrant-system}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
LLM_NAMESPACE="${LLM_NAMESPACE:-llm}"
ISTIO_NAMESPACE="${ISTIO_NAMESPACE:-istio-system}"
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-redhat-ods-operator}"
APPLICATIONS_NAMESPACE="${APPLICATIONS_NAMESPACE:-redhat-ods-applications}"
DEFAULT_AITENANT_NAME="${DEFAULT_AITENANT_NAME:-models-as-a-service}"
DEFAULT_TENANT_CONFIG_NAME="${DEFAULT_TENANT_CONFIG_NAME:-default-tenant}"

_derive_infra_namespace() {
  case "$DEPLOYMENT_NAMESPACE" in
    redhat-ods-applications) echo "redhat-ai-gateway-infra" ;;
    opendatahub) echo "odh-ai-gateway-infra" ;;
    *) echo "$DEPLOYMENT_NAMESPACE" ;;
  esac
}
MAAS_API_DEPLOYMENT_NAMESPACE="${MAAS_API_DEPLOYMENT_NAMESPACE:-$(_derive_infra_namespace)}"

_log() {
  echo "$*" | tee -a "$_dest/collection.log"
}

_k() {
  if command -v oc >/dev/null 2>&1; then
    oc "$@"
  elif command -v kubectl >/dev/null 2>&1; then
    kubectl "$@"
  else
    return 127
  fi
}

_save_yaml() {
  local outfile="$1"
  shift
  mkdir -p "$(dirname "$outfile")"
  if _k get "$@" -o yaml >"$outfile" 2>"${outfile}.err"; then
    if [[ ! -s "$outfile" ]]; then
      echo "# empty result for: $*" >"$outfile"
    fi
  else
    {
      echo "# failed to collect: $*"
      cat "${outfile}.err" 2>/dev/null || true
    } >"$outfile"
  fi
  rm -f "${outfile}.err"
}

_save_text() {
  local outfile="$1"
  local label="$2"
  shift 2
  mkdir -p "$(dirname "$outfile")"
  {
    echo "=== ${label} ($(date -u +%Y-%m-%dT%H:%M:%SZ)) ==="
    "$@" 2>&1 || true
  } >"$outfile"
}

_save_events() {
  local ns="$1"
  local outfile="$_dest/namespaces/events-${ns}.txt"
  _save_text "$outfile" "events in ${ns}" \
    _k get events -n "$ns" --sort-by='.lastTimestamp'
}

_save_pod_logs_grep() {
  local ns="$1"
  local label_selector="$2"
  local pattern="$3"
  local outfile="$4"
  mkdir -p "$(dirname "$outfile")"
  {
    echo "=== logs ns=${ns} selector=${label_selector} pattern=${pattern} ==="
    local pod
    pod=$(_k get pods -n "$ns" -l "$label_selector" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [[ -z "$pod" ]]; then
      echo "no pod found"
      _k get pods -n "$ns" -o wide 2>/dev/null || true
      return 0
    fi
    _k logs -n "$ns" "$pod" --since=45m 2>/dev/null | grep -iE "$pattern" | tail -500 || true
    echo ""
    echo "--- last 80 lines (unfiltered) ---"
    _k logs -n "$ns" "$pod" --since=45m 2>/dev/null | tail -80 || true
  } >"$outfile"
}

_resource_short_name() {
  local resource="$1"
  echo "${resource%%.*}"
}

_resource_is_namespaced() {
  local resource="$1"
  local short
  short="$(_resource_short_name "$resource")"
  local namespaced
  namespaced="$(_k api-resources --no-headers -o wide 2>/dev/null | awk -v r="$short" '$1==r {print $3; exit}')"
  [[ "${namespaced}" == "true" ]]
}

_resolve_cluster_resource() {
  local want="$1"
  shift
  local candidate
  for candidate in "$@"; do
    if _k api-resources -o name 2>/dev/null | grep -qx "$candidate"; then
      echo "$candidate"
      return 0
    fi
  done
  return 1
}

_collect_api_group_resources() {
  local group="$1"
  local outdir="$2"
  mkdir -p "$_dest/$outdir"
  local resource safe
  while IFS= read -r resource; do
    [[ -z "${resource}" ]] && continue
    safe="${resource//./-}"
    if _resource_is_namespaced "$resource"; then
      _save_yaml "$_dest/$outdir/${safe}-all.yaml" "$resource" -A
    else
      _save_yaml "$_dest/$outdir/${safe}-all.yaml" "$resource"
    fi
  done < <(_k api-resources --api-group="$group" -o name 2>/dev/null || true)
}

_collect_gateway_api() {
  mkdir -p "$_dest/gateway-api"
  _collect_api_group_resources "gateway.networking.k8s.io" "gateway-api"

  local httproute_resource
  httproute_resource="$(_resolve_cluster_resource httproute \
    httproutes.gateway.networking.k8s.io \
    httproute.gateway.networking.k8s.io \
    httproutes \
    httproute || true)"
  if [[ -n "${httproute_resource}" ]]; then
    _save_yaml "$_dest/gateway-api/httproutes-cluster-all.yaml" "${httproute_resource}" -A
    _save_text "$_dest/gateway-api/httproutes-wide.txt" "HTTPRoutes (all namespaces)" bash -c "
      _k get '${httproute_resource}' -A -o wide 2>/dev/null || true
      echo ''
      _k get '${httproute_resource}' -A -o custom-columns=\
NS:.metadata.namespace,NAME:.metadata.name,HOSTS:.spec.hostnames,GW:.spec.parentRefs[*].name,AGE:.metadata.creationTimestamp \
        2>/dev/null || true
    "
    local ns
    for ns in \
      "$GATEWAY_NAMESPACE" \
      "$MAAS_SUBSCRIPTION_NAMESPACE" \
      "$MAAS_API_DEPLOYMENT_NAMESPACE" \
      "$AITENANT_NAMESPACE" \
      "$LLM_NAMESPACE" \
      "$DEPLOYMENT_NAMESPACE"; do
      if _k get namespace "$ns" &>/dev/null; then
        _save_yaml "$_dest/gateway-api/httproutes-${ns}.yaml" "${httproute_resource}" -n "$ns"
        _save_text "$_dest/gateway-api/httproutes-${ns}.txt" "HTTPRoutes in ${ns}" \
          _k get "${httproute_resource}" -n "$ns" -o wide
      fi
    done
  else
    _save_text "$_dest/gateway-api/httproutes-wide.txt" "HTTPRoutes (resource not found)" \
      bash -c '_k api-resources 2>/dev/null | grep -i httproute || echo "no httproute API resource"'
  fi

  local gw_resource
  gw_resource="$(_resolve_cluster_resource gateway \
    gateways.gateway.networking.k8s.io \
    gateway.gateway.networking.k8s.io \
    gateways \
    gateway || true)"
  if [[ -n "${gw_resource}" ]]; then
    _save_yaml "$_dest/gateway-api/gateways-cluster-all.yaml" "${gw_resource}" -A
    _save_text "$_dest/gateway-api/gateways-wide.txt" "Gateways (all namespaces)" \
      _k get "${gw_resource}" -A -o wide
    if _k get namespace "$GATEWAY_NAMESPACE" &>/dev/null; then
      _save_yaml "$_dest/gateway-api/gateways-${GATEWAY_NAMESPACE}.yaml" "${gw_resource}" -n "$GATEWAY_NAMESPACE"
    fi
  fi

  local refgrant_resource
  refgrant_resource="$(_resolve_cluster_resource referencegrant \
    referencegrants.gateway.networking.k8s.io \
    referencegrant.gateway.networking.k8s.io \
    referencegrants \
    referencegrant || true)"
  if [[ -n "${refgrant_resource}" ]]; then
    _save_yaml "$_dest/gateway-api/referencegrants-cluster-all.yaml" "${refgrant_resource}" -A
  fi
}

_collect_kuadrant_policies() {
  mkdir -p "$_dest/kuadrant"
  local resource
  for resource in \
    authpolicies.kuadrant.io \
    tokenratelimitpolicies.kuadrant.io; do
    if _k api-resources -o name 2>/dev/null | grep -qx "$resource"; then
      _save_yaml "$_dest/kuadrant/${resource//./-}-all.yaml" "$resource" -A
      _save_text "$_dest/kuadrant/${resource//./-}-wide.txt" "$resource (all namespaces)" \
        _k get "$resource" -A -o wide
    fi
  done
}

_collect_istio_gateway_networking() {
  mkdir -p "$_dest/istio-gateway"
  for resource in \
    envoyfilters.networking.istio.io \
    destinationrules.networking.istio.io \
    serviceentries.networking.istio.io \
    virtualservices.networking.istio.io; do
    if _k api-resources -o name 2>/dev/null | grep -qx "$resource"; then
      _save_yaml "$_dest/istio-gateway/${resource//./-}-${GATEWAY_NAMESPACE}.yaml" \
        "$resource" -n "$GATEWAY_NAMESPACE"
    fi
  done
}

_collect_maas_inventory() {
  _save_text "$_dest/summaries/maas-resources-wide.txt" "MaaS / inference CR inventory" bash -c "
    for kind in \
      maastenantconfig \
      aitenant \
      tenant \
      maassubscription \
      maasauthpolicy \
      maasmodelref \
      externalmodel \
      config.maas.opendatahub.io; do
      echo \"--- \${kind} ---\"
      _k get \"\${kind}\" -A -o wide 2>/dev/null || _k get \"\${kind}\" -o wide 2>/dev/null || echo '  (not found)'
      echo ''
    done
    for kind in externalmodels externalproviders; do
      echo \"--- inference.\${kind} ---\"
      _k get \"\${kind}.inference.opendatahub.io\" -A -o wide 2>/dev/null || echo '  (not found)'
      echo ''
    done
  "
}

_log "=== MaaS must-gather started at $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
_log "dest=${_dest}"
_log "DEPLOYMENT_NAMESPACE=${DEPLOYMENT_NAMESPACE}"
_log "MAAS_SUBSCRIPTION_NAMESPACE=${MAAS_SUBSCRIPTION_NAMESPACE}"
_log "MAAS_API_DEPLOYMENT_NAMESPACE=${MAAS_API_DEPLOYMENT_NAMESPACE}"
_log "AITENANT_NAMESPACE=${AITENANT_NAMESPACE}"
_log "GATEWAY_NAMESPACE=${GATEWAY_NAMESPACE}"

cat >"$_dest/README.txt" <<EOF
MaaS e2e diagnostics (ai-gateway-controller)

This directory complements gather-openshift/ from oc adm must-gather.
OpenShift must-gather does not collect maas.opendatahub.io CRs or tenant
readiness details needed for default-tenant / AITenant flake debugging.

Key files for tenant-not-ready failures:
  crs/maastenantconfig-default-tenant.yaml
  crs/aitenant-models-as-a-service.yaml
  crs/config-default.yaml
  summaries/tenant-readiness.txt
  summaries/maas-resources-wide.txt
  gateway-api/httproutes-wide.txt
  gateway-api/httproutes-cluster-all.yaml
  gateway-api/httproutes-${GATEWAY_NAMESPACE}.yaml
  crs/maas/*-all.yaml (every maas.opendatahub.io kind)
  crs/inference/*-all.yaml (inference.opendatahub.io ExternalModel/Provider)
  logs/maas-controller-tenant.log
  workloads/payload-processing-openshift-ingress.yaml
  namespaces/events-${MAAS_SUBSCRIPTION_NAMESPACE}.txt
  namespaces/events-${AITENANT_NAMESPACE}.txt
EOF

# --- summaries (quick jsonpath views) ---
mkdir -p "$_dest/summaries"
_save_text "$_dest/summaries/tenant-readiness.txt" "default tenant readiness" bash -c "
  echo '--- MaasTenantConfig/${DEFAULT_TENANT_CONFIG_NAME} (${MAAS_SUBSCRIPTION_NAMESPACE}) ---'
  _k get maastenantconfig '${DEFAULT_TENANT_CONFIG_NAME}' -n '${MAAS_SUBSCRIPTION_NAMESPACE}' -o jsonpath='phase={.status.phase}{\"\\n\"}{range .status.conditions[*]}{.type}={.status} reason={.reason} msg={.message}{\"\\n\"}{end}' 2>/dev/null || echo 'not found'
  echo ''
  echo '--- AITenant/${DEFAULT_AITENANT_NAME} (${AITENANT_NAMESPACE}) ---'
  _k get aitenant '${DEFAULT_AITENANT_NAME}' -n '${AITENANT_NAMESPACE}' -o jsonpath='phase={.status.phase} tenantNs={.status.tenantNamespace}{\"\\n\"}{range .status.conditions[*]}{.type}={.status} reason={.reason} msg={.message}{\"\\n\"}{end}' 2>/dev/null || echo 'not found'
"

_save_text "$_dest/summaries/cluster-pressure.txt" "cluster pressure" bash -c "
  _k get nodes -o wide 2>/dev/null || true
  echo ''
  _k get pods -A --field-selector=status.phase=Pending -o wide 2>/dev/null || true
  echo ''
  _k get pods -A | grep -E 'CrashLoop|OOM|Error' 2>/dev/null || true
"

# --- priority CRs (full YAML) ---
_save_yaml "$_dest/crs/maastenantconfig-default-tenant.yaml" \
  maastenantconfig "${DEFAULT_TENANT_CONFIG_NAME}" -n "$MAAS_SUBSCRIPTION_NAMESPACE"
_save_yaml "$_dest/crs/aitenant-models-as-a-service.yaml" \
  aitenant "${DEFAULT_AITENANT_NAME}" -n "$AITENANT_NAMESPACE"
_save_yaml "$_dest/crs/config-default.yaml" config.maas.opendatahub.io default

# Discover and dump every maas.opendatahub.io + inference.opendatahub.io kind (cluster-wide).
_collect_api_group_resources "maas.opendatahub.io" "crs/maas"
_collect_api_group_resources "inference.opendatahub.io" "crs/inference"
_collect_maas_inventory

# Gateway API HTTPRoutes/Gateways (oc adm must-gather omits these).
_collect_gateway_api
_collect_kuadrant_policies
_collect_istio_gateway_networking

for cr in \
  "llminferenceservices.serving.kserve.io:-A" \
  "aigateways.components.platform.opendatahub.io:-A"; do
  kind="${cr%%:*}"
  args="${cr#*:}"
  # shellcheck disable=SC2086
  _save_yaml "$_dest/crs/${kind//./-}-all.yaml" "$kind" $args
done

# --- workloads ---
_save_yaml "$_dest/workloads/maas-controller-deployment.yaml" \
  deployment maas-controller -n "$DEPLOYMENT_NAMESPACE"
_save_yaml "$_dest/workloads/ai-gateway-controller-deployment.yaml" \
  deployment ai-gateway-controller -n "$DEPLOYMENT_NAMESPACE"
_save_yaml "$_dest/workloads/payload-processing-openshift-ingress.yaml" \
  deployment payload-processing -n "$GATEWAY_NAMESPACE"
_save_text "$_dest/workloads/maas-api-deployments.txt" "maas-api deployments" \
  _k get deploy -n "$MAAS_API_DEPLOYMENT_NAMESPACE" -l app.kubernetes.io/name=maas-api -o wide
_save_text "$_dest/workloads/gateway-namespace.txt" "gateway namespace workloads" \
  _k get deploy,pods,svc -n "$GATEWAY_NAMESPACE" -o wide

# --- events ---
for ns in \
  "$MAAS_SUBSCRIPTION_NAMESPACE" \
  "$AITENANT_NAMESPACE" \
  "$DEPLOYMENT_NAMESPACE" \
  "$MAAS_API_DEPLOYMENT_NAMESPACE" \
  "$GATEWAY_NAMESPACE" \
  "$LLM_NAMESPACE" \
  "$AUTHORINO_NAMESPACE"; do
  if _k get namespace "$ns" &>/dev/null; then
    _save_events "$ns"
  fi
done

# --- worker / e2e tenant namespaces ---
_save_text "$_dest/worker-tenants/namespaces.txt" "e2e worker tenant namespaces" bash -c "
  _k get ns -o name 2>/dev/null | grep -E 'e2e-models-e2e-worker|e2e-worker' || true
"
while IFS= read -r ns_line; do
  [[ -z "$ns_line" ]] && continue
  ns="${ns_line#namespace/}"
  _save_yaml "$_dest/worker-tenants/${ns}-maassubscriptions.yaml" \
    maassubscriptions.maas.opendatahub.io -n "$ns"
  _save_yaml "$_dest/worker-tenants/${ns}-maastenantconfig.yaml" \
    maastenantconfig -n "$ns" 2>/dev/null || true
  _save_yaml "$_dest/worker-tenants/${ns}-maasauthpolicies.yaml" \
    maasauthpolicies.maas.opendatahub.io -n "$ns" 2>/dev/null || true
  _save_yaml "$_dest/worker-tenants/${ns}-maasmodelrefs.yaml" \
    maasmodelrefs.maas.opendatahub.io -n "$ns" 2>/dev/null || true
  httproute_resource="$(_resolve_cluster_resource httproute \
    httproutes.gateway.networking.k8s.io httproute.gateway.networking.k8s.io httproutes httproute || true)"
  if [[ -n "${httproute_resource}" ]]; then
    _save_yaml "$_dest/worker-tenants/${ns}-httproutes.yaml" "${httproute_resource}" -n "$ns"
  fi
  _save_events "$ns"
done < <(_k get ns -o name 2>/dev/null | grep -E 'e2e-models-e2e-worker|e2e-worker' || true)

# --- controller logs (filtered + tail) ---
_save_pod_logs_grep "$DEPLOYMENT_NAMESPACE" "app=maas-controller" \
  'default-tenant|models-as-a-service|TenantConfig|tenant.*ready|error|fail' \
  "$_dest/logs/maas-controller-tenant.log"
_save_pod_logs_grep "$DEPLOYMENT_NAMESPACE" "app.kubernetes.io/name=ai-gateway-controller" \
  'models-as-a-service|praxis|finalizer|error|fail' \
  "$_dest/logs/ai-gateway-controller-tenant.log"
_save_pod_logs_grep "$GATEWAY_NAMESPACE" "app=payload-processing" \
  'error|fail|warn' \
  "$_dest/logs/payload-processing.log"

_log "=== MaaS must-gather finished at $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
echo "MaaS diagnostics written to ${_dest}"
