#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
command -v yq >/dev/null || { echo 'yq is required for the static render test' >&2; exit 1; }
TEST_STATE=$(mktemp -d)
OUTPUT="$TEST_STATE/rendered"
trap 'rm -rf "$TEST_STATE"' EXIT

export OPENSHIFT_E2E_CONTROLLER_NAMESPACE=xmp-controller-test
export OPENSHIFT_E2E_BACKEND_NAMESPACE=xmp-backend-test
export OPENSHIFT_E2E_TENANT_NAMESPACE=xmp-tenant-test
export OPENSHIFT_E2E_RUN_ID=render-test-123
export OPENSHIFT_E2E_GATEWAY_NAME=xmp-gateway
export OPENSHIFT_E2E_GATEWAY_NAMESPACE=xmp-controller-test
export OPENSHIFT_E2E_GATEWAY_TLS_SECRET=xmp-gateway-tls
export ROLE_NAME=xmp-controller-role-render-test-123
export CONTROLLER_IMAGE=registry.example.test/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export EXTPROC_IMAGE=registry.example.test/extproc@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
export PRAXIS_IMAGE=registry.example.test/praxis@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
export KATAN_IMAGE=ghcr.io/nerdalert/llm-katan@sha256:11379a1ec2fd69dc121eada6c544eb423a7c074414507dc1d474f4abba9df75a
export OPENSHIFT_E2E_USER=render-user
export CLIENT_CA_CONFIGMAP=xmp-gateway-ca-render-test

"$ROOT/test/openshift-env/render-manifests.sh" "$OUTPUT"
files=("$OUTPUT"/*.yaml)
(( ${#files[@]} == 8 )) || { echo "expected eight rendered templates" >&2; exit 1; }
if grep -R -nE '\$\{[A-Za-z_][A-Za-z0-9_]*\}|(stringData:|data:.*api-key|Authorization:|Bearer |token:)' "$OUTPUT"; then
  echo "rendered output contains an unresolved or credential-shaped value" >&2
  exit 1
fi
grep -R -q 'name: xmp-gateway' "$OUTPUT/10-gateway.yaml"
grep -R -q 'provider-a.xmp-backend-test.svc.cluster.local' "$OUTPUT/40-model-fixtures.yaml"
grep -R -q 'namespace: xmp-tenant-test' "$OUTPUT/40-model-fixtures.yaml"
grep -R -q 'external-model-praxis.opendatahub.io/run-id: render-test-123' "$OUTPUT"
grep -R -q 'ghcr.io/nerdalert/llm-katan@sha256:' "$OUTPUT/30-provider-fixtures.yaml"
[[ $(grep -c -- '--validate-keys' "$OUTPUT/30-provider-fixtures.yaml") -eq 2 ]]
if grep -q '^  labels:.*app: provider-[ab]' "$OUTPUT/30-provider-fixtures.yaml"; then
  echo 'provider metadata gained an unreviewed app label' >&2
  exit 1
fi
grep -R -q 'registry.example.test/controller@sha256:' "$OUTPUT/20-controller.yaml"
grep -R -q 'registry.example.test/praxis@sha256:' "$OUTPUT/20-controller.yaml"
grep -R -q 'xmp-gateway' "$OUTPUT/20-controller.yaml"
if grep -En 'name: (provider-a|provider-b|demo-model|xmp-client-)|endpoint: provider-' "$ROOT/test/openshift-env/provision.sh"; then
  echo "stable fixture remains inline in provision.sh" >&2
  exit 1
fi
for file in "${files[@]}"; do yq eval '.' "$file" >/dev/null; done
echo 'render-manifests: PASS'
