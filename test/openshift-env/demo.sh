#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
printf '%s\n' 'OpenShift External Models -> Praxis demonstration'
printf '%s\n' 'User story: change the selected test endpoint while keeping the tenant data plane running.'
printf '%s\n' 'Request path: client -> Gateway -> Authorino -> ExtProc -> Praxis -> test endpoint.'
printf '%s\n' 'Running the executable qualification; raw evidence remains under the run evidence directory.'
if "$ROOT/test/openshift-env/e2e.sh"; then
  printf '%s\n' 'RESULT: PASS'
else
  printf '%s\n' 'RESULT: PARTIAL or FAIL; inspect results.json for the first failed boundary.' >&2
  exit 1
fi
printf '%s\n' 'Credential rotation: NOT DEMONSTRATED'
printf '%s\n' 'Two-tenant MaaS authorization: NOT DEMONSTRATED (issue #23)'
