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

#==================== 1. 检查是否已在运行（含连通性验证） ====================#
log_step "检查 cri-multiplex 运行状态"
if pgrep -f "cri-multiplex" > /dev/null 2>&1 && [ -S "${SOCKET}" ]; then
    # 验证 socket 实际可连通（不只是文件存在）
    if crictl --runtime-endpoint "unix://${SOCKET}" info > /dev/null 2>&1; then
        log_pass "cri-multiplex 已在运行且可连通，socket: ${SOCKET}"
        exit 0
    else
        log_info "进程存在但 socket 不可连通，杀掉旧进程..."
        # 注意：不能用 pkill -f "cri-multiplex"，会匹配到脚本自身路径
        # 只匹配实际运行的二进制（带 -socket 参数的进程）
        pkill -9 -f "cri-multiplex -socket" 2>/dev/null || true
        rm -f "${SOCKET}"
        sleep 1
    fi
fi

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
log_step "启动 cri-multiplex"
rm -f "${SOCKET}"

cd "${MULTIPLEX_DIR}"
# 使用 setsid 完全脱离会话，防止脚本退出时进程被杀
setsid ./cri-multiplex \
    -socket "${SOCKET}" \
    -containerd-socket "${CONTAINERD_SOCKET}" \
    > /tmp/cri-multiplex.log 2>&1 < /dev/null &

# 等待 socket 就绪
retries=10
while [ $retries -gt 0 ]; do
    if [ -S "${SOCKET}" ]; then
        # 验证进程仍在运行
        if pgrep -f "cri-multiplex" > /dev/null 2>&1; then
            log_pass "cri-multiplex 已启动，socket: ${SOCKET}"
            log_info "日志: /tmp/cri-multiplex.log"
            exit 0
        fi
    fi
    sleep 1
    retries=$((retries-1))
done

log_fail "cri-multiplex 启动超时"
log_info "日志内容:"
cat /tmp/cri-multiplex.log 2>/dev/null || true
exit 1
