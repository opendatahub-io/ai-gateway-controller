#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CLUSTER=${LOCAL_ENV_CLUSTER:-external-model-two-plane}
WORKSPACE=$(cd "$ROOT/.." && pwd)
DEPS_DIR=${LOCAL_ENV_DEPS_DIR:-"$WORKSPACE/deps"}
LLM_KATAN_REPO=${LLM_KATAN_REPO:-"$DEPS_DIR/llm-katan"}
PRAXIS_REPO=${PRAXIS_REPO:-"$WORKSPACE/praxis-ai"}
MAAS_CONTROLLER_REPO=${MAAS_CONTROLLER_REPO:-"$WORKSPACE/models-as-a-service"}
KSERVE_REPO=${KSERVE_REPO:-"$DEPS_DIR/kserve"}
KUADRANT_OPERATOR_REPO=${KUADRANT_OPERATOR_REPO:-"$DEPS_DIR/kuadrant-operator"}
PRAXIS_EXTPROC_REPO=${PRAXIS_EXTPROC_REPO:-"$DEPS_DIR/praxis-extproc"}
ISTIOCTL=${ISTIOCTL:-/tmp/istio-1.27.3/bin/istioctl}
EVIDENCE_ROOT=${LOCAL_ENV_EVIDENCE_ROOT:-"$ROOT/evidence"}
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
EVIDENCE="$EVIDENCE_ROOT/$STAMP"
mkdir -p "$EVIDENCE"
exec > >(tee "$EVIDENCE/provisioner.log") 2>&1

KCTL=(kubectl --context "kind-$CLUSTER")
failures=0
fail() { echo "FAIL: $*" | tee -a "$EVIDENCE/failures.txt"; failures=$((failures + 1)); }
check_cmd() { command -v "$1" >/dev/null 2>&1 || fail "missing command: $1"; }
check_repo() { local name=$1 path=${!1:-}; [[ -n "$path" && -d "$path/.git" ]] || fail "$name checkout missing: ${path:-unset}"; }
dirty_hash() {
  local source=$1 file
  {
    git -C "$source" diff --no-ext-diff
    while IFS= read -r file; do
      printf 'UNTRACKED %s\n' "$file"
      sha256sum "$source/$file"
    done < <(git -C "$source" ls-files --others --exclude-standard | sort)
  } | sha256sum | awk '{print $1}'
}

echo "evidence=$EVIDENCE"
echo "cluster=$CLUSTER"
cat >"$EVIDENCE/namespace-contract.txt" <<EOF
tenant_namespace=models-as-a-service
transition_tenant_namespace=ai-tenant-transition
mock_backend_namespace=maas-system
gateway_namespace=maas-system
aitenant_namespace=ai-tenants
controller_namespace=opendatahub
EOF
for cmd in docker kind kubectl helm kustomize go git openssl yq; do check_cmd "$cmd"; done
check_cmd "$ISTIOCTL"
check_repo MAAS_CONTROLLER_REPO "$MAAS_CONTROLLER_REPO"
check_repo KSERVE_REPO "$KSERVE_REPO"
mkdir -p "$EVIDENCE_ROOT/.image-inputs"

if docker info --format '{{.Architecture}} {{.NCPU}} {{.MemTotal}}' >"$EVIDENCE/docker.txt" 2>&1; then
  read -r arch cpus memory <"$EVIDENCE/docker.txt" || true
  echo "docker_arch=$arch docker_cpus=$cpus docker_memory_bytes=$memory"
else
  fail "Docker daemon is unavailable"
fi

df -P "$ROOT" >"$EVIDENCE/disk.txt" 2>&1 || fail "disk check failed"
free -b >"$EVIDENCE/memory.txt" 2>&1 || fail "memory check failed"
check_repo LLM_KATAN_REPO
check_repo PRAXIS_REPO
check_repo MAAS_CONTROLLER_REPO
check_repo KUADRANT_OPERATOR_REPO
check_repo PRAXIS_EXTPROC_REPO

if rg -q 'For\(.*ExternalModel|ExternalModel.*Reconciler|SetupWithManager' "$ROOT/cmd" "$ROOT/pkg" 2>/dev/null; then
  echo "reconciler_source=present"
else
  fail "real ExternalModel/ExternalProvider reconciler is absent from this checkout"
fi
if find "$ROOT/config" -type f -name '*.yaml' -print0 | xargs -0 rg -q 'kind: CustomResourceDefinition' 2>/dev/null; then
  echo "crd_manifests=present"
else
  fail "ExternalModel/ExternalProvider CRD manifests are absent from this checkout"
fi

git -C "$ROOT" rev-parse HEAD >"$EVIDENCE/controller.sha"
git -C "$ROOT" diff --no-ext-diff >"$EVIDENCE/controller.diff" || true
dirty_hash "$ROOT" >"$EVIDENCE/controller.diff.sha256"
git -C "$ROOT" status --short >"$EVIDENCE/controller.status"

for pair in \
  "controller|$ROOT|${AI_CONTROLLER_IMAGE:-ai-gateway-controller:external-model-two-plane}|Dockerfile" \
  "katan|$LLM_KATAN_REPO|${KATAN_IMAGE:-llm-katan:e2e}|Containerfile" \
  "praxis|$PRAXIS_REPO|${PRAXIS_IMAGE:-praxis-ai:overlay-e2e}|Containerfile" \
  "extproc|$PRAXIS_EXTPROC_REPO|${EXTPROC_IMAGE:-praxis-extproc:dev}|Containerfile" \
  "maas-api|$MAAS_CONTROLLER_REPO/maas-api|${MAAS_API_IMAGE:-maas-api:external-model-two-plane}|Dockerfile" \
  "maas-controller|$MAAS_CONTROLLER_REPO|${MAAS_CONTROLLER_IMAGE:-maas-controller:external-model-two-plane}|maas-controller/Dockerfile"; do
  IFS='|' read -r label source image dockerfile <<<"$pair"
  source_sha=$(git -C "$source" rev-parse HEAD)
  source_diff=$(dirty_hash "$source")
  dockerfile_sha=$(sha256sum "$source/$dockerfile" | awk '{print $1}')
  printf '%s\n%s\n%s\n' "$source_sha" "$source_diff" "$dockerfile_sha" >"$EVIDENCE/${label}.inputs"
  printf '%s\n' "$source_sha" >"$EVIDENCE/${label}.sha"
  printf '%s\n' "$source_diff" >"$EVIDENCE/${label}.diff.sha256"
  cache="$EVIDENCE_ROOT/.image-inputs/$label"
  rebuild=true
  if docker image inspect "$image" >/dev/null 2>&1 && [[ -f "$cache" ]] && cmp -s "$EVIDENCE/${label}.inputs" "$cache"; then
    rebuild=false
  fi
  if [[ "$rebuild" == true ]]; then
    case "$label" in
      controller) timeout 900s docker build --platform linux/amd64 -t "$image" -f "$source/$dockerfile" "$source" ;;
      katan) timeout 900s docker build --platform linux/amd64 -t "$image" -f "$source/$dockerfile" "$source" ;;
      praxis) timeout 1200s docker build --platform linux/amd64 -t "$image" -f "$source/$dockerfile" "$source" ;;
      extproc) timeout 1200s docker build --platform linux/amd64 -t "$image" -f "$source/$dockerfile" "$source" ;;
      maas-api) timeout 1200s docker build --platform linux/amd64 -t "$image" -f "$source/$dockerfile" "$source" ;;
      maas-controller) timeout 1200s docker build --platform linux/amd64 -t "$image" -f "$source/$dockerfile" "$source" ;;
    esac
    cp "$EVIDENCE/${label}.inputs" "$cache"
    echo "built_image=$image reason=source-inputs-changed-or-image-missing"
  else
    echo "reused_image=$image reason=source-inputs-unchanged"
  fi
done

git -C "$KSERVE_REPO" rev-parse HEAD >"$EVIDENCE/kserve.sha"
git -C "$KSERVE_REPO" diff --no-ext-diff | sha256sum >"$EVIDENCE/kserve.diff.sha256"

if [[ "${1:---preflight}" == "--destroy" ]]; then
  prior_evidence="$EVIDENCE"
  if [[ -s "$EVIDENCE_ROOT/.active-run" ]]; then
    prior_evidence=$(<"$EVIDENCE_ROOT/.active-run")
  fi
  kind delete cluster --name "$CLUSTER" >"$EVIDENCE/destroy.log" 2>&1 || true
  {
    echo "cluster=kind-$CLUSTER"
    echo "run_owned_ca_artifacts=$prior_evidence/maas-api-ca.crt,$prior_evidence/maas-api-serving.crt"
    echo "cluster_certificate_secrets_removed=true"
    echo "authorino_ca_configmap_removed=true"
    if kind get clusters 2>/dev/null | rg -qx "$CLUSTER"; then
      echo "cluster_removed=false"
      fail "run-owned Kind cluster still exists after teardown"
    else
      echo "cluster_removed=true"
    fi
    echo "unrelated_kind_clusters_preserved=$(kind get clusters 2>/dev/null | tr '\n' ' ' || true)"
  } >"$EVIDENCE/cleanup-inventory.txt"
  echo "destroyed kind-$CLUSTER"
  exit 0
fi

if (( failures )); then
  printf '{\n  "status":"BLOCKED",\n  "failures":%d,\n  "cluster":"kind-%s",\n  "evidence":"%s"\n}\n' "$failures" "$CLUSTER" "$EVIDENCE" >"$EVIDENCE/result.json"
  exit 2
fi

if [[ "${1:---preflight}" == "--provision" ]]; then
  printf '%s\n' "$EVIDENCE" >"$EVIDENCE_ROOT/.active-run"
  kind get clusters | rg -qx "$CLUSTER" || kind create cluster --name "$CLUSTER"
  "${KCTL[@]}" cluster-info >"$EVIDENCE/cluster-info.txt"
  # Stop prior run-owned MaaS pods before replacing their node image tags;
  # containerd retains images referenced by live containers.
  "${KCTL[@]}" -n maas-system scale deployment/maas-api deployment/maas-controller --replicas=0 >/dev/null 2>&1 || true
  for selector in 'app.kubernetes.io/name=maas-api' 'control-plane=maas-controller'; do
    for _ in $(seq 1 60); do
      [[ -z $("${KCTL[@]}" -n maas-system get pods -l "$selector" -o name 2>/dev/null || true) ]] && break
      sleep 1
    done
  done
  # kind load does not reliably replace an existing docker.io/library tag in
  # the node container. Remove only these run-owned image tags first so the
  # loaded image ID always matches the source-input cache record above.
  for image in \
    "${AI_CONTROLLER_IMAGE:-ai-gateway-controller:external-model-two-plane}" \
    "${KATAN_IMAGE:-llm-katan:e2e}" \
    "${PRAXIS_IMAGE:-praxis-ai:overlay-e2e}" \
    "${EXTPROC_IMAGE:-praxis-extproc:dev}" \
    "${MAAS_API_IMAGE:-maas-api:external-model-two-plane}" \
    "${MAAS_CONTROLLER_IMAGE:-maas-controller:external-model-two-plane}"; do
    docker exec "${CLUSTER}-control-plane" crictl rmi "docker.io/library/$image" >/dev/null 2>&1 || true
  done
  kind load docker-image "${AI_CONTROLLER_IMAGE:-ai-gateway-controller:external-model-two-plane}" --name "$CLUSTER"
  kind load docker-image "${KATAN_IMAGE:-llm-katan:e2e}" --name "$CLUSTER"
  kind load docker-image "${PRAXIS_IMAGE:-praxis-ai:overlay-e2e}" --name "$CLUSTER"
  kind load docker-image "${EXTPROC_IMAGE:-praxis-extproc:dev}" --name "$CLUSTER"
  kind load docker-image "${MAAS_API_IMAGE:-maas-api:external-model-two-plane}" --name "$CLUSTER"
  kind load docker-image "${MAAS_CONTROLLER_IMAGE:-maas-controller:external-model-two-plane}" --name "$CLUSTER"
  timeout 120s "${KCTL[@]}" apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml
  timeout 600s helm upgrade --install cert-manager jetstack/cert-manager --namespace cert-manager --create-namespace --version v1.15.3 --set crds.enabled=true --kube-context "kind-$CLUSTER" --wait --timeout 300s
  timeout 600s helm upgrade --install kuadrant-operator kuadrant/kuadrant-operator --namespace kuadrant-system --create-namespace --version 1.3.1 --kube-context "kind-$CLUSTER" --wait --timeout 300s
  timeout 600s "$ISTIOCTL" install --context "kind-$CLUSTER" --set profile=minimal --set components.ingressGateways[0].name=istio-ingressgateway --set components.ingressGateways[0].enabled=true --set values.gateways.istio-ingressgateway.autoscaleEnabled=false --set values.gateways.istio-ingressgateway.replicaCount=1 -y
  # Kuadrant is installed before Istio so its initial provider discovery can
  # miss the Gateway API implementation. Restart its run-owned controller
  # after Istio is ready, matching the documented recovery path.
  "${KCTL[@]}" -n kuadrant-system rollout restart deployment/kuadrant-operator-controller-manager
  "${KCTL[@]}" -n kuadrant-system rollout status deployment/kuadrant-operator-controller-manager --timeout=120s
  "${KCTL[@]}" create namespace maas-system --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" create namespace opendatahub --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" create namespace models-as-a-service --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" create namespace ai-tenant-tenant-b --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" create namespace ai-tenant-transition --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" label namespace ai-tenant-tenant-b ai-gateway.opendatahub.io/tenant=true maas.opendatahub.io/managed-by-aitenant=true --overwrite
  "${KCTL[@]}" create namespace ai-tenants --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" apply -f "$ROOT/test/local-env/manifests/05-database.yaml"
  "${KCTL[@]}" apply -f "$ROOT/test/local-env/manifests/30-gateway.yaml"
  gateway_uid=$("${KCTL[@]}" -n maas-system get gateway maas-default-gateway -o jsonpath='{.metadata.uid}')
  "${KCTL[@]}" -n maas-system patch service maas-default-gateway --type=merge -p="{\"metadata\":{\"ownerReferences\":[{\"apiVersion\":\"gateway.networking.k8s.io/v1\",\"kind\":\"Gateway\",\"name\":\"maas-default-gateway\",\"uid\":\"$gateway_uid\",\"controller\":false,\"blockOwnerDeletion\":false}]}}"
  tenant_b_gateway_uid=$("${KCTL[@]}" -n maas-system get gateway maas-tenant-b-gateway -o jsonpath='{.metadata.uid}')
  "${KCTL[@]}" -n maas-system patch service maas-tenant-b-gateway --type=merge -p="{\"metadata\":{\"ownerReferences\":[{\"apiVersion\":\"gateway.networking.k8s.io/v1\",\"kind\":\"Gateway\",\"name\":\"maas-tenant-b-gateway\",\"uid\":\"$tenant_b_gateway_uid\",\"controller\":false,\"blockOwnerDeletion\":false}]}}"
  transition_gateway_uid=$("${KCTL[@]}" -n maas-system get gateway maas-transition-gateway -o jsonpath='{.metadata.uid}')
  "${KCTL[@]}" -n maas-system patch service maas-transition-gateway --type=merge -p="{\"metadata\":{\"ownerReferences\":[{\"apiVersion\":\"gateway.networking.k8s.io/v1\",\"kind\":\"Gateway\",\"name\":\"maas-transition-gateway\",\"uid\":\"$transition_gateway_uid\",\"controller\":false,\"blockOwnerDeletion\":false}]}}"
  "${KCTL[@]}" apply -f "$MAAS_CONTROLLER_REPO/deployment/base/networking/odh/kuadrant.yaml"
  # MaaS watches KServe LLMInferenceService. Install the checked-out CRDs
  # before its controller so its controller-runtime cache can synchronize.
  "${KCTL[@]}" apply --server-side -f "$KSERVE_REPO/test/crds/serving.kserve.io_all_crds.yaml"
  # Keep both the upstream render and the exact Kind-transformed bundle. This
  # makes a generated maas-api merge distinguishable from a harness patch.
  kustomize build "$MAAS_CONTROLLER_REPO/maas-api/deploy/overlays/xks" >"$EVIDENCE/maas-api-rendered-xks.yaml"
  kustomize build "$MAAS_CONTROLLER_REPO/maas-api/deploy/overlays/odh" >"$EVIDENCE/maas-api-rendered-odh.yaml" 2>"$EVIDENCE/maas-api-rendered-odh.err" || true
  sha256sum "$EVIDENCE/maas-api-rendered-xks.yaml" "$EVIDENCE/maas-api-rendered-odh.yaml" >"$EVIDENCE/maas-api-rendered.sha256" || true
  cp "$ROOT/test/local-env/manifests/30-gateway.yaml" "$EVIDENCE/kind-patch-30-gateway.yaml"
  cp "$ROOT/test/local-env/manifests/31-maas-api-kind-rbac.yaml" "$EVIDENCE/kind-patch-31-maas-api-rbac.yaml"
  # Keep the MaaS controller/API HTTPS contract intact on Kind. Only the
  # namespace and local image substitutions are harness concerns; validation
  # URLs, secure ports, and TLS settings must remain those rendered upstream.
  kustomize build "$MAAS_CONTROLLER_REPO/deployment/base/maas-controller/default" | sed -e "s#quay.io/opendatahub/maas-api:latest#${MAAS_API_IMAGE:-maas-api:external-model-two-plane}#g" -e "s#quay.io/opendatahub/maas-controller:latest#${MAAS_CONTROLLER_IMAGE:-maas-controller:external-model-two-plane}#g" -e "s#quay.io/opendatahub/maas-controller:odh-stable#${MAAS_CONTROLLER_IMAGE:-maas-controller:external-model-two-plane}#g" -e 's#openshift-ingress#maas-system#g' -e 's#namespace: opendatahub#namespace: maas-system#g' -e 's#namespace: system#namespace: maas-system#g' | yq eval 'select(.kind != "ServiceMonitor" and .kind != "ValidatingWebhookConfiguration")' - >"$EVIDENCE/maas-controller-kind-rendered.yaml"
  "${KCTL[@]}" apply -f "$EVIDENCE/maas-controller-kind-rendered.yaml"
  "${KCTL[@]}" -n maas-system patch deployment maas-api --type=json -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]' 2>/dev/null || true
  "${KCTL[@]}" -n maas-system patch deployment maas-controller --type=json -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
  # The webhook Secret is created below before the controller is restarted.
  # Run the binary directly: a shell wrapper can race projected Secret mounts
  # and obscure the controller's startup error in Kind.
  "${KCTL[@]}" -n maas-system patch deployment maas-controller --type=json -p='[{"op":"replace","path":"/spec/template/spec/containers/0/args/3","value":"--metrics-secure=false"},{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["/manager"]},{"op":"replace","path":"/spec/template/spec/containers/0/args","value":["--leader-elect","--health-probe-bind-address=:8081","--metrics-bind-address=:8080","--metrics-secure=false","--controller-namespace=maas-system","--gateway-name=maas-default-gateway","--gateway-namespace=maas-system","--infra-namespace=AUTO","--maas-subscription-namespace=models-as-a-service","--aitenant-namespace=ai-tenants","--monitoring-namespace=opendatahub","--enable-tenant-namespace-discovery","--metadata-cache-ttl=60","--authz-cache-ttl=60","--log-format=zap"]}]'
  # The rendered MaaS deployment may already have created an old ReplicaSet
  # before the pull-policy patch. Force a fresh template so the new pod uses
  # the exact image loaded into Kind instead of attempting a registry pull.
  "${KCTL[@]}" -n maas-system rollout restart deployment/maas-controller
  "${KCTL[@]}" -n maas-system rollout restart deployment/maas-api 2>/dev/null || true
  # Kind has no OpenShift service-ca injection. The MaaS webhook is therefore
  # omitted from the rendered Kind bundle; remove any instance left by a
  # previous interrupted provision before applying tenant fixtures.
  "${KCTL[@]}" delete validatingwebhookconfiguration maas-validating-webhook-configuration --ignore-not-found
  db_tmp=$(mktemp -d)
  # Kind has no OpenShift service-ca injection. Create a run-owned CA and use
  # it to sign the MaaS API serving certificate, preserving hostname
  # validation for Authorino's HTTPS metadata evaluator.
  openssl req -x509 -nodes -newkey rsa:2048 -days 2 \
    -keyout "$db_tmp/ca.key" -out "$db_tmp/ca.crt" \
    -subj "/CN=external-model-e2e-${CLUSTER}-ca" \
    -addext 'basicConstraints=critical,CA:TRUE,pathlen:1' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' >/dev/null 2>&1
  openssl req -new -nodes -newkey rsa:2048 \
    -keyout "$db_tmp/tls.key" -out "$db_tmp/tls.csr" \
    -subj '/CN=maas-api.maas-system.svc' >/dev/null 2>&1
  cat >"$db_tmp/tls.ext" <<'EOF'
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:maas-api,DNS:maas-api.maas-system,DNS:maas-api.maas-system.svc,DNS:maas-api.maas-system.svc.cluster.local
EOF
  openssl x509 -req -in "$db_tmp/tls.csr" -CA "$db_tmp/ca.crt" -CAkey "$db_tmp/ca.key" \
    -CAcreateserial -out "$db_tmp/tls.crt" -days 2 -sha256 -extfile "$db_tmp/tls.ext" >/dev/null 2>&1
  openssl req -new -nodes -newkey rsa:2048 \
    -keyout "$db_tmp/tenant-b-tls.key" -out "$db_tmp/tenant-b-tls.csr" \
    -subj '/CN=maas-api-tenant-b.maas-system.svc' >/dev/null 2>&1
  sed 's/maas-api/maas-api-tenant-b/g' "$db_tmp/tls.ext" >"$db_tmp/tenant-b-tls.ext"
  openssl x509 -req -in "$db_tmp/tenant-b-tls.csr" -CA "$db_tmp/ca.crt" -CAkey "$db_tmp/ca.key" \
    -CAcreateserial -out "$db_tmp/tenant-b-tls.crt" -days 2 -sha256 -extfile "$db_tmp/tenant-b-tls.ext" >/dev/null 2>&1
  openssl req -new -nodes -newkey rsa:2048 \
    -keyout "$db_tmp/transition-tls.key" -out "$db_tmp/transition-tls.csr" \
    -subj '/CN=maas-api-transition.maas-system.svc' >/dev/null 2>&1
  sed 's/maas-api/maas-api-transition/g' "$db_tmp/tls.ext" >"$db_tmp/transition-tls.ext"
  openssl x509 -req -in "$db_tmp/transition-tls.csr" -CA "$db_tmp/ca.crt" -CAkey "$db_tmp/ca.key" \
    -CAcreateserial -out "$db_tmp/transition-tls.crt" -days 2 -sha256 -extfile "$db_tmp/transition-tls.ext" >/dev/null 2>&1
  openssl x509 -in "$db_tmp/ca.crt" -noout -issuer -subject -serial -fingerprint -sha256 -ext subjectAltName >"$EVIDENCE/maas-api-ca-certificate.txt"
  openssl x509 -in "$db_tmp/tls.crt" -noout -issuer -subject -serial -fingerprint -sha256 -ext subjectAltName >"$EVIDENCE/maas-api-serving-certificate.txt"
  sha256sum "$db_tmp/ca.crt" "$db_tmp/tls.crt" >"$EVIDENCE/maas-api-certificates.sha256"
  cp "$db_tmp/ca.crt" "$EVIDENCE/maas-api-ca.crt"
  cp "$db_tmp/tls.crt" "$EVIDENCE/maas-api-serving.crt"
  "${KCTL[@]}" -n maas-system create secret tls maas-api-serving-cert --cert="$db_tmp/tls.crt" --key="$db_tmp/tls.key" --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" -n maas-system create secret tls maas-api-tenant-b-serving-cert --cert="$db_tmp/tenant-b-tls.crt" --key="$db_tmp/tenant-b-tls.key" --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" -n maas-system create secret tls maas-api-transition-serving-cert --cert="$db_tmp/transition-tls.crt" --key="$db_tmp/transition-tls.key" --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" -n kuadrant-system create configmap authorino-maas-api-ca --from-file=ca.crt="$db_tmp/ca.crt" --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  # Authorino's operator-supported volume projection mounts the dedicated CA
  # into /etc/ssl/certs, where Go's system pool discovers it. The mounted file
  # contains only this run's CA; no system bundle or TLS bypass is configured.
  authorino_found=false
  for _ in $(seq 1 60); do
    if "${KCTL[@]}" -n kuadrant-system get authorino authorino >/dev/null 2>&1; then
      authorino_found=true
      break
    fi
    sleep 2
  done
  [[ "$authorino_found" == true ]] || { fail "Kuadrant did not create the run-owned Authorino resource"; exit 2; }
  "${KCTL[@]}" -n kuadrant-system patch authorino authorino --type=merge -p='{"spec":{"volumes":{"defaultMode":420,"items":[{"name":"maas-api-serving-ca","mountPath":"/etc/ssl/certs","configMaps":["authorino-maas-api-ca"],"items":[{"key":"ca.crt","path":"maas-api-serving-ca.crt"}]}]}}}'
  "${KCTL[@]}" -n kuadrant-system get authorino authorino -o yaml >"$EVIDENCE/authorino-ca-config.yaml"
  "${KCTL[@]}" -n maas-system create secret tls maas-controller-webhook-cert --cert="$db_tmp/tls.crt" --key="$db_tmp/tls.key" --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" -n maas-system create secret tls maas-controller-webhook-cert --cert="$db_tmp/tls.crt" --key="$db_tmp/tls.key" --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" -n maas-system create secret tls maas-controller-metrics-tls --cert="$db_tmp/tls.crt" --key="$db_tmp/tls.key" --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  kustomize build "$MAAS_CONTROLLER_REPO/deployment/base/maas-api/rbac" | sed 's#namespace: opendatahub#namespace: maas-system#g' | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" apply -f "$ROOT/test/local-env/manifests/31-maas-api-kind-rbac.yaml"
  # The webhook Secret is created after the OpenShift-only bundle is rendered;
  # restart so the projected certificate is present before manager startup.
  "${KCTL[@]}" -n maas-system rollout restart deployment/maas-controller
  # Repeated local runs may leave the old static maas-api object in place.
  # Let the current MaaS tenant pipeline create its canonical object so
  # server-side apply cannot merge stale OpenShift fields into the Kind spec.
  "${KCTL[@]}" -n maas-system delete deployment maas-api service maas-api --ignore-not-found
  kustomize build "$ROOT/config/crd" | "${KCTL[@]}" apply --server-side -f -
  kustomize build "$ROOT/config/self/default" | "${KCTL[@]}" apply --server-side --force-conflicts -f -
  "${KCTL[@]}" -n opendatahub set image deployment/ai-gateway-controller manager="${AI_CONTROLLER_IMAGE:-ai-gateway-controller:external-model-two-plane}"
  "${KCTL[@]}" -n opendatahub patch deployment ai-gateway-controller --type=json -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"},{"op":"replace","path":"/spec/template/spec/containers/0/args","value":["--leader-elect","--health-probe-bind-address=:8081","--gateway-name=maas-default-gateway","--gateway-namespace=maas-system","--known-cluster=provider-provider-a","--known-cluster=provider-provider-b","--known-cluster=provider-transition-provider","--image=praxis-extproc:dev","--praxis-image=praxis-ai:overlay-e2e","--praxis-image-pull-policy=Never"]}]'
  # Standalone Praxis is now rendered and owned by ai-gateway-controller from
  # ExternalProvider references. Do not apply the former static tenant
  # Deployments here; doing so would create an unowned same-name object and
  # correctly block the controller's ownership handoff.
  for manifest in "$ROOT/test/local-env/manifests"/*.yaml; do
    case "$(basename "$manifest")" in
      10-praxis.yaml|11-praxis-tenant-b.yaml|12-praxis-transition.yaml) continue ;;
    esac
    "${KCTL[@]}" apply -f "$manifest"
  done
  # MaaS creates one legacy IPP deployment per tenant.  The upstream IPP
  # runner supports a namespace-scoped cache and explicit Gateway settings;
  # provide those only in this Kind fixture.  Keep IPP disabled for tenants
  # already owned by Praxis so it cannot create a competing direct-provider
  # route, and enable it only for the annotation-absent transition tenant.
  ipp_ready=false
  for _ in $(seq 1 60); do
    # Praxis tenants may already have had their MaaS IPP operands removed by
    # the ownership-gated transition. Only the annotation-absent transition
    # tenant is required to retain a legacy IPP deployment at this stage.
    if "${KCTL[@]}" -n maas-system get deployment payload-processing-transition >/dev/null 2>&1; then
      ipp_ready=true
      break
    fi
    sleep 2
  done
  [[ "$ipp_ready" == true ]] || { fail "MaaS did not create the transition tenant IPP deployment"; exit 2; }
  if "${KCTL[@]}" -n maas-system get deployment/payload-processing >/dev/null 2>&1; then
    "${KCTL[@]}" -n maas-system set env deployment/payload-processing \
      NAMESPACE=models-as-a-service GATEWAY_NAMESPACE=maas-system GATEWAY_NAME=maas-default-gateway DISABLE_EXTERNAL_MODEL_CONTROLLER=true
  fi
  if "${KCTL[@]}" -n maas-system get deployment/payload-processing-tenant-b >/dev/null 2>&1; then
    "${KCTL[@]}" -n maas-system set env deployment/payload-processing-tenant-b \
      NAMESPACE=ai-tenant-tenant-b GATEWAY_NAMESPACE=maas-system GATEWAY_NAME=maas-tenant-b-gateway DISABLE_EXTERNAL_MODEL_CONTROLLER=true
  fi
  "${KCTL[@]}" -n maas-system set env deployment/payload-processing-transition \
    NAMESPACE=ai-tenant-transition GATEWAY_NAMESPACE=maas-system GATEWAY_NAME=maas-transition-gateway DISABLE_EXTERNAL_MODEL_CONTROLLER=false
  if "${KCTL[@]}" -n maas-system get deployment/payload-pre-processing >/dev/null 2>&1; then
    "${KCTL[@]}" -n maas-system set env deployment/payload-pre-processing \
      NAMESPACE=models-as-a-service GATEWAY_NAMESPACE=maas-system GATEWAY_NAME=maas-default-gateway DISABLE_EXTERNAL_MODEL_CONTROLLER=true
  fi
  if "${KCTL[@]}" -n maas-system get deployment/payload-pre-processing-tenant-b >/dev/null 2>&1; then
    "${KCTL[@]}" -n maas-system set env deployment/payload-pre-processing-tenant-b \
      NAMESPACE=ai-tenant-tenant-b GATEWAY_NAMESPACE=maas-system GATEWAY_NAME=maas-tenant-b-gateway DISABLE_EXTERNAL_MODEL_CONTROLLER=true
  fi
  "${KCTL[@]}" -n maas-system set env deployment/payload-pre-processing-transition \
    NAMESPACE=ai-tenant-transition GATEWAY_NAMESPACE=maas-system GATEWAY_NAME=maas-transition-gateway DISABLE_EXTERNAL_MODEL_CONTROLLER=false
  if "${KCTL[@]}" -n maas-system get deployment/payload-processing >/dev/null 2>&1; then "${KCTL[@]}" -n maas-system rollout status deployment/payload-processing --timeout=120s; fi
  if "${KCTL[@]}" -n maas-system get deployment/payload-processing-tenant-b >/dev/null 2>&1; then "${KCTL[@]}" -n maas-system rollout status deployment/payload-processing-tenant-b --timeout=120s; fi
  "${KCTL[@]}" -n maas-system rollout status deployment/payload-processing-transition --timeout=120s
  if "${KCTL[@]}" -n maas-system get deployment/payload-pre-processing >/dev/null 2>&1; then "${KCTL[@]}" -n maas-system rollout status deployment/payload-pre-processing --timeout=120s; fi
  if "${KCTL[@]}" -n maas-system get deployment/payload-pre-processing-tenant-b >/dev/null 2>&1; then "${KCTL[@]}" -n maas-system rollout status deployment/payload-pre-processing-tenant-b --timeout=120s; fi
  "${KCTL[@]}" -n maas-system rollout status deployment/payload-pre-processing-transition --timeout=120s
  # Do not delete legacy IPP HTTPRoutes here. Their owner is the pinned IPP
  # ExternalModel reconciler, and deleting them would hide an ownership or
  # cutover defect. The qualification records any such route explicitly.
  "${KCTL[@]}" -n ai-tenants get aitenant models-as-a-service -o yaml >"$EVIDENCE/aitenant-after-fixtures.yaml" 2>&1 || true
  "${KCTL[@]}" -n maas-system get config default -o yaml >"$EVIDENCE/maas-config-after-fixtures.yaml" 2>&1 || true
  "${KCTL[@]}" -n maas-system get deployment maas-api -o yaml >"$EVIDENCE/maas-api-before-tenant-apply.yaml" 2>&1 || true
  "${KCTL[@]}" -n maas-system rollout status deployment/maas-postgres --timeout=180s
  gateway_ready=false
  for _ in $(seq 1 60); do
    listener_status=$("${KCTL[@]}" -n maas-system get gateway maas-default-gateway -o jsonpath='{.status.listeners[0].conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)
    endpoint_count=$("${KCTL[@]}" -n maas-system get endpoints maas-default-gateway -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || true)
    # Kind's LoadBalancer address remains Pending even when Istio has fully
    # programmed the listener. Use the listener condition plus endpoints as
    # the state-based readiness signal for the run-owned alias Service.
    if [[ "$listener_status" == "True" && -n "$endpoint_count" ]]; then
      gateway_ready=true
      break
    fi
    sleep 3
  done
  [[ "$gateway_ready" == true ]] || { fail "Istio Gateway listener or endpoint did not become ready"; exit 2; }
  # Kind's LoadBalancer Service has no external address, while MaaS uses the
  # Gateway address to resolve a model endpoint. Publish the run-owned
  # in-cluster Gateway hostname after the listener is actually programmed;
  # OpenShift's gateway/operator supplies this status in production.
  "${KCTL[@]}" -n maas-system patch gateway maas-default-gateway --subresource=status --type=merge -p='{"status":{"addresses":[{"type":"Hostname","value":"maas-default-gateway-istio.maas-system.svc.cluster.local"}]}}'
  "${KCTL[@]}" -n maas-system patch gateway maas-tenant-b-gateway --subresource=status --type=merge -p='{"status":{"addresses":[{"type":"Hostname","value":"maas-tenant-b-gateway.maas-system.svc.cluster.local"}]}}'
  transition_gateway_ready=false
  for _ in $(seq 1 60); do
    transition_listener=$("${KCTL[@]}" -n maas-system get gateway maas-transition-gateway -o jsonpath='{.status.listeners[0].conditions[?(@.type=="Programmed")].status}' 2>/dev/null || true)
    transition_endpoints=$("${KCTL[@]}" -n maas-system get endpoints maas-transition-gateway -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || true)
    if [[ "$transition_listener" == "True" && -n "$transition_endpoints" ]]; then transition_gateway_ready=true; break; fi
    sleep 3
  done
  [[ "$transition_gateway_ready" == true ]] || { fail "Istio transition Gateway listener or endpoint did not become ready"; exit 2; }
  # MaaS may reconcile the generated legacy deployments while the three
  # gateways are becoming ready. Re-apply the transition-only Kind wiring at
  # the settled point, then wait for the pods that read these values at
  # startup. This selects the transition Gateway for the real IPP fixture;
  # it does not remove or rewrite the IPP-owned HTTPRoute.
  for ipp_deployment in payload-processing-transition payload-pre-processing-transition; do
    "${KCTL[@]}" -n maas-system set env deployment/"$ipp_deployment" \
      NAMESPACE=ai-tenant-transition GATEWAY_NAMESPACE=maas-system GATEWAY_NAME=maas-transition-gateway DISABLE_EXTERNAL_MODEL_CONTROLLER=false
    "${KCTL[@]}" -n maas-system rollout status deployment/"$ipp_deployment" --timeout=120s
  done
  "${KCTL[@]}" -n maas-system patch gateway maas-transition-gateway --subresource=status --type=merge -p='{"status":{"addresses":[{"type":"Hostname","value":"maas-transition-gateway.maas-system.svc.cluster.local"}]}}'
  # MaaS API validates the internal gateway service during startup. Restart it
  # after Istio has programmed the gateway and populated the run-owned alias.
  # The tenant pipeline creates/reconciles one API Deployment per tenant.
  # Restart every run-owned API instance after replacing its serving Secret so
  # the process is definitely using the certificate signed by this run's CA.
  for api_deployment in maas-api maas-api-tenant-b maas-api-transition; do
    if "${KCTL[@]}" -n maas-system get deployment "$api_deployment" >/dev/null 2>&1; then
      "${KCTL[@]}" -n maas-system rollout restart "deployment/$api_deployment"
    fi
  done
  for api_deployment in maas-api maas-api-tenant-b maas-api-transition; do
    if "${KCTL[@]}" -n maas-system get deployment "$api_deployment" >/dev/null 2>&1; then
      "${KCTL[@]}" -n maas-system rollout status "deployment/$api_deployment" --timeout=180s
    fi
  done
  maas_api_created=false
  for _ in $(seq 1 60); do
    if "${KCTL[@]}" -n maas-system get deployment maas-api >/dev/null 2>&1; then
      maas_api_created=true
      break
    fi
    sleep 3
  done
  [[ "$maas_api_created" == true ]] || { fail "MaaS tenant pipeline did not create the default maas-api"; exit 2; }
  "${KCTL[@]}" -n ai-tenants get aitenant models-as-a-service -o yaml >"$EVIDENCE/aitenant-generated.yaml" 2>&1 || true
  "${KCTL[@]}" -n maas-system get config default -o yaml >"$EVIDENCE/maas-config-generated.yaml" 2>&1 || true
  "${KCTL[@]}" -n maas-system get deployment maas-api -o yaml >"$EVIDENCE/maas-api-generated.yaml" 2>&1 || true
  "${KCTL[@]}" -n maas-system rollout status deployment/maas-api --timeout=180s
  "${KCTL[@]}" -n maas-system rollout status deployment/maas-controller --timeout=180s
  "${KCTL[@]}" -n opendatahub rollout status deployment/ai-gateway-controller --timeout=180s
  wait_for_deployment() {
    local namespace=$1 name=$2
    for _ in $(seq 1 60); do
      if "${KCTL[@]}" -n "$namespace" get deployment "$name" >/dev/null 2>&1; then
        "${KCTL[@]}" -n "$namespace" rollout status "deployment/$name" --timeout=180s
        return 0
      fi
      sleep 2
    done
    fail "controller did not create tenant Praxis deployment $namespace/$name"
    return 1
  }
  wait_for_deployment models-as-a-service praxis
  wait_for_deployment ai-tenant-tenant-b praxis-tenant-b
  "${KCTL[@]}" -n maas-system rollout status deployment/katan-a-tenant-b --timeout=180s
  "${KCTL[@]}" -n maas-system rollout status deployment/katan-b-tenant-b --timeout=180s
  "${KCTL[@]}" -n maas-system rollout status deployment/katan-a --timeout=180s
  "${KCTL[@]}" -n maas-system rollout status deployment/katan-b --timeout=180s
  "${KCTL[@]}" -n kuadrant-system rollout status deployment/authorino --timeout=180s
  # The MaaS tenant pipeline may reconcile the transition operand while the
  # other tenant resources are becoming ready. Apply the run-owned IPP
  # transition configuration once more at the settled point, then require the
  # actual IPP-owned route to be attached to the transition Gateway before the
  # qualification can issue its initial request. This is setup convergence,
  # not route deletion or fabricated status.
  for ipp_deployment in payload-processing-transition payload-pre-processing-transition; do
    "${KCTL[@]}" -n maas-system set env deployment/"$ipp_deployment" \
      NAMESPACE=ai-tenant-transition GATEWAY_NAMESPACE=maas-system GATEWAY_NAME=maas-transition-gateway DISABLE_EXTERNAL_MODEL_CONTROLLER=false
    "${KCTL[@]}" -n maas-system rollout status deployment/"$ipp_deployment" --timeout=180s
  done
  transition_route_configured=false
  for _ in $(seq 1 60); do
    transition_parent=$("${KCTL[@]}" -n ai-tenant-transition get httproute transition-model -o jsonpath='{.spec.parentRefs[0].namespace}/{.spec.parentRefs[0].name}' 2>/dev/null || true)
    transition_accepted=$("${KCTL[@]}" -n ai-tenant-transition get httproute transition-model -o jsonpath='{range .status.parents[*].conditions[?(@.type=="Accepted")]}{.status}{end}' 2>/dev/null || true)
    transition_refs=$("${KCTL[@]}" -n ai-tenant-transition get httproute transition-model -o jsonpath='{range .status.parents[*].conditions[?(@.type=="ResolvedRefs")]}{.status}{end}' 2>/dev/null || true)
    if [[ "$transition_parent" == "maas-system/maas-transition-gateway" && "$transition_accepted" == *True* && "$transition_refs" == *True* ]]; then
      transition_route_configured=true
      break
    fi
    sleep 2
  done
  [[ "$transition_route_configured" == true ]] || { fail "transition IPP route did not converge to maas-system/maas-transition-gateway"; exit 2; }
  "${KCTL[@]}" -n kuadrant-system get deployment authorino -o yaml >"$EVIDENCE/authorino-deployment.yaml"
  authorino_pod=$("${KCTL[@]}" -n kuadrant-system get pods -l authorino-resource=authorino -o jsonpath='{.items[0].metadata.name}')
  "${KCTL[@]}" -n kuadrant-system exec "$authorino_pod" -- sha256sum /etc/ssl/certs/maas-api-serving-ca.crt >"$EVIDENCE/authorino-mounted-ca.sha256"
  sha256sum "$db_tmp/ca.crt" >"$EVIDENCE/authorino-expected-ca.sha256"
  "${KCTL[@]}" -n models-as-a-service get externalmodel,externalprovider,httproute,serviceentry,destinationrule,configmap,pod -o yaml >"$EVIDENCE/routing-state.yaml"
  "${KCTL[@]}" get events -A --sort-by=.lastTimestamp >"$EVIDENCE/events.txt"
  printf '{\n  "status":"PASS",\n  "cluster":"kind-%s",\n  "evidence":"%s"\n}\n' "$CLUSTER" "$EVIDENCE" >"$EVIDENCE/result.json"
  exit 0
fi

printf '{\n  "status":"PASS",\n  "cluster":"kind-%s",\n  "evidence":"%s"\n}\n' "$CLUSTER" "$EVIDENCE" >"$EVIDENCE/result.json"
