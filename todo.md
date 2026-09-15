# ai-gateway-controller E2E / Konflux — TODO

## TEMP — revert before merge (praxis-proxy/ai#699 CI pin)

E2E/prow/Tekton default `PRAXIS_EXTPROC_IMAGE` to `quay.io/maas/odh-praxis-extproc:pr699-76cb977`
(praxis-proxy/ai#699 `llmisvc_model_provider_resolver` @ `76cb977ad65235a9d965efc014c87bcad00e4960`).
**Undo before merging this PR:**

- [ ] `test/e2e/scripts/maas-image-defaults.sh` — remove PR699 default block; fall back to `odh-stable` / snapshot
- [ ] `test/e2e/scripts/prow_run_ai_gateway_controller_test.sh` — remove TEMP header comment
- [ ] `hack/tekton/pr-group-testing-pipeline.yaml` — restore `odh-praxis-extproc:${AIGC_TAG}` fallback (or odh-praxis-extproc-ci snapshot only)
- [ ] `test/e2e/scripts/run_e2e_tests.sh` — uncomment `test_external_models.py` when external-model reconciler is ready

Publish the PR699 praxis image to Quay (if not already present) before `/group-test`:

Published at `quay.io/maas/odh-praxis-extproc:pr699-76cb977` (maas org on [Quay](https://quay.io/organization/maas)).
Before merge to opendatahub: either promote to `quay.io/opendatahub/odh-praxis-extproc` or keep maas org pin until Konflux builds praxis with #699.

## Done in this repo

- [x] MaaS e2e sync + **kustomize-mode** deploy (prow owns MaaS; no ODH ModelsAsService path)
- [x] Sync includes `deployment/` manifests from MaaS (no local `maas-controller/` source tree)
- [x] Default MaaS images: `quay.io/opendatahub/maas-api:latest` and `maas-controller:latest` (main Konflux pushes)
- [x] PR image: `AI_GATEWAY_CONTROLLER_IMAGE` from Konflux snapshot → `deploy-ai-gateway-controller.sh`

## Done in odh-konflux-central fork

[jland-redhat/odh-konflux-central](https://github.com/jland-redhat/odh-konflux-central) `main` (synced with upstream):

- [x] `integration-tests/ai-gateway-controller/pr-group-testing-pipeline.yaml` (includes PR699 praxis pin + TODO; synced from `hack/tekton/`)
- [x] `gitops/integration-testing-prerequisites.yaml` — `ai-gateway-controller-group`

- [ ] Open PR to `opendatahub-io/odh-konflux-central` and merge (sync `hack/tekton/pr-group-testing-pipeline.yaml`: must-gather → `gather-openshift/`, PRAXIS maas Quay pin)
- [ ] After merge: point `.tekton/ai-gateway-controller-group-test.yaml` at upstream konflux-central
- [ ] Add `odh-praxis-extproc-ci` to group-components once Konflux builds praxis for PRs (until then pipeline uses `odh-praxis-extproc:${AIGC_TAG}`)

## Konflux cluster setup (manual)

- [ ] `ai-gateway-controller-group` component under `group-testing`
- [ ] `konflux-integration-runner` pull access to `quay.io/opendatahub/odh-ai-gateway-controller`
- [ ] PAC `git_auth_secret` in `open-data-hub-tenant`

## Image policy

| Component | Image source |
|-----------|----------------|
| `maas-api` | `quay.io/opendatahub/maas-api:latest` (MaaS `main` push) |
| `maas-controller` | `quay.io/opendatahub/maas-controller:latest` (MaaS `main` push) |
| `ai-gateway-controller` | PR snapshot digest from `odh-ai-gateway-controller-ci` |
| `praxis-extproc` | **TEMP:** `quay.io/maas/odh-praxis-extproc:pr699-76cb977` (praxis-proxy/ai#699); revert to `odh-praxis-extproc-ci` / `odh-stable` before merge |

Override MaaS tags with `MAAS_IMAGE_TAG` or explicit `MAAS_*_IMAGE` env vars.

## Known gaps

- **Deploy mode:** `DEPLOY_MODE=kustomize` (default) — matches MaaS CI (`install-odh.sh` comment: MaaS via kustomize, not operator). Avoids ModelsAsService prerequisite races (`maas-db-config` in wrong ns, Authorino TLS ordering, operator/image skew).
- Operator has no `RELATED_IMAGE_ODH_AI_GATEWAY_CONTROLLER_IMAGE` — CI installs controller via kustomize and scales down `payload-processing`
- After `sync-maas-e2e-tests.sh`, `patch-maas-deploy-for-aigc.sh` re-applies deploy.sh fixes
- Cert-manager/LWS: idempotent install skips duplicate OperatorGroups on clusters with prior partial installs
- User Workload Monitoring warning on showback is non-blocking for e2e

## Local run

```bash
./hack/scripts/sync-maas-e2e-tests.sh
AI_GATEWAY_CONTROLLER_IMAGE=quay.io/opendatahub/odh-ai-gateway-controller:odh-pr \
  ./test/e2e/scripts/prow_run_ai_gateway_controller_test.sh
```
