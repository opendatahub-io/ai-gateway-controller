#!/usr/bin/env bash
# ai-gateway-controller-only e2e artifact hooks (not in upstream MaaS).
set -euo pipefail

collect_must_gather() {
  local dest="${1:-$ARTIFACTS_DIR/gather-openshift}"
  local logfile="${2:-$ARTIFACTS_DIR/must-gather.log}"
  mkdir -p "$(dirname "$logfile")" "$dest"
  if ! command -v oc >/dev/null 2>&1; then
    echo "  Skipping must-gather (oc not found)" >>"$logfile"
    return 0
  fi
  echo "Collecting OpenShift must-gather to $dest (log: $logfile) ..."
  {
    echo "=== must-gather started at $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
    oc adm must-gather --dest-dir "$dest"
    echo "=== must-gather finished at $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
  } >>"$logfile" 2>&1 || true
  echo "  must-gather complete (see $logfile)"
}

collect_aigc_e2e_artifacts() {
  if [[ "$(type -t collect_e2e_artifacts 2>/dev/null)" != "function" ]]; then
    echo "WARN: collect_e2e_artifacts not loaded from MaaS auth_utils" >&2
    return 0
  fi
  collect_e2e_artifacts
  if [[ "${E2E_COLLECT_MUST_GATHER:-false}" == "true" ]]; then
    collect_must_gather "$ARTIFACTS_DIR/gather-openshift" "$ARTIFACTS_DIR/must-gather.log"
  fi
}
