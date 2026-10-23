#!/usr/bin/env bash
# shellcheck disable=SC2015
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT"
CLUSTER=external-model-two-plane
NS=models-as-a-service
EVIDENCE="$ROOT/evidence/$(date -u +%Y%m%dT%H%M%SZ)-extproc-e2e"
if [[ -v LOCAL_ENV_CLUSTER && -n "$LOCAL_ENV_CLUSTER" ]]; then CLUSTER=$LOCAL_ENV_CLUSTER; fi
if [[ -v LOCAL_ENV_NAMESPACE && -n "$LOCAL_ENV_NAMESPACE" ]]; then NS=$LOCAL_ENV_NAMESPACE; fi
if [[ -v LOCAL_ENV_EVIDENCE && -n "$LOCAL_ENV_EVIDENCE" ]]; then EVIDENCE=$LOCAL_ENV_EVIDENCE; fi
API_NS=maas-system
EXT_DEPLOYMENT=payload-processing-external-model
EXT_SERVICE=payload-processing-external-model
EXT_CONFIG=payload-processing-external-model-plugins
SEED_FAILED_HOME_MODEL=${LOCAL_ENV_SEED_FAILED_HOME_MODEL:-false}
mkdir -p "$EVIDENCE"
exec > >(tee "$EVIDENCE/e2e.log") 2>&1
evidence_label() {
  case "$1" in
    "$ROOT"/*) printf '%s\n' "${1#"$ROOT"/}" ;;
    *) printf '%s\n' 'run-evidence' ;;
  esac
}
kctl() { kubectl --context "kind-$CLUSTER" "$@"; }

RESULTS="$EVIDENCE/results.json"
TMP_DIR=$(mktemp -d)
APF=""
GPF=""
KEY_ID=""
KEY_REVOKED=false
ACTIVE_ASSERTION=""
CA_CERT=""
AUTH_CFG=""
ADMIN_CFG=""
OVERRIDE_SECRET='provider-model-override-credentials'
MODEL_OVERRIDE_ACTIVE=false
DUPLICATE_ACTIVE=false
RECOMPUTE="$EVIDENCE/recompute-digest"
go build -o "$RECOMPUTE" "$ROOT/test/kind-env/recompute_digest.go"

python3 - "$RESULTS" <<'PY'
import json, os, sys, tempfile
p=sys.argv[1]
fd,t=tempfile.mkstemp(prefix=".results.",dir=os.path.dirname(p))
with os.fdopen(fd,"w") as f:
    json.dump({"status":"RUNNING","assertions":[],"requests":[]},f,indent=2); f.flush(); os.fsync(f.fileno())
os.replace(t,p)
PY

write_result() {
  local mode=$1
  shift
  python3 - "$RESULTS" "$mode" "$@" <<'PY'
import json, os, sys, tempfile
p,mode,*args=sys.argv[1:]
d=json.load(open(p))
if mode=="record":
    n,name,result,observed=args
    d["assertions"].append({"number":int(n),"name":name,"result":result,"observed":observed})
elif mode=="request":
    name,status,provider=args
    d["requests"].append({"name":name,"http_status":status,"provider":provider})
elif mode=="finish":
    d["status"]=args[0]
fd,t=tempfile.mkstemp(prefix=".results.",dir=os.path.dirname(p))
with os.fdopen(fd,"w") as f:
    json.dump(d,f,indent=2,sort_keys=True); f.flush(); os.fsync(f.fileno())
os.replace(t,p)
PY
}

record() {
  ACTIVE_ASSERTION=$2
  write_result record "$@"
  printf '%s %s %s %s\n' "$1" "$3" "$2" "$4"
}

revoke_key() {
  [[ -n "$KEY_ID" && -n "$APF" ]] || return 0
  local status
  status=$(timeout 20s curl --noproxy '*' --cacert "$CA_CERT" --resolve "maas-api.maas-system.svc.cluster.local:$API_PORT:127.0.0.1" -sS -o /dev/null -w '%{http_code}' --config "$ADMIN_CFG" -X DELETE "https://maas-api.maas-system.svc.cluster.local:$API_PORT/v1/api-keys/$KEY_ID" 2>/dev/null || true)
  printf 'key_id=%s status=%s\n' "$KEY_ID" "$status" >"$EVIDENCE/key-revocation.txt"
  [[ "$status" == 200 || "$status" == 204 || "$status" == 404 ]] && KEY_REVOKED=true
}

finish() {
  local rc=$?
  set +e
  if [[ "$DUPLICATE_ACTIVE" == true ]]; then
    timeout 20s kubectl --context "kind-$CLUSTER" -n "$NS" patch externalmodel demo-model --type=json \
      -p='[{"op":"remove","path":"/spec/externalProviderRefs/2"}]' >/dev/null 2>&1 || true
  fi
  if [[ "$MODEL_OVERRIDE_ACTIVE" == true ]]; then
    timeout 20s kubectl --context "kind-$CLUSTER" -n "$NS" patch externalmodel demo-model --type=json \
      -p='[{"op":"remove","path":"/spec/externalProviderRefs/0/auth"}]' >/dev/null 2>&1 || true
  fi
  timeout 20s kubectl --context "kind-$CLUSTER" -n "$NS" delete secret "$OVERRIDE_SECRET" --ignore-not-found >/dev/null 2>&1 || true
  if [[ "$KEY_REVOKED" != true ]]; then revoke_key; fi
  [[ -n "$APF" ]] && kill "$APF" 2>/dev/null || true
  [[ -n "$GPF" ]] && kill "$GPF" 2>/dev/null || true
  rm -f "$AUTH_CFG" "$ADMIN_CFG"
  rm -rf "$TMP_DIR"
  if jq -e '.status=="RUNNING"' "$RESULTS" >/dev/null 2>&1; then
    write_result finish FAIL
    printf 'active_assertion=%s exit_code=%s\n' "$ACTIVE_ASSERTION" "$rc" >"$EVIDENCE/incomplete.txt"
  fi
  exit "$rc"
}
stop_on_signal() {
  exit 130
}
trap finish EXIT
trap stop_on_signal INT TERM

start_pf() {
  local name=$1 service=$2 local_port=$3 remote_port=$4
  timeout 600s kubectl --context "kind-$CLUSTER" -n "$API_NS" port-forward "svc/$service" "$local_port:$remote_port" >"$EVIDENCE/port-forward-$name.log" 2>&1 &
  echo $!
}

wait_port() {
  local p=$1
  for _ in $(seq 1 60); do
    if timeout 3s bash -c "cat </dev/null >/dev/tcp/127.0.0.1/$p" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}

identity() {
  kctl -n "$NS" get pods -l "app=payload-processing-external-model,maas.opendatahub.io/tenant-instance=$EXT_DEPLOYMENT" -o json 2>/dev/null |
    jq -r '[.items[]|select(.metadata.deletionTimestamp==null)|{uid:.metadata.uid,restarts:([.status.containerStatuses[]?.restartCount]|add//0)}] | sort_by(.uid) | .[0] | [.uid, (.restarts|tostring)] | join(" ")'
}

overlay_state() {
  local cm="$TMP_DIR/overlay.json" content_file="$TMP_DIR/content.json" mounted="$TMP_DIR/mounted.json"
  kctl -n "$NS" get configmap routing-overlay -o json >"$cm"
  timeout 5s kubectl --context "kind-$CLUSTER" -n "$NS" exec "deploy/$EXT_DEPLOYMENT" -- cat /etc/praxis/routing/routing-overlay.json >"$mounted" 2>/dev/null || true
  local declared generation recomputed mounted_digest serving_revision accepted_revision
  jq -r '.data["routing-overlay.json"] // empty' "$cm" >"$content_file"
  declared=$(jq -r '.metadata.annotations["inference.opendatahub.io/routing-overlay-content-digest"]' "$cm")
  generation=$(jq -r '.metadata.annotations["inference.opendatahub.io/routing-overlay-source-generation"]' "$cm")
  recomputed=$("$RECOMPUTE" "$content_file" 2>/dev/null || true)
  mounted_digest=$("$RECOMPUTE" "$mounted" 2>/dev/null || true)
  serving_revision=$(timeout 5s kubectl --context "kind-$CLUSTER" -n "$NS" logs "deploy/$EXT_DEPLOYMENT" --tail=100 2>/dev/null |
    rg 'accepted_revision' | tail -1 | grep -oE '[0-9a-f]{64}' | head -2 | tr '\n' ' ' || true)
  accepted_revision=$(awk '{print $1}' <<<"$serving_revision")
  serving_revision=$(awk '{print $2}' <<<"$serving_revision")
  printf 'generation=%s declared=%s recomputed=%s mounted=%s accepted=%s serving=%s a=%s b=%s\n' "$generation" "$declared" "$recomputed" "$mounted_digest" "$accepted_revision" "$serving_revision" "$(jq -r '.data["routing-overlay.json"]' "$cm"|grep -q provider-provider-a&&echo true||echo false)" "$(jq -r '.data["routing-overlay.json"]' "$cm"|grep -q provider-provider-b&&echo true||echo false)"
}

field_value() {
  local key=$1 state=$2 part
  for part in $state; do
    if [[ "$part" == "$key="* ]]; then
      printf '%s\n' "${part#*=}"
      return 0
    fi
  done
  return 1
}

wait_overlay() {
  local wanted=$1 stable=0 a b active_other declared recomputed mounted accepted serving
  for _ in $(seq 1 90); do
    a=$(overlay_state)
    if [[ "$wanted" == a ]]; then active_other=b; else active_other=a; fi
    declared=$(field_value declared "$a" || true)
    recomputed=$(field_value recomputed "$a" || true)
    mounted=$(field_value mounted "$a" || true)
    accepted=$(field_value accepted "$a" || true)
    serving=$(field_value serving "$a" || true)
    if [[ "$a" == *" $wanted=true"* && "$a" == *" $active_other=false"* &&
      -n "$declared" && "$declared" == "$recomputed" && "$declared" == "$mounted" &&
      "$declared" == "$accepted" && "$declared" == "$serving" ]]; then
      sleep 2
      b=$(overlay_state)
      if [[ "$a" == "$b" ]]; then
        stable=$((stable+1)); printf '%s\n' "$b" >"$EVIDENCE/overlay-$wanted-$stable.txt"
        [[ "$stable" -ge 2 ]] && return 0
      else stable=0; fi
    else stable=0; fi
    sleep 2
  done
  return 1
}

last_known_good_state() {
  local wanted=$1 mounted="$TMP_DIR/lkg-mounted.json" serving_revision accepted_revision
  timeout 5s kubectl --context "kind-$CLUSTER" -n "$NS" exec "deploy/$EXT_DEPLOYMENT" -- \
    cat /etc/praxis/routing/routing-overlay.json >"$mounted" 2>/dev/null || true
  local mounted_digest active_wanted active_other
  mounted_digest=$("$RECOMPUTE" "$mounted" 2>/dev/null || true)
  active_wanted=$(jq -e --arg cluster "provider-provider-$wanted" '.overlay.candidates[]?.cluster == $cluster' "$mounted" >/dev/null 2>&1 && echo true || echo false)
  active_other=$(jq -e --arg cluster "provider-provider-$([[ "$wanted" == a ]] && echo b || echo a)" '.overlay.candidates[]?.cluster == $cluster' "$mounted" >/dev/null 2>&1 && echo true || echo false)
  serving_revision=$(timeout 5s kubectl --context "kind-$CLUSTER" -n "$NS" logs "deploy/$EXT_DEPLOYMENT" --tail=100 2>/dev/null |
    rg 'accepted_revision' | tail -1 | grep -oE '[0-9a-f]{64}' | head -2 | tr '\n' ' ' || true)
  accepted_revision=$(awk '{print $1}' <<<"$serving_revision")
  serving_revision=$(awk '{print $2}' <<<"$serving_revision")
  printf 'mounted=%s accepted=%s serving=%s wanted=%s other=%s\n' \
    "$mounted_digest" "$accepted_revision" "$serving_revision" "$active_wanted" "$active_other"
}

wait_last_known_good() {
  local wanted=$1 expected_digest=$2 stable=0 a b mounted accepted serving
  for _ in $(seq 1 90); do
    a=$(last_known_good_state "$wanted")
    mounted=$(field_value mounted "$a" || true)
    accepted=$(field_value accepted "$a" || true)
    serving=$(field_value serving "$a" || true)
    if [[ "$a" == *" wanted=true"* && "$a" == *" other=false"* &&
      "$mounted" == "$expected_digest" && "$accepted" == "$expected_digest" &&
      "$serving" == "$expected_digest" ]]; then
      sleep 2
      b=$(last_known_good_state "$wanted")
      if [[ "$a" == "$b" ]]; then
        stable=$((stable+1)); printf '%s\n' "$b" >"$EVIDENCE/last-known-good-$stable.txt"
        [[ "$stable" -ge 2 ]] && return 0
      else stable=0; fi
    else stable=0; fi
    sleep 2
  done
  return 1
}

gateway_route_state() {
  local provider=$1 route_dump
  route_dump=$(timeout 5s kubectl --context "kind-$CLUSTER" -n "$API_NS" exec deploy/maas-default-gateway-istio -c istio-proxy -- \
    curl -sS http://127.0.0.1:15000/config_dump 2>/dev/null || true)
  jq -c --arg provider "$provider" '
    [.. | objects | select(.match? and .route?.cluster?) |
      select((.match.headers // [] | any(.name == "X-AI-Routing-Candidate" and .string_match.exact == ("provider-" + $provider))) or
             (.route.cluster == ("outbound|443||" + $provider + ".maas-system.svc.cluster.local"))) |
      {match, cluster:.route.cluster, host_rewrite:.route.host_rewrite_literal}] | sort_by(.cluster, (.match|tojson))
  ' <<<"$route_dump" | sha256sum | awk '{print $1}'
}

wait_gateway_route() {
  local provider=$1 stable=0 previous="" current
  for _ in $(seq 1 180); do
    current=$(gateway_route_state "$provider")
    if [[ "$current" != e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 && -n "$current" && "$current" == "$previous" ]]; then
      stable=$((stable + 1))
      printf 'provider=%s route_fingerprint=%s stable_sample=%s\n' "$provider" "$current" "$stable" >>"$EVIDENCE/gateway-route-$provider.txt"
      [[ "$stable" -ge 2 ]] && return 0
    else
      stable=0
    fi
    previous=$current
    sleep 1
  done
  return 1
}

wait_gateway_provider() {
  local provider=$1 host dump
  host="${provider}-ext.maas-system.svc.cluster.local"
  for _ in $(seq 1 120); do
    dump=$(timeout 5s kubectl --context "kind-$CLUSTER" -n "$API_NS" exec deploy/maas-default-gateway-istio -c istio-proxy -- \
      curl -sS http://127.0.0.1:15000/clusters 2>/dev/null || true)
    if awk -v prefix="${host}::" 'index($0, prefix) && index($0, "::health_flags::healthy") { found=1 } END { exit found ? 0 : 1 }' <<<"$dump"; then
      printf 'provider=%s endpoint_health=healthy\n' "$provider" >"$EVIDENCE/provider-cluster-$provider.txt"
      return 0
    fi
    sleep 1
  done
  printf 'provider=%s endpoint_health=not-healthy\n' "$provider" >"$EVIDENCE/provider-cluster-$provider.txt"
  return 1
}

preauth_healthy_endpoint_count() {
  local slices
  slices=$(kctl -n "$API_NS" get endpointslices -l "kubernetes.io/service-name=payload-pre-processing" -o json 2>/dev/null || printf '{"items":[]}')
  jq '[.items[]?.endpoints[]? | select((.conditions.ready // true) == true and (.conditions.terminating // false) == false) | .addresses[]?] | length' <<<"$slices"
}

preauth_endpoint_address_count() {
  local slices
  slices=$(kctl -n "$API_NS" get endpointslices -l "kubernetes.io/service-name=payload-pre-processing" -o json 2>/dev/null || printf '{"items":[]}')
  jq '[.items[]?.endpoints[]?.addresses[]?] | length' <<<"$slices"
}

preauth_live_pod_count() {
  local selector pods
  selector=$(kctl -n "$API_NS" get service/payload-pre-processing -o json 2>/dev/null \
    | jq -r '.spec.selector | to_entries | map("\(.key)=\(.value)") | join(",")')
  [[ -n "$selector" ]] || return 1
  pods=$(kctl -n "$API_NS" get pods -l "$selector" -o json 2>/dev/null || printf '{"items":[]}')
  jq '[.items[] | select(.status.phase != "Succeeded" and .status.phase != "Failed")] | length' <<<"$pods"
}

capture_preauth_diagnostics() {
  local label=$1 selector gateway_pod
  kctl -n "$API_NS" get deployment/payload-pre-processing -o json >"$EVIDENCE/preauth-$label-deployment.json" 2>"$EVIDENCE/preauth-$label-deployment.stderr" || true
  kctl -n "$API_NS" get service/payload-pre-processing -o json >"$EVIDENCE/preauth-$label-service.json" 2>"$EVIDENCE/preauth-$label-service.stderr" || true
  kctl -n "$API_NS" get endpoints/payload-pre-processing -o json >"$EVIDENCE/preauth-$label-endpoints.json" 2>"$EVIDENCE/preauth-$label-endpoints.stderr" || true
  kctl -n "$API_NS" get endpointslices -l "kubernetes.io/service-name=payload-pre-processing" -o json >"$EVIDENCE/preauth-$label-endpointslices.json" 2>"$EVIDENCE/preauth-$label-endpointslices.stderr" || true
  selector=$(kctl -n "$API_NS" get service/payload-pre-processing -o json 2>/dev/null | jq -r '.spec.selector | to_entries | map("\(.key)=\(.value)") | join(",")')
  if [[ -n "$selector" ]]; then
    kctl -n "$API_NS" get pods -l "$selector" -o json >"$EVIDENCE/preauth-$label-pods.json" 2>"$EVIDENCE/preauth-$label-pods.stderr" || true
  else
    printf '{"items":[]}\n' >"$EVIDENCE/preauth-$label-pods.json"
    printf 'pre-auth Service has no pod selector\n' >"$EVIDENCE/preauth-$label-pods.stderr"
  fi

  gateway_pod=$(kctl -n "$API_NS" get pods -l 'gateway.networking.k8s.io/gateway-name=maas-default-gateway' \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -z "$gateway_pod" ]]; then
    printf 'default Gateway pod not found\n' >"$EVIDENCE/preauth-$label-envoy-admin.stderr"
    return 0
  fi
  printf '%s\n' "$gateway_pod" >"$EVIDENCE/preauth-$label-gateway-pod.txt"
  timeout 15s kubectl --context "kind-$CLUSTER" -n "$API_NS" exec "pod/$gateway_pod" -c istio-proxy -- \
    curl -fsS http://127.0.0.1:15000/clusters >"$EVIDENCE/preauth-$label-envoy-clusters.txt" \
    2>"$EVIDENCE/preauth-$label-envoy-clusters.stderr" || true
  timeout 15s kubectl --context "kind-$CLUSTER" -n "$API_NS" exec "pod/$gateway_pod" -c istio-proxy -- \
    curl -fsS 'http://127.0.0.1:15000/clusters?format=json' >"$EVIDENCE/preauth-$label-envoy-clusters.json" \
    2>"$EVIDENCE/preauth-$label-envoy-clusters-json.stderr" || true
  timeout 15s kubectl --context "kind-$CLUSTER" -n "$API_NS" exec "pod/$gateway_pod" -c istio-proxy -- \
    curl -fsS http://127.0.0.1:15000/config_dump >"$EVIDENCE/preauth-$label-envoy-config-dump.json" \
    2>"$EVIDENCE/preauth-$label-envoy-config-dump.stderr" || true
  jq -c '[.. | objects | select(.name? == "envoy.filters.http.ext_proc.external-model-pre" or .name? == "envoy.filters.http.ext_proc.ipp-pre") | {name, failure_mode_allow:(.typed_config.failure_mode_allow // null), grpc_service:(.typed_config.grpc_service // null), processing_mode:(.typed_config.processing_mode // null)}]' \
    "$EVIDENCE/preauth-$label-envoy-config-dump.json" >"$EVIDENCE/preauth-$label-envoy-filter-config.json" 2>"$EVIDENCE/preauth-$label-envoy-filter-config.stderr" || printf '[]\n' >"$EVIDENCE/preauth-$label-envoy-filter-config.json"
  timeout 15s kubectl --context "kind-$CLUSTER" -n "$API_NS" exec "pod/$gateway_pod" -c istio-proxy -- \
    curl -fsS 'http://127.0.0.1:15000/stats?format=json' >"$EVIDENCE/preauth-$label-envoy-stats.json" \
    2>"$EVIDENCE/preauth-$label-envoy-stats.stderr" || true
  jq -r '[.stats[]? | select((.name // "") | contains("istiocustom.istio_requests_total") and contains("provider-selection-required-demo-model") and contains("response_code.500")) | .value] | add // 0' \
    "$EVIDENCE/preauth-$label-envoy-stats.json" >"$EVIDENCE/preauth-$label-envoy-500-counter.txt" 2>/dev/null || printf 'unknown\n' >"$EVIDENCE/preauth-$label-envoy-500-counter.txt"
  jq -r '[.cluster_statuses[]? | select(.name == "payload-pre-processing-extproc") | .host_statuses[]?.stats[]? | select(.name == "cx_connect_fail") | (.value // "0" | tonumber)] | add // 0' \
    "$EVIDENCE/preauth-$label-envoy-clusters.json" >"$EVIDENCE/preauth-$label-envoy-extproc-connect-fail-counter.txt" 2>/dev/null || printf 'unknown\n' >"$EVIDENCE/preauth-$label-envoy-extproc-connect-fail-counter.txt"
  jq -r '[.cluster_statuses[]? | select(.name == "payload-pre-processing-extproc") | .host_statuses[]?.stats[]? | select(.name == "rq_error") | (.value // "0" | tonumber)] | add // 0' \
    "$EVIDENCE/preauth-$label-envoy-clusters.json" >"$EVIDENCE/preauth-$label-envoy-extproc-rq-error-counter.txt" 2>/dev/null || printf 'unknown\n' >"$EVIDENCE/preauth-$label-envoy-extproc-rq-error-counter.txt"
}

wait_preauth_unavailable() {
  local ready observed_generation generation live_pods endpoints all_addresses
  for _ in $(seq 1 180); do
    ready=$(kctl -n "$API_NS" get deployment/payload-pre-processing -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
    observed_generation=$(kctl -n "$API_NS" get deployment/payload-pre-processing -o jsonpath='{.status.observedGeneration}' 2>/dev/null || true)
    generation=$(kctl -n "$API_NS" get deployment/payload-pre-processing -o jsonpath='{.metadata.generation}' 2>/dev/null || true)
    live_pods=$(preauth_live_pod_count || printf 'unknown')
    endpoints=$(preauth_healthy_endpoint_count || printf 'unknown')
    all_addresses=$(preauth_endpoint_address_count || printf 'unknown')
    if [[ "${ready:-0}" == 0 && -n "$generation" && "$observed_generation" == "$generation" &&
      "$live_pods" == 0 && "$endpoints" == 0 && "$all_addresses" == 0 ]]; then
      printf 'desired_replicas=0 ready_replicas=0 observed_generation=%s generation=%s live_pods_including_terminating=0 ready_endpoints=0 all_endpoint_addresses=0\n' \
        "$observed_generation" "$generation" >"$EVIDENCE/preauth-unavailable-precondition.txt"
      return 0
    fi
    sleep 1
  done
  printf 'desired_replicas=%s ready_replicas=%s observed_generation=%s generation=%s live_pods_including_terminating=%s ready_endpoints=%s all_endpoint_addresses=%s\n' \
    "$(kctl -n "$API_NS" get deployment/payload-pre-processing -o jsonpath='{.spec.replicas}' 2>/dev/null || printf 'unknown')" \
    "${ready:-unknown}" "${observed_generation:-unknown}" "${generation:-unknown}" "$live_pods" "$endpoints" "$all_addresses" \
    >"$EVIDENCE/preauth-unavailable-precondition.txt"
  return 1
}

sanitize() {
  local input=$1 output=$2
  jq -c '{id:(.id//null),model:(.model//null),provider:(.provider//.backend//.choices[0].message.content//null),error:(.error.type//null)}|with_entries(select(.value!=null))' "$input" >"$output" 2>/dev/null || printf '{"body":"redacted"}\n' >"$output"
}

gateway() {
  local name=$1 cfg=$2 extra=${3:-} body="$TMP_DIR/request.json" raw="$TMP_DIR/$1.raw"
  printf '%s\n' '{"model":"demo","messages":[{"role":"user","content":"hello"}]}' >"$body"
  if [[ -n "$extra" ]]; then
    timeout 30s curl --noproxy '*' -sS --max-time 20 -o "$raw" -w '%{http_code}' --config "$cfg" --config "$extra" -H 'content-type: application/json' --data-binary @"$body" "http://127.0.0.1:$GATEWAY_PORT/$NS/demo/v1/chat/completions" >"$TMP_DIR/$name.status" 2>/dev/null || true
  else
    timeout 30s curl --noproxy '*' -sS --max-time 20 -o "$raw" -w '%{http_code}' --config "$cfg" -H 'content-type: application/json' --data-binary @"$body" "http://127.0.0.1:$GATEWAY_PORT/$NS/demo/v1/chat/completions" >"$TMP_DIR/$name.status" 2>/dev/null || true
  fi
  sanitize "$raw" "$EVIDENCE/$name-response.json"
  cat "$TMP_DIR/$name.status"
}

status_assert() {
  local n=$1 name=$2 expected=$3 got=$4
  [[ "$got" == "$expected" ]] && record "$n" "$name" PASS "http=$got" || record "$n" "$name" FAIL "expected=$expected observed=$got"
}

wait_model_phase() {
  local wanted=$1 phase
  for _ in $(seq 1 90); do
    phase=$(kctl -n "$NS" get externalmodel demo-model -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [[ "$phase" == "$wanted" ]] && return 0
    sleep 2
  done
  return 1
}

gateway_body() {
  local name=$1 cfg=$2 path_model=$3 body_model=$4 extra=${5:-}
  local body="$TMP_DIR/$name.json" raw="$TMP_DIR/$name.raw"
  printf '{"model":"%s","messages":[{"role":"user","content":"hello"}]}' "$body_model" >"$body"
  if [[ -n "$extra" ]]; then
    timeout 30s curl --noproxy '*' -sS --max-time 20 -o "$raw" -w '%{http_code}' \
      --config "$cfg" --config "$extra" -H 'content-type: application/json' \
      --data-binary @"$body" "http://127.0.0.1:$GATEWAY_PORT/$NS/$path_model/v1/chat/completions" \
      >"$TMP_DIR/$name.status" 2>/dev/null || true
  else
    timeout 30s curl --noproxy '*' -sS --max-time 20 -o "$raw" -w '%{http_code}' \
      --config "$cfg" -H 'content-type: application/json' --data-binary @"$body" \
      "http://127.0.0.1:$GATEWAY_PORT/$NS/$path_model/v1/chat/completions" \
      >"$TMP_DIR/$name.status" 2>/dev/null || true
  fi
  sanitize "$raw" "$EVIDENCE/$name-response.json"
  cat "$TMP_DIR/$name.status"
}

stream_request() {
  local trace="$EVIDENCE/stream-timing.txt" body="$TMP_DIR/stream.json" curl_rc
  printf '%s\n' '{"model":"demo","messages":[{"role":"user","content":"stream"}],"stream":true}' >"$body"
  set +e
  timeout 45s curl --noproxy '*' --no-buffer -sS --connect-timeout 5 --max-time 40 \
    --config "$AUTH_CFG" -H 'content-type: application/json' --data-binary @"$body" \
    -w '\n__STATUS__%{http_code}\n' "http://127.0.0.1:$GATEWAY_PORT/$NS/demo/v1/chat/completions" \
    2>"$TMP_DIR/stream.err" |
    python3 -c 'import sys,time; n=0; first=None; last=None; status=""; started=time.monotonic();
for raw in sys.stdin:
    line=raw.strip()
    if line.startswith("__STATUS__"): status=line.removeprefix("__STATUS__")
    elif line.startswith("data:") and line != "data: [DONE]":
        n += 1; elapsed=(time.monotonic()-started)*1000
        if first is None: first=elapsed
        last=elapsed
        print(f"chunk={n} elapsed_ms={elapsed:.0f}")
print(f"chunks={n} first_ms={first or 0:.0f} last_ms={last or 0:.0f} status={status}")' >"$trace"
  curl_rc=${PIPESTATUS[0]}
  set -e
  printf 'curl_rc=%s\n' "$curl_rc" >>"$trace"
  cat "$trace"
}

provider_request_count() {
  local deployment=$1
  timeout 5s kubectl --context "kind-$CLUSTER" -n "$API_NS" logs "deploy/$deployment" --tail=300 2>/dev/null |
    rg -c 'POST /v1/chat/completions' || true
}

write_model_override_secret() {
  kctl -n "$NS" create secret generic "$OVERRIDE_SECRET" \
    --from-literal=api-key=kind-only-dummy --dry-run=client -o yaml |
    kctl apply -f - >/dev/null
}

finalize_results() {
  local failed
  failed=$(jq '[.assertions[] | select(.result != "PASS")] | length' "$RESULTS")
  if [[ "$failed" == 0 ]]; then
    write_result finish PASS
    return 0
  fi
  write_result finish FAIL
  printf 'qualification=FAIL assertions=%s failed=%s\n' "$(jq '.assertions|length' "$RESULTS")" "$failed"
  return 1
}

echo "cluster=$CLUSTER"
echo "evidence=$(evidence_label "$EVIDENCE")"
standalone=true
for r in deployments/praxis services/praxis configmaps/praxis-config; do kctl get "$r" -A >/dev/null 2>&1 && standalone=false; done
[[ "$standalone" == true ]] && record 1 standalone_praxis_absent PASS "resources_absent=true" || record 1 standalone_praxis_absent FAIL "resources_present=true"
if [[ "$SEED_FAILED_HOME_MODEL" == true ]]; then
  failed_provider_phase=$(kctl -n "$NS" get externalprovider failed-bootstrap-provider -o jsonpath='{.status.phase}' 2>/dev/null || true)
  failed_model_phase=$(kctl -n "$NS" get externalmodel failed-bootstrap-model -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [[ "$failed_provider_phase" == Failed && "$failed_model_phase" == Failed ]]; then
    record 38 failed_home_model_remains_failed PASS "provider_phase=Failed model_phase=Failed"
  else
    record 38 failed_home_model_remains_failed FAIL "provider_phase=${failed_provider_phase:-missing} model_phase=${failed_model_phase:-missing}"
  fi
fi
pre=$(kctl -n "$API_NS" get deploy/payload-pre-processing -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
post=$(kctl -n "$NS" get "deploy/$EXT_DEPLOYMENT" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
[[ "$pre" == 1 && "$post" == 1 ]] && record 2 extproc_ready PASS "pre=$pre post=$post" || record 2 extproc_ready FAIL "pre=$pre post=$post"
route=$(kctl -n "$NS" get httproute external-model-demo-model -o json 2>/dev/null || echo '{}')
accepted=$(jq -r '[.status.parents[]?.conditions[]?|select(.type=="Accepted")|.status]|any(.=="True")'<<<"$route")
resolved=$(jq -r '[.status.parents[]?.conditions[]?|select(.type=="ResolvedRefs")|.status]|any(.=="True")'<<<"$route")
[[ "$accepted" == true && "$resolved" == true ]] && record 3 route_status PASS "accepted=true resolved_refs=true" || record 3 route_status FAIL "accepted=$accepted resolved_refs=$resolved"
fqdn=$EXT_SERVICE.$NS.svc.cluster.local
ef=$(kctl -n "$API_NS" get envoyfilter payload-processing-external-model-filters -o json 2>/dev/null || echo '{}')
dr=$(kctl -n "$API_NS" get destinationrule "$EXT_SERVICE" -o json 2>/dev/null || echo '{}')
ef_host=$(jq -r '[.spec.configPatches[]? | select(.applyTo=="CLUSTER") | .patch.value // empty | select(.name=="payload-processing-external-model-extproc") | .load_assignment.endpoints[]?.lb_endpoints[]?.endpoint.address.socket_address.address // empty] | first // empty' <<<"$ef")
dr_host=$(jq -r '.spec.host // empty' <<<"$dr")
if [[ "$ef_host" == "$fqdn" && "$dr_host" == "$fqdn" ]]; then
  record 4 namespace_split PASS "envoy_cluster_host=$ef_host destination_rule_host=$dr_host"
else
  record 4 namespace_split FAIL "expected=$fqdn envoy_cluster_host=${ef_host:-absent} destination_rule_host=${dr_host:-absent}"
fi
cfg=$(kctl -n "$NS" get configmap "$EXT_CONFIG" -o jsonpath='{.data.extproc\.yaml}' 2>/dev/null || true)
grep -q intelligent_route<<<"$cfg"&&grep -q credential_inject<<<"$cfg"&&record 5 post_auth_chain PASS "filters_present=true"||record 5 post_auth_chain FAIL "filters_present=false"
if wait_overlay a; then record 6 overlay_a PASS "two_stable_samples=true"; else record 6 overlay_a FAIL "two_stable_samples=false"; fi
EVIDENCE_ROOT=${LOCAL_ENV_EVIDENCE_ROOT:-"$ROOT/evidence"}
ACTIVE_RUN=""
if [[ -s "$EVIDENCE_ROOT/.active-run" ]]; then
  ACTIVE_RUN=$(<"$EVIDENCE_ROOT/.active-run")
fi
CA_CERT="$EVIDENCE/maas-api-ca.crt"
if ! kctl -n kuadrant-system get configmap authorino-maas-api-ca -o jsonpath='{.data.ca\.crt}' >"$CA_CERT" 2>/dev/null || [[ ! -s "$CA_CERT" ]]; then
  CA_CERT=$(find "${ACTIVE_RUN:-$EVIDENCE_ROOT}" -name maas-api-ca.crt -type f 2>/dev/null | sort -r | head -1)
fi
[[ -s "$CA_CERT" ]]&&record 7 ca_available PASS "ca_present=true"||{ record 7 ca_available FAIL "ca_present=false"; exit 1; }
API_PORT=$((18000+($$%1000))); GATEWAY_PORT=$((API_PORT+1))
APF=$(start_pf api maas-api "$API_PORT" 8443); GPF=$(start_pf gateway maas-default-gateway-istio "$GATEWAY_PORT" 80)
wait_port "$API_PORT"&&wait_port "$GATEWAY_PORT"&&record 8 port_forward_ready PASS "api=true gateway=true"||{ record 8 port_forward_ready FAIL "ready=false"; exit 1; }
ACTIVE_ASSERTION=initial_gateway_route_convergence
wait_gateway_route provider-a || { echo 'provider=provider-a stable=false' >"$EVIDENCE/gateway-route-failure.txt"; exit 1; }
wait_gateway_provider provider-a || { echo 'provider=provider-a endpoint_health=not-healthy' >"$EVIDENCE/provider-cluster-failure.txt"; exit 1; }
ADMIN_CFG="$TMP_DIR/admin.cfg"; printf '%s\n' 'header = "X-MaaS-Username: kind-user"' 'header = "X-MaaS-Group: [\"system:authenticated\"]"' >"$ADMIN_CFG"
key_body="$TMP_DIR/key.json"
key_status=$(timeout 20s curl --noproxy '*' --cacert "$CA_CERT" --resolve "maas-api.maas-system.svc.cluster.local:$API_PORT:127.0.0.1" -sS -o "$key_body" -w '%{http_code}' --config "$ADMIN_CFG" -H 'content-type: application/json' --data '{"name":"issue39-kind-e2e","ephemeral":true,"subscription":"kind-e2e-subscription"}' "https://maas-api.maas-system.svc.cluster.local:$API_PORT/v1/api-keys" 2>/dev/null||true)
KEY_ID=$(jq -r '.id//.keyId//.metadata.id//empty' "$key_body" 2>/dev/null||true); KEY_VALUE=$(jq -r '.key//.apiKey//.token//empty' "$key_body" 2>/dev/null||true)
if [[ "$key_status" == 201 && -n "$KEY_ID" && -n "$KEY_VALUE" ]]; then AUTH_CFG="$TMP_DIR/auth.cfg"; printf 'header = "Authorization: Bearer %s"\n' "$KEY_VALUE">"$AUTH_CFG"; chmod 600 "$AUTH_CFG"; printf 'http_status=201 key_id_present=true key_value_saved=false\n'>"$EVIDENCE/key-created.txt"; record 9 key_created PASS "http=201"; else record 9 key_created FAIL "http=$key_status"; exit 1; fi
unauth_cfg="$TMP_DIR/unauth.cfg"; :>"$unauth_cfg"
unauth_before_a=$(provider_request_count katan-a); unauth_before_b=$(provider_request_count katan-b)
u=$(gateway unauthenticated "$unauth_cfg")
unauth_after_a=$(provider_request_count katan-a); unauth_after_b=$(provider_request_count katan-b)
status_assert 10 unauthenticated 401 "$u"
if [[ "$u" == 401 && "$unauth_before_a" == "$unauth_after_a" && "$unauth_before_b" == "$unauth_after_b" ]]; then
  record 39 unauthenticated_provider_isolation PASS "http=401 provider_a_unchanged=true provider_b_unchanged=true"
else
  record 39 unauthenticated_provider_isolation FAIL "http=$u provider_a_before=$unauth_before_a provider_a_after=$unauth_after_a provider_b_before=$unauth_before_b provider_b_after=$unauth_after_b"
fi
a=$(gateway provider_a "$AUTH_CFG"); status_assert 11 provider_a_request 200 "$a"; write_result request provider_a "$a" "A"
provider_a_body=$(cat "$EVIDENCE/provider_a-response.json")
if [[ "$a" == 200 && "$provider_a_body" == *katan-a-* && "$provider_a_body" != *katan-b-* ]]; then
  record 12 provider_a_attribution PASS "backend=katan-a"
else
  record 12 provider_a_attribution FAIL "expected_backend=katan-a observed=redacted"
fi
spoof_cfg="$TMP_DIR/spoof.cfg"; printf 'header = "x-ai-routing-candidate: provider-provider-b"\n'>"$spoof_cfg"; s=$(gateway caller_selection_spoof "$AUTH_CFG" "$spoof_cfg"); status_assert 13 caller_selection_spoof 200 "$s"
spoof_body=$(cat "$EVIDENCE/caller_selection_spoof-response.json")
if [[ "$s" == 200 && "$spoof_body" == *katan-a-* && "$spoof_body" != *katan-b-* ]]; then
  record 14 spoof_override_resistance PASS "caller_selection_ignored=true backend=katan-a"
else
  record 14 spoof_override_resistance FAIL "expected_backend=katan-a observed=redacted"
fi
sink=$(kctl -n "$NS" get svc provider-selection-required-demo-model -o json 2>/dev/null|jq -r 'if (.spec.selector//{})=={} then "absent" else "present" end'||true); [[ "$sink" == absent ]]&&record 15 sink_fail_closed PASS "selector=absent"||record 15 sink_fail_closed FAIL "selector=$sink"
unknown=$(timeout 30s curl --noproxy '*' -sS --max-time 20 -o "$TMP_DIR/unknown.raw" -w '%{http_code}' --config "$AUTH_CFG" -H 'content-type: application/json' --data '{"model":"unknown-model","messages":[{"role":"user","content":"hello"}]}' "http://127.0.0.1:$GATEWAY_PORT/$NS/unknown-model/v1/chat/completions" 2>/dev/null||true); sanitize "$TMP_DIR/unknown.raw" "$EVIDENCE/unknown-response.json"; status_assert 16 unknown_model 404 "$unknown"
before=$(identity||true)
kctl -n "$NS" patch externalmodel demo-model --type=json -p='[{"op":"replace","path":"/spec/externalProviderRefs/0/weight","value":0},{"op":"replace","path":"/spec/externalProviderRefs/1/weight","value":1}]'>/dev/null
wait_overlay b&&record 17 overlay_b PASS "two_stable_samples=true"||record 17 overlay_b FAIL "two_stable_samples=false"
ACTIVE_ASSERTION=provider_b_gateway_route_convergence
wait_gateway_route provider-b || { echo 'provider=provider-b stable=false' >"$EVIDENCE/gateway-route-failure.txt"; exit 1; }
wait_gateway_provider provider-b || { echo 'provider=provider-b endpoint_health=not-healthy' >"$EVIDENCE/provider-cluster-failure.txt"; exit 1; }
b=$(gateway provider_b "$AUTH_CFG"); write_result request provider_b "$b" "B"
provider_b_body=$(cat "$EVIDENCE/provider_b-response.json")
if [[ "$b" == 200 && "$provider_b_body" == *katan-b-* && "$provider_b_body" != *katan-a-* ]]; then
  record 18 provider_b_request PASS "http=200 backend=katan-b"
else
  record 18 provider_b_request FAIL "expected=http=200 backend=katan-b observed=redacted"
fi
after=$(identity||true); [[ "$before" == "$after" && "$after" == *" 0" ]]&&record 19 extproc_uid_restarts_stable PASS "before=$before after=$after"||record 19 extproc_uid_restarts_stable FAIL "before=$before after=$after"
kctl -n "$NS" patch externalmodel demo-model --type=json -p='[{"op":"replace","path":"/spec/externalProviderRefs/0/weight","value":1},{"op":"replace","path":"/spec/externalProviderRefs/1/weight","value":0}]'>/dev/null; wait_overlay a&&record 20 provider_a_reset PASS "stable=true"||record 20 provider_a_reset FAIL "stable=false"
ACTIVE_ASSERTION=provider_a_reset_gateway_route_convergence
wait_gateway_route provider-a || { echo 'provider=provider-a stable=false' >"$EVIDENCE/gateway-route-failure.txt"; exit 1; }
wait_gateway_provider provider-a || { echo 'provider=provider-a endpoint_health=not-healthy' >"$EVIDENCE/provider-cluster-failure.txt"; exit 1; }
noop_before=$(kctl -n "$NS" get configmap routing-overlay -o json|jq -c '{generation:.metadata.generation,resourceVersion:.metadata.resourceVersion,digest:.metadata.annotations["inference.opendatahub.io/routing-overlay-content-digest"],data:.data["routing-overlay.json"]}')
kctl -n "$NS" get externalmodel demo-model -o json|jq 'del(.metadata.resourceVersion,.metadata.uid,.metadata.creationTimestamp,.metadata.managedFields,.status)'|kctl apply -f - >/dev/null; sleep 4
noop_after=$(kctl -n "$NS" get configmap routing-overlay -o json|jq -c '{generation:.metadata.generation,resourceVersion:.metadata.resourceVersion,digest:.metadata.annotations["inference.opendatahub.io/routing-overlay-content-digest"],data:.data["routing-overlay.json"]}')
[[ "$noop_before" == "$noop_after" ]]&&record 21 semantic_noop PASS "unchanged=true"||record 21 semantic_noop FAIL "changed=true"
valid="$TMP_DIR/valid.json"; kctl -n "$NS" get cm routing-overlay -o jsonpath='{.data.routing-overlay\.json}'>"$valid"; kctl -n "$NS" get cm routing-overlay -o json|jq '.data["routing-overlay.json"]="{invalid-overlay"'|kctl apply -f - >/dev/null; sleep 5
valid_digest=$("$RECOMPUTE" "$valid")
ACTIVE_ASSERTION=last_known_good
if ! wait_last_known_good a "$valid_digest"; then
  record 22 last_known_good FAIL "convergence=false"
  exit 1
fi
lkg=$(gateway last_known_good "$AUTH_CFG")
if [[ "$lkg" == 200 ]]; then record 22 last_known_good PASS "converged=true http=200"; else record 22 last_known_good FAIL "converged=true expected=http=200 observed=$lkg"; fi
kctl -n "$NS" get cm routing-overlay -o json|jq --rawfile overlay "$valid" '.data["routing-overlay.json"]=$overlay'|kctl apply -f - >/dev/null; wait_overlay a&&record 23 valid_recovery PASS "stable=true"||record 23 valid_recovery FAIL "stable=false"
stream_result=$(stream_request)
printf '%s\n' "$stream_result" >"$EVIDENCE/stream-observed.txt"
stream_status=$(awk -F'status=' '/chunks=/{print $2}' "$EVIDENCE/stream-timing.txt" | tail -1)
stream_chunks=$(awk -F'[ =]' '/chunks=/{print $2}' "$EVIDENCE/stream-timing.txt" | tail -1)
stream_first=$(awk -F'[ =]' '/chunks=/{print $4}' "$EVIDENCE/stream-timing.txt" | tail -1)
stream_last=$(awk -F'[ =]' '/chunks=/{print $6}' "$EVIDENCE/stream-timing.txt" | tail -1)
if [[ "$stream_status" == 200 && "$stream_chunks" =~ ^[0-9]+$ && "$stream_chunks" -ge 2 &&
  "$stream_first" =~ ^[0-9]+$ && "$stream_last" =~ ^[0-9]+$ && "$stream_last" -gt "$stream_first" ]]; then
  record 27 streaming_two_chunks PASS "http=200 chunks=$stream_chunks first_ms=$stream_first last_ms=$stream_last"
else
  record 27 streaming_two_chunks FAIL "expected=http=200 chunks>=2 delayed=true observed=$(tr '\n' ' ' <"$EVIDENCE/stream-timing.txt")"
fi

write_model_override_secret
kctl -n "$NS" patch externalmodel demo-model --type=json -p='[{"op":"add","path":"/spec/externalProviderRefs/0/auth","value":{"type":"apikey","secretRef":{"name":"provider-model-override-credentials"}}}]' >/dev/null
MODEL_OVERRIDE_ACTIVE=true
override_ready=false
if wait_model_phase Ready && wait_overlay a; then override_ready=true; fi
override_ref=$(kctl -n "$NS" exec "deploy/$EXT_DEPLOYMENT" -- cat /etc/praxis/routing/routing-overlay.json 2>/dev/null |
  jq -r '.overlay.candidates[]? | select(.cluster=="provider-provider-a") | .credential.secretRef.name' 2>/dev/null || true)
config_override_ref=$(kctl -n "$NS" get configmap "$EXT_CONFIG" -o jsonpath='{.data.extproc\.yaml}' 2>/dev/null | grep -o "$OVERRIDE_SECRET" | head -1 || true)
volume_override_ref=$(kctl -n "$NS" get deploy "$EXT_DEPLOYMENT" -o json 2>/dev/null |
  jq -r --arg secret "$OVERRIDE_SECRET" '[.spec.template.spec.volumes[]?.projected.sources[]?.secret.name] | any(.==$secret)' 2>/dev/null || true)
printf 'overlay_secret_ref=%s extproc_config_reference=%s deployment_secret_projection=%s\n' "$override_ref" "$config_override_ref" "$volume_override_ref" >"$EVIDENCE/model-override-references.txt"
if [[ "$override_ready" == true && "$override_ref" == "$OVERRIDE_SECRET" && -n "$config_override_ref" && "$volume_override_ref" == true ]]; then
  override_request=$(gateway provider_a_model_override "$AUTH_CFG")
  status_assert 28 model_level_credential_override 200 "$override_request"
else
  record 28 model_level_credential_override FAIL "ready=$override_ready overlay_secret_ref=$override_ref config_reference=$([[ -n "$config_override_ref" ]] && echo true || echo false) projection=$volume_override_ref"
fi
override_digest=$(field_value declared "$(overlay_state)" || true)
kctl -n "$NS" delete secret "$OVERRIDE_SECRET" --ignore-not-found >/dev/null
kctl -n "$NS" annotate externalmodel demo-model "external-model-e2e/override-probe=$(date +%s)" --overwrite >/dev/null
override_failed=false
if wait_model_phase Failed; then override_failed=true; fi
after_delete_digest=$(field_value declared "$(overlay_state)" || true)
printf 'phase_failed=%s prior_digest=%s after_delete_digest=%s\n' "$override_failed" "$override_digest" "$after_delete_digest" >"$EVIDENCE/model-override-failure.txt"
if [[ "$override_failed" == true && "$after_delete_digest" == "$override_digest" ]]; then
  record 29 model_override_missing_fails_closed PASS "phase=Failed serving_digest_unchanged=true"
else
  record 29 model_override_missing_fails_closed FAIL "phase_failed=$override_failed serving_digest_unchanged=$([[ "$after_delete_digest" == "$override_digest" ]] && echo true || echo false)"
fi
write_model_override_secret
recovered=false
if wait_model_phase Ready && wait_overlay a; then recovered=true; fi
if [[ "$recovered" == true ]]; then
  recovered_request=$(gateway provider_a_model_override_recovery "$AUTH_CFG")
  [[ "$recovered_request" == 200 ]] && record 30 model_override_recovery PASS "http=200" || record 30 model_override_recovery FAIL "expected=http=200 observed=$recovered_request"
else
  record 30 model_override_recovery FAIL "ready=false"
fi
kctl -n "$NS" patch externalmodel demo-model --type=json -p='[{"op":"remove","path":"/spec/externalProviderRefs/0/auth"}]' >/dev/null
MODEL_OVERRIDE_ACTIVE=false
kctl -n "$NS" delete secret "$OVERRIDE_SECRET" --ignore-not-found >/dev/null
if wait_model_phase Ready && wait_overlay a; then record 31 model_override_cleanup PASS "restored_provider_credentials=true"; else record 31 model_override_cleanup FAIL "convergence=false"; exit 1; fi

duplicate_before=$(field_value declared "$(overlay_state)" || true)
duplicate_ref=$(kctl -n "$NS" get externalmodel demo-model -o json | jq -c '.spec.externalProviderRefs[0] | .weight=1')
jq -n --argjson value "$duplicate_ref" '[{"op":"add","path":"/spec/externalProviderRefs/-","value":$value}]' >"$TMP_DIR/duplicate-patch.json"
kctl -n "$NS" patch externalmodel demo-model --type=json --patch-file "$TMP_DIR/duplicate-patch.json" >/dev/null
DUPLICATE_ACTIVE=true
duplicate_failed=false
if wait_model_phase Failed; then duplicate_failed=true; fi
duplicate_after=$(field_value declared "$(overlay_state)" || true)
printf 'phase_failed=%s prior_digest=%s after_digest=%s\n' "$duplicate_failed" "$duplicate_before" "$duplicate_after" >"$EVIDENCE/duplicate-provider-binding.txt"
if [[ "$duplicate_failed" == true && "$duplicate_after" == "$duplicate_before" ]]; then
  record 32 duplicate_active_provider_binding_rejected PASS "phase=Failed serving_digest_unchanged=true"
else
  record 32 duplicate_active_provider_binding_rejected FAIL "phase_failed=$duplicate_failed serving_digest_unchanged=$([[ "$duplicate_after" == "$duplicate_before" ]] && echo true || echo false)"
fi
kctl -n "$NS" patch externalmodel demo-model --type=json -p='[{"op":"remove","path":"/spec/externalProviderRefs/2"}]' >/dev/null
DUPLICATE_ACTIVE=false
if wait_model_phase Ready && wait_overlay a; then record 33 duplicate_binding_recovery PASS "restored=true"; else record 33 duplicate_binding_recovery FAIL "convergence=false"; exit 1; fi

preauth_before_a=$(provider_request_count katan-a)
preauth_before_b=$(provider_request_count katan-b)
capture_preauth_diagnostics before-stop
kctl -n "$API_NS" scale deployment/payload-pre-processing --replicas=0 >/dev/null
preauth_stopped=false
if wait_preauth_unavailable; then preauth_stopped=true; fi
capture_preauth_diagnostics after-stop
preauth_500_before=$(cat "$EVIDENCE/preauth-after-stop-envoy-500-counter.txt" 2>/dev/null || printf 'unknown')
preauth_connect_fail_before=$(cat "$EVIDENCE/preauth-after-stop-envoy-extproc-connect-fail-counter.txt" 2>/dev/null || printf 'unknown')
preauth_rq_error_before=$(cat "$EVIDENCE/preauth-after-stop-envoy-extproc-rq-error-counter.txt" 2>/dev/null || printf 'unknown')
conflict_model_cfg="$TMP_DIR/conflict-model.cfg"; printf '%s\n' 'header = "X-Gateway-Model-Name: demo"' >"$conflict_model_cfg"
preauth_request_id="preauth-unavailable-$(date -u +%Y%m%dT%H%M%SZ)-$$"
printf 'header = "X-Request-ID: %s"\n' "$preauth_request_id" >>"$conflict_model_cfg"
trust_status=$(gateway_body preauth_unavailable "$AUTH_CFG" demo unauthorized-model "$conflict_model_cfg")
preauth_after_a=$(provider_request_count katan-a)
preauth_after_b=$(provider_request_count katan-b)
capture_preauth_diagnostics after-request
preauth_500_after=$(cat "$EVIDENCE/preauth-after-request-envoy-500-counter.txt" 2>/dev/null || printf 'unknown')
preauth_connect_fail_after=$(cat "$EVIDENCE/preauth-after-request-envoy-extproc-connect-fail-counter.txt" 2>/dev/null || printf 'unknown')
preauth_rq_error_after=$(cat "$EVIDENCE/preauth-after-request-envoy-extproc-rq-error-counter.txt" 2>/dev/null || printf 'unknown')
preauth_500_delta=unknown
preauth_connect_fail_delta=unknown
preauth_rq_error_delta=unknown
if [[ "$preauth_500_before" =~ ^[0-9]+$ && "$preauth_500_after" =~ ^[0-9]+$ ]]; then
  preauth_500_delta=$((preauth_500_after - preauth_500_before))
fi
if [[ "$preauth_connect_fail_before" =~ ^[0-9]+$ && "$preauth_connect_fail_after" =~ ^[0-9]+$ ]]; then
  preauth_connect_fail_delta=$((preauth_connect_fail_after - preauth_connect_fail_before))
fi
if [[ "$preauth_rq_error_before" =~ ^[0-9]+$ && "$preauth_rq_error_after" =~ ^[0-9]+$ ]]; then
  preauth_rq_error_delta=$((preauth_rq_error_after - preauth_rq_error_before))
fi
preauth_filter_fail_closed=$(jq -r 'any(.[]; .name == "envoy.filters.http.ext_proc.external-model-pre" and .failure_mode_allow != true)' \
  "$EVIDENCE/preauth-after-stop-envoy-filter-config.json" 2>/dev/null || printf 'false')
preauth_filter_active=$(jq -r '[.. | objects | select(.name? == "models-as-a-service.external-model-demo-model.1") | .typed_per_filter_config["envoy.filters.http.ext_proc.external-model-pre"]? | select(. != null and .disabled != true)] | length > 0' \
  "$EVIDENCE/preauth-after-stop-envoy-config-dump.json" 2>/dev/null || printf 'false')
preauth_endpoints_at_request=$(preauth_healthy_endpoint_count || printf 'unknown')
preauth_all_addresses_at_request=$(preauth_endpoint_address_count || printf 'unknown')
preauth_pods_at_request=$(preauth_live_pod_count || printf 'unknown')
preauth_desired_at_request=$(kctl -n "$API_NS" get deployment/payload-pre-processing -o jsonpath='{.spec.replicas}' 2>/dev/null || printf 'unknown')
printf 'request_id=%s\npreauth_stopped=%s desired_replicas_at_request=%s live_pods_including_terminating_at_request=%s ready_endpoints_at_request=%s all_endpoint_addresses_at_request=%s http=%s response_bytes=%s provider_a_requests_before=%s provider_a_requests_after=%s provider_b_requests_before=%s provider_b_requests_after=%s envoy_500_before=%s envoy_500_after=%s envoy_500_delta=%s extproc_connect_fail_before=%s extproc_connect_fail_after=%s extproc_connect_fail_delta=%s extproc_rq_error_before=%s extproc_rq_error_after=%s extproc_rq_error_delta=%s external_model_pre_filter_fail_closed=%s external_model_pre_route_filter_active=%s\n' \
  "$preauth_request_id" "$preauth_stopped" "$preauth_desired_at_request" "$preauth_pods_at_request" "$preauth_endpoints_at_request" "$preauth_all_addresses_at_request" "$trust_status" \
  "$(wc -c <"$TMP_DIR/preauth_unavailable.raw" 2>/dev/null || printf 'unknown')" "$preauth_before_a" "$preauth_after_a" "$preauth_before_b" "$preauth_after_b" \
  "$preauth_500_before" "$preauth_500_after" "$preauth_500_delta" "$preauth_connect_fail_before" "$preauth_connect_fail_after" "$preauth_connect_fail_delta" \
  "$preauth_rq_error_before" "$preauth_rq_error_after" "$preauth_rq_error_delta" "$preauth_filter_fail_closed" "$preauth_filter_active" >"$EVIDENCE/preauth-unavailable.txt"
kctl -n "$API_NS" logs deployment/maas-default-gateway-istio -c istio-proxy --since=2m >"$EVIDENCE/preauth-unavailable-gateway-logs.txt" 2>&1 || true
if [[ "$preauth_stopped" == true && "$preauth_desired_at_request" == 0 && "$preauth_pods_at_request" == 0 && "$preauth_endpoints_at_request" == 0 && "$preauth_all_addresses_at_request" == 0 &&
  "$preauth_filter_fail_closed" == true && "$preauth_filter_active" == true && "$trust_status" == 500 && "$preauth_500_delta" == 1 &&
  "$preauth_connect_fail_delta" == 1 && "$preauth_rq_error_delta" == 1 &&
  "$preauth_before_a" == "$preauth_after_a" && "$preauth_before_b" == "$preauth_after_b" ]]; then
  record 34 preauth_unavailable_trust_boundary PASS "http=500 envoy_500_delta=1 extproc_connect_fail_delta=1 extproc_rq_error_delta=1 fail_closed=true provider_a_contact=false provider_b_contact=false"
else
  record 34 preauth_unavailable_trust_boundary FAIL "stopped=$preauth_stopped desired=$preauth_desired_at_request pods=$preauth_pods_at_request ready_endpoints=$preauth_endpoints_at_request all_endpoint_addresses=$preauth_all_addresses_at_request http=$trust_status envoy_500_delta=$preauth_500_delta extproc_connect_fail_delta=$preauth_connect_fail_delta extproc_rq_error_delta=$preauth_rq_error_delta filter_fail_closed=$preauth_filter_fail_closed filter_active=$preauth_filter_active provider_a_contact=$([[ "$preauth_before_a" == "$preauth_after_a" ]] && echo false || echo true) provider_b_contact=$([[ "$preauth_before_b" == "$preauth_after_b" ]] && echo false || echo true)"
fi
kctl -n "$API_NS" scale deployment/payload-pre-processing --replicas=1 >/dev/null
preauth_restored=false
for _ in $(seq 1 90); do
  ready=$(kctl -n "$API_NS" get deployment/payload-pre-processing -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
  if [[ "$ready" == 1 ]]; then preauth_restored=true; break; fi
  sleep 1
done
[[ "$preauth_restored" == true ]] && record 35 preauth_restored PASS "ready_replicas=1" || record 35 preauth_restored FAIL "ready_replicas=${ready:-0}"
POST_SA=$(kctl -n "$NS" get "deployment/$EXT_DEPLOYMENT" -o jsonpath='{.spec.template.spec.serviceAccountName}' 2>/dev/null || true)
printf 'serviceAccount=%s\n' "$POST_SA" >"$EVIDENCE/post-auth-service-account.txt"
if [[ "$POST_SA" != "$EXT_DEPLOYMENT" ]]; then
  record 24 secret_api_denied FAIL "unexpected_post_auth_service_account=$POST_SA"
else
  can=$(kctl auth can-i get secrets --as="system:serviceaccount:$NS:$POST_SA" 2>/dev/null||true)
  [[ "$can" == no ]]&&record 24 secret_api_denied PASS "service_account=$POST_SA can_i=no"||record 24 secret_api_denied FAIL "service_account=$POST_SA can_i=$can"
fi
if ! rg -n -i '(authorization:|bearer[[:space:]]+[A-Za-z0-9._-]{12,}|api[_-]?key[=:][[:space:]]*[A-Za-z0-9._-]{12,})' "$EVIDENCE" --glob '!e2e.log' --glob '!*.err' >/dev/null 2>&1; then record 36 credential_scan PASS "clean=true"; else record 36 credential_scan FAIL "clean=false"; fi
revoke_key; [[ "$KEY_REVOKED" == true ]]&&record 37 key_revoked PASS "key_id=$KEY_ID"||record 37 key_revoked FAIL "revoked=false"
finalize_results
