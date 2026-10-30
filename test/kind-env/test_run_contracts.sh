#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
source "$ROOT/test/kind-env/lib/image_refs.sh"
source "$ROOT/test/kind-env/lib/maas_api_image_identity.sh"
source "$ROOT/test/kind-env/lib/registry_image_identity.sh"

run_image_resolution() {
  env -i PATH="$PATH" "$@" bash -c '
    set -e
    source "$1"
    resolve_katan_image_refs || exit $?
    printf "%s\t%s\t%s\n" "$KATAN_EFFECTIVE_IMAGE" "$KATAN_PULL_IMAGE" "$KATAN_KIND_IMAGE"
  ' bash "$ROOT/test/kind-env/lib/image_refs.sh"
}

digest=ghcr.io/example/llm-katan@sha256:0123456789abcdef
resolved=$(run_image_resolution BUILD_KATAN=false KATAN_IMAGE="$digest")
IFS=$'\t' read -r effective pull kind <<<"$resolved"
[[ "$pull" == "$digest" ]] || { echo "digest pull reference changed: $pull" >&2; exit 1; }
[[ "$kind" == "llm-katan:external-model-two-plane" && "$kind" != *@* ]] || {
  echo "digest path produced invalid Kind tag: $kind" >&2
  exit 1
}

tag=registry.example/llm-katan:qualification
resolved=$(run_image_resolution BUILD_KATAN=false KATAN_IMAGE="$tag")
IFS=$'\t' read -r effective pull kind <<<"$resolved"
[[ "$pull" == "$tag" ]] || { echo "ordinary tag pull reference changed: $pull" >&2; exit 1; }
[[ "$kind" == "llm-katan:external-model-two-plane" && "$kind" != *@* ]] || {
  echo "ordinary-tag path produced invalid Kind tag: $kind" >&2
  exit 1
}

if run_image_resolution BUILD_KATAN=true KATAN_IMAGE="$digest" >/dev/null 2>&1; then
  echo "digest was accepted as a Docker build target" >&2
  exit 1
fi
if run_image_resolution BUILD_KATAN=false KATAN_IMAGE="$tag" KATAN_KIND_IMAGE="$digest" >/dev/null 2>&1; then
  echo "digest was accepted as an explicit Kind transport tag" >&2
  exit 1
fi

runtime_image_info='{"status":{"id":"sha256:platform-config","repoDigests":["127.0.0.1:32768/maas-api@sha256:platform","127.0.0.1:32768/maas-api@sha256:index"],"repoTags":["127.0.0.1:32768/maas-api:run"]}}'
containerd_image_matches_pushed_ref "$runtime_image_info" \
  "127.0.0.1:32768/maas-api@sha256:index" "127.0.0.1:32768/maas-api:run" || {
  echo "containerd did not match the pushed digest when it followed the platform digest" >&2
  exit 1
}
if containerd_image_matches_pushed_ref "$runtime_image_info" \
  "127.0.0.1:32768/maas-api@sha256:missing" "127.0.0.1:32768/maas-api:run"; then
  echo "containerd accepted an image with a different pushed digest" >&2
  exit 1
fi
if containerd_image_matches_pushed_ref "$runtime_image_info" \
  "127.0.0.1:32768/maas-api@sha256:index" "127.0.0.1:32768/maas-api:missing"; then
  echo "containerd accepted an image with a different tag" >&2
  exit 1
fi

maas_deployment='{"spec":{"selector":{"matchLabels":{"app.kubernetes.io/name":"maas-api"}},"template":{"metadata":{"labels":{"app.kubernetes.io/name":"maas-api","maas.opendatahub.io/tenant-instance":"maas-api-tenant-b"}}}}}'
maas_selector=$(maas_api_deployment_pod_selector "$maas_deployment")
[[ "$maas_selector" == *'maas.opendatahub.io/tenant-instance=maas-api-tenant-b'* ]] || {
  echo "MaaS API pod selector omitted the unique tenant-instance label: $maas_selector" >&2
  exit 1
}
maas_image_digest='127.0.0.1:32768/maas-api@sha256:run'
maas_pods='{"items":[{"metadata":{"name":"old","deletionTimestamp":null},"spec":{"containers":[{"name":"maas-api","image":"maas-api:old"}]},"status":{"phase":"Running","containerStatuses":[{"name":"maas-api","ready":true,"image":"maas-api:old","imageID":"127.0.0.1:32768/maas-api@sha256:old"}]}},{"metadata":{"name":"terminating-new","deletionTimestamp":"2026-10-19T00:00:00Z"},"spec":{"containers":[{"name":"maas-api","image":"maas-api:run"}]},"status":{"phase":"Running","containerStatuses":[{"name":"maas-api","ready":true,"image":"maas-api:alias","imageID":"127.0.0.1:32768/maas-api@sha256:run"}]}}]}'
if maas_api_pods_have_exact_ready_image "$maas_pods" "maas-api:run" "$maas_image_digest"; then
  echo "MaaS API runtime check accepted only stale or terminating pods" >&2
  exit 1
fi
maas_pods=$(jq -c --arg digest "$maas_image_digest" '.items += [{"metadata":{"name":"current","deletionTimestamp":null},"spec":{"containers":[{"name":"maas-api","image":"maas-api:run"}]},"status":{"phase":"Running","containerStatuses":[{"name":"maas-api","ready":true,"image":"maas-api:alias","imageID":$digest}]}}]' <<<"$maas_pods")
maas_api_pods_have_exact_ready_image "$maas_pods" "maas-api:run" "$maas_image_digest" || {
  echo "MaaS API runtime check rejected the requested run tag and exact digest when status.image reported an older alias" >&2
  exit 1
}
if maas_api_pods_have_exact_ready_image "$(jq -c '.items[2].status.containerStatuses[0].imageID = "127.0.0.1:32768/maas-api@sha256:wrong"' <<<"$maas_pods")" "maas-api:run" "$maas_image_digest"; then
  echo "MaaS API runtime check accepted a pod with a different runtime digest" >&2
  exit 1
fi
if maas_api_pods_have_exact_ready_image "$(jq -c '.items[2].spec.containers[0].image = "maas-api:old"' <<<"$maas_pods")" "maas-api:run" "$maas_image_digest"; then
  echo "MaaS API runtime check accepted a pod that did not request the run tag" >&2
  exit 1
fi

run_source=$(<"$ROOT/test/kind-env/run.sh")
rg -q 'KIND_EXPERIMENTAL_DOCKER_NETWORK="\$REGISTRY_NETWORK" kind create cluster' <<<"$run_source" || {
  echo "Kind cluster creation does not use the run-owned registry network" >&2
  exit 1
}
rg -q 'crictl pull "\$MAAS_API_RUNTIME_IMAGE"' <<<"$run_source" || {
  echo "the Kind node does not verify access to the source-built MaaS API registry image" >&2
  exit 1
}
rg -q 'docker network connect --gw-priority=-1 "\$REGISTRY_NETWORK" "\$node"' <<<"$run_source" || {
  echo "the run-owned registry network may replace the Kind node's existing default route" >&2
  exit 1
}
rg -q 'RELATED_IMAGE_ODH_MAAS_API_IMAGE=\$\{MAAS_API_RUNTIME_IMAGE\}' <<<"$run_source" || {
  echo "the MaaS controller is not configured with the run-owned API registry image" >&2
  exit 1
}
rg -q 'apply_maas_api_translation_schema' <<<"$run_source" || {
  echo "the run harness does not expose the MaaS apiTranslation schema fixture overlay" >&2
  exit 1
}
rg -q 'MAAS_API_TRANSLATION_SCHEMA_REV' <<<"$run_source" || {
  echo "the MaaS apiTranslation schema fixture is not revision-pinned" >&2
  exit 1
}
rg -q 'set image "deployment/\$api_deployment" "maas-api=\$\{MAAS_API_RUNTIME_IMAGE\}"' <<<"$run_source" || {
  echo "tenant API Deployments do not select the run-owned source image" >&2
  exit 1
}
rg -q 'shared-translation-auth-diagnostic\.yaml.*continue' <<<"$run_source" || {
  echo "default manifest provisioning does not exclude the opt-in translation diagnostic AuthPolicies" >&2
  exit 1
}
if ! rg -q 'if args\.diagnostic_route_auth:' "$ROOT/test/kind-env/e2e-api-translation.py" ||
  ! rg -q 'shared-translation-auth-diagnostic\.yaml' "$ROOT/test/kind-env/e2e-api-translation.py"; then
    echo "translation diagnostic AuthPolicies are not applied only through their explicit opt-in" >&2
    exit 1
fi
if rg -q 'patch deployment maas-api|patch deployment "\$api_deployment"' <<<"$run_source"; then
  echo "the harness overrides MaaS API imagePullPolicy instead of satisfying its Always pull" >&2
  exit 1
fi

for policy_fixture in \
  "$ROOT/test/kind-env/manifests/50-route-policies.yaml" \
  "$ROOT/test/kind-env/manifests/51-route-policies-tenant-b.yaml"; do
  if rg -n '^[[:space:]]*kind:[[:space:]]*AuthPolicy[[:space:]]*$' "$policy_fixture" >/dev/null; then
    echo "ExternalModel fixture $policy_fixture must not add a route AuthPolicy over the MaaSAuthPolicy" >&2
    exit 1
  fi
done
if ! rg -q 'wait_normal_auth_policy' "$ROOT/test/kind-env/e2e-extproc.sh" ||
  ! rg -q 'ConflictingAuthPolicy' "$ROOT/test/kind-env/e2e-extproc.sh" ||
  ! rg -q 'invalid_key_no_provider_contact' "$ROOT/test/kind-env/e2e-extproc.sh" ||
  ! rg -q 'timeout 5s kubectl --context "kind-\$CLUSTER" -n "\$API_NS" logs deploy/maas-api' "$ROOT/test/kind-env/e2e-extproc.sh" ||
  ! rg -q 'maas_validation_and_subscription_callbacks' "$ROOT/test/kind-env/e2e-extproc.sh"; then
  echo "base E2E does not gate on MaaS policy ownership and credential denial before provider contact" >&2
  exit 1
fi

ready_pods=$(jq -r -f "$ROOT/test/kind-env/controller-ready-manager-pods.jq" <<'JSON'
{"items":[
  {"metadata":{"name":"current","deletionTimestamp":null},"spec":{"nodeName":"kind-node"},"status":{"phase":"Running","containerStatuses":[{"name":"manager","ready":true,"imageID":"containerd://sha256:current","image":"controller:tag"}]}},
  {"metadata":{"name":"terminating","deletionTimestamp":"2026-10-18T00:00:00Z"},"spec":{"nodeName":"kind-node"},"status":{"phase":"Running","containerStatuses":[{"name":"manager","ready":true,"imageID":"containerd://sha256:old","image":"controller:old"}]}}
]}
JSON
)
[[ "$(wc -l <<<"$ready_pods")" == 1 && "$ready_pods" == current$'\t'kind-node$'\t'containerd://sha256:current$'\t'controller:tag ]] || {
  echo "controller pod filter did not exclude the terminating pod: $ready_pods" >&2
  exit 1
}

(
  eval "$(sed -n '/^provider_request_count() {$/,/^}$/p' "$ROOT/test/kind-env/e2e-extproc.sh")"
  CLUSTER=contract-test
  API_NS=maas-system
  timeout() { printf '%s\n' 'GET /health' 'POST /v1/chat/completions'; }
  [[ "$(provider_request_count katan-a)" == 1 ]] || { echo 'provider request count must reflect successful log reads' >&2; exit 1; }
  timeout() { printf '%s\n' 'GET /health'; }
  [[ "$(provider_request_count katan-a)" == 0 ]] || { echo 'successful logs with no requests must count as zero' >&2; exit 1; }
  timeout() { return 124; }
  if provider_request_count katan-a; then
    echo 'failed provider log reads must not be treated as zero provider contact' >&2
    exit 1
  fi
)

(
  eval "$(sed -n '/^maas_callback_counts() {$/,/^}$/p' "$ROOT/test/kind-env/e2e-extproc.sh")"
  eval "$(sed -n '/^record_maas_callback_assertion() {$/,/^}$/p' "$ROOT/test/kind-env/e2e-extproc.sh")"
  CLUSTER=contract-test
  API_NS=maas-system
  timeout() { printf '%s\n' '/internal/v1/api-keys/validate' '/internal/v1/subscriptions/select'; }
  [[ "$(maas_callback_counts)" == '1 1' ]] || { echo 'MaaS callbacks must reflect successful log reads' >&2; exit 1; }
  timeout() { printf '%s\n' 'GET /health'; }
  [[ "$(maas_callback_counts)" == '0 0' ]] || { echo 'successful logs without callbacks must count as zero' >&2; exit 1; }
  timeout() { printf '%s\n' '/internal/v1/api-keys/validate'; return 124; }
  if maas_callback_counts; then
    echo 'failed or partial MaaS log reads must not produce callback counts' >&2
    exit 1
  fi
  record() { [[ "$1" == 44 && "$2" == maas_validation_and_subscription_callbacks && "$3" == "$expected" ]]; }
  expected=FAIL
  record_maas_callback_assertion unavailable '10 10' request-test
  record_maas_callback_assertion '10 10' unavailable request-test
  record_maas_callback_assertion unavailable unavailable request-test
  record_maas_callback_assertion '10 10' '10 11' request-test
  expected=PASS
  record_maas_callback_assertion '10 10' '11 11' request-test
)

(
  eval "$(sed -n '/^wait_normal_auth_policy() {$/,/^}$/p' "$ROOT/test/kind-env/e2e-extproc.sh")"
  readiness_tmp=$(mktemp -d)
  trap 'rm -rf "$readiness_tmp"' EXIT
  NS=models-as-a-service
  API_NS=maas-system
  EVIDENCE=$readiness_tmp
  seq() { printf '%s\n' 1 2 3; }
  sleep() { :; }
  kctl() {
    case "$*" in
      *'get maasauthpolicy '*)
        printf '%s\n' '{"metadata":{"generation":1},"status":{"conditions":[{"type":"Ready","status":"True","observedGeneration":1},{"type":"ConflictingAuthPolicy","status":"False","observedGeneration":1}]}}'
        ;;
      *'get authpolicy '*)
        printf '%s\n' '{"status":{"conditions":[{"type":"Accepted","status":"True"},{"type":"Enforced","status":"True"}]}}'
        ;;
      'get authpolicies.kuadrant.io -A -o json')
        local count
        count=$(<"$readiness_tmp/list-count")
        count=$((count+1))
        printf '%s' "$count" >"$readiness_tmp/list-count"
        case "$scenario" in
          failed) return 1 ;;
          timeout) return 124 ;;
          interrupted) if [[ "$count" == 2 ]]; then return 1; fi ;;
          recovered) if [[ "$count" == 1 ]]; then return 1; fi ;;
          conflict)
            printf '%s\n' '{"items":[{"metadata":{"namespace":"models-as-a-service","name":"conflicting"},"spec":{"targetRef":{"kind":"HTTPRoute","name":"external-model-demo-model"}}}]}'
            return 0
            ;;
        esac
        printf '%s\n' '{"items":[]}'
        ;;
      *) return 1 ;;
    esac
  }
  for scenario in failed timeout conflict interrupted; do
    printf '0' >"$readiness_tmp/list-count"
    if wait_normal_auth_policy; then
      echo "AuthPolicy readiness must fail for $scenario listing" >&2
      exit 1
    fi
    if [[ "$scenario" == failed || "$scenario" == timeout ]]; then
      jq -e '.auth_policy_listing_succeeded == false and .route_targeted_auth_policies == null' \
        "$EVIDENCE/normal-auth-policy-state.json" >/dev/null || {
        echo 'failed listings must be recorded as unavailable, not an empty policy list' >&2
        exit 1
      }
    fi
  done
  for scenario in empty recovered; do
    printf '0' >"$readiness_tmp/list-count"
    wait_normal_auth_policy || { echo "successful stable empty listings must become ready for $scenario" >&2; exit 1; }
    jq -e '.auth_policy_listing_succeeded == true and .route_targeted_auth_policies == []' \
      "$EVIDENCE/normal-auth-policy-state.json" >/dev/null || {
      echo 'successful empty listings must be recorded explicitly' >&2
      exit 1
    }
  done
)

echo "Kind run harness contracts passed (digest/tag handling, terminating pod exclusion, exact MaaS runtime image identity, opt-in diagnostic auth, provider-log failure handling, MaaS callback-log failure handling, and AuthPolicy listing failures)."
