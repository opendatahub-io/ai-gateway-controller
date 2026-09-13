# Local two-plane environment

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

The executable qualification currently records 29 uniquely numbered entries.
In addition to the
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
manifests contain no Kind-only certificate, alias, or image-policy adaptation.
The run-owned Katan Services expose port 443 mapped to their plain HTTP 8000
listener solely so the test backend satisfies the controller's HTTPS-shaped
ExternalName target; the production ServiceEntry/DestinationRule contract is
unchanged.

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
