#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
stage() {
  local number=$1 purpose=$2 action=$3
  printf '\n==============================================================================\n'
  printf 'Stage %s\n\nPURPOSE\n%s\n\nACTION\n%s\n' "$number" "$purpose" "$action"
}
printf '%s\n' 'OpenShift External Models -> Praxis demonstration'
printf '%s\n' 'User story: a platform engineer changes the selected test endpoint while the tenant data plane stays running.'
printf '%s\n' 'Request path: client -> Gateway/Envoy -> Authorino -> ExtProc -> Praxis -> credential-enforcing test endpoint.'
stage 1 'Run the live, machine-readable qualification.' 'Use the controller-owned assertions and preserve their evidence.'
if "$ROOT/test/openshift-env/e2e.sh"; then
  printf '%s\n' 'OBSERVED: qualification completed with a final result.'
  printf '%s\n' 'EXPECTED: every implemented OpenShift assertion is backed by a live observation.'
  printf '%s\n' 'RESULT: PASS'
else
  printf '%s\n' 'OBSERVED: qualification did not complete successfully.' >&2
  printf '%s\n' 'EXPECTED: inspect the unique run evidence and first failed assertion.' >&2
  printf '%s\n' 'RESULT: FAIL' >&2
  exit 1
fi
stage 2 'State the qualification boundary.' 'Review the generated result and evidence path; no narrative logic is duplicated here.'
printf '%s\n' 'OBSERVED: Test Provider A/B switching, overlay convergence, and workload identity are reported by e2e.sh.'
printf '%s\n' 'EXPECTED: no automatic failover or commercial-provider behavior is implied.'
printf '%s\n' 'RESULT: PASS'
printf '\nScope table\n%-42s %s\n%-42s %s\n%-42s %s\n' \
  'Credential rotation' 'NOT DEMONSTRATED' \
  'Two-tenant MaaS authorization' 'NOT DEMONSTRATED (issue #23)' \
  'IPP transition and rollback' 'FOLLOW-UP'
