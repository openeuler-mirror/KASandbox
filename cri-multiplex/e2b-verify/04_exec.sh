#!/bin/bash
###############################################################################
# 04_exec.sh — Exec 能力验证
#
# 覆盖验证指南：
#   5.2 Exec（非交互式验证，检查返回 URL）
#   以及 UpdateContainerResources / CheckpointContainer 等附加接口
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "04 — Exec 能力验证"

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

#==================== 5.2 Exec ====================#
log_step "5.2 Exec"
output=$(grpc_call "runtime.v1.RuntimeService/Exec" \
    "{\"container_id\": \"${CONTAINER_ID}\", \"cmd\": [\"echo\", \"world\"], \"tty\": false}") || true
if echo "${output}" | grep -q "url"; then
    log_pass "Exec 返回 URL"
    log_info "URL: $(echo "${output}" | grep -oP '"url":\s*"\K[^"]+')"
else
    log_fail "Exec 异常: ${output}"
fi

#==================== 附加接口 ====================#

log_step "5.5 UpdateContainerResources"
output=$(grpc_call "runtime.v1.RuntimeService/UpdateContainerResources" \
    "{\"container_id\": \"${CONTAINER_ID}\"}") || true
if echo "${output}" | grep -q "Unimplemented"; then
    log_pass "UpdateContainerResources 返回 Unimplemented（预期行为）"
else
    log_fail "UpdateContainerResources 异常: ${output}"
fi

log_step "5.6 CheckpointContainer"
output=$(grpc_call "runtime.v1.RuntimeService/CheckpointContainer" \
    "{\"container_id\": \"${CONTAINER_ID}\"}") || true
if echo "${output}" | grep -q "Unimplemented"; then
    log_pass "CheckpointContainer 返回 Unimplemented（预期行为）"
else
    log_fail "CheckpointContainer 异常: ${output}"
fi

log_step "5.7 ContainerStats"
output=$(grpc_call "runtime.v1.RuntimeService/ContainerStats" \
    "{\"container_id\": \"${CONTAINER_ID}\"}") || true
if echo "${output}" | grep -q "stats"; then
    log_pass "ContainerStats 返回正确"
else
    log_fail "ContainerStats 异常: ${output}"
fi

log_step "5.8 ListContainerStats"
output=$(grpc_call "runtime.v1.RuntimeService/ListContainerStats") || true
if echo "${output}" | grep -q "stats\|^{}"; then
    log_pass "ListContainerStats 返回正确"
else
    log_fail "ListContainerStats 异常: ${output}"
fi

log_step "5.9 ListMetricDescriptors"
output=$(grpc_call "runtime.v1.RuntimeService/ListMetricDescriptors") || true
if echo "${output}" | grep -q "Unimplemented"; then
    log_pass "ListMetricDescriptors 返回 Unimplemented（预期行为）"
else
    log_fail "ListMetricDescriptors 异常: ${output}"
fi

log_step "5.10 ListPodSandboxStats"
output=$(grpc_call "runtime.v1.RuntimeService/ListPodSandboxStats") || true
if echo "${output}" | grep -q "Unimplemented"; then
    log_pass "ListPodSandboxStats 返回 Unimplemented（预期行为）"
else
    log_fail "ListPodSandboxStats 异常: ${output}"
fi

log_step "5.11 ReopenContainerLog"
output=$(grpc_call "runtime.v1.RuntimeService/ReopenContainerLog" \
    "{\"container_id\": \"${CONTAINER_ID}\"}") || true
if echo "${output}" | grep -q "^{}\|^$"; then
    log_pass "ReopenContainerLog 成功"
else
    log_fail "ReopenContainerLog 异常: ${output}"
fi

log_step "5.12 PortForward"
output=$(grpc_call "runtime.v1.RuntimeService/PortForward" \
    "{\"pod_sandbox_id\": \"${POD_UID}\"}") || true
if echo "${output}" | grep -q "Unimplemented"; then
    log_pass "PortForward 返回 Unimplemented（预期行为）"
else
    log_fail "PortForward 异常: ${output}"
fi

#==================== 清理 ====================#
log_step "清理资源"
cleanup_container "${CONTAINER_ID}"
cleanup_pod "${POD_UID}"
log_info "清理完成"

print_summary
exit 0
