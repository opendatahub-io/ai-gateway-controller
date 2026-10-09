#!/usr/bin/env bash
# Post-fetch patches for MaaS e2e tests under ai-gateway-controller CI.
#
# Goals (group-test flake mitigation; not product fixes):
#   1. Keep EnvoyFilter rename ≤ 63 chars (tenant ID ≤ 21 with current base name).
#   2. After the serial maasApi override test, pin shared maas-api back to 1 replica
#      so parallel AITenant bootstraps are not stuck on updated replicas 1/2.
#   3. Give bootstrap_aitenant_tenant a longer Ready wait via env.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${PROJECT_ROOT:-$(cd "${SCRIPT_DIR}/../../.." && pwd)}"
MAAS_CHECKOUT_ROOT="${MAAS_CHECKOUT_ROOT:-${PROJECT_ROOT}/test/maas-e2e}"
MAAS_E2E_DIR="${MAAS_E2E_DIR:-${MAAS_CHECKOUT_ROOT}/test/e2e}"
TESTS_DIR="${MAAS_E2E_DIR}/tests"

if [[ ! -d "${TESTS_DIR}" ]]; then
  echo "ERROR: ${TESTS_DIR} not found — run fetch-maas-e2e.sh first" >&2
  exit 1
fi

HELPERS="${TESTS_DIR}/multitenancy_helpers.py"
CONFTEST="${TESTS_DIR}/conftest.py"
TEST_TENANT="${TESTS_DIR}/test_tenant.py"

NAME_CAP_MARKER="ai-gateway-controller: cap tenant ID for EnvoyFilter 63-char limit"
SHARED_PREFIX_MARKER="ai-gateway-controller: shorten shared tenant prefixes"
REPLICAS_PIN_MARKER="ai-gateway-controller: pin maas-api replicas to 1 after override test"
READY_TIMEOUT_MARKER="ai-gateway-controller: E2E_AITENANT_READY_TIMEOUT for bootstrap"

if [[ -f "${HELPERS}" ]] && ! grep -qF "${NAME_CAP_MARKER}" "${HELPERS}"; then
  python3 - <<'PY' "${HELPERS}" "${NAME_CAP_MARKER}"
import sys
path, marker = sys.argv[1], sys.argv[2]
text = open(path).read()
old = '''def new_named_tenant_case(prefix: str) -> dict[str, str]:
    """Create a stable-ish tenant case with a caller-provided DNS-safe prefix."""
    suffix = uuid.uuid4().hex[:6]
    tenant_name = f"{prefix}-{suffix}"
    return {'''
new = f'''def new_named_tenant_case(prefix: str) -> dict[str, str]:
    """Create a stable-ish tenant case with a caller-provided DNS-safe prefix."""
    suffix = uuid.uuid4().hex[:6]
    tenant_name = f"{{prefix}}-{{suffix}}"
    # {marker}
    # payload-processing-external-model-filters (41) + "-" + tenantID must be ≤ 63.
    max_tenant_id = 21
    if len(tenant_name) > max_tenant_id:
        keep_prefix = max_tenant_id - 1 - len(suffix)
        if keep_prefix < 1:
            tenant_name = tenant_name[:max_tenant_id]
        else:
            tenant_name = f"{{prefix[:keep_prefix]}}-{{suffix}}"
    return {{'''
if old not in text:
    print(f"WARN: {path}: new_named_tenant_case block not found; skip name cap", file=sys.stderr)
    sys.exit(0)
open(path, "w").write(text.replace(old, new, 1))
print(f"Patched {path} (tenant ID length cap)")
PY
fi

if [[ -f "${HELPERS}" ]] && ! grep -qF "${READY_TIMEOUT_MARKER}" "${HELPERS}"; then
  python3 - <<'PY' "${HELPERS}" "${READY_TIMEOUT_MARKER}"
import sys
path, marker = sys.argv[1], sys.argv[2]
text = open(path).read()
old = '''    apply_aitenant(case)
    wait_for_json(AITENANT_KIND, case["tenant_label_name"], AITENANT_NAMESPACE, predicate=aitenant_ready)
    wait_for_json(
        "maastenantconfig",
        TENANT_CR_NAME,
        case["tenant_ns"],
        predicate=bridge_tenant_owned_by_aitenant(case),
    )'''
new = f'''    apply_aitenant(case)
    # {marker}
    _aitenant_ready_timeout = int(os.environ.get("E2E_AITENANT_READY_TIMEOUT", "360"))
    wait_for_json(
        AITENANT_KIND,
        case["tenant_label_name"],
        AITENANT_NAMESPACE,
        predicate=aitenant_ready,
        timeout=_aitenant_ready_timeout,
    )
    wait_for_json(
        "maastenantconfig",
        TENANT_CR_NAME,
        case["tenant_ns"],
        predicate=bridge_tenant_owned_by_aitenant(case),
        timeout=_aitenant_ready_timeout,
    )'''
if old not in text:
    print(f"WARN: {path}: bootstrap_aitenant_tenant wait block not found; skip ready timeout", file=sys.stderr)
    sys.exit(0)
open(path, "w").write(text.replace(old, new, 1))
print(f"Patched {path} (AITenant Ready timeout env)")
PY
fi

if [[ -f "${CONFTEST}" ]] && ! grep -qF "${SHARED_PREFIX_MARKER}" "${CONFTEST}"; then
  python3 - <<'PY' "${CONFTEST}" "${SHARED_PREFIX_MARKER}"
import sys
path, marker = sys.argv[1], sys.argv[2]
text = open(path).read()
old = '''    worker = _xdist_worker_suffix()
    case_a = new_named_tenant_case(f"e2e-shared-a-{worker}")
    case_b = new_named_tenant_case(f"e2e-shared-b-{worker}")'''
new = f'''    worker = _xdist_worker_suffix()
    # {marker}
    case_a = new_named_tenant_case(f"e2e-sa-{{worker}}")
    case_b = new_named_tenant_case(f"e2e-sb-{{worker}}")'''
if old not in text:
    print(f"WARN: {path}: shared tenant prefixes not found; skip", file=sys.stderr)
    sys.exit(0)
open(path, "w").write(text.replace(old, new, 1))
print(f"Patched {path} (shared tenant prefixes)")
PY
fi

if [[ -f "${TEST_TENANT}" ]] && ! grep -qF "${REPLICAS_PIN_MARKER}" "${TEST_TENANT}"; then
  python3 - <<'PY' "${TEST_TENANT}" "${REPLICAS_PIN_MARKER}"
import sys
path, marker = sys.argv[1], sys.argv[2]
text = open(path).read()
old = '''        finally:
            _restore_tenant_spec(baseline, original_spec)


class TestTenantContract:'''
new = f'''        finally:
            _restore_tenant_spec(baseline, original_spec)
            # {marker}
            # kubectl apply of an empty original_spec may leave maasApi.replicas=2;
            # pin back so parallel AITenant bootstraps are not stuck on 1/2.
            _oc_run(
                [
                    "patch",
                    "maastenantconfig",
                    TENANT_NAME,
                    "-n",
                    _ns(),
                    "--type=merge",
                    "-p",
                    json.dumps({{"spec": {{"maasApi": {{"replicas": 1}}}}}}),
                ]
            )


class TestTenantContract:'''
if old not in text:
    # Newer MaaS trees insert more serial IPP resource tests between restore and Contract.
    old2 = '''        finally:
            _restore_tenant_spec(baseline, original_spec)

    @pytest.mark.serial
    def test_payload_processing_and_pre_processing_resources(self):'''
    new2 = f'''        finally:
            _restore_tenant_spec(baseline, original_spec)
            # {marker}
            _oc_run(
                [
                    "patch",
                    "maastenantconfig",
                    TENANT_NAME,
                    "-n",
                    _ns(),
                    "--type=merge",
                    "-p",
                    json.dumps({{"spec": {{"maasApi": {{"replicas": 1}}}}}}),
                ]
            )

    @pytest.mark.serial
    def test_payload_processing_and_pre_processing_resources(self):'''
    if old2 in text:
        open(path, "w").write(text.replace(old2, new2, 1))
        print(f"Patched {path} (pin maas-api replicas after override)")
        sys.exit(0)
    print(f"WARN: {path}: maasApi restore finally block not found; skip replicas pin", file=sys.stderr)
    sys.exit(0)
open(path, "w").write(text.replace(old, new, 1))
print(f"Patched {path} (pin maas-api replicas after override)")
PY
fi

echo "MaaS e2e test patches for ai-gateway-controller applied (idempotent)"
