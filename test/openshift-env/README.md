# OpenShift External Model and Praxis validation

This directory provides a controller-owned OpenShift test environment for the
External Model to Praxis integration. It installs the required test stack,
deploys source-matched components, exercises the real Gateway request path, and
records evidence without changing production manifests.

The intended request path is:

```text
client -> OpenShift load balancer -> Gateway/Envoy -> Kuadrant/Authorino
       -> Praxis ExtProc -> tenant-local Praxis -> external provider fixture
```

The scripts are for disposable OpenShift qualification environments. They do
not install an OpenShift cluster and must not be used to replace components in
a shared or production cluster.

## What the suite validates

The executable suite validates the following behavior with real requests and
state-based checks:

- controller, tenant, ExternalProvider, ExternalModel, HTTPRoute, ExtProc, and
  Praxis readiness;
- verified TLS at the public Gateway;
- unauthenticated requests are rejected;
- MaaS API-key creation, use, and revocation;
- authenticated routing to two credential-enforcing provider fixtures;
- provider selection changes without restarting Praxis;
- unknown-model handling;
- semantic configuration no-op behavior;
- invalid-overlay last-known-good behavior;
- declared, recomputed, and mounted overlay convergence;
- tenant Praxis ServiceAccount denial of Secret API access;
- credential-pattern scans over functional evidence; and
- reset to the first provider after validation.

The current qualification is single-tenant. Multi-tenant MaaS authorization
dispatch, credential rotation, IPP-to-Praxis transition and rollback, and
additional provider authentication strategies require separate qualification.

## Resource ownership

The harness distinguishes resources it creates from shared platform resources.

Run-owned resources include the test Gateway, provider fixtures, controller
deployment, temporary RBAC, registry project, test certificates, compatibility
resources, and evidence. These resources carry the run identifier and may be
removed only after their ownership is verified.

Shared resources include platform CRDs, the default MaaS tenant, platform
namespaces, Istio, Kuadrant, Authorino, Limitador, KServe, and other installed
operators. The harness may configure a documented shared resource when required
for qualification, but it must first record its identity and original state.
Cleanup restores that state instead of deleting the shared resource.

The cleanup scripts never remove finalizers to force deletion.

## Prerequisites

Required tools:

- `oc`
- `kubectl`
- `docker`
- `skopeo`
- `helm`
- `kustomize`
- `openssl`
- `jq`
- `yq`
- `curl`
- `tar`
- `sha256sum`
- `go`
- `make`
- `git`
- `bash`

The current `oc` session must be authenticated as a cluster administrator on a
disposable OpenShift cluster. The cluster must have working default storage,
an integrated image registry, and sufficient capacity for the platform and
test workloads.

The scripts expect sibling source checkouts for:

- ai-gateway-controller;
- models-as-a-service;
- Praxis AI;
- Praxis ExtProc;
- KServe;
- Kuadrant Operator; and
- LLM-Katan source provenance.

Use immutable source revisions and image digests. Do not qualify floating image
tags or dirty source without recording the source and dirty-content hashes in
the generated image evidence.

## Isolated credentials and state

Create an isolated kubeconfig from the active session:

```sh
cd test/openshift-env
export OPENSHIFT_E2E_STATE="$PWD/.state/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$OPENSHIFT_E2E_STATE"
oc config view --raw --minify > "$OPENSHIFT_E2E_STATE/kubeconfig"
chmod 600 "$OPENSHIFT_E2E_STATE/kubeconfig"
export OPENSHIFT_KUBECONFIG="$OPENSHIFT_E2E_STATE/kubeconfig"
```

The state directory is ignored by Git. Do not place login commands, bearer
tokens, API keys, or kubeconfig contents in the README, shell history, evidence,
or command-line arguments.

The functional suite creates an ephemeral MaaS API key through an in-cluster
client, supplies it over standard input, and revokes it on normal exit, failure,
or interruption. Secret values and Authorization headers must not be printed or
stored as evidence.

## Required inputs

Set source roots to pinned checkouts:

```sh
export PRAXIS_REPO=/path/to/praxis-ai
export PRAXIS_EXTPROC_REPO=/path/to/praxis-extproc
export MAAS_CONTROLLER_REPO=/path/to/models-as-a-service
export KSERVE_REPO=/path/to/kserve
export KUADRANT_OPERATOR_REPO=/path/to/kuadrant-operator
```

The Praxis ExtProc and MaaS checkouts must be clean. The controller and Praxis
worktree hashes are recorded when local changes are intentionally qualified.

The supported controller image override must be an immutable digest:

```sh
export OPENSHIFT_E2E_CONTROLLER_IMAGE='registry.example/controller@sha256:<digest>'
export KATAN_IMAGE='ghcr.io/nerdalert/llm-katan@sha256:11379a1ec2fd69dc121eada6c544eb423a7c074414507dc1d474f4abba9df75a'
```

When the controller override is omitted, provisioning builds it from the
current checkout. Praxis, ExtProc, MaaS API, and MaaS controller are built from
their configured source checkouts. Provisioning publishes these images to a
run-owned registry project, resolves their pushed digests, and deploys those
digests.

## MaaS compatibility proof

OpenShift qualification requires a MaaS controller that implements the Praxis
opt-in contract. `preflight.sh` requires immutable proof for:

- the `maas.opendatahub.io/payload-processing-type: praxis` selection;
- skipping tenant IPP resources for a Praxis-selected tenant;
- ownership-safe release and cleanup of IPP resources; and
- a source or image identity matching the MaaS implementation under test.

Do not bypass this gate by installing a second MaaS controller or replacing a
managed MaaS deployment without an explicit, reversible test plan.

## End-to-end workflow

Run the scripts from this directory:

```sh
./preflight.sh
./bootstrap.sh
./provision.sh
./e2e.sh
./demo.sh
./inspect.sh
./destroy.sh
```

`preflight.sh` checks tools, authentication, source inputs, immutable image
references, cluster access, and MaaS compatibility before creating tenant test
resources.

`bootstrap.sh` installs or verifies the platform dependencies required by the
test. It enforces one Istio control plane, excludes Sail-managed duplicate
Istio installation, verifies webhook trust and live certificates, and records
operator and operand provenance.

`provision.sh` builds or resolves images, configures registry access, applies
controller CRDs before fixtures, deploys the source-matched MaaS and controller
components, prepares the default tenant for Praxis, and creates the provider and
authorization fixtures.

`e2e.sh` performs the machine-readable qualification. It uses bounded probes,
does not retry received HTTP responses, and atomically records failures,
including the active assertion when interrupted.

`demo.sh` presents the same validated behavior in a narrative format. It does
not replace the executable qualification and must not convert an unproven stage
into a pass.

`inspect.sh` records the deployed state and relevant status without collecting
credential values.

`destroy.sh` verifies ownership, revokes remaining test credentials, restores
shared state, deletes run-owned resources, and returns nonzero when cleanup is
incomplete.

## Installation order

The automated install order is intentional:

1. Validate the isolated kubeconfig, source revisions, and compatibility proof.
2. Install or verify Gateway API and certificate management.
3. Install exactly one supported Istio control plane and verify admission TLS.
4. Install the pinned Kuadrant catalog without a Sail-created Istio instance.
5. Verify Kuadrant, Authorino, and Limitador operator and runtime provenance.
6. Install or verify KServe and the MaaS CRDs and RBAC.
7. Build and publish source-matched MaaS, controller, Praxis, and ExtProc images.
8. Grant exact ServiceAccounts access to the run-owned registry project.
9. Deploy images by resolved digest and wait for rollouts.
10. Apply controller-owned CRDs before creating ExternalProvider or
    ExternalModel fixtures.
11. Reuse the source-created default MaaS tenant for single-tenant validation.
12. Apply the Praxis opt-in and wait for the resolved tenant namespace.
13. Create provider, subscription, policy, Gateway, and TLS fixtures.
14. Wait for policies, routes, overlays, mounts, and workloads to converge.
15. Run functional qualification and the narrative demo.
16. Reset routing, inspect evidence, and perform ownership-checked cleanup.

## Default MaaS API service

The source-matched MaaS installation creates the default tenant and canonical
`maas-system/maas-api` Service. Single-tenant qualification uses that native
Service when its selector, ready endpoints, and service-ca certificate are
valid.

A run-owned TLS compatibility proxy is permitted only when a non-default
tenant-qualified MaaS API lacks the shared callback address expected by the
generated policy. The proxy may provide TLS name compatibility only. It must
not manufacture authorization responses, disable verification, redirect calls
between tenants, or conceal a failed MaaS callback.

This adapter does not prove multi-tenant callback dispatch. That remains a
separate MaaS integration concern shared by IPP and Praxis paths.

## Registry authorization

Attaching a pull Secret to a ServiceAccount does not by itself grant access to
an OpenShift image stream. Provisioning creates a run-specific pull Secret and
an exact `system:image-puller` RoleBinding in the image project for each
ServiceAccount that needs an image.

Before accepting a rollout, the harness records:

- the RoleBinding subject;
- the ServiceAccount identity;
- the original and test `imagePullSecrets`; and
- the running pod image ID.

Cleanup restores the original ServiceAccount configuration and removes only the
run-labeled Secret and RoleBinding after verifying their ownership.

## Provider fixture contract

The provider fixtures run the pinned LLM-Katan image as credential-enforcing,
OpenAI-compatible test endpoints. They use a writable `/tmp` home directory to
work under OpenShift's restricted security policy.

Provider A and Provider B exist to prove a routing change, not load balancing
or latency behavior. Both providers are declared before the baseline request.
The ExternalModel initially selects Provider A, then changes to Provider B. The
suite verifies overlay convergence, Provider B attribution, and an unchanged
Praxis pod identity and restart count.

The qualified credential mapping is deliberately narrow:

```text
provider type: openai
API format:    openai-chat
CRD auth type: apikey
Praxis action: inject the projected token as a bearer credential
```

The overlay contains only a Secret reference. Secret bytes reach Praxis through
a tenant-local projected volume. Unsupported provider/API combinations, SigV4,
OAuth2, and unknown authentication types fail closed.

## Overlay convergence

Before sending a request after a provider change, the suite requires two stable
observations of all of the following:

- the expected provider in the controller ConfigMap;
- the expected overlay generation and declared digest;
- semantic digest recomputation using the controller implementation;
- the same digest and provider in the file mounted by Praxis; and
- stable Praxis pod identity and restart count.

This prevents a request from racing ahead of the projected ConfigMap update.
Raw file hashes are not equivalent to the controller's semantic digest and must
not be substituted.

## Evidence and result rules

Each run writes to a unique evidence directory under `OPENSHIFT_E2E_STATE`.
Evidence should include:

- source revisions and dirty-content hashes;
- built and deployed image digests;
- operator, operand, CRD, and webhook provenance;
- readiness and route conditions;
- sanitized HTTP status and provider attribution;
- overlay generations and digests;
- Praxis UID and restart counts;
- cleanup results; and
- a machine-readable assertion result.

An assertion may pass only from an observed result. A received HTTP response is
never retried. Transport status `000` may be retried only within a bounded
Gateway readiness probe. Missing functionality must be recorded as
`NOT_DEMONSTRATED` or a failure, never inferred from another assertion.

The exit and signal traps convert an unfinished `RUNNING` result into `FAIL`,
preserve the active assertion, revoke any active key, and leave existing failed
evidence intact.

## Troubleshooting

Start with the first failed assertion and its evidence. Do not patch generated
routes, overlays, policies, or authorization responses to make a run pass.

Common checks:

```sh
oc --kubeconfig "$OPENSHIFT_KUBECONFIG" get nodes
oc --kubeconfig "$OPENSHIFT_KUBECONFIG" get pods -A
oc --kubeconfig "$OPENSHIFT_KUBECONFIG" get gateway,httproute -A
oc --kubeconfig "$OPENSHIFT_KUBECONFIG" get authpolicy -A
oc --kubeconfig "$OPENSHIFT_KUBECONFIG" get deployment,service,endpointslice -A
```

For image-pull failures, verify the exact ServiceAccount, RoleBinding subject,
registry project, pull Secret, and deployed digest.

For admission failures, verify there is exactly one Istio control plane and
that the webhook Service, endpoints, CA bundle, live certificate, and SNI agree.

For authorization failures, distinguish the stages:

1. public Gateway TLS;
2. API-key validation callback;
3. subscription or policy evaluation;
4. ExtProc processing;
5. Praxis provider selection and credential injection; and
6. provider response.

Record sanitized status and identity metadata only. Do not print the API key or
Authorization header while diagnosing a request.

For overlay failures, compare the controller ConfigMap, declared semantic
digest, recomputed digest, mounted Praxis file, expected provider, pod UID, and
restart count. Wait for stable convergence rather than adding request retries.

## Static validation

Run the controller and harness checks before publishing changes:

```sh
go test ./...
go vet ./...
make lint

for file in test/openshift-env/*.sh; do
  bash -n "$file"
done

shellcheck test/openshift-env/*.sh
./test/openshift-env/test-bootstrap-single-istio.sh
./test/openshift-env/test-destroy-order.sh
git diff --check
```

Static checks do not replace a cold OpenShift run. Changes to provisioning,
shared-state restoration, credentials, or cleanup require a fresh deployment,
functional qualification, demo, and cleanup verification on a disposable
cluster.
