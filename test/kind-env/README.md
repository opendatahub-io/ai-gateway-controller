# Kind two-plane environment

This directory is the bounded entrypoint for the local Kind environment
described by ADR 0001. It creates and uses only the named Kind context,
records provenance and cluster state, and supports `--preflight`,
`--provision`, and `--destroy`.

MaaS tenant opt-in uses its current annotation contract:

```yaml
metadata:
  annotations:
    maas.opendatahub.io/payload-processing-type: praxis
```

An absent, empty, or different value remains on the existing IPP path. The
controller reads this annotation but does not write the AITenant or claim
MaaS-owned IPP resources. A typed AITenant selector would require a separate
approved API proposal.

The controller watches ExternalModel and ExternalProvider, publishes transport
resources before the content-addressed overlay, and the local manifests
provide two Katan backends plus standalone Praxis. Istio, Kuadrant, and the
MaaS platform remain explicit prerequisites for the full authenticated chain;
the provisioner records a failure rather than silently substituting them.

The executable qualification records numbered routing assertions plus
separate transition follow-up assertions. The `routing` mode counts only
functional routing assertions; `transition` records the separate transition
fixture. In addition to the
core transport, routing, hot-reload, digest, and last-known-good checks, it
proves semantic no-op stability after a real provider watch event and provider
status-gate loss/recovery. An
explicitly non-Ready provider is excluded from the next resolved route set;
the previously distributed overlay remains intact until a valid replacement
is published. A newly-created provider with an empty phase is admitted once
for bootstrap so its transport resources can establish Ready.

The local configuration now provisions two independently serving tenant
stacks. Tenant A is `models-as-a-service`; tenant B is
`ai-tenant-tenant-b`. Their Gateway, Praxis Service, overlay ConfigMap,
ExternalModel, ExternalProvider, and transport resources are distinct. Katan
backends remain in `maas-system`, and evidence records that backend namespace
separately. The extended run sends positive tenant-B traffic and verifies it
survives tenant-A mutation. A separate `ai-tenant-transition` fixture is
annotation absent and reserved for real MaaS IPP cutover/rollback qualification.

### Namespace boundaries

The production-shaped split is deliberate:

```text
client -> Gateway/Envoy + Kuadrant + ExtProc       (maas-system)
                              |
                              v
                 HTTPRoute parent reference
                              |
       tenant HTTPRoute -> standalone Praxis Service (tenant namespace)
                              |
                              v
                 provider Service/mesh transport    (tenant namespace)
                              |
                              v
                 external or fixture backend        (maas-system)
```

The HTTPRoute is created in the resolved tenant namespace and attaches to a
Gateway in `maas-system`. Gateway listeners explicitly allow routes from the
tenant namespaces. The route backend is the same-namespace Praxis Service, so
the route does not need a `ReferenceGrant`; no broad cross-namespace backend
grant is installed. If a future design sends a backendRef to another
namespace, that design must add a ReferenceGrant in the backend namespace
limited to this Gateway API Service reference.

Provider backend fixtures are separate from tenant state: they live in
`maas-system`, while the controller-created ExternalName Service, ServiceEntry,
DestinationRule, HTTPRoute, overlay, and projected Secret volume live in the
resolved tenant namespace. Kubernetes Secret projection is namespace-bound;
each Praxis ServiceAccount has token automount disabled and no Secret API
permission. The E2E captures route parent/backend namespaces, Gateway
`allowedRoutes`, ReferenceGrant inventory, projected Secret identities, and
`kubectl auth can-i` denial for Praxis cross-tenant Secret reads. Production
manifests contain no Kind-only certificate, callback identity, or image-policy
adaptation.

### Shared MaaS authorization callback

The Kind workflow does not install a callback proxy or response-faking adapter.
Authorino calls the `maas-api` Service directly, and that Service selects
exactly one Ready source-built MaaS API pod. The real MaaS API performs key
validation and subscription selection. The Kind-only certificate and CA
reproduce the trusted TLS relationship that OpenShift normally supplies; they
do not authorize requests, choose a tenant, rewrite callback bodies, or
manufacture responses.

The qualification attempts to record the real
`/internal/v1/api-keys/validate` and
`/internal/v1/subscriptions/select` access-log observations during an
authenticated Gateway request, plus selected tenant/subscription data when
the real API emits it and TLS fingerprints. Direct API key creation is only
fixture setup and is not counted as callback proof. No key or Authorization
value is written to evidence.

The current route-scoped Kind fixture observes API-key validation and the full
authorized Praxis request. The generated AuthPolicy is inspected before
classifying subscription-selection callback evidence: if the policy requires
`/internal/v1/subscriptions/select`, an absent callback fails the routing
qualification; if it does not, the behavior is recorded separately as
`NOT_DEMONSTRATED` and is excluded from the functional routing total. No
canned response or synthetic MaaS service is used.

This proves single-tenant MaaS authorization callback compatibility. It does
not prove tenant-aware dispatch for multiple tenant-qualified MaaS APIs; that
broader shared-URL behavior remains `NOT_DEMONSTRATED` and is a separate MaaS
design issue affecting both the existing IPP path and Praxis mode. Production
manifests contain none of these Kind-only certificate or callback adaptations.

The Kind certificate and CA fixture can be removed when the Kind deployment
provides the same trusted `maas-api` Service identity and CA relationship as
the target platform. The direct Service selector and one-Ready-pod assertion
should remain: they verify that the test reaches the real MaaS API rather than
an adapter.

For the Katan fixture, the provisioner records the public multi-architecture
digest
`ghcr.io/nerdalert/llm-katan@sha256:11379a1ec2fd69dc121eada6c544eb423a7c074414507dc1d474f4abba9df75a`
and pulls its `linux/amd64` child digest
`sha256:a8bf18109e2db641ef4a63efe65f69d4d6554f1128a174053de89a8b81b4284d`
before loading that platform image under a local Kind name. The local name is
a container-runtime transport reference only; the published digest and source
commit are recorded in provisioning evidence, and the fixture is still the
published Katan image rather than a mock service.
`KATAN_IMAGE` overrides the image and `BUILD_KATAN=true` is the explicit local
source-build path.

### Kind-only security compatibility

The production Praxis workload leaves pod UID, GID, and fsGroup unset so an
OpenShift restricted SCC can assign the namespace-safe identity. Kind does not
perform that admission mutation and rejects the image's named non-root user
when `runAsNonRoot` is set. After the controller creates each tenant Praxis
Deployment, the Kind provisioner applies the fixture-only numeric identity
`65532` and records that transformation in the provision evidence. This
transform is not in production manifests and is not used by the OpenShift
workflow.
The run-owned Katan Services expose port 443 mapped to their plain HTTP 8000
listener solely so the test backend satisfies the controller's HTTPS-shaped
ExternalName target; the production ServiceEntry/DestinationRule contract is
unchanged.

The Katan backends are credential-enforcing fixtures, not permissive traffic
sinks. Praxis providers use the run-only `kind-only-dummy` value and the
annotation-absent IPP transition fixture uses its separate
`transition-provider-key` value through the dedicated `katan-transition`
Deployment. These values are qualification fixtures only and are never
production credentials. The E2E first proves direct requests without a
credential or with the wrong credential receive HTTP 401. The authenticated
Gateway request carries the MaaS API key in `Authorization`, while the backend
accepts only the distinct projected provider credential; its attributed HTTP
200 proves Praxis replaced the caller credential. A separate request proves a
client-supplied `x-api-key` cannot replace it either. Duplicate `Authorization`
header ordering is outside this claim. The expected provider credential is kept
out of logs and evidence.

Run the static fixture regression check with:

```console
./test/kind-env/test-provider-credentials.sh
```

Credential rotation is implemented in Praxis AI's existing `credential_inject`
filter: projected Secret files are revalidated by an event-driven watcher and
swapped as complete `ArcSwap` snapshots, with invalid replacements failing
closed. The controller renders the tenant-scoped standalone Praxis Deployment,
its reference-only filter configuration, and a deduplicated projected provider
Secret volume. The generated ServiceAccount has token automount disabled.
The controller-owned ExtProc Deployment remains a separate Envoy processing
component. Existing Secret content rotation does not alter the pod template;
changing the referenced Secret set intentionally changes the template and may
roll out the tenant Praxis pod.

The transition fixture uses a real annotation-absent AITenant and MaaS model
resources. It reached real IPP ownership in the fresh run, but the existing IPP
request/cutover path was blocked by a stale IPP ExternalModel-owned HTTPRoute.
The narrowly scoped MaaS change now deletes that route only after the IPP writer
stops and only when its labels and exact owner UID prove ownership; ambiguous,
user-owned, or controller-owned routes remain untouched. The current annotation
contract and controller-owned cleanup are
preserved and must not be replaced by a typed AITenant field.

The readiness assertions are intentionally limited: this controller currently
uses the persisted ExternalProvider phase as a reconciliation gate, not as an
independent endpoint-health observation. The runtime test therefore proves
last-known-good serving and recovery around an explicit status transition. A
separate provider-health producer and its contract remain unimplemented.

The corrected qualification uses the run-owned CA with `curl --cacert`; TLS
verification is enabled and no HTTP downgrade or insecure flag is used. Evidence
records only CA/certificate fingerprints and paths, never keys or credentials.

Required source checkouts are supplied through environment variables:

* `LLM_KATAN_REPO`
* `PRAXIS_REPO`
* `MAAS_CONTROLLER_REPO`
* `KUADRANT_OPERATOR_REPO`
* `PRAXIS_EXTPROC_REPO`

No kubeconfig context is read implicitly. Provisioning uses only
`kind-${LOCAL_ENV_CLUSTER}`. Images are built from these checkouts when absent
and loaded into Kind with `imagePullPolicy: Never`.

## Mock external provider

The E2E currently builds its mock provider from
[`yossiovadia/llm-katan`](https://github.com/yossiovadia/llm-katan) at commit
`a5a47568ac6daf1d4bd8b356e7b350cce9ceca2a`. LLM-Katan is used only to provide
deterministic provider-compatible responses for routing, hot-swap, and
last-known-good assertions. It is not deployed as part of the production
architecture. It may eventually be replaced by a smaller purpose-built mock
that satisfies the same E2E contract.
