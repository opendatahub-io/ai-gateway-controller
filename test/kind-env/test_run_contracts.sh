#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
source "$ROOT/test/kind-env/lib/image_refs.sh"
source "$ROOT/test/kind-env/lib/maas_api_image_identity.sh"
source "$ROOT/test/kind-env/lib/registry_image_identity.sh"

published_extproc_ref='quay.io/opendatahub/odh-praxis-extproc@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'
published_extproc_runtime='{"status":{"id":"sha256:config","repoDigests":["quay.io/opendatahub/odh-praxis-extproc@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"]}}'
image_reference_is_immutable_digest "$published_extproc_ref" || {
  echo "published ExtProc digest reference was rejected" >&2
  exit 1
}
if image_reference_is_immutable_digest 'quay.io/opendatahub/odh-praxis-extproc:odh-stable'; then
  echo "mutable ExtProc image tag was accepted as immutable" >&2
  exit 1
fi
extproc_image_mode_is_valid published "$published_extproc_ref" 24f2d003ed303a950b6bd8ab042f0d62a5ab4278 linux/amd64 "" || {
  echo "valid published ExtProc qualification inputs were rejected" >&2
  exit 1
}
for invalid_extproc_mode in \
  "published quay.io/opendatahub/odh-praxis-extproc:odh-stable 24f2d003ed303a950b6bd8ab042f0d62a5ab4278 linux/amd64" \
  "published $published_extproc_ref short linux/amd64" \
  "published $published_extproc_ref 24f2d003ed303a950b6bd8ab042f0d62a5ab4278 linux/ppc64le" \
  "published $published_extproc_ref 24f2d003ed303a950b6bd8ab042f0d62a5ab4278 linux/amd64 /local/praxis-ai" \
  "unknown $published_extproc_ref 24f2d003ed303a950b6bd8ab042f0d62a5ab4278 linux/amd64"; do
  read -r mode image source_sha platform ai_repo <<<"$invalid_extproc_mode"
  if extproc_image_mode_is_valid "$mode" "$image" "$source_sha" "$platform" "$ai_repo"; then
    echo "invalid published ExtProc qualification inputs were accepted: $invalid_extproc_mode" >&2
    exit 1
  fi
done
extproc_image_mode_is_valid source praxis-extproc:local "" linux/amd64 "" || {
  echo "default source-built ExtProc mode was rejected" >&2
  exit 1
}
containerd_image_has_exact_digest_reference "$published_extproc_runtime" "$published_extproc_ref" || {
  echo "containerd image identity rejected the exact requested ExtProc digest" >&2
  exit 1
}
if containerd_image_has_exact_digest_reference "$published_extproc_runtime" 'quay.io/opendatahub/odh-praxis-extproc@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff'; then
  echo "containerd image identity accepted a different ExtProc digest" >&2
  exit 1
fi
containerd_pod_image_id_matches_runtime "$published_extproc_runtime" 'containerd://sha256:config' || {
  echo "pod config image ID was not correlated with the inspected runtime image" >&2
  exit 1
}
containerd_pod_image_id_matches_runtime "$published_extproc_runtime" 'docker-pullable://quay.io/opendatahub/odh-praxis-extproc@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' || {
  echo "pod digest image ID was not correlated with the inspected runtime image" >&2
  exit 1
}
if containerd_pod_image_id_matches_runtime "$published_extproc_runtime" 'docker-pullable://quay.io/opendatahub/odh-praxis-extproc@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff'; then
  echo "pod image ID matched a digest absent from the inspected runtime image" >&2
  exit 1
fi

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
maas_image_digest='127.0.0.1:32768/maas-api@sha256:index'
maas_node_repo_digests='127.0.0.1:32768/maas-api@sha256:platform,127.0.0.1:32768/maas-api@sha256:index'
maas_pods='{"items":[{"metadata":{"name":"old","deletionTimestamp":null},"spec":{"containers":[{"name":"maas-api","image":"maas-api:old"}]},"status":{"phase":"Running","containerStatuses":[{"name":"maas-api","ready":true,"image":"maas-api:old","imageID":"127.0.0.1:32768/maas-api@sha256:old"}]}},{"metadata":{"name":"terminating-new","deletionTimestamp":"2026-10-19T00:00:00Z"},"spec":{"containers":[{"name":"maas-api","image":"maas-api:run"}]},"status":{"phase":"Running","containerStatuses":[{"name":"maas-api","ready":true,"image":"maas-api:alias","imageID":"127.0.0.1:32768/maas-api@sha256:run"}]}}]}'
if maas_api_pods_have_exact_ready_image "$maas_pods" "maas-api:run" "$maas_image_digest" "$maas_node_repo_digests"; then
  echo "MaaS API runtime check accepted only stale or terminating pods" >&2
  exit 1
fi
maas_pods=$(jq -c --arg digest "$maas_image_digest" '.items += [{"metadata":{"name":"current","deletionTimestamp":null},"spec":{"containers":[{"name":"maas-api","image":"maas-api:run"}]},"status":{"phase":"Running","containerStatuses":[{"name":"maas-api","ready":true,"image":"maas-api:alias","imageID":$digest}]}}]' <<<"$maas_pods")
maas_api_pods_have_exact_ready_image "$maas_pods" "maas-api:run" "$maas_image_digest" "$maas_node_repo_digests" || {
  echo "MaaS API runtime check rejected a requested run tag and node-verified digest alias" >&2
  exit 1
}
maas_pods=$(jq -c '.items[2].status.containerStatuses[0].imageID = "127.0.0.1:32768/maas-api@sha256:platform"' <<<"$maas_pods")
maas_api_pods_have_exact_ready_image "$maas_pods" "maas-api:run" "$maas_image_digest" "$maas_node_repo_digests" || {
  echo "MaaS API runtime check rejected a platform-manifest alias from the verified node image" >&2
  exit 1
}
if maas_api_pods_have_exact_ready_image "$(jq -c '.items[2].status.containerStatuses[0].imageID = "127.0.0.1:32768/maas-api@sha256:wrong"' <<<"$maas_pods")" "maas-api:run" "$maas_image_digest" "$maas_node_repo_digests"; then
  echo "MaaS API runtime check accepted a pod with a different runtime digest" >&2
  exit 1
fi
if maas_api_pods_have_exact_ready_image "$(jq -c '.items[2].spec.containers[0].image = "maas-api:old"' <<<"$maas_pods")" "maas-api:run" "$maas_image_digest" "$maas_node_repo_digests"; then
  echo "MaaS API runtime check accepted a pod that did not request the run tag" >&2
  exit 1
fi
if maas_api_pods_have_exact_ready_image "$maas_pods" "maas-api:run" "$maas_image_digest" "127.0.0.1:32768/maas-api@sha256:platform"; then
  echo "MaaS API runtime check accepted node digests without the expected pushed digest" >&2
  exit 1
fi

run_source=$(<"$ROOT/test/kind-env/run.sh")
published_extproc_gate=$(sed -n '/if \[\[ "$label" == extproc && "$EXTPROC_IMAGE_MODE" == published \]\]/,/^  fi/p' \
  "$ROOT/test/kind-env/run.sh")
rg -q 'git -C "\$source" status --porcelain' <<<"$published_extproc_gate" || {
  echo "published ExtProc source cleanliness check does not inspect git status" >&2
  exit 1
}
if rg -q '\[\[ -z "\$source_diff" \]\]' <<<"$published_extproc_gate"; then
  echo "published ExtProc source cleanliness check confuses the content fingerprint with dirty status" >&2
  exit 1
fi
published_extproc_load=$(sed -n '/if \[\[ "\$EXTPROC_IMAGE_MODE" == published \]\]/,/^  fi/p' \
  "$ROOT/test/kind-env/run.sh" | sed -n '/crictl pull "\$EXTPROC_IMAGE"/,$p')
rg -q 'crictl pull "\$EXTPROC_IMAGE"' <<<"$published_extproc_load" || {
  echo "published ExtProc qualification does not pull the digest directly into the Kind node" >&2
  exit 1
}
rg -q 'kind load docker-image "\$EXTPROC_IMAGE" --name "\$CLUSTER"' "$ROOT/test/kind-env/run.sh" || {
  echo "source-built ExtProc qualification no longer uses Kind image loading" >&2
  exit 1
}
authorino_gate=$(sed -n '/authorino_deployment_ready=false/,/set env deployment\/authorino/p' "$ROOT/test/kind-env/run.sh")
rg -q 'authorino_deployment_deadline=\$\(\(SECONDS \+ 180\)\)' <<<"$authorino_gate" || {
  echo "Authorino Deployment readiness wait is not bounded to 180 seconds" >&2
  exit 1
}
rg -q 'maas-api-serving-ca.*authorino-maas-api-ca' <<<"$authorino_gate" || {
  echo "Authorino readiness does not require the run-owned MaaS CA projection" >&2
  exit 1
}
rg -q 'rollout status deployment/authorino --timeout=8s' <<<"$authorino_gate" || {
  echo "Authorino readiness does not wait for its projected-CA Deployment rollout" >&2
  exit 1
}
rg -q 'authorino-readiness-timeout-events.txt' <<<"$authorino_gate" || {
  echo "Authorino readiness timeout does not preserve workload/event diagnostics" >&2
  exit 1
}
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
(
  eval "$(sed -n '/^cleanup_owned_translation_diagnostic_authpolicies() {$/,/^}$/p' "$ROOT/test/kind-env/run.sh")"
  EVIDENCE=$(mktemp -d)
  trap 'rm -rf "$EVIDENCE"' EXIT
  KCTL=(fake_kubectl)
  fake_kubectl() {
    case "$*" in
      *'get authpolicy api-translation-messages-auth'*)
        printf '%s\n' '{"metadata":{"name":"api-translation-messages-auth","namespace":"models-as-a-service","labels":{"app.kubernetes.io/managed-by":"api-translation-kind-e2e-diagnostic"}},"spec":{"targetRef":{"group":"gateway.networking.k8s.io","kind":"HTTPRoute","name":"api-translation-messages"}}}' ;;
      *'get authpolicy api-translation-openai-auth'*)
        printf '%s\n' '{"metadata":{"name":"api-translation-openai-auth","namespace":"models-as-a-service","labels":{"app.kubernetes.io/managed-by":"another-owner"}},"spec":{"targetRef":{"group":"gateway.networking.k8s.io","kind":"HTTPRoute","name":"api-translation-openai"}}}' ;;
      *'delete authpolicy api-translation-messages-auth'*) printf '%s\n' "$*" >>"$EVIDENCE/deletions.txt" ;;
      *'delete authpolicy api-translation-openai-auth'*) echo 'non-owned policy was deleted' >&2; return 1 ;;
      *) echo "unexpected diagnostic policy operation: $*" >&2; return 1 ;;
    esac
  }
  cleanup_owned_translation_diagnostic_authpolicies
  [[ "$(wc -l <"$EVIDENCE/deletions.txt")" -eq 1 ]] || {
    echo 'diagnostic policy cleanup did not delete exactly one owned policy' >&2; exit 1;
  }
  rg -q 'preserved non-owned diagnostic AuthPolicy name api-translation-openai-auth' \
    "$EVIDENCE/diagnostic-authpolicy-cleanup.txt" || {
      echo 'diagnostic policy cleanup did not preserve the non-owned policy' >&2; exit 1;
    }
)
if rg -q 'patch deployment maas-api|patch deployment "\$api_deployment"' <<<"$run_source"; then
  echo "the harness overrides MaaS API imagePullPolicy instead of satisfying its Always pull" >&2
  exit 1
fi

route_policy_manifest=$(<"$ROOT/test/kind-env/manifests/50-route-policies.yaml")
if rg -q '^kind: AuthPolicy$' <<<"$route_policy_manifest"; then
  echo "ExternalModel route fixture must inherit MaaS Gateway AuthPolicy instead of adding a conflicting AuthPolicy" >&2
  exit 1
fi
rg -q '^kind: TokenRateLimitPolicy$' <<<"$route_policy_manifest" || {
  echo "ExternalModel route fixture lost its token-rate-limit policy" >&2
  exit 1
}
translation_source=$(<"$ROOT/test/kind-env/e2e-api-translation.py")
rg -q 'remove_stale_externalmodel_auth_fixture' <<<"$translation_source" || {
  echo "translation E2E does not safely remove the exact known legacy AuthPolicy fixture" >&2
  exit 1
}
rg -q 'metadata.get\("uid"\) == DIAGNOSTIC_BYPASS_AUTH_UID' <<<"$translation_source" || {
  echo "diagnostic AuthPolicy cleanup does not verify its recorded UID" >&2
  exit 1
}
rg -q 'ConflictingAuthPolicy' <<<"$translation_source" || {
  echo "MaaS AuthPolicy readiness does not reject conflicting route policies" >&2
  exit 1
}
rg -q 'praxis_extproc_body_size_rejections_total' <<<"$translation_source" || {
  echo "request-size qualification does not attribute ExtProc's selected-upstream limit" >&2
  exit 1
}
rg -q 'selected-upstream input guard before body translation' <<<"$translation_source" || {
  echo "request-size evidence does not identify the ExtProc guard accurately" >&2
  exit 1
}
if rg -q 'rejected_by_translation_filter|Praxis AI OpenAI-to-Anthropic request-size check' <<<"$translation_source"; then
  echo "request-limit assertion still attributes ExtProc rejection to the AI translator" >&2
  exit 1
fi
rg -q 'translator_max_request_body_bytes' "$ROOT/test/kind-env/api_translation_contract.py" || {
  echo "mounted configuration parser does not distinguish request and response limits" >&2
  exit 1
}

if rg -q '\[\[ "\$extproc_branch" == main' <<<"$run_source"; then
  echo "ExtProc provenance gate incorrectly requires a branch name instead of matching source revision" >&2
  exit 1
fi
rg -q 'extproc_head.*EXPECTED_EXTPROC_SOURCE_SHA' <<<"$run_source" || {
  echo "ExtProc provenance gate does not require the configured exact source SHA" >&2
  exit 1
}

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

provider_manifest="$ROOT/test/kind-env/manifests/provider-selection-proof.yaml"
provider_source="$ROOT/test/kind-env/provider-recorder/main.go"
for suffix in a b c; do
  deployment="provider-selection-proof-${suffix}"
  yq -e "select(.kind == \"Deployment\" and .metadata.name == \"${deployment}\") | .spec.template.spec.securityContext.fsGroup == 65532" "$provider_manifest" >/dev/null || {
    echo "${deployment} recorder receipt volume lacks its unprivileged writable fsGroup" >&2
    exit 1
  }
  yq -e "select(.kind == \"Deployment\" and .metadata.name == \"${deployment}\") | .spec.template.spec.volumes[] | select(.name == \"observations\") | has(\"emptyDir\")" "$provider_manifest" >/dev/null || {
    echo "${deployment} recorder receipt storage is not ephemeral" >&2
    exit 1
  }
  yq -e "select(.kind == \"Deployment\" and .metadata.name == \"${deployment}\") | .spec.template.spec.containers[] | select(.name == \"recorder\") | .env[] | select(.name == \"ENABLE_TEST_OBSERVATIONS\") | .value == \"true\"" "$provider_manifest" >/dev/null || {
    echo "${deployment} recorder does not enable its test-only receipt endpoint" >&2
    exit 1
  }
done
rg -q '"proof-c":        "proof-c"' "$provider_source" || {
  echo "provider recorder does not recognize the third static-weight fixture" >&2
  exit 1
}
rg -q -- '--known-cluster=provider-provider-proof-c' <<<"$run_source" || {
  echo "Kind controller allowlist omits provider C" >&2
  exit 1
}
rg -q 'make_katan_tls provider-proof-c-tls provider-proof-c-ext' <<<"$run_source" || {
  echo "Kind provisioner does not issue the verified TLS certificate for provider C" >&2
  exit 1
}


(
  handoff_poll=$(sed -n '/^    handoff_ready=false$/,/^    if \[\[ "$handoff_ready" != true \]\]; then$/p' \
    "$ROOT/test/kind-env/run.sh" | sed '$d')
  [[ -n "$handoff_poll" ]] || { echo 'could not extract the bootstrap Deployment handoff poll' >&2; exit 1; }
  KCTL=(mock_kubectl)
  deployment_name=payload-processing
  seq() { printf '%s\n' 1 2 3; }
  sleep() { :; }
  mock_kubectl() {
    [[ "$*" == '-n maas-system get deployment payload-processing --ignore-not-found -o json' ]] || return 1
    case "$scenario" in
      absent) return 0 ;;
      owned) printf '%s\n' '{"metadata":{"labels":{"app.kubernetes.io/managed-by":"ai-gateway-controller"}}}' ;;
      unowned) printf '%s\n' '{"metadata":{"labels":{}}}' ;;
      denied) return 1 ;;
      timeout) return 124 ;;
      partial) printf '%s\n' '{"metadata":{"labels":{"app.kubernetes.io/managed-by":"ai-gateway-controller"}}}'; return 1 ;;
      recovering)
        if [[ ! -e "$handoff_tmp/retried" ]]; then
          touch "$handoff_tmp/retried"
          return 124
        fi
        printf '%s\n' '{"metadata":{"labels":{"app.kubernetes.io/managed-by":"ai-gateway-controller"}}}'
        ;;
      *) return 1 ;;
    esac
  }
  handoff_tmp=$(mktemp -d)
  trap 'rm -rf "$handoff_tmp"' EXIT
  for scenario in denied timeout partial unowned; do
    eval "$handoff_poll"
    [[ "$handoff_ready" == false ]] || { echo "handoff must not succeed for $scenario Deployment reads" >&2; exit 1; }
  done
  for scenario in absent owned recovering; do
    eval "$handoff_poll"
    [[ "$handoff_ready" == true ]] || { echo "handoff must accept a confirmed $scenario Deployment" >&2; exit 1; }
  done
)


# Check the actual Docker RepoDigests expression against multiple entries.
rg -q 'index\(\$d\) != null' "$ROOT/test/kind-env/run.sh"
for digests in '["other@sha256:abc","expected@sha256:def"]' '["expected@sha256:def","other@sha256:abc"]'; do
  [[ "$(jq -r --arg d 'expected@sha256:def' 'index($d) != null' <<<"$digests")" == true ]]
done

if rg -q '^kind: AuthPolicy$' "$ROOT/test/kind-env/manifests/51-route-policies-tenant-b.yaml"; then
  echo "tenant B must inherit the MaaS Gateway policy, as tenant A does" >&2
  exit 1
fi

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

(
  eval "$(sed -n '/^revoke_key() {$/,/^}$/p' "$ROOT/test/kind-env/e2e-extproc.sh")"
  revocation_tmp=$(mktemp -d)
  trap 'rm -rf "$revocation_tmp"' EXIT
  EVIDENCE=$revocation_tmp
  KEY_ID=contract-test-key
  APF=contract-test-port-forward
  KEY_REVOKED=false
  CA_CERT=unused API_PORT=unused ADMIN_CFG=unused
  timeout() {
    printf '%s\n' called >>"$revocation_tmp/calls"
    printf '%s' "$mock_status"
  }
  mock_status=503
  if revoke_key; then
    echo 'failed revocation must remain retryable' >&2
    exit 1
  fi
  [[ "$KEY_REVOKED" == false ]] || { echo 'failed revocation marked the key revoked' >&2; exit 1; }
  mock_status=200
  revoke_key
  mock_status=000
  revoke_key
  [[ "$KEY_REVOKED" == true && $(wc -l <"$revocation_tmp/calls") -eq 2 ]] || {
    echo 'successful revocation must not be retried after a port-forward stops' >&2
    exit 1
  }
  rg -q 'status=200$' "$EVIDENCE/key-revocation.txt" || {
    echo 'repeat cleanup overwrote the successful revocation receipt' >&2
    exit 1
  }
)

finish_source=$(sed -n '/^finish() {$/,/^}$/p' "$ROOT/test/kind-env/e2e-extproc.sh")
revoke_line=$(rg -n '^  revoke_key$' <<<"$finish_source" | cut -d: -f1)
api_stop_line=$(rg -n 'kill "\$APF"' <<<"$finish_source" | cut -d: -f1)
[[ -n "$revoke_line" && -n "$api_stop_line" && "$revoke_line" -lt "$api_stop_line" ]] || {
  echo 'cleanup must revoke the key before stopping its API port-forward' >&2
  exit 1
}

echo "Kind run harness contracts passed (digest/tag handling, terminating pod exclusion, exact MaaS runtime image identity, opt-in diagnostic auth, provider-log failure handling, MaaS callback-log failure handling, and AuthPolicy listing failures)."
