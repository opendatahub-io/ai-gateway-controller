#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
BACKENDS="$ROOT/test/kind-env/manifests/00-backends.yaml"
TRANSITION="$ROOT/test/kind-env/manifests/42-transition-fixtures.yaml"

command -v yq >/dev/null 2>&1 || { echo "yq is required" >&2; exit 2; }

for deployment in katan-a katan-b katan-a-tenant-b katan-b-tenant-b; do
  args=$(yq -r "select(.kind == \"Deployment\" and .metadata.name == \"$deployment\") | .spec.template.spec.containers[0].args | join(\" \")" "$BACKENDS")
  expected=kind-only-dummy
  [[ "$deployment" == *-tenant-b ]] && expected=tenant-b-controller-only-reference
  [[ "$args" == *"--validate-keys"* ]] || { echo "$deployment does not enforce API keys" >&2; exit 1; }
  [[ "$args" == *"--api-keys"*"openai=$expected"* ]] || { echo "$deployment has the wrong Praxis fixture key" >&2; exit 1; }
done

transition_args=$(yq -r 'select(.kind == "Deployment" and .metadata.name == "katan-transition") | .spec.template.spec.containers[0].args | join(" ")' "$BACKENDS")
[[ "$transition_args" == *"--validate-keys"*"openai=transition-provider-key"* ]] || { echo "transition backend is not independently credentialed" >&2; exit 1; }
[[ "$(yq -r 'select(.kind == "Secret" and .metadata.name == "transition-provider-credentials") | .stringData["api-key"]' "$TRANSITION")" == transition-provider-key ]] || { echo "transition Secret does not match its backend" >&2; exit 1; }
[[ "$(yq -r 'select(.kind == "Service" and .metadata.name == "provider-a-legacy") | .spec.selector.app' "$BACKENDS")" == katan-transition ]] || { echo "IPP compatibility Service is not isolated" >&2; exit 1; }

if rg -n 'llm-katan(:|@)' "$BACKENDS" | rg -v '@sha256:' >/dev/null; then
  echo "a backend image is not pinned by digest" >&2
  exit 1
fi

echo "provider credential fixture regression checks passed"
