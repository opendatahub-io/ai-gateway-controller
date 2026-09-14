#!/usr/bin/env bash
# shellcheck disable=SC2068,SC2016,SC2015
set -euo pipefail
# Reviewed remote-command array expansions are required for the isolated
# kubeconfig wrapper and stdin-based credential transfer.
# shellcheck disable=SC2068,SC2016
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
STATE=${OPENSHIFT_E2E_STATE:-"$ROOT/.openshift-state"}
# shellcheck disable=SC1091
source "$STATE/run.env"
command -v timeout >/dev/null || { echo "timeout is required" >&2; exit 1; }
OC=(timeout --foreground 45s oc --kubeconfig "${OPENSHIFT_KUBECONFIG:-$STATE/kubeconfig}")
OUT="$OPENSHIFT_E2E_EVIDENCE_ROOT/e2e-$(date -u +%Y%m%dT%H%M%SZ)-$$"
mkdir -p "$OUT"
RECOMPUTE_BIN="$OUT/recompute-digest"
go build -o "$RECOMPUTE_BIN" "$ROOT/test/openshift-env/recompute_digest.go"
AITENANT_NAME="${OPENSHIFT_E2E_AITENANT_NAME:-models-as-a-service}"
RESULTS="$OUT/results.json"
tmp="$RESULTS.tmp.$$"
printf '{"suite":"openshift-routing","functional":"RUNNING","assertions":[]}' >"$tmp"
mv "$tmp" "$RESULTS"
CURRENT_ASSERTION="initialization"
FINALIZED=false
ADMIN=""
KEY=""
KEY_ID=""
HOST=""
CLIENT=""
revoke_key() {
  [[ -n "$ADMIN" && -n "$KEY_ID" && -n "$HOST" && -n "$CLIENT" ]] || return 0
  local status
  status=$(printf '%s\n' "$ADMIN" | "${OC[@]}" exec -i "$CLIENT" -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -- sh -c 'read -r token; curl --silent --show-error --output /dev/null --write-out "%{http_code}" --max-time 20 --cacert /etc/xmp/ca/ca.crt -X DELETE "https://'"$HOST"'/v1/api-keys/'"$KEY_ID"'" -H "Authorization: Bearer $token"' 2>/dev/null || true)
  printf '%s\n' "$status" >"$OUT/key-revoke-status.txt"
  [[ "$status" == 200 ]] || return 1
  KEY_ID=""
}
finalize_on_exit() {
  local rc=${1:-$?} current final
  [[ "$FINALIZED" == true ]] && return "$rc"
  FINALIZED=true
  if [[ -n "$KEY_ID" ]] && ! revoke_key; then
    rc=1
    CURRENT_ASSERTION="API-key revocation after failure"
  fi
  [[ -f "$RESULTS" ]] || exit "$rc"
  current=$(jq -r '.functional // "UNKNOWN"' "$RESULTS" 2>/dev/null || printf 'UNKNOWN')
  if [[ "$current" == RUNNING ]]; then
    final="$RESULTS.final.$$.${RANDOM}"
    jq --arg assertion "$CURRENT_ASSERTION" --arg detail "qualification exited before completing this assertion" \
      '.functional="FAIL" | .failure={assertion:$assertion,detail:$detail}' "$RESULTS" >"$final"
    mv "$final" "$RESULTS"
  fi
  return "$rc"
}
trap 'finalize_on_exit "$?"' EXIT
trap 'exit 143' INT TERM HUP
record() {
  local id=$1 name=$2 status=$3 boundary=$4 detail=$5
  CURRENT_ASSERTION="$name"
  jq --arg id "$id" --arg name "$name" --arg status "$status" --arg boundary "$boundary" --arg detail "$detail" '.assertions += [{id:$id,name:$name,status:$status,boundary:$boundary,detail:$detail}]' "$RESULTS" >"$tmp"
  mv "$tmp" "$RESULTS"
}
wait_json() {
  local label=$1 command=$2 timeout=${3:-300} start now
  start=$(date +%s)
  while :; do
    if eval "$command" 2>/dev/null; then return 0; fi
    now=$(date +%s)
    (( now - start >= timeout )) && { echo "$label did not converge after ${timeout}s" >&2; return 1; }
    sleep 2
  done
}
"${OC[@]}" get deployment ai-gateway-controller -n "$OPENSHIFT_E2E_CONTROLLER_NAMESPACE" -o json >"$OUT/controller.json"
wait_json "controller deployment" "${OC[*]} get deployment ai-gateway-controller -n $OPENSHIFT_E2E_CONTROLLER_NAMESPACE -o json | jq -e '.status.availableReplicas == 1' >/dev/null" || { record 1 "controller deployment Ready" FAIL deployment "controller did not become available"; exit 1; }
record 1 "controller deployment Ready" PASS deployment "availableReplicas=1"
"${OC[@]}" get aitenant "$AITENANT_NAME" -n ai-tenants -o json >"$OUT/aitenant.json"
wait_json "AITenant Ready" "${OC[*]} get aitenant $AITENANT_NAME -n ai-tenants -o json | jq -e '[.status.conditions[]? | select(.type==\"Ready\" and .status==\"True\")] | length == 1' >/dev/null" || { record 2 "AITenant Ready" FAIL maas "MaaS did not report Ready"; exit 1; }
record 2 "AITenant Ready" PASS maas "MaaS reported Ready"
wait_json "provider Ready" "${OC[*]} get externalprovider provider-a -n $OPENSHIFT_E2E_TENANT_NAMESPACE -o json | jq -e '.status.phase == \"Ready\"' >/dev/null" || { record 3 "ExternalProvider Ready" FAIL controller "provider status did not converge"; exit 1; }
record 3 "ExternalProvider Ready" PASS controller "status.phase=Ready"
wait_json "model Ready" "${OC[*]} get externalmodel demo-model -n $OPENSHIFT_E2E_TENANT_NAMESPACE -o json | jq -e '.status.phase == \"Ready\"' >/dev/null" || { record 4 "ExternalModel Ready" FAIL controller "model status did not converge"; exit 1; }
record 4 "ExternalModel Ready" PASS controller "status.phase=Ready"
"${OC[@]}" get httproute -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o json >"$OUT/httproutes.json"
wait_json "HTTPRoute acceptance" "${OC[*]} get httproute -n $OPENSHIFT_E2E_TENANT_NAMESPACE -o json | jq -e '[.items[].status.parents[]?.conditions[]? | select(.type==\"Accepted\" and .status==\"True\")] | length > 0' >/dev/null" || { record 5 "HTTPRoute Accepted" FAIL gateway "no Accepted=True parent"; exit 1; }
record 5 "HTTPRoute Accepted" PASS gateway "Accepted=True observed"
wait_json "HTTPRoute references" "${OC[*]} get httproute -n $OPENSHIFT_E2E_TENANT_NAMESPACE -o json | jq -e '[.items[].status.parents[]?.conditions[]? | select(.type==\"ResolvedRefs\" and .status==\"True\")] | length > 0' >/dev/null" || { record 6 "HTTPRoute ResolvedRefs" FAIL gateway "no ResolvedRefs=True parent"; exit 1; }
record 6 "HTTPRoute ResolvedRefs" PASS gateway "ResolvedRefs=True observed"
"${OC[@]}" get deployment,service,serviceaccount,configmap -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o json >"$OUT/tenant-resources.json"
wait_json "Praxis deployment" "${OC[*]} get deployment -n $OPENSHIFT_E2E_TENANT_NAMESPACE -l app.kubernetes.io/managed-by=ai-gateway-controller -o json | jq -e '.items | length == 1 and .[0].status.availableReplicas == 1' >/dev/null" || { record 7 "standalone Praxis Ready" FAIL tenant "tenant Praxis deployment did not become available"; exit 1; }
record 7 "standalone Praxis Ready" PASS tenant "tenant-local deployment available"
PRAXIS_SA=$("${OC[@]}" get deployment -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -l app.kubernetes.io/managed-by=ai-gateway-controller -o jsonpath='{.items[0].spec.template.spec.serviceAccountName}' 2>/dev/null || true)
if [[ -z "$PRAXIS_SA" ]]; then
  record 8 "Praxis Secret API denied" FAIL rbac "standalone Praxis ServiceAccount was not found"
  exit 1
fi
SECRET_API_ACCESS=$("${OC[@]}" auth can-i get secrets --as="system:serviceaccount:$OPENSHIFT_E2E_TENANT_NAMESPACE:$PRAXIS_SA" -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" || true)
if [[ "$SECRET_API_ACCESS" == no ]]; then
  record 8 "Praxis Secret API denied" PASS rbac "tenant Praxis ServiceAccount denied Secret reads"
else
  record 8 "Praxis Secret API denied" FAIL rbac "tenant Praxis ServiceAccount can read Secrets"
  exit 1
fi
"${OC[@]}" get pods -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -l app=praxis -o json >"$OUT/praxis-pods.json"
"${OC[@]}" get events -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" --sort-by=.lastTimestamp >"$OUT/events.txt" 2>/dev/null || :
jq '.functional="RUNNING" | .note="functional request qualification"' "$RESULTS" >"$tmp"
mv "$tmp" "$RESULTS"
CLIENT="xmp-client-$OPENSHIFT_E2E_RUN_ID"
# The command array is intentionally expanded in several remote-command
# contexts below; keep these reviewed expansions visible to ShellCheck.
# shellcheck disable=SC2068,SC2016
HOST=$(${OC[@]} get gateway "$OPENSHIFT_E2E_GATEWAY_NAME" -n "$OPENSHIFT_E2E_GATEWAY_NAMESPACE" -o jsonpath='{.status.addresses[0].value}')
URL="https://$HOST/$OPENSHIFT_E2E_TENANT_NAMESPACE/demo/v1/chat/completions"
request() {
  local key=$1 url=$2 body=$3
  if [[ "$key" == none ]]; then
    printf '%s' "$body" | ${OC[@]} exec -i "$CLIENT" -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -- sh -c 'curl --silent --show-error --output /tmp/xmp-request --write-out "%{http_code}" --max-time 30 --cacert /etc/xmp/ca/ca.crt -X POST "$1" -H "Content-Type: application/json" --data-binary @-' sh "$url"
    return
  fi
  printf '%s\n%s' "$key" "$body" | ${OC[@]} exec -i "$CLIENT" -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -- sh -c 'read -r key; curl --silent --show-error --output /tmp/xmp-request --write-out "%{http_code}" --max-time 30 --cacert /etc/xmp/ca/ca.crt -X POST "$1" -H "Authorization: Bearer $key" -H "Content-Type: application/json" --data-binary @-' sh "$url"
}
wait_for_gateway_tls() {
  local deadline=$((SECONDS + 180)) status
  while (( SECONDS < deadline )); do
    status=$(request none "$URL" "$request_body" 2>/dev/null || true)
    if [[ "$status" == 401 ]]; then
      return 0
    fi
    # A response is definitive. Only transport status 000 may be retried
    # while the externally programmed load balancer becomes reachable.
    if [[ "$status" != 000 ]]; then
      printf 'Gateway readiness returned unexpected HTTP status %s\n' "$status" >&2
      return 1
    fi
    sleep 2
  done
  printf 'Gateway TLS endpoint did not become reachable before deadline\n' >&2
  return 1
}
wait_for_praxis_overlay() {
  local expected_provider=$1 expected_generation=$2 expected_digest=$3
  local stable=0 previous="" observation config_content mounted_content static_config generation digest recomputed mounted_digest
  local deadline=$((SECONDS + 180))
  while (( SECONDS < deadline )); do
    observation=""
    config_content=$(${OC[@]} get configmap routing-overlay -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o jsonpath='{.data.routing-overlay\.json}' 2>/dev/null || true)
    static_config=$(${OC[@]} get configmap praxis-config -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o jsonpath='{.data.config\.yaml}' 2>/dev/null || true)
    generation=$(${OC[@]} get configmap routing-overlay -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o jsonpath='{.metadata.annotations.inference\.opendatahub\.io/routing-overlay-source-generation}' 2>/dev/null || true)
    digest=$(${OC[@]} get configmap routing-overlay -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o jsonpath='{.metadata.annotations.inference\.opendatahub\.io/routing-overlay-content-digest}' 2>/dev/null || true)
    mounted_content=$(${OC[@]} exec deploy/praxis -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -- cat /etc/praxis/routing/routing-overlay.json 2>/dev/null || true)
    printf '%s' "$config_content" >"$OUT/config-overlay.json"
    printf '%s' "$mounted_content" >"$OUT/mounted-overlay.json"
    recomputed=$($RECOMPUTE_BIN "$OUT/config-overlay.json" 2>/dev/null || true)
    mounted_digest=$($RECOMPUTE_BIN "$OUT/mounted-overlay.json" 2>/dev/null || true)
    # Bind the expected revision only after the requested provider is present;
    # otherwise a reset can accidentally bind the previous provider's stable
    # revision and reject the subsequent valid transition.
    if [[ -z "$expected_generation" && -n "$generation" && "$digest" =~ ^[0-9a-f]{64}$ && "$config_content" == *"provider-provider-$expected_provider"* && "$mounted_content" == *"provider-provider-$expected_provider"* ]]; then
      expected_generation="$generation"
      expected_digest="$digest"
    fi
    if [[ "$generation" == "$expected_generation" && "$digest" == "$expected_digest" && "$digest" =~ ^[0-9a-f]{64}$ && "$recomputed" == "$digest" && "$mounted_digest" == "$digest" && "$config_content" == *"provider-provider-$expected_provider"* && "$mounted_content" == *"provider-provider-$expected_provider"* && "$static_config" == *"provider-provider-a"* && "$static_config" == *"provider-provider-b"* ]]; then
      observation="$generation:$digest:$mounted_digest:$expected_provider"
      if [[ "$observation" == "$previous" ]]; then stable=$((stable + 1)); else stable=1; previous="$observation"; fi
      [[ "$stable" -ge 2 ]] && return 0
    else
      stable=0
      previous=""
    fi
    sleep 2
  done
  printf 'overlay convergence failed: expected provider=%s generation=%s digest=%s observed generation=%s digest=%s recomputed=%s mounted=%s\n' "$expected_provider" "$expected_generation" "$expected_digest" "$generation" "$digest" "$recomputed" "$mounted_digest" >&2
  return 1
}
${OC[@]} patch externalmodel demo-model -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" --type=json -p='[{"op":"replace","path":"/spec/externalProviderRefs/0/weight","value":1},{"op":"replace","path":"/spec/externalProviderRefs/1/weight","value":0}]' >"$OUT/baseline-provider-a.txt"
wait_json "first endpoint controller reconciliation" "${OC[*]} get externalmodel demo-model -n $OPENSHIFT_E2E_TENANT_NAMESPACE -o json | jq -e '.status.phase == \"Ready\"' >/dev/null" 180 || { record baseline "First endpoint baseline" FAIL cleanup "controller did not reconcile first endpoint"; exit 1; }
wait_json "first endpoint ConfigMap" "${OC[*]} get configmap routing-overlay -n $OPENSHIFT_E2E_TENANT_NAMESPACE -o json | jq -e '.data[\"routing-overlay.json\"] | contains(\"provider-provider-a\") and (contains(\"provider-provider-b\") | not)' >/dev/null" 180 || { record baseline "First endpoint baseline" FAIL cleanup "first endpoint ConfigMap did not converge"; exit 1; }
wait_for_praxis_overlay a "" "" || { record baseline "First endpoint baseline" FAIL cleanup "Praxis projection did not converge"; exit 1; }
request_body='{"model":"demo","messages":[{"role":"user","content":"qualification"}]}'
wait_for_gateway_tls || { record 9 "Gateway TLS endpoint" FAIL authorization "TLS endpoint did not reach the expected unauthenticated response"; exit 1; }
unauth=$(request none "$URL" "$request_body" 2>/dev/null || true)
[[ "$unauth" == 401 ]] && record 9 "Unauthenticated request" PASS authorization "first test endpoint returned HTTP 401" || { record 9 "Unauthenticated request" FAIL authorization "expected 401, observed $unauth"; exit 1; }
ADMIN=$(${OC[@]} whoami -t)
SUB=$(${OC[@]} get maassubscription -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o jsonpath='{.items[0].metadata.name}')
printf '%s\n%s' "$ADMIN" "{\"name\":\"xmp-$OPENSHIFT_E2E_RUN_ID\",\"subscription\":\"$SUB\"}" | ${OC[@]} exec -i "$CLIENT" -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -- sh -c 'read -r token; curl --silent --show-error --output /tmp/xmp-key --write-out "%{http_code}" --max-time 20 --cacert /etc/xmp/ca/ca.crt -X POST "https://'"$HOST"'/v1/api-keys" -H "Authorization: Bearer $token" -H "Content-Type: application/json" --data-binary @-' >"$OUT/key-create-status.txt"
key_json=$(${OC[@]} exec "$CLIENT" -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -- sh -c 'cat /tmp/xmp-key && : > /tmp/xmp-key'); KEY=$(printf '%s' "$key_json" | sed -n 's/.*"key":"\([^"]*\)".*/\1/p'); KEY_ID=$(printf '%s' "$key_json" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[[ $(cat "$OUT/key-create-status.txt") == 201 && -n "$KEY" && -n "$KEY_ID" ]] || { record 10 "API-key creation" FAIL authorization "real MaaS key creation failed"; exit 1; }
record 10 "API-key creation" PASS authorization "HTTP 201; key withheld"
a_status=$(request "$KEY" "$URL" "$request_body" 2>/dev/null || true); a_body=$(${OC[@]} exec "$CLIENT" -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -- sh -c 'sed -n "s/.*host=\([^,\" ]*\).*/\1/p" /tmp/xmp-request' | head -1 || true)
[[ "$a_status" == 200 && "$a_body" == *provider-a* ]] && record 11 "First test endpoint" PASS routing "HTTP 200; Test Provider A attribution observed" || { record 11 "First test endpoint" FAIL routing "expected HTTP 200 from first endpoint"; exit 1; }
unknown=$(request "$KEY" "https://$HOST/$OPENSHIFT_E2E_TENANT_NAMESPACE/missing-model/v1/chat/completions" '{"model":"missing-model","messages":[{"role":"user","content":"qualification"}]}' 2>/dev/null || true)
[[ "$unknown" == 404 ]] && record 12 "Unknown model" PASS routing "HTTP 404" || { record 12 "Unknown model" FAIL routing "expected HTTP 404, observed $unknown"; exit 1; }
before=$(${OC[@]} get pods -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -l app=praxis -o json | jq -r '.items[0] | [.metadata.uid,([.status.containerStatuses[]?.restartCount]|add // 0)] | @tsv')
${OC[@]} patch externalmodel demo-model -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" --type=json -p='[{"op":"replace","path":"/spec/externalProviderRefs/0/weight","value":0},{"op":"replace","path":"/spec/externalProviderRefs/1/weight","value":1}]' >"$OUT/endpoint-switch.txt"
wait_json "second endpoint overlay" "${OC[*]} get configmap routing-overlay -n $OPENSHIFT_E2E_TENANT_NAMESPACE -o json | jq -e '.data[\"routing-overlay.json\"] | contains(\"provider-provider-b\")' >/dev/null" 180 || { record 13-converge "Endpoint switch convergence" FAIL routing "second endpoint overlay did not converge"; exit 1; }
wait_for_praxis_overlay b "" "" || { record 13-converge "Endpoint switch convergence" FAIL routing "second endpoint projection did not converge"; exit 1; }
b_status=$(request "$KEY" "$URL" "$request_body" 2>/dev/null || true); b_body=$(${OC[@]} exec "$CLIENT" -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -- sh -c 'sed -n "s/.*host=\([^,\" ]*\).*/\1/p" /tmp/xmp-request' | head -1 || true)
[[ "$b_status" == 200 && "$b_body" == *provider-b* ]] && record 13 "Second test endpoint" PASS routing "HTTP 200; Test Provider B attribution observed" || { record 13 "Second test endpoint" FAIL routing "expected HTTP 200 from second endpoint"; exit 1; }
after=$(${OC[@]} get pods -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -l app=praxis -o json | jq -r '.items[0] | [.metadata.uid,([.status.containerStatuses[]?.restartCount]|add // 0)] | @tsv')
[[ "$before" == "$after" ]] && record 14 "Praxis identity stable" PASS tenant "UID and restart count unchanged" || { record 14 "Praxis identity stable" FAIL tenant "before=$before after=$after"; exit 1; }
noop_before=$(${OC[@]} get externalmodel demo-model -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o json | jq -r '.metadata.generation')
noop_cm_before=$(${OC[@]} get configmap routing-overlay -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o json | jq -r '[.metadata.generation,.metadata.resourceVersion,.metadata.annotations["inference.opendatahub.io/routing-overlay-content-digest"]] | @tsv')
${OC[@]} get externalmodel demo-model -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o json | jq 'del(.metadata.creationTimestamp,.metadata.generation,.metadata.managedFields,.metadata.resourceVersion,.metadata.uid,.status)' | ${OC[@]} apply --server-side --field-manager=openshift-e2e-noop -f - >"$OUT/semantic-noop.txt"
noop_after=$(${OC[@]} get externalmodel demo-model -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o json | jq -r '.metadata.generation')
noop_cm_after=$(${OC[@]} get configmap routing-overlay -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o json | jq -r '[.metadata.generation,.metadata.resourceVersion,.metadata.annotations["inference.opendatahub.io/routing-overlay-content-digest"]] | @tsv')
[[ "$noop_before" == "$noop_after" && "$noop_cm_before" == "$noop_cm_after" ]] && record 15 "Semantic no-op" PASS routing "ExternalModel generation and overlay generation, resourceVersion, and digest unchanged" || { record 15 "Semantic no-op" FAIL routing "before=$noop_before/$noop_cm_before after=$noop_after/$noop_cm_after"; exit 1; }
valid_overlay=$(${OC[@]} get configmap routing-overlay -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -o jsonpath='{.data.routing-overlay\.json}')
printf '%s' "$valid_overlay" >"$OUT/valid-overlay-before-lkg.json"
${OC[@]} patch configmap routing-overlay -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" --type=merge -p='{"data":{"routing-overlay.json":"{invalid-overlay"}}' >"$OUT/invalid-overlay-injection.txt"
sleep 3
lkg_mounted=$(${OC[@]} exec deploy/praxis -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -- cat /etc/praxis/routing/routing-overlay.json 2>/dev/null || true)
lkg_status=$(request "$KEY" "$URL" "$request_body" 2>/dev/null || true)
[[ "$lkg_status" == 200 && "$lkg_mounted" == *provider-provider-b* ]] && record 16 "Invalid overlay LKG" PASS routing "malformed replacement retained the accepted Provider B route" || { record 16 "Invalid overlay LKG" FAIL routing "expected HTTP 200 with mounted Provider B last-known-good overlay, observed $lkg_status"; exit 1; }
# The publisher intentionally refuses to chain from a tampered envelope.  The
# documented, run-owned recovery is to remove only that corrupted ConfigMap;
# the next normal ExternalModel reconciliation recreates the valid envelope.
${OC[@]} delete configmap routing-overlay -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" --ignore-not-found >"$OUT/lkg-recovery-delete.txt"
${OC[@]} patch externalmodel demo-model -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" --type=json -p='[{"op":"replace","path":"/spec/externalProviderRefs/0/weight","value":1},{"op":"replace","path":"/spec/externalProviderRefs/1/weight","value":0}]' >"$OUT/reset-provider-a.txt"
wait_for_praxis_overlay a "" "" || { record 18 "Provider A reset" FAIL cleanup "baseline did not restore"; exit 1; }
reset_status=$(request "$KEY" "$URL" "$request_body" 2>/dev/null || true); reset_body=$(${OC[@]} exec "$CLIENT" -n "$OPENSHIFT_E2E_TENANT_NAMESPACE" -- sh -c 'sed -n "s/.*host=\([^,\" ]*\).*/\1/p" /tmp/xmp-request' | head -1 || true)
[[ "$reset_status" == 200 && "$reset_body" == *provider-a* ]] && record 19 "Provider A reset request" PASS routing "HTTP 200; first test endpoint restored" || { record 19 "Provider A reset request" FAIL routing "expected HTTP 200 from first endpoint after reset"; exit 1; }
revoke_key && record 17 "API-key revocation" PASS authorization "HTTP 200" || { record 17 "API-key revocation" FAIL authorization "revocation failed"; exit 1; }
if rg -i 'authorization:|sk-oai-|Bearer[[:space:]]+sk-' "$OUT" >/dev/null 2>&1; then record 20 "Credential leak scan" FAIL security "sensitive pattern found"; else record 20 "Credential leak scan" PASS security "no credential patterns found"; fi
jq '.functional=(if (([.assertions[] | select(.status=="FAIL")] | length) == 0 and ([.assertions[] | select(.status=="NOT_DEMONSTRATED")] | length) == 0) then "PASS" else "PARTIAL" end) | .note="Single-tenant routing; two-tenant MaaS authorization remains issue #23"' "$RESULTS" >"$tmp"
mv "$tmp" "$RESULTS"
printf '%s\n' "$RESULTS"
if [[ "$(jq '[.assertions[] | select(.status=="FAIL")] | length' "$RESULTS")" -ne 0 ]]; then exit 1; fi
