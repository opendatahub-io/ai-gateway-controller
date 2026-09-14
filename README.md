# ai-gateway-controller

Installs the `praxis-extproc` dataplane manifests. Deployed by
`ai-gateway-operator` as a sibling of `maas-controller`.

See [DESIGN.md](./DESIGN.md) for scope, architecture rationale, and what is
explicitly deferred. Read `DESIGN.md` before adding code.

See [CONTRIBUTING.md](./CONTRIBUTING.md) for the PR process, CI checks, and
development setup.

## Kind integration environment

The [Kind environment](./test/kind-env/README.md) provides reproducible
functional integration and routing validation using pinned source revisions
and locally loaded images. It is isolated from production manifests and
records evidence for every qualification run.
