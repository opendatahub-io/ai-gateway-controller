# OpenAI-to-Anthropic translation runtime upgrade

The `openai_chat_completions_to_anthropic` filter now treats its installation
as authorization to translate a selected Anthropic provider. AIGC continues to
enforce `MaasTenantConfig.spec.apiTranslation.enabled` by conditionally
installing or removing both OpenAI-to-Anthropic filters and the provider
translation profile. The permission field on `MaasTenantConfig` is not being
removed.

## Draft release dependency

**Pending compatible published ExtProc image: `<EXTProc_IMAGE_DIGEST>`.**
The packaged image is unchanged in this draft. Replace it with the verified
compatible artifact and qualify that image before merging or deploying this change.

## Image/config compatibility

The old and new filter implementations have no common enabled-translator
configuration:

| Praxis AI runtime | Translator filter configuration | Result |
| --- | --- | --- |
| Old | `api_translation_enabled: true` | Accepted; selected Anthropic requests translate. |
| Old | Field omitted, filter installed | Accepted; the old default is false, so selected Anthropic requests fail closed. |
| New | Field omitted, filter installed | Accepted; selected Anthropic requests translate. |
| New | Old field present | Rejected because the new config denies unknown fields. |

Therefore neither the ExtProc image nor the generated config can be rolled out
independently while translation filters are installed. AIGC currently updates
the mutable plugins ConfigMap and the ExtProc Deployment as separate API
applies. A config hash on the Pod template triggers a rollout, but does not
make the ConfigMap and Deployment update atomic; existing Pods can observe a
new projected ConfigMap before replacement Pods are ready.

## Coordinated upgrade

Use a maintenance window for OpenAI Chat Completions requests to Anthropic
providers. Do not claim zero downtime for translated traffic.

1. On the old AIGC and ExtProc pair, set
   `MaasTenantConfig.spec.apiTranslation.enabled=false` for every affected
   tenant. Wait until the shared and tenant-local ExtProc configurations no
   longer contain either translation filter, all current ExtProc Pods are
   Ready, and the corresponding Pod template/config revisions have converged.
2. Roll out the ExtProc image containing the compatible Praxis AI change to
   every shared and tenant-local ExtProc workload. Keep translation disabled.
   Wait for all old Pods to terminate and all new Pods to be Ready with the
   expected image IDs and accepted config.
3. Deploy the AIGC version that omits `api_translation_enabled` from generated
   filter config. Keep tenant translation permission disabled and confirm the
   filter remains absent after reconciliation.
4. Re-enable translation permission tenant by tenant. Confirm both translation
   filters and the selected-provider profile are installed, the generated
   config is accepted by the new runtime, and authenticated Anthropic JSON/SSE
   requests succeed before proceeding to the next tenant.

Until a compatible ExtProc image is published, use only an explicit local
image override in isolated qualification. Do not replace the packaged image
reference with a guessed tag or digest.

## Rollback

1. Disable translation permission and wait for the new AIGC controller to
   remove both translator filters from every affected ExtProc config.
2. Restore the old AIGC configuration generator and old ExtProc image while
   permission remains disabled. Either may be rolled back first only while
   the translation filters remain absent.
3. Wait until all ExtProc Pods run the old image and accept their current
   configs. Then re-enable translation permission so old AIGC emits the old
   `api_translation_enabled: true` field.

If the maintenance window is interrupted, keep translation disabled until the
entire serving fleet is on one compatible image/config pair. Do not restore an
old config while any new-AI Pod remains, or enable a field-free translator
config while any old-AI Pod remains.

The final release/image pin must be updated only after the compatible ExtProc
artifact is published and its immutable digest and source provenance are
verified. Kind source-image qualification is not a substitute for that
release-image check.
