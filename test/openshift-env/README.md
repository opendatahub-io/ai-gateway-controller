# OpenShift integration environment

This directory owns the controller's production-shaped OpenShift qualification.
It is separate from `test/kind-env`; it does not generate a test CA, alter image
pull policy, remove webhooks or ServiceMonitors, fabricate Gateway status, or
disable TLS verification.

On a fresh disposable cluster this harness bootstraps the minimum pinned
platform stack required for the qualification. It does not install the full
RHOAI operator or claim operator-lifecycle qualification.

The environment uses an isolated kubeconfig and three run-owned namespaces:
controller, resolved tenant, and provider backend. It creates one uniquely
named, run-labeled Gateway resource in the required shared Gateway namespace.
Every run is labeled with
`external-model-praxis.opendatahub.io/run-id`. The scripts refuse to delete a
namespace whose label does not match the recorded run.

## Prerequisites

The supported run uses a dedicated/disposable OpenShift cluster. Missing
components listed below are provisioned by `bootstrap.sh` and `provision.sh`;
incompatible pre-existing components are a preflight failure. The scripts never
replace a managed component or start a second unscoped reconciler. Required
local tools are:

- `oc`, authenticated to the target cluster;
- `docker`, `helm`, `kustomize`, `curl`, `tar`, `jq`, and `sha256sum`;
- clean checkouts of the pinned MaaS, Praxis AI, Praxis ExtProc,
  KServe, and Kuadrant operator sources.

The cluster must provide an OpenShift internal image registry and enough CPU
and memory for Istio, Kuadrant, PostgreSQL, MaaS, the controller, Praxis, and
the provider fixtures. A fresh cluster is preferred. An existing MaaS
deployment is accepted only with a verified immutable image and source proof
for the Praxis selector and `SkipIPP` behavior.

Create an isolated kubeconfig from the authenticated session, or log in to one
without printing the token. Keep it under ignored state and set
`OPENSHIFT_KUBECONFIG`; never use the default kubeconfig in the documented run.
Do not put tokens, passwords, or Secret data in evidence.

The documented workflow is:

```bash
export OPENSHIFT_E2E_STATE="$PWD/.openshift-state"
export OPENSHIFT_KUBECONFIG="$OPENSHIFT_E2E_STATE/kubeconfig"
./test/openshift-env/preflight.sh
./test/openshift-env/provision.sh
./test/openshift-env/e2e.sh
./test/openshift-env/inspect.sh
./test/openshift-env/destroy.sh
```

For a source-qualified run, export the clean pinned checkout paths before
`provision.sh`:

```bash
export PRAXIS_REPO=/path/to/praxis-ai
export PRAXIS_EXTPROC_REPO=/path/to/praxis-extproc
export MAAS_CONTROLLER_REPO=/path/to/models-as-a-service
export KSERVE_REPO=/path/to/kserve
export KUADRANT_OPERATOR_REPO=/path/to/kuadrant-operator
```

To qualify an already published controller artifact, use its complete digest
reference. This skips only the controller source build; the remaining
source-matched application images still follow the normal build and publish
path:

```bash
export OPENSHIFT_E2E_CONTROLLER_IMAGE='ghcr.io/nerdalert/ai-gateway-controller@sha256:7640854bf53829b4d0b25aac3151b8bf685c9e046ce2c7cf80da2058d1fe33f7'
```

The digest is the executable artifact. Source commits and dirty-tree hashes
remain provenance and must still be recorded. Never replace the digest with a
floating tag.

If the external digest fails before container start with an OpenShift kubelet
error such as `invalid username/password` or `unauthorized`, preserve the pod
events and do not count the deployment as a routing failure. Verify anonymous
access to the image from the cluster first. If the published artifact is not
actually pullable, unset `OPENSHIFT_E2E_CONTROLLER_IMAGE` and use the normal
source build/push path into the run-owned internal registry; record that the
published-image access claim was not usable for this cluster. Do not add a
registry credential to evidence or silently use a different digest.

Run `preflight.sh` before provisioning on every cluster. It records a
read-only baseline, verifies tools and cluster capabilities, and distinguishes
expected-to-be-installed components from incompatible pre-existing components.
It requires an immutable MaaS compatibility proof when MaaS is already
installed; on a fresh cluster it records that provisioning must install the
matching image. Provisioning then installs the missing stack, builds and
publishes immutable images, creates only run-owned fixtures, and waits for the
MaaS-resolved tenant namespace. Never repair a failed provision by patching
live resources.

Before provisioning, preflight requires an explicit compatibility proof for the
installed MaaS image. Set `MAAS_PRAXIS_COMPATIBILITY_PROOF` if using a
non-default location; the file must contain the exact immutable deployment
image and the source commit that implements Praxis `SkipIPP` and ownership-safe
cleanup:

```text
image=...@sha256:...
source_commit=<verified-commit>
```

This check is intentionally behavior/provenance based. A healthy MaaS pod or
webhook alone does not prove Praxis opt-in support. The RHOAI 3.5 image found
on the shared development cluster is not accepted without such proof because
it renders payload-processing resources for Praxis-selected tenants. The
preflight failure occurs before an AITenant is created; no second MaaS
controller or managed Deployment replacement is permitted by this harness.

For a matching image, create the proof after verifying the image digest and
source provenance from the supported build pipeline; do not hand-author an
unverified file:

```bash
cat > "$OPENSHIFT_E2E_STATE/maas-praxis-compatibility.env" <<'EOF'
image=<exact-installed-image>@sha256:<verified-digest>
source_commit=<verified-MaaS-commit>
EOF
export MAAS_PRAXIS_COMPATIBILITY_PROOF="$OPENSHIFT_E2E_STATE/maas-praxis-compatibility.env"
```

The placeholder values must be replaced with verified values before use. The
proof is rejected if the image does not match the deployed immutable image.

## Automated installation order

`provision.sh` is the automated installation entrypoint after preflight. It
performs these steps in order and writes logs for each step below the run
evidence directory:

1. install Gateway API CRDs when absent;
2. install pinned cert-manager;
3. download and install pinned Istio and its run-owned OpenShift SCC;
4. install the pinned Kuadrant operator and apply only the three independent
   configuration resources `authorino.yaml`, `limitador.yaml`, and
   `kuadrant.yaml` from
   `config/install/configure/standard/`; deliberately exclude `sail.yaml`;
5. install the pinned minimal KServe CRDs;
6. create a run-owned passthrough Route to the OpenShift internal registry;
7. build and push immutable controller, Praxis AI, ExtProc, MaaS API, and MaaS
   controller images; use the public pinned Katan digest directly;
8. deploy the pinned MaaS API/controller through its repository-native direct
   Kustomize lane;
9. create the run-owned AITenant, controller, Gateway, and provider fixtures;
10. create the tenant's MaaSModelRef, MaaSAuthPolicy, and MaaSSubscription in
    the resolved tenant namespace;
11. wait for readiness, HTTPRoute acceptance/reference resolution, overlay
    publication, and request-path convergence.

The `MaaSModelRef`, `MaaSAuthPolicy`, and `MaaSSubscription` are created in the
resolved tenant namespace returned by the run-owned `AITenant`. This is the
source-matched MaaS tenant-namespace discovery contract for non-default
tenants. MaaS is configured with the run Gateway name and namespace so model
status and policy generation use the same Gateway. The subscription name is
run-qualified. Before creating an API key, wait for each resource's
`status.conditions[type=Ready]`; a failed condition is a fixture failure and
must be preserved in evidence.

For a request-path diagnostic, obtain the key only through the normal MaaS API
from the persistent in-cluster client. Keep it in process memory and send it
to the client over stdin; never put it in a command argument or saved output.
Record only API status, Secret namespace/name/key and UID/resourceVersion, and
non-secret backend attribution. Use the run Gateway's real hostname and its
serving certificate for TLS verification; never use `-k`.

For the HTTPS listener, provisioning first creates the run-owned Gateway and
waits for its real OpenShift load-balancer hostname. It then generates a
short-lived run-owned certificate whose SAN is that assigned hostname, creates
the referenced Secret in the Gateway namespace, and waits for the listener to
become programmed. The certificate is verified from the served endpoint and
only certificate metadata/fingerprints are retained; private keys are removed
after Secret creation and never enter evidence. This ordering is required
because the hostname is not known before the Gateway is programmed.

After Authorino and Kuadrant are installed, provisioning restarts the
run-installed Kuadrant controller through its normal Deployment lifecycle and
waits for it to rediscover the Ready Authorino dependency. AuthPolicy
acceptance is required before functional routing is considered available.
Policy conditions and controller logs are retained on failure without any
credential or token material.

After Istio installation, the bootstrapper records an admission-chain
inventory and requires exactly one `istiod` control plane. Every
`validation.istio.io` webhook must target that control plane's `istiod` Service,
have a non-empty CA bundle matching the installed root certificate, and have
ready endpoints. The harness also performs an `openssl s_client
-verify_return_error` check against the live Istio discovery listener using the
installed root certificate and webhook SNI. A server-side dry-run `VirtualService`
admission probe is the final gate before MaaS and AITenant creation. A second
Istio installation, foreign webhook configuration, or certificate-chain
mismatch is a blocking environment condition; the harness preserves diagnostics
and does not patch or delete unowned webhooks.

Istio's namespace, control-plane resources, Services, and admission webhooks
are shared platform infrastructure. They are inventoried and validated, but
are never relabeled as run-owned and are not removed by `destroy.sh`. Only the
run-owned Istio SCC is labeled and removed after its ownership is revalidated.

### Functional request-path boundary

Infrastructure readiness is not functional routing proof. The functional suite
must mint a run-owned ephemeral API key through the tenant MaaS API, keep the
plaintext value only in protected process memory or temporary client input, and
send requests through the real Gateway hostname with certificate verification
enabled. It records only HTTP status, provider attribution, overlay metadata,
and workload identity.

The current single-tenant qualification preserves MaaS's existing shared
callback URL. The OpenShift-only compatibility adapter creates a run-owned
`maas-system/maas-api` Service with a service-ca certificate for both
`maas-api.maas-system.svc` and
`maas-api.maas-system.svc.cluster.local`. It terminates TLS in a run-owned
proxy and forwards over verified TLS to the canonical tenant-qualified MaaS
API Service. It never mutates the MaaS-owned Deployment and is not installed
by production manifests. The adapter proves only this single-tenant callback
compatibility; tenant-aware MaaS callback routing remains a separate MaaS-wide
follow-up affecting both the existing IPP path and Praxis integration.

The client-visible inference URL must be built as
`/<resolved-tenant-namespace>/<model>/v1/chat/completions` (for example,
`/<resolved-tenant-namespace>/demo/v1/chat/completions`). The `demo` segment is
required by the generated AuthPolicy's subscription selector. An intentionally
incorrect path that omits the model segment is a negative authorization case
and must return `403`; it is not evidence of a TLS or MaaS failure.

This is component-level OpenShift qualification, not MaaS or AI Gateway
Operator lifecycle qualification. No floating image tag, Kind-generated CA,
`imagePullPolicy: Never`, fabricated status, or insecure TLS option is used.
The existing IPP path is not modified.

## Commands

```bash
./test/openshift-env/preflight.sh
./test/openshift-env/provision.sh
./test/openshift-env/e2e.sh
./test/openshift-env/inspect.sh
./test/openshift-env/destroy.sh
```

`preflight.sh` captures read-only cluster baseline and writes `.openshift-state/run.env`.
Provisioning and qualification are intentionally not claimed complete until
production-pullable images, operator-managed Gateway behavior, service-ca trust,
SCC admission, and real authenticated requests have been observed.

The current `e2e.sh` implements eight OpenShift infrastructure assertions:
controller and MaaS readiness, AITenant/provider/model readiness, HTTPRoute
acceptance and reference resolution, standalone Praxis readiness, and denied
Secret API access. It records `PARTIAL` until the authenticated
Gateway-to-ExtProc-to-Praxis request fixture and the remaining routing
assertions are implemented; this boundary must not be reported as a routing
qualification pass.

Destroy ordering is strict: the run-owned AITenant is deleted first while the
run controller, Gateway, and resolved tenant namespace remain available. The
script waits for the controller cleanup finalizer and AITenant disappearance
before deleting any controller, Gateway, RBAC, or namespace resource. A timeout
or failed deletion preserves the remaining resources and evidence and returns
`PARTIAL` with a nonzero exit status; finalizers are never force-removed.

The tenant-scoped standalone Praxis workload remains owned by
`ai-gateway-controller` in this initial integration. The operator installs and
configures the controller and supplies approved defaults; moving tenant workload
ownership to the operator requires maintainer review.

The existing IPP path and shared RHOAI resources are outside the run-owned
namespace set and are not modified. The Experimental repository remains a
portable demonstration and contains no OpenShift deployment logic.

### Run-specific deployment notes

The MaaS API image is published in the run-owned provider image project. Its
pull authorization is a RoleBinding in that project using the exact subject
`system:serviceaccount:maas-system:maas-api` and the
`system:image-puller` ClusterRole. Attaching a pull Secret to the ServiceAccount
alone is insufficient. Before accepting the rollout, inspect the RoleBinding,
verify the exact subject, and record the running pod's immutable image digest.
Cleanup removes this RoleBinding only after rechecking its run label and exact
ownership.

The source-matched MaaS installation creates the default tenant
`ai-tenants/models-as-a-service` and its canonical `maas-system/maas-api`
Service. The single-tenant qualification reuses that tenant rather than
creating a second AITenant on the same Gateway. Before adding the Praxis opt-in,
record the tenant's original annotations and labels; cleanup must restore that
metadata and must not delete a MaaS-owned default tenant. The native shared
Service is preferred when its selector, ready EndpointSlice, and service-ca
certificate SANs are valid. A transparent TLS adapter is only used for a
non-default tenant-qualified API and must never manufacture authorization
responses or redirect between tenants.

The MaaS API NetworkPolicy permits API callers from `maas-system`, the Gateway
namespace, and the Authorino namespace. A run-owned persistent client used to
mint the ephemeral key therefore runs in an allowed namespace, while Gateway
inference requests still originate through the public Gateway hostname. The
client's key remains inside the pod and is supplied over stdin; it is never an
argument, log line, transcript, or evidence file. The subscription and policy
fixtures use the identity returned by `oc whoami`, not a hardcoded demo user.

The controller image is deployed by resolved digest. If a controller source
fix is required after the initial deployment, build and publish only that
image under a new run-specific input, record its digest, update the Deployment,
and wait for rollout before interpreting subsequent tenant state. Do not reuse
the previous controller digest after changing controller source.

When a run fails, preserve the run evidence and inspect the first failed
boundary before retrying. In particular, a MaaS API key-creation HTTP response
with `AUTH_FAILURE` is a definitive application result, not a transport retry;
inspect the authenticated user, subscription owner, policy subject, API
endpoint, and MaaS logs without recording the key or Authorization header.

### Authorino service-ca trust

For a component-level OpenShift run, create a run-owned ConfigMap in the
Authorino namespace with
`service.beta.openshift.io/inject-cabundle: "true"`. Wait until OpenShift
injects a non-empty `service-ca.crt`, record only its SHA-256, and mount it
through the supported `Authorino.spec.volumes` API. Wait for the CR-managed
Authorino rollout, verify the mounted-file hash equals the injected bundle,
and run an in-pod `openssl s_client -verify_return_error` check against
`maas-api.maas-system.svc.cluster.local:8443` with that CA and SNI. Do not
replace the MaaS certificate, use `--insecure`, or patch generated
AuthPolicies. A failed trust check blocks request qualification.

Any API key exposed during a failed diagnostic is not considered clean. Revoke
it through the normal MaaS API lifecycle before continuing, record only its
identifier and sanitized revocation response, scan shell history and evidence,
and mint a new opaque key. Never copy a key into evidence or command output.

The automated order in `provision.sh` is important on a fresh cluster. It
first waits for the source-matched native `maas-api` Service, ready
EndpointSlice, and its service-ca certificate. It then creates the run-owned
ConfigMap and waits for OpenShift to inject `service-ca.crt`:

```sh
oc --kubeconfig "$OPENSHIFT_KUBECONFIG" -n kuadrant-system \
  get configmap "xmp-service-ca-$OPENSHIFT_E2E_RUN_ID" \
  -o jsonpath='{.data.service-ca\\.crt}' | wc -c
```

The provisioner records only the byte count and SHA-256, and patches the
run's `Authorino` custom resource through its supported `spec.volumes`
field. It waits for the CR-managed rollout, compares the mounted CA hash with
the injected bundle, and performs a hostname/SNI-verified request from the
Authorino pod to `maas-api.maas-system.svc.cluster.local:8443`. The gate
requires `ssl_verify_result=0` before AuthPolicy-driven request
qualification. It never changes the MaaS Deployment or certificate and never
uses `--insecure`. On failure, inspect:

```sh
"$OPENSHIFT_E2E_STATE/evidence/$OPENSHIFT_E2E_RUN_ID/provision/authorino-service-ca.txt"
"$OPENSHIFT_E2E_STATE/evidence/$OPENSHIFT_E2E_RUN_ID/provision/authorino-service-ca-patch.log"
"$OPENSHIFT_E2E_STATE/evidence/$OPENSHIFT_E2E_RUN_ID/provision/authorino-to-maas-tls.txt"
```

This service-ca step is OpenShift E2E compatibility setup only. It is not a
production manifest and proves trust for the single-tenant shared callback;
tenant-aware MaaS callback dispatch remains outside this qualification.

The OpenShift Katan fixture is also constrained by the platform security
context. Its published image writes persistent statistics beneath `$HOME/.llm-katan`;
the fixture sets `HOME=/tmp` and mounts a run-local `emptyDir` at `/tmp`.
This preserves the read-only root filesystem contract, does not change the
image, and is applied by `provision.sh` before the provider-readiness gate.
Verify the running image digest and zero restarts before using provider
responses as evidence.

In the retained run, the service-ca trust and MaaS callbacks passed, and the
fixture started with a writable statistics directory. The first authenticated
request exposed a controller mapping defect: the overlay candidate had no
credential reference for the CRD `apikey` type, so Praxis could not select the
projected credential and the real provider returned HTTP 500 for missing
Authorization. The controller now maps only the explicitly qualified
`openai` + `openai-chat` + `apikey` combination to Praxis `bearer_token`,
which is an internal consumer strategy, not a CRD value. The overlay still
contains only a Secret reference and never the Secret value. Other
provider/API-format combinations, SigV4, OAuth2, and unknown types fail
closed. Rebuild and redeploy the controller from this source before treating
the request-path qualification as current evidence; do not add a live overlay
patch or make the fixture accept unauthenticated traffic.

The mapping is deliberately provider-specific. An `apikey` does not by itself
mean `Authorization: Bearer`: provider and API format are both required. This
run qualifies only the OpenAI chat contract. Adding Anthropic, Azure, API-key
headers, SigV4, OAuth2, or another provider requires a separate explicit wire
mapping and tests; it must not inherit the OpenAI mapping.

The current live handoff also verifies the ownership boundary for the ExtProc
configuration. MaaS marks its old `payload-processing-plugins` ConfigMap with
`opendatahub.io/managed=false` when Praxis is selected. The controller may
claim that explicitly released object and publishes the required
`extproc.yaml` and `pre-extproc.yaml`; it still refuses every other unlabeled
or foreign object. This is a controller/MaaS handoff, not an IPP change.

The reproducible handoff check is: wait for the released ConfigMap, apply the
controller-owned render, verify that both `extproc.yaml` and
`pre-extproc.yaml` are present, then roll the ExtProc Deployment through its
normal controller-owned update. In the retained validation run, the first
failure was the ExtProc process mounting the pre-handoff IPP-only ConfigMap;
after the ownership-gated apply and rollout, the ExtProc pod was Ready with
zero restarts and an authenticated Gateway request returned HTTP 200 from the
real Provider A fixture. This evidence is a functional boundary result, not
a claim that the current eight-assertion OpenShift script is a complete
routing qualification.

On the component-level OpenShift stack validated here, Authorino's generated
HTTP metadata callbacks use the existing shared MaaS URL and require the
Authorino runtime trust store to recognize the OpenShift service-ca issuer.
The run must prove both callbacks, `/internal/v1/api-keys/validate` and
`/internal/v1/subscriptions/select`, through the real callback path. A direct
client call to MaaS is not equivalent. If Authorino reports
`x509: certificate signed by unknown authority`, stop at that boundary,
preserve one sanitized debug trace, restore the previous Authorino log level,
and report functional routing as NOT DEMONSTRATED until the trust bundle is
configured reproducibly. Do not use `--insecure`, patch generated AuthPolicies,
or substitute a canned callback response.

### Reprovisioning notes from the latest run

Use this sequence when starting on another disposable OpenShift cluster. These
notes capture observed ordering requirements; they are not a substitute for
the commands in `preflight.sh`, `bootstrap.sh`, `provision.sh`, and `e2e.sh`.

1. Authenticate with `oc`, create the isolated kubeconfig under the harness
   state directory, and run `preflight.sh`. Capture the baseline before any
   mutation. Missing components that the bootstrap installs are expected;
   incompatible pre-existing components are blocking.

2. Run `bootstrap.sh` once. It must install exactly one pinned Istio control
   plane, exclude the Kuadrant `sail.yaml` Istio resource, and wait for Istio
   webhook endpoints, CA trust, and admission dry-run success. Do not apply
   the aggregate Kuadrant standard Kustomization because it creates a second
   `Istio/default` control plane.

3. Run `provision.sh` in its documented order. The Gateway address must be
   obtained from live Gateway status before generating the run-owned Gateway
   certificate. The certificate SAN must contain that observed hostname.

4. Before creating tenant fixtures, wait for the MaaS webhook Deployment,
   Service endpoints, CA bundle, serving certificate, and server-side
   AITenant dry-run. Do not treat a present Deployment or CRD as sufficient.

5. For internal-registry images, use the OpenShift registry CA when logging in
   from the host. The image-puller RoleBinding must grant
   `system:image-puller` to the exact `maas-system/maas-api` ServiceAccount and
   to the run-owned workload ServiceAccounts that pull private images. A
   ServiceAccount imagePullSecret alone is insufficient. Record image digests,
   never registry tokens.

6. Configure Authorino service-ca trust only through its supported volume
   configuration. Wait for the injected `service-ca.crt`, restart through the
   Authorino CR lifecycle, verify the mounted bundle fingerprint, and run the
   in-cluster TLS check with the MaaS Service DNS name. Require
   `ssl_verify_result=0`; never use `--insecure`.

7. The Katan fixture needs a writable `$HOME` under OpenShift restricted
   security. The test-only provider Deployment sets `HOME=/tmp` and mounts a
   run-owned `emptyDir` at `/tmp`. This is required for the published Katan
   image and does not alter production workloads.

8. The controller credential contract is provider-specific. The currently
   qualified combination is OpenAI + `openai-chat` + CRD `apikey`, which maps
   to the internal Praxis `bearer_token` strategy. The overlay must contain
   only the tenant-local Secret reference. Unsupported provider/API formats
   must fail closed; do not solve them by changing the provider fixture.

9. During Praxis opt-in, MaaS marks its shared ExtProc plugin ConfigMap with
   `opendatahub.io/managed=false`. The controller must wait for that explicit
   handoff, then publish and claim the complete ConfigMap including
   `extproc.yaml` and `pre-extproc.yaml`. Do not omit it, manually patch it, or
   adopt any other foreign object. Wait for both ExtProc Deployments to be
   Ready before sending requests.

10. Create a fresh ephemeral MaaS key only after all readiness gates pass.
    Keep its plaintext value in protected process memory or stdin to the
    persistent client pod. Suppress the creation response, record only key
    metadata, revoke the key after the request, and scan evidence before
    reporting a credential-clean run.

11. Run the functional suite from the Provider A baseline, then the narrative
    demo. Preserve each failed attempt in a separate evidence directory. The
    existing eight OpenShift assertions are infrastructure checks only; they
    must not be reported as complete routing qualification. Provider B,
    unknown-model, semantic no-op, last-known-good, and demo stages require
    their corresponding live operations before being marked `PASS`.

12. Leave the cluster retained only after resetting Provider A and verifying
    Praxis and ExtProc readiness. If cleanup is needed, delete the AITenant
    first and keep the controller and Gateway alive until finalization clears;
    never force-remove a finalizer.

Observed failure boundaries that must remain visible in future evidence:

- Registry publication can fail if the host-side registry client does not use
  the OpenShift registry CA.
- ExtProc can pull successfully but crash if the MaaS-owned handoff ConfigMap
  lacks `extproc.yaml`.
- A valid overlay without a credential reference causes the real provider to
  return HTTP 500 for missing `Authorization`.
- A request path missing the tenant and model segments is an expected
  authorization negative test, not a routing success.
- An API-key value exposed in terminal output invalidates the credential-clean
  claim even if saved evidence is later redacted and scans clean.

### Single-Istio ownership boundary

The pinned Kuadrant source groups `limitador.yaml`, `authorino.yaml`,
`sail.yaml`, and `kuadrant.yaml` in one standard Kustomization. This harness
installs and validates one pinned Istio 1.26.2 control plane in `istio-system`,
so it does not apply that aggregate Kustomization. It applies the three source
files independently, directly from the pinned checkout:

```text
config/install/configure/standard/authorino.yaml
config/install/configure/standard/limitador.yaml
config/install/configure/standard/kuadrant.yaml
```

`sail.yaml` is intentionally excluded because it creates
`sailoperator.io/Istio/default` and a second `gateway-system` control plane.
The Sail operator may remain installed when it is part of the pinned Kuadrant
installation bundle, but no Sail `Istio` custom resource is created in this
topology. The bootstrapper fails closed unless exactly one `istiod` Deployment
exists, it is `istio-system/istiod`, its Service has ready endpoints, its
validation webhooks target that Service with a matching CA bundle, its live
certificate verifies with the installed root and SNI, and server-side Istio
admission succeeds.

If an interrupted disposable run already created both control planes, first
save the Istio/Sail parent, owner, webhook, Service, endpoint, certificate, and
workload inventory. Delete only the evidence-proven harness-created
`istio.sailoperator.io/default` parent and wait for its normal Sail-owned
children and webhook entries to disappear; never delete child Deployments or
remove finalizers directly. Then rerun bootstrap with the evidence-gated
recovery variable documented by the run evidence. On a non-disposable cluster,
a foreign Sail `Istio` resource is a blocking conflict. Production operator
integration may choose Sail instead, but it must select one Istio owner and
version consistently.

## Convergence, diagnostics, and recovery

After provisioning, run `e2e.sh`; it uses bounded state-based waits and
45-second per-command subprocess limits for
controller status, Gateway programming, HTTPRoute `Accepted=True` and
`ResolvedRefs=True`, overlay revisions, and Praxis serving state. Transport
failures may be retried, but a received HTTP response is never retried.

Each assertion update is written through a temporary file and rename. If a
subprocess exits unexpectedly, the exit trap changes the active suite from
`RUNNING` to `FAIL` and records the last assertion boundary before returning.

Use `inspect.sh` for a compact inventory of images, pod UIDs/restarts, SCC,
service-ca metadata, routes, ownership, and overlay revisions:

```bash
./test/openshift-env/inspect.sh
oc --kubeconfig "$OPENSHIFT_KUBECONFIG" get pods -A
oc --kubeconfig "$OPENSHIFT_KUBECONFIG" get gateway,httproute,aitenant,externalmodel,externalprovider -A
```

These are diagnostic commands, not substitutes for the automated workflow. If
a wait fails, preserve the evidence path and inspect the first failing
boundary, its logs, and Events. Do not apply an undocumented live patch,
delete a foreign object, bypass TLS, or remove a finalizer. `destroy.sh` is the
supported cleanup operation: it deletes the run-owned AITenant first and keeps
the controller and Gateway available until its cleanup finalizer clears. A
timeout or failed deletion returns a partial cleanup result and leaves resources
for diagnosis; it never force-removes finalizers. Cleanup verifies that
run-owned namespaces are gone and returns nonzero if any owned resource or
deletion error remains.

If manual recovery is unavoidable, first record the exact observed ownership
and failure in evidence and turn the reviewed recovery into a harness change
before rerunning. Never recover by recreating shared operators, starting a
second MaaS reconciler, or deleting resources by common name alone.

## Source inputs and ownership

The scripts take repository paths from environment variables and contain no
machine-specific home directories. Use clean checkouts pinned to the revisions
required by the qualification:

```bash
export MAAS_CONTROLLER_REPO=/path/to/models-as-a-service
export PRAXIS_REPO=/path/to/praxis-ai
export PRAXIS_EXTPROC_REPO=/path/to/praxis-extproc
export KSERVE_REPO=/path/to/kserve
export KUADRANT_OPERATOR_REPO=/path/to/kuadrant-operator
```

The Kuadrant checkout must be exactly the `v1.4.2` tag. The bootstrap uses
the immutable catalog reference
`quay.io/kuadrant/kuadrant-operator-catalog@sha256:8734980493c3105716fdd2d6b7ddf21f27079cc3ba1c58621038abef2ce4dc8e`.
That catalog provides Kuadrant Operator v1.4.2 and Authorino Operator v0.23.1.
The Authorino Operator version and runtime version are separate: this catalog
resolves the runtime to Authorino `0.24.0`, currently observed on amd64 as
`sha256:96b1b9737cf5f546d132e45bd04513096c76a5655e151741306e886e598fc999`.
Bootstrap records and verifies both the Operator CSV and the runtime's
`authorino version` output and image ID. A stale deployment-script comment
that conflates these versions is not used as provenance.
The bootstrap rejects an untagged checkout or a floating catalog image. It
records the resolved pod image IDs and related-image references in evidence;
the source tag records provenance and the catalog digest selects the
executable bundle.

The standard Kuadrant configuration is applied as three source files
(`authorino.yaml`, `limitador.yaml`, and `kuadrant.yaml`). `sail.yaml` is
deliberately excluded because this harness already owns the single pinned
Istio 1.26.2 control plane in `istio-system`. The v1.4.2 catalog may install
the Sail operator as a dependency, but no `sailoperator.io/Istio/default`
resource is created. Bootstrap fails if a second Istio control plane or Sail
Istio resource appears.

Provisioning rejects dirty or staged dependency sources. It records source
revisions, Dockerfile hashes, image references, and image digests without
recording credentials. Standalone Praxis is tenant-local and has no Kubernetes
Secret API permission; ExtProc remains the Envoy request-processing component
and has no provider Secret permission.

## Inspection and evidence

Evidence is written below `$OPENSHIFT_E2E_STATE/evidence/<run-id>/` and excludes
kubeconfig contents, tokens, Secret data, and authorization headers. Use:

```bash
./test/openshift-env/inspect.sh
oc --kubeconfig "$OPENSHIFT_KUBECONFIG" get pods -A
oc --kubeconfig "$OPENSHIFT_KUBECONFIG" get gateway,httproute,aitenant,externalmodel,externalprovider -A
```

`destroy.sh` verifies run labels and ownership, waits for AITenant finalization,
and only then removes run-owned controller, Gateway, RBAC, provider, and tenant
resources. A timeout is a partial cleanup result; it never force-removes a
finalizer. If a shared operator component is missing or incompatible, retain
the baseline evidence and use a dedicated cluster rather than installing a
second unscoped controller.
## Current source-built qualification record

The latest disposable run was `20260914180432-12580`, using isolated state
under `.openshift-state/cold-20260914T180432Z`. It deployed the controller from
a locally source-built image, not the published GHCR image, because the cluster
kubelet rejected the public image pull with `unauthorized`. The deployed image
and digest are recorded in
`evidence/20260914180432-12580/provision/image-digests.txt`.

The executed workflow was:

```sh
./test/openshift-env/preflight.sh
./test/openshift-env/bootstrap.sh
./test/openshift-env/provision.sh
./test/openshift-env/e2e.sh
```

`provision.sh` creates one run-owned persistent curl client in the resolved
tenant namespace. It mounts only the public Gateway certificate. Supply an
opaque API key to `oc exec -i` over stdin; never put it in an argument, log, or
evidence file. Build the inference URL as:

```text
https://<Gateway-status-host>/<resolved-tenant-namespace>/demo/v1/chat/completions
```

Do not send `X-Gateway-Model-Name` from the caller: that is controller/ExtProc
state and caller override is intentionally rejected.

Live sanitized observations are in
`.openshift-state/cold-20260914T180432Z/evidence/20260914180432-12580/functional-live/`:

| Observation | Result |
|---|---|
| Gateway TLS without credentials | `401` |
| Real MaaS API-key creation | `201` |
| Authenticated Provider A request | `200` |
| Provider attribution | `provider-a-...:8000` |
| Unknown model | `404` |
| Key revocation | `200` |

This is source-built single-tenant functional evidence. Provider B switching,
semantic no-op, invalid-overlay last-known-good behavior, second-tenant
authorization, and credential rotation remain `NOT_DEMONSTRATED` until their
live operations are captured.

## Default-tenant cleanup

When `OPENSHIFT_E2E_AITENANT_NAME=models-as-a-service`, the tenant and its
namespace are shared MaaS state. Before applying Praxis opt-in,
`provision.sh` records the original AITenant labels, annotations, and spec in
`.openshift-state/<run>/aitenant-original.json`. `destroy.sh` verifies run
ownership, restores that metadata, and deletes only run-labeled fixture
resources; it never deletes the default AITenant or namespace. Dedicated
run-owned tenants use the AITenant-first finalization sequence, keeping the
controller and Gateway alive until finalizers clear.

Missing original metadata or ambiguous ownership causes cleanup to fail closed
with diagnostics. Never remove a finalizer or delete the shared namespace.
# Current qualification corrections

The standalone Praxis configuration uses each referenced `ExternalProvider.spec.endpoint` as its upstream host. It does not synthesize a Service name
in the tenant namespace. The endpoint must be a host or host:port; malformed URLs are rejected. OpenShift fixtures create both test endpoints and
both provider references before the baseline Praxis UID is recorded. Provider A is selected with weight 1 and Provider B is present with weight 0;
the switch changes weights only, so credential projections and static Praxis clusters do not change during the route test.

After an invalid-overlay failure-injection, the controller intentionally refuses to chain from a tampered envelope. The run-owned recovery is to
delete only the corrupted `routing-overlay` ConfigMap and let the next normal ExternalModel reconciliation recreate it; never patch status or use an
unowned resource. The qualification waits for two stable ConfigMap/mounted-file digest observations before each request and binds a new generation
only after the requested provider is observed.
