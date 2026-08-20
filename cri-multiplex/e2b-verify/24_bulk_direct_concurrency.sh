#!/bin/bash
###############################################################################
# 24_bulk_direct_concurrency.sh — direct sandbox 并发创建性能测试（不带预热池）
#
# 重启 cri-multiplex（预热池强制关闭），先后并发创建 100 / 200 个 direct
# sandbox，统计端到端耗时与 CNI ADD 耗时分布（avg/p50/p90/p99/min/max）。
# 性能对比脚本，不加入 run_all.sh。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"
source "${SCRIPT_DIR}/lib/bulk_direct.sh"

log_section "24 — direct sandbox 并发创建性能（不带预热池）"

export HIDE_SANDBOX_LABEL="flux-sandbox.io/direct=true"
export E2B_FORCE_RESTART=1
# 强制关闭预热池，即使调用方环境里带了 CNI_POOL_ENABLED/CNI_POOL_SIZE
export CNI_POOL_ENABLED=0
export CNI_POOL_SIZE=0
BASE_POD_JSON="${POD_JSON}"

cleanup_24_tmp() {
    cleanup_direct_sandbox_batch || true
    if [ -n "${POD_JSON:-}" ] && [ "${POD_JSON}" != "${BASE_POD_JSON}" ]; then
        rm -f "${POD_JSON}" || true
    fi
}
trap cleanup_24_tmp EXIT

"${SCRIPT_DIR}/01_start_multiplex.sh"

log_step "2.1 确认预热池未开启"
cmdline="$(cri_multiplex_cmdline)"
if echo "${cmdline}" | grep -q -- "-cni-pool-enabled"; then
    log_fail "本用例要求不带预热池，但启动参数包含 -cni-pool-enabled: ${cmdline}"
    print_summary
    exit 1
fi
log_pass "cri-multiplex 启动参数未开启预热池"

log_step "2.2 批量创建 100 个 direct sandbox 并统计耗时"
prepare_direct_sandbox_batch "bulk24-100" 100 || {
    log_fail "批量创建 100 个 direct sandbox 失败"
    print_summary
    exit 1
}
elapsed_ms="${BATCH_ELAPSED_MS}"
log_pass "100 个 direct sandbox 创建完成，端到端耗时 $(fmt_ms "${elapsed_ms}")"
report_cni_add_stats "bulk24-100" || true

log_step "2.3 清理 100 个 direct sandbox"
cleanup_direct_sandbox_batch

log_step "2.4 等待 1 分钟"
sleep 60
log_pass "已等待 60 秒"

log_step "2.5 批量创建 200 个 direct sandbox 并统计耗时"
prepare_direct_sandbox_batch "bulk24-200" 200 || {
    log_fail "批量创建 200 个 direct sandbox 失败"
    print_summary
    exit 1
}
elapsed_ms="${BATCH_ELAPSED_MS}"
log_pass "200 个 direct sandbox 创建完成，端到端耗时 $(fmt_ms "${elapsed_ms}")"
report_cni_add_stats "bulk24-200" || true

log_step "2.6 清理 200 个 direct sandbox"
cleanup_direct_sandbox_batch

print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    exit 0
fi
exit 1
