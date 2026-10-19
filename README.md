# ai-gateway-controller

Installs the `praxis-extproc` dataplane manifests and reconciles
`ExternalModel` / `ExternalProvider`. Deployed by `ai-gateway-operator` as a
sibling of `maas-controller`.

## Documentation

| Doc | Audience |
| --- | --- |
| [Architecture](./docs/architecture.md) | Deployment topology and who owns what |
| [Dataplane selection](./docs/dataplane-selection.md) | Praxis vs IPP per tenant; handoff states |
| [External models](./docs/external-models.md) | When and how ExternalModels are served |
| [Praxis dataplane evolution](./docs/praxis-dataplane-evolution.md) | Request-path map (Envoy filters → Praxis) |
| [DESIGN.md](./DESIGN.md) | Implementer detail — read before adding code |
| [CONTRIBUTING.md](./CONTRIBUTING.md) | PR process, CI checks, development setup |

## Production image contract

The ODH operator and release packaging own the ExtProc dataplane image used by
an installed controller. They provide `praxis-extproc-image` through
`config/self/default/params.env`; the controller Deployment passes that value
to `--image`. There is no standalone `praxis-ai` hop: Gateway → ExtProc is the
dataplane, and ExternalModel HTTPRoutes backend to provider ExternalName
Services.

## Kind integration environment

The [Kind environment](./test/kind-env/README.md) provides reproducible
functional integration and routing validation using pinned source revisions
and locally loaded images. It is isolated from production manifests and
records evidence for every qualification run.
