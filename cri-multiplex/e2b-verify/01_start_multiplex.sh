#!/bin/bash
###############################################################################
# 01_start_multiplex.sh — 启动 cri-multiplex
#
# 职责：
#   1. 构建 cri-multiplex（如需要）
#   2. 启动 cri-multiplex（如未运行）
#   3. 验证 socket 就绪
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "01 — 启动 cri-multiplex"

E2B_CNI_ENABLED="${E2B_CNI_ENABLED:-1}"
E2B_FORCE_RESTART="${E2B_FORCE_RESTART:-0}"
if [ "${E2B_CNI_ENABLED}" != "0" ] && [ "${E2B_CNI_ENABLED}" != "1" ]; then
    log_fail "E2B_CNI_ENABLED 必须是 0 或 1，当前值: ${E2B_CNI_ENABLED}"
    exit 1
fi
if [ "${E2B_FORCE_RESTART}" != "0" ] && [ "${E2B_FORCE_RESTART}" != "1" ]; then
    log_fail "E2B_FORCE_RESTART 必须是 0 或 1，当前值: ${E2B_FORCE_RESTART}"
    exit 1
fi
MODE_DESC="非 CNI"
if [ "${E2B_CNI_ENABLED}" = "1" ]; then
    MODE_DESC="CNI"
fi

#==================== 1. 检查是否已在运行（含连通性验证） ====================#
log_step "检查 cri-multiplex 运行状态（目标模式: ${MODE_DESC}）"
if cri_multiplex_ready && [ "${E2B_FORCE_RESTART}" != "1" ]; then
    # 验证 socket 实际可连通（不只是文件存在）
    current_cni=0
    if cri_multiplex_cni_enabled; then
        current_cni=1
    fi
    if [ "${current_cni}" = "${E2B_CNI_ENABLED}" ]; then
        log_pass "cri-multiplex 已在运行且模式匹配，socket: ${SOCKET}, mode=${MODE_DESC}"
        exit 0
    else
        log_info "cri-multiplex 已运行但模式不匹配，准备重启为 ${MODE_DESC} 模式"
    fi
elif [ "${E2B_FORCE_RESTART}" = "1" ]; then
    log_info "E2B_FORCE_RESTART=1，准备强制重启 cri-multiplex"
fi

old_pids=$(cri_multiplex_pids)
if [ -n "${old_pids}" ]; then
    log_info "停止旧 cri-multiplex 进程: ${old_pids}"
    kill ${old_pids} 2>/dev/null || true
    sleep 1
    old_pids=$(cri_multiplex_pids)
    if [ -n "${old_pids}" ]; then
        kill -9 ${old_pids} 2>/dev/null || true
    fi
fi
rm -f "${SOCKET}"

#==================== 2. 构建 ====================#
log_step "构建 cri-multiplex"
if [ ! -f "${MULTIPLEX_DIR}/cri-multiplex" ]; then
    log_info "执行 go build..."
    (cd "${MULTIPLEX_DIR}" && go build ./cmd/cri-multiplex) || {
        log_fail "构建 cri-multiplex 失败"
        exit 1
    }
fi
log_pass "cri-multiplex 二进制已就绪"

#==================== 3. 启动 ====================#
log_step "启动 cri-multiplex（${MODE_DESC} 模式）"
rm -f "${SOCKET}"

cd "${MULTIPLEX_DIR}"
args=(
    -socket "${SOCKET}"
    -containerd-socket "${CONTAINERD_SOCKET}"
    -e2b-backend grpc
    -orchestrator-address "${ORCHESTRATOR_ADDRESS}"
    -orchestrator-proxy-address "${ORCHESTRATOR_PROXY_ADDRESS}"
)
if [ "${E2B_CNI_ENABLED}" = "1" ]; then
    args+=(
        -e2b-cni-enabled
        -cni-conf-dir "${CNI_CONF_DIR}"
        -cni-bin-dir "${CNI_BIN_DIR}"
        -cni-ifname "${CNI_IFNAME}"
        -cni-netns-dir "${CNI_NETNS_DIR}"
    )
fi

# 使用 setsid 完全脱离会话，防止脚本退出时进程被杀
setsid ./cri-multiplex "${args[@]}" > /tmp/cri-multiplex.log 2>&1 < /dev/null &

# 等待 socket 就绪
retries=10
while [ $retries -gt 0 ]; do
    if cri_multiplex_ready; then
        log_pass "cri-multiplex 已启动，socket: ${SOCKET}, mode=${MODE_DESC}"
        log_info "日志: /tmp/cri-multiplex.log"
        exit 0
    fi
    sleep 1
    retries=$((retries-1))
done

log_fail "cri-multiplex 启动超时"
log_info "日志内容:"
cat /tmp/cri-multiplex.log 2>/dev/null || true
exit 1
