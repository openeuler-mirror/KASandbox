#!/bin/bash
###############################################################################
# 23_restart_multiplex_hide_direct.sh — 重启 cri-multiplex 并隐藏 direct sandbox
#
# 在 01_start_multiplex.sh 的现有参数基础上，追加：
#   -hide-sandbox-label flux-sandbox.io/direct=true
#
# 并发创建性能测试已拆出：
#   24_bulk_direct_concurrency.sh      — 不带预热池
#   25_bulk_direct_concurrency_pool.sh — 带预热池
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"
source "${SCRIPT_DIR}/lib/bulk_direct.sh"

log_section "23 — 重启 cri-multiplex 并隐藏 direct sandbox"

export HIDE_SANDBOX_LABEL="flux-sandbox.io/direct=true"
export E2B_FORCE_RESTART=1
BASE_POD_JSON="${POD_JSON}"
DIRECT_POD_ID=""

cleanup_23_tmp() {
    if [ -n "${DIRECT_POD_ID}" ]; then
        cleanup_pod "${DIRECT_POD_ID}" || true
    fi
    if [ -n "${POD_JSON:-}" ] && [ "${POD_JSON}" != "${BASE_POD_JSON}" ]; then
        rm -f "${POD_JSON}" || true
    fi
}
trap cleanup_23_tmp EXIT

"${SCRIPT_DIR}/01_start_multiplex.sh"

log_step "1.1 检查 hide-sandbox-label 已注入"
cmdline="$(cri_multiplex_cmdline)"
if echo "${cmdline}" | grep -q -- "-hide-sandbox-label flux-sandbox.io/direct=true"; then
    log_pass "cri-multiplex 启动参数包含 -hide-sandbox-label flux-sandbox.io/direct=true"
else
    log_fail "cri-multiplex 启动参数未包含 direct 隐藏标签: ${cmdline}"
fi

log_step "1.2 创建 bypasskubelet/direct Pod"
prepare_direct_pod_json "hide-direct" "${BASE_POD_JSON}" || exit 1
add_direct_label_to_pod_json "${POD_JSON}"
DIRECT_POD_ID="$(run_pod_sandbox)" || {
    log_fail "创建 bypasskubelet/direct Pod 失败"
    print_summary
    exit 1
}
log_pass "创建 bypasskubelet/direct Pod 成功，Pod ID: ${DIRECT_POD_ID}"

log_step "1.3 kubelet 风格查询不应看到 direct Pod"
output=$(grpc_call "runtime.v1.RuntimeService/ListPodSandbox" '{}' 2>&1) || true
if echo "${output}" | grep -q "${DIRECT_POD_ID}"; then
    log_fail "未带 direct label 的 ListPodSandbox 仍查到了 direct Pod: ${output}"
else
    log_pass "未带 direct label 的 ListPodSandbox 未查到 direct Pod"
fi

log_step "1.4 带 direct label 查询应能看到 direct Pod"
output=$(grpc_call "runtime.v1.RuntimeService/ListPodSandbox" \
    '{"filter":{"label_selector":{"flux-sandbox.io/direct":"true"}}}' 2>&1) || true
if echo "${output}" | grep -q "${DIRECT_POD_ID}"; then
    log_pass "带 direct label 的 ListPodSandbox 查到了 direct Pod"
else
    log_fail "带 direct label 的 ListPodSandbox 未查到 direct Pod: ${output}"
fi

print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    exit 0
fi
exit 1
