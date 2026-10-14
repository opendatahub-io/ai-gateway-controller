# ai-gateway-controller

Installs the `praxis-extproc` dataplane manifests. Deployed by
`ai-gateway-operator` as a sibling of `maas-controller`.

See [DESIGN.md](./DESIGN.md) for scope, architecture rationale, and what is
explicitly deferred. Read `DESIGN.md` before adding code.

See [CONTRIBUTING.md](./CONTRIBUTING.md) for the PR process, CI checks, and
development setup.

## Production image contract

The ODH operator and release packaging own the ExtProc dataplane image used by
an installed controller. They provide `praxis-extproc-image` through
`config/self/default/params.env`; the controller Deployment passes that value
to `--image`. There is no standalone `praxis-ai` hop: Gateway → ExtProc is the
dataplane, and ExternalModel HTTPRoutes backend to provider ExternalName
Services.

## Responses and Conversations

`AITenant.spec.responses.enabled` enables both APIs. When omitted or false,
Chat Completions uses no state storage.

Before enablement, install MaaS with the Responses AITenant schema and token-policy
exemptions for model-free state operations. Use an ExtProc image with Conversations,
certificate-only PostgreSQL, store bootstrap, `database_url_file` support and
outbound Responses body rewriting.
Install controller RBAC permitting Secret projection create/update/delete;
changing its image does not update packaged permissions.

After provisioning the database binding described below, set these fields on the
owning AITenant:

```yaml
spec:
  responses:
    enabled: true
    storage:
      mode: PlatformDefault
      deletionPolicy: Retain
```

Only `PlatformDefault` storage and `Retain` deletion are supported. Leave
`retention.maxAge` unset; expiry is not supported.

Provision shared bindings in `--infrastructure-namespace` (default
`INFRA_NAMESPACE` or `redhat-ai-gateway-infra`). `responses-db-config` supplies
`DB_CONNECTION_URL`, `tls.crt` and PKCS#8 `tls.key`; `responses-db-ca` supplies `ca.crt`.
The Responses database and credentials must be separate from MaaS API-key storage.
The controller reads credentials directly without caching Secret data and copies
only these keys into private `responses-runtime-<tenant>` Secrets. URLs never enter
ConfigMaps or status. Foreign runtime bindings are rejected. A same-image init container copies
the key into memory-backed storage as the assigned non-root UID with mode 0600.
Either platform Secret changing triggers enabled-tenant reconciliation and rollout.
Changing the resolved database endpoint/name requires an explicit cutover, not an
ordinary Secret update. PostgreSQL uses `verify-full` and certificate
authentication, not passwords. The platform provisions the database separately.
Each shared backend pool is capped at two connections with a ten-second acquire
timeout. Size the platform database for all tenant replicas and rollout overlap.

Standard `/v1/responses` and `/v1/conversations` routes require a dedicated tenant
Gateway and inherit its MaaS AuthPolicy. Post-auth ownership uses the tenant
UID, fixed realm `urn:maas:authorino` and Authorino's `X-MaaS-Username`.
The Gateway must reject client-supplied identity headers. Username/issuer changes
require an ownership migration review; this is not an immutable user subject.
Model-route hostname overrides are carried onto the new API route.

Readiness uses the named ExtProc gRPC health service, not a TCP connection or an
AITenant condition. Missing Secret keys or invalid/unavailable databases prevent
serving. Unsupported `retention.maxAge`, storage modes and deletion policies
withdraw routes and retire any old stateful workload.
Chat Completions shares that workload, so enabled stateful startup failures also
block that tenant's ExternalModel chat traffic. Responses-disabled tenants do not
configure state stores.

Disabling Responses removes API routes and storage configuration but retains the
private binding as a database-identity fence for re-enable. Tenant cleanup removes
the private binding, never the platform Secrets or shared database. Recreating the
tenant with a new UID cannot inherit retained objects. Shared credentials mean
runtime compromise can expose other tenants' data; runtime separation is not a
database security boundary.

## ExternalModel network isolation

By default, the controller installs a dedicated ExternalModel ExtProc
NetworkPolicy, retained until tenant cleanup to protect draining pods.
The shared policy does not select it. The dedicated policy allows only the
resolved Gateway on port 9004, monitoring on 9090, and DNS/PostgreSQL egress,
not other Gateways or Kubernetes API access. This applies even without Responses.
`--skip-network-policy` omits controller-managed NetworkPolicy installation;
use it only when the installation supplies equivalent networking.
Other additive policies can widen access.

## Kind integration environment

The [Kind environment](./test/kind-env/README.md) provides reproducible
functional integration and routing validation using pinned source revisions
and locally loaded images. It is isolated from production manifests and
records evidence for every qualification run.
