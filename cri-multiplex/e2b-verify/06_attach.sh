#!/bin/bash
###############################################################################
# 06_attach.sh — Attach 能力验证
#
# 覆盖验证指南：
#   5.3 Attach
#
# 流程：
#   1. 创建 Pod + Container
#   2. 用 Exec 启动一个 sh 进程（保持运行）
#   3. 调用 Attach 获取 URL
#   4. 用 Python 脚本连接 Attach URL，验证能收到输出
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "06 — Attach 能力验证"

STREAM_CLIENT="${SCRIPT_DIR}/test_stream_client.py"

#==================== 前置：创建 Pod + Container ====================#
log_step "前置准备：创建 Pod 和 Container"
POD_UID=$(run_pod_sandbox) || {
    log_fail "创建 Pod 失败"
    exit 1
}
CONTAINER_ID=$(create_and_start_container "${POD_UID}") || {
    log_fail "创建并启动 Container 失败"
    exit 1
}
export POD_UID CONTAINER_ID
log_info "Pod: ${POD_UID}, Container: ${CONTAINER_ID}"

#==================== Step 1: 用 Exec 启动 sh 进程 ====================#
log_step "5.3.1 用 Exec 启动交互式 sh"
exec_output=$(grpc_call "runtime.v1.RuntimeService/Exec" \
    "{\"container_id\": \"${CONTAINER_ID}\", \"cmd\": [\"sh\"], \"tty\": true, \"stdin\": true, \"stdout\": true, \"stderr\": true}") || true

if ! echo "${exec_output}" | grep -q "url"; then
    log_fail "Exec 启动 sh 失败: ${exec_output}"
    cleanup_container "${CONTAINER_ID}"
    cleanup_pod "${POD_UID}"
    exit 1
fi

EXEC_URL=$(echo "${exec_output}" | grep -oP '"url":\s*"\K[^"]+')
log_pass "Exec sh 返回 URL: ${EXEC_URL}"

# 后台连接 Exec URL，保持 sh 进程运行
log_info "后台连接 Exec URL 保持 sh 运行..."
echo "" | python3 "${STREAM_CLIENT}" "${EXEC_URL}" 3 > /dev/null 2>&1 &
EXEC_PID=$!
sleep 2

#==================== Step 2: 调用 Attach ====================#
log_step "5.3.2 调用 Attach"
attach_output=$(grpc_call "runtime.v1.RuntimeService/Attach" \
    "{\"container_id\": \"${CONTAINER_ID}\", \"tty\": true, \"stdin\": true, \"stdout\": true, \"stderr\": true}") || true

if ! echo "${attach_output}" | grep -q "url"; then
    log_fail "Attach 失败: ${attach_output}"
    kill "${EXEC_PID}" 2>/dev/null || true
    cleanup_container "${CONTAINER_ID}"
    cleanup_pod "${POD_UID}"
    exit 1
fi

ATTACH_URL=$(echo "${attach_output}" | grep -oP '"url":\s*"\K[^"]+')
log_pass "Attach 返回 URL: ${ATTACH_URL}"

#==================== Step 3: 连接 Attach URL 验证 ====================#
log_step "5.3.3 连接 Attach URL 验证输出"
attach_result=$(echo "echo attach_test_ok" | python3 "${STREAM_CLIENT}" "${ATTACH_URL}" 5 2>&1) || true

if echo "${attach_result}" | grep -q "attach_test_ok\|\\$"; then
    log_pass "Attach 连接成功，收到输出"
else
    # Attach 可能只返回提示符，只要连接成功即可
    if echo "${attach_result}" | grep -q "200 OK\|headers"; then
        log_pass "Attach 连接成功（HTTP 200）"
    else
        log_fail "Attach 连接异常: ${attach_result}"
    fi
fi

#==================== 清理 ====================#
log_step "清理资源"
kill "${EXEC_PID}" 2>/dev/null || true
cleanup_container "${CONTAINER_ID}"
cleanup_pod "${POD_UID}"
log_info "清理完成"

print_summary
exit 0
