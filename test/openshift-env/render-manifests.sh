#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
TEMPLATE_DIR="$ROOT/test/openshift-env/manifests"
command -v envsubst >/dev/null || { echo "gettext (envsubst) is required; install the gettext package" >&2; exit 1; }
OUTPUT_DIR=${1:-"${OPENSHIFT_E2E_STATE:-$ROOT/.openshift-state}/rendered-manifests"}
if (($# > 0)); then
  shift
fi
mkdir -p "$OUTPUT_DIR"

require_var() {
  local name=$1
  [[ -n "${!name:-}" ]] || { echo "required render variable is unset: $name" >&2; exit 1; }
}

render_one() {
  local template=$1
  local vars=$2
  local name
  name=$(basename "$template" .tmpl)
  for variable in $vars; do
    require_var "${variable:2:${#variable}-3}" # remove the leading ${ and trailing }
  done
  envsubst "$vars" <"$template" >"$OUTPUT_DIR/$name"
  if grep -Eq '\$\{[A-Za-z_][A-Za-z0-9_]*\}' "$OUTPUT_DIR/$name"; then
    echo "unrendered variable remains in $OUTPUT_DIR/$name" >&2
    exit 1
  fi
}

# The values are intentionally literal envsubst allowlists, not shell expansions.
# shellcheck disable=SC2016
declare -A ALLOWLIST=(
  [00-namespaces.yaml.tmpl]='${OPENSHIFT_E2E_CONTROLLER_NAMESPACE} ${OPENSHIFT_E2E_BACKEND_NAMESPACE} ${OPENSHIFT_E2E_RUN_ID}'
  [05-image-puller.yaml.tmpl]='${OPENSHIFT_E2E_BACKEND_NAMESPACE} ${OPENSHIFT_E2E_CONTROLLER_NAMESPACE} ${OPENSHIFT_E2E_TENANT_NAMESPACE} ${OPENSHIFT_E2E_RUN_ID}'
  [10-gateway.yaml.tmpl]='${OPENSHIFT_E2E_GATEWAY_NAME} ${OPENSHIFT_E2E_GATEWAY_NAMESPACE} ${OPENSHIFT_E2E_GATEWAY_TLS_SECRET} ${OPENSHIFT_E2E_RUN_ID}'
  [20-controller.yaml.tmpl]='${OPENSHIFT_E2E_CONTROLLER_NAMESPACE} ${OPENSHIFT_E2E_RUN_ID} ${ROLE_NAME} ${CONTROLLER_IMAGE} ${EXTPROC_IMAGE} ${PRAXIS_IMAGE} ${OPENSHIFT_E2E_TENANT_NAMESPACE} ${OPENSHIFT_E2E_GATEWAY_NAME} ${OPENSHIFT_E2E_GATEWAY_NAMESPACE}'
  [30-provider-fixtures.yaml.tmpl]='${OPENSHIFT_E2E_BACKEND_NAMESPACE} ${OPENSHIFT_E2E_RUN_ID} ${KATAN_IMAGE}'
  [40-model-fixtures.yaml.tmpl]='${OPENSHIFT_E2E_TENANT_NAMESPACE} ${OPENSHIFT_E2E_BACKEND_NAMESPACE} ${OPENSHIFT_E2E_RUN_ID}'
  [50-maas-fixtures.yaml.tmpl]='${OPENSHIFT_E2E_TENANT_NAMESPACE} ${OPENSHIFT_E2E_RUN_ID} ${OPENSHIFT_E2E_USER}'
  [60-client.yaml.tmpl]='${OPENSHIFT_E2E_TENANT_NAMESPACE} ${OPENSHIFT_E2E_RUN_ID} ${CLIENT_CA_CONFIGMAP}'
)

templates=("$@")
if ((${#templates[@]} == 0)); then
  templates=("$TEMPLATE_DIR"/*.yaml.tmpl)
else
  for index in "${!templates[@]}"; do
    # shellcheck disable=SC2004
    templates[$index]="$TEMPLATE_DIR/${templates[$index]}"
  done
fi
for template in "${templates[@]}"; do
  [[ -f "$template" ]] || { echo "manifest template not found: $template" >&2; exit 1; }
  key=$(basename "$template")
  [[ -n "${ALLOWLIST[$key]:-}" ]] || { echo "no explicit allowlist for $key" >&2; exit 1; }
  render_one "$template" "${ALLOWLIST[$key]}"
done
printf 'rendered %d manifest template(s) into %s\n' "${#templates[@]}" "$OUTPUT_DIR"
