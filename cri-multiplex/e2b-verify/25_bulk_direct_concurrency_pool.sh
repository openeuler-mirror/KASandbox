#!/bin/bash
###############################################################################
# 25_bulk_direct_concurrency_pool.sh — direct sandbox 并发创建性能测试（带预热池）
#
# 重启 cri-multiplex 并开启 CNI 预热池（-cni-pool-enabled -cni-pool-size 200），
# 每批并发创建前先等预热池灌满，先后并发创建 100 / 200 个 direct sandbox，
# 统计端到端耗时与 CNI ADD 耗时分布（avg/p50/p90/p99/min/max）。
# 性能对比脚本，不加入 run_all.sh。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"
source "${SCRIPT_DIR}/lib/bulk_direct.sh"

log_section "25 — direct sandbox 并发创建性能（带预热池）"

export HIDE_SANDBOX_LABEL="flux-sandbox.io/direct=true"
export E2B_FORCE_RESTART=1
export CNI_POOL_ENABLED=1
export CNI_POOL_SIZE=200
BASE_POD_JSON="${POD_JSON}"

cleanup_25_tmp() {
    cleanup_direct_sandbox_batch || true
    if [ -n "${POD_JSON:-}" ] && [ "${POD_JSON}" != "${BASE_POD_JSON}" ]; then
        rm -f "${POD_JSON}" || true
    fi
}
trap cleanup_25_tmp EXIT

"${SCRIPT_DIR}/01_start_multiplex.sh"

log_step "3.1 确认预热池已开启"
cmdline="$(cri_multiplex_cmdline)"
if echo "${cmdline}" | grep -q -- "-cni-pool-enabled" && echo "${cmdline}" | grep -q -- "-cni-pool-size 200"; then
    log_pass "cri-multiplex 启动参数包含 -cni-pool-enabled -cni-pool-size 200"
else
    log_fail "cri-multiplex 启动参数未正确开启预热池: ${cmdline}"
    print_summary
    exit 1
fi

log_step "3.2 批量创建 100 个 direct sandbox 并统计耗时"
wait_cni_pool_full 900 || {
    log_fail "CNI 预热池未就绪，跳过批量创建"
    print_summary
    exit 1
}
prepare_direct_sandbox_batch "bulk25-100" 100 || {
    log_fail "批量创建 100 个 direct sandbox 失败"
    print_summary
    exit 1
}
elapsed_ms="${BATCH_ELAPSED_MS}"
log_pass "100 个 direct sandbox 创建完成，端到端耗时 $(fmt_ms "${elapsed_ms}")"
report_cni_add_stats "bulk25-100" || true

log_step "3.3 清理 100 个 direct sandbox"
cleanup_direct_sandbox_batch

log_step "3.4 等待 1 分钟"
sleep 60
log_pass "已等待 60 秒"

log_step "3.5 批量创建 200 个 direct sandbox 并统计耗时"
wait_cni_pool_full 900 || {
    log_fail "CNI 预热池未就绪，跳过批量创建"
    print_summary
    exit 1
}
prepare_direct_sandbox_batch "bulk25-200" 200 || {
    log_fail "批量创建 200 个 direct sandbox 失败"
    print_summary
    exit 1
}
elapsed_ms="${BATCH_ELAPSED_MS}"
log_pass "200 个 direct sandbox 创建完成，端到端耗时 $(fmt_ms "${elapsed_ms}")"
report_cni_add_stats "bulk25-200" || true

log_step "3.6 清理 200 个 direct sandbox"
cleanup_direct_sandbox_batch

print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    exit 0
fi
exit 1
