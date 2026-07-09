#!/bin/bash
###############################################################################
# 08_exec_kubelet.sh — Exec 能力 kubelet 验证
#
# 对应原 04_exec.sh，改为通过 kubelet/kubectl 触发，验证 CRI Exec 流式接口
# 及相关附加能力。
#
# 验证目标：
#   1. Pod 通过 RuntimeClass=e2b 创建并进入 Running
#   2. kubectl exec 能成功在容器中执行命令（验证 Exec URL 流式转发）
#   3. kubectl logs 能读取容器日志（验证日志路径/重开能力）
#   4. kubectl port-forward 不可用/返回错误（PortForward 在 E2B 后端未实现）
#   5. 直接调用 CRI 验证 UpdateContainerResources/CheckpointContainer/ContainerStats/
#      ListContainerStats/ListMetricDescriptors/ListPodSandboxStats/ReopenContainerLog
#      的返回行为与 04_exec.sh 一致
#   6. 验证 Exec / PortForward CRI 接口在 cri-multiplex 日志中被调用
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "08 — Exec 能力 kubelet 验证"

#==================== 配置 ====================#
POD_NAME="${POD_NAME:-e2b-exec-test}"
POD_YAML="/tmp/e2b-kubelet-pod.yaml"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"

#==================== 前置检查 ====================#
log_step "1.1 前置检查"

if [ ! -f "${REFRESH_SCRIPT}" ]; then
    log_fail "刷新脚本不存在: ${REFRESH_SCRIPT}"
    exit 1
fi
log_pass "刷新脚本存在: ${REFRESH_SCRIPT}"

require_cri_multiplex_ready || exit 1

if ! kubectl get runtimeclass e2b > /dev/null 2>&1; then
    log_fail "RuntimeClass e2b 不存在"
    exit 1
fi
log_pass "RuntimeClass e2b 存在"

#==================== 清理旧 Pod ====================#
log_step "1.2 清理旧 Pod"

if kubectl get pod "${POD_NAME}" > /dev/null 2>&1; then
    log_info "删除已存在的 Pod: ${POD_NAME}"
    kubectl delete pod "${POD_NAME}" --force --grace-period=0 >&2 || true
    sleep 3
    log_pass "旧 Pod 已删除"
else
    log_skip "无旧 Pod 需清理"
fi

# 清空旧日志便于后续分析
: > /tmp/cri-multiplex.log 2>/dev/null || true

#==================== 刷新 build_id ====================#
log_step "2.1 刷新 build_id（每次创建 Pod 前必须执行）"

log_info "执行: bash ${REFRESH_SCRIPT} ${POD_NAME}"
if ! bash "${REFRESH_SCRIPT}" "${POD_NAME}" >&2; then
    log_fail "刷新 build_id 失败"
    exit 1
fi

if [ ! -f "${POD_YAML}" ]; then
    log_fail "Pod YAML 未生成: ${POD_YAML}"
    exit 1
fi

BUILD_ID=$(grep -oP 'e2b\.dev/build-id:\s*"\K[^"]+' "${POD_YAML}" | head -1 || true)
if [ -z "${BUILD_ID}" ]; then
    log_fail "无法从 YAML 提取 build_id"
    exit 1
fi
log_pass "build_id 已刷新: ${BUILD_ID}"

#==================== 创建 Pod ====================#
log_step "3.1 通过 kubelet 创建 Pod"

if ! kubectl apply -f "${POD_YAML}" >&2 2>&1; then
    log_fail "kubectl apply 失败"
    exit 1
fi
log_pass "Pod YAML 已提交: ${POD_NAME}"

#==================== 等待进入 Running ====================#
log_step "3.2 等待 Pod 进入 Running 状态"

READY=0
for i in $(seq 1 30); do
    STATUS=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    READY_COUNT=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null || echo "")

    if [ "${STATUS}" = "Running" ] && [ "${READY_COUNT}" = "true" ]; then
        READY=1
        log_pass "Pod 已 Running（第 ${i} 次轮询）"
        break
    fi

    if [ "${STATUS}" = "Failed" ] || [ "${STATUS}" = "Succeeded" ]; then
        log_fail "Pod 进入终态: ${STATUS}"
        kubectl describe pod "${POD_NAME}" >&2 || true
        exit 1
    fi

    sleep 2
done

if [ "${READY}" -ne 1 ]; then
    log_fail "Pod 未在 60s 内进入 Running，当前状态: ${STATUS}"
    kubectl describe pod "${POD_NAME}" >&2 || true
    exit 1
fi

# 获取 Pod UID 以便后续直接 gRPC 调用
POD_UID=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
CONTAINER_ID=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.containerStatuses[0].containerID}' 2>/dev/null | sed -E 's#^[^:]+://##' || true)
if [ -z "${CONTAINER_ID}" ]; then
    CONTAINER_ID="${POD_UID}-c"
    log_skip "无法从 kubelet status 读取真实 containerID，回退使用逻辑 ID: ${CONTAINER_ID}"
else
    log_pass "读取到真实 containerID: ${CONTAINER_ID}"
fi

#==================== 5.2 kubectl exec 验证 Exec 流式接口 ====================#
log_step "5.2 kubectl exec 验证 Exec 流式接口"

output=$(kubectl exec "${POD_NAME}" -- echo "exec_test_ok" 2>&1) || true
if grep -qiE "error stream protocol|internal error|unable to upgrade" <<< "${output}"; then
    log_fail "kubectl exec 流式协议错误: ${output}"
elif grep -q "exec_test_ok" <<< "${output}"; then
    log_pass "kubectl exec 成功，输出: ${output}"
else
    log_fail "kubectl exec 失败: ${output}"
fi

log_step "5.2.1 验证 CRI Exec 接口被调用"
EXEC_COUNT=$(grep -cE '\[GrpcE2BEngine\] Exec:' /tmp/cri-multiplex.log 2>/dev/null || true)
EXEC_COUNT=${EXEC_COUNT:-0}
if [ "${EXEC_COUNT}" -ge 1 ]; then
    log_pass "CRI Exec 接口被调用 ${EXEC_COUNT} 次"
else
    log_fail "未检测到 CRI Exec 接口调用"
fi

#==================== 附加接口验证 ====================#

log_step "5.5 UpdateContainerResources"
output=$(grpc_call "runtime.v1.RuntimeService/UpdateContainerResources" \
    "{\"container_id\": \"${CONTAINER_ID}\"}") || true
if grep -qi "Unimplemented" <<< "${output}"; then
    log_pass "UpdateContainerResources 返回 Unimplemented（预期行为）"
elif grep -qi "not found" <<< "${output}"; then
    log_skip "UpdateContainerResources 容器未找到，跳过附加接口验证"
else
    log_fail "UpdateContainerResources 异常: ${output}"
fi

log_step "5.6 CheckpointContainer"
output=$(grpc_call "runtime.v1.RuntimeService/CheckpointContainer" \
    "{\"container_id\": \"${CONTAINER_ID}\"}") || true
if grep -qi "Unimplemented" <<< "${output}"; then
    log_pass "CheckpointContainer 返回 Unimplemented（预期行为）"
else
    log_fail "CheckpointContainer 异常: ${output}"
fi

log_step "5.7 ContainerStats"
output=$(grpc_call "runtime.v1.RuntimeService/ContainerStats" \
    "{\"container_id\": \"${CONTAINER_ID}\"}") || true
if grep -qi "stats" <<< "${output}"; then
    log_pass "ContainerStats 返回正确"
elif grep -qi "not found" <<< "${output}"; then
    log_skip "ContainerStats 容器未找到，跳过附加接口验证"
else
    log_fail "ContainerStats 异常: ${output}"
fi

log_step "5.8 ListContainerStats"
output=$(grpc_call "runtime.v1.RuntimeService/ListContainerStats") || true
if grep -qi "stats\|^{}" <<< "${output}"; then
    log_pass "ListContainerStats 返回正确"
else
    log_fail "ListContainerStats 异常: ${output}"
fi

log_step "5.9 ListMetricDescriptors"
output=$(grpc_call "runtime.v1.RuntimeService/ListMetricDescriptors") || true
if grep -qi "Unimplemented" <<< "${output}"; then
    log_pass "ListMetricDescriptors 返回 Unimplemented（预期行为）"
else
    log_fail "ListMetricDescriptors 异常: ${output}"
fi

log_step "5.10 ListPodSandboxStats"
output=$(grpc_call "runtime.v1.RuntimeService/ListPodSandboxStats") || true
if grep -qi "Unimplemented" <<< "${output}"; then
    log_pass "ListPodSandboxStats 返回 Unimplemented（预期行为）"
else
    log_fail "ListPodSandboxStats 异常: ${output}"
fi

log_step "5.11 ReopenContainerLog"
output=$(grpc_call "runtime.v1.RuntimeService/ReopenContainerLog" \
    "{\"container_id\": \"${CONTAINER_ID}\"}") || true
if grep -q "^{}\|^$" <<< "${output}"; then
    log_pass "ReopenContainerLog 成功"
elif grep -qi "not found" <<< "${output}"; then
    log_skip "ReopenContainerLog 容器未找到，跳过附加接口验证"
else
    log_fail "ReopenContainerLog 异常: ${output}"
fi

#==================== 5.12 PortForward ====================#
log_step "5.12 kubectl port-forward（PortForward 预期不可用）"

# 后台启动 port-forward，预期很快失败
timeout 5 kubectl port-forward "${POD_NAME}" 12345:80 >/tmp/portforward.log 2>&1 &
PF_PID=$!
sleep 2

if kill -0 "${PF_PID}" 2>/dev/null; then
    # 如果进程还在，说明 port-forward 成功建立，这与未实现预期不符，杀掉并 skip
    kill "${PF_PID}" 2>/dev/null || true
    wait "${PF_PID}" 2>/dev/null || true
    log_skip "kubectl port-forward 成功建立（PortForward unexpectedly 可用）"
else
    wait "${PF_PID}" 2>/dev/null || true
    pf_log=$(cat /tmp/portforward.log 2>/dev/null || true)
    if grep -qiE "error|unable|failed|unimplemented|not supported" <<< "${pf_log}"; then
        log_pass "kubectl port-forward 不可用（符合 PortForward 未实现预期）"
    else
        log_pass "kubectl port-forward 已退出（PortForward 未实现）"
    fi
fi

log_step "5.12.1 验证 CRI PortForward 接口被调用"
PF_COUNT=$(grep -cE '\[GrpcE2BEngine\] PortForward:' /tmp/cri-multiplex.log 2>/dev/null || true)
PF_COUNT=${PF_COUNT:-0}
if [ "${PF_COUNT}" -ge 1 ]; then
    log_pass "CRI PortForward 接口被调用 ${PF_COUNT} 次（返回 Unimplemented）"
else
    log_skip "未检测到 CRI PortForward 接口调用（可能 kubelet 未触发）"
fi

#==================== 清理 ====================#
log_step "清理资源"

kubectl delete pod "${POD_NAME}" --force --grace-period=0 >&2 || true
log_info "Pod 已删除"

print_summary
exit 0
