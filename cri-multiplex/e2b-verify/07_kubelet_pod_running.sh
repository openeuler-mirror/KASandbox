#!/bin/bash
###############################################################################
# 07_kubelet_pod_running.sh — kubelet 对接验证：Pod 保持 Running 状态
#
# 验证目标：
#   1. 通过 kubelet + RuntimeClass=e2b 创建的 Pod 能进入 Running 状态
#   2. Pod 启动后保持 Running，RESTARTS=0，不被 kubelet 误调 StopContainer
#   3. StopContainer/StopPodSandbox 能力保留用于暂停沙箱（不在此用例主动触发）
#   4. Pod 删除后沙箱被销毁（RemovePodSandbox 触发）
#
# 前置条件：
#   - kubelet 已配置容器运行时端点指向 cri-multiplex
#   - RuntimeClass e2b 已创建
#   - cri-multiplex 已启动（01_start_multiplex.sh）
#
# 用法:
#   ./07_kubelet_pod_running.sh                # 默认 pod 名 e2b-kubelet-test
#   POD_NAME=custom ./07_kubelet_pod_running.sh
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "07 — kubelet 对接：Pod 保持 Running 验证"

#==================== 配置 ====================#
POD_NAME="${POD_NAME:-e2b-kubelet-test}"
POD_YAML="/tmp/e2b-kubelet-pod.yaml"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-/home/zrj/refresh_build_id.sh}"
OBSERVE_SECONDS="${OBSERVE_SECONDS:-30}"

#==================== 前置检查 ====================#
log_step "1.1 前置检查"

# 检查 refresh_build_id.sh
if [ ! -f "${REFRESH_SCRIPT}" ]; then
    log_fail "刷新脚本不存在: ${REFRESH_SCRIPT}"
    exit 1
fi
log_pass "刷新脚本存在: ${REFRESH_SCRIPT}"

# 检查 cri-multiplex 是否运行
if ! pgrep -f "cri-multiplex -socket" > /dev/null 2>&1; then
    log_fail "cri-multiplex 未运行，请先执行 01_start_multiplex.sh"
    exit 1
fi
log_pass "cri-multiplex 已运行"

# 检查 kubelet RuntimeClass
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

CREATE_TIME=$(date +%s)
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

#==================== 观察 Running 稳定性 ====================#
log_step "4.1 观察 ${OBSERVE_SECONDS}s 内 Pod 保持 Running 且 RESTARTS=0"

sleep "${OBSERVE_SECONDS}"

STATUS=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
RESTARTS=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo "unknown")

if [ "${STATUS}" = "Running" ] && [ "${RESTARTS}" = "0" ]; then
    log_pass "Pod 持续 Running，RESTARTS=0"
else
    log_fail "Pod 状态异常: phase=${STATUS}, restartCount=${RESTARTS}"
    kubectl describe pod "${POD_NAME}" >&2 || true
    exit 1
fi

#==================== 验证无 Stop 调用 ====================#
log_step "4.2 验证 cri-multiplex 日志无 StopContainer/StopPodSandbox"

# 只检查本次 Pod 创建后的日志
LOG_SINCE=$(date -d "@${CREATE_TIME}" '+%Y/%m/%d %H:%M:%S' 2>/dev/null || echo "")
STOP_COUNT=0
if [ -f /tmp/cri-multiplex.log ]; then
    # 日志中含 StopContainer / StopPodSandbox 的行数
    STOP_COUNT=$(grep -acE "StopContainer:|StopPodSandbox:" /tmp/cri-multiplex.log 2>/dev/null || echo "0")
fi

if [ "${STOP_COUNT}" -eq 0 ]; then
    log_pass "日志中无 StopContainer/StopPodSandbox 调用（应保持 Running，不被误 Stop）"
else
    log_fail "检测到 ${STOP_COUNT} 次 Stop 调用，kubelet 可能误判容器需重建"
    log_info "相关日志（最多 10 行）:"
    grep -aE "StopContainer:|StopPodSandbox:" /tmp/cri-multiplex.log 2>/dev/null | tail -10 >&2 || true
    exit 1
fi

#==================== 验证 CreateContainer 仅一次 ====================#
log_step "4.3 验证 CreateContainer 仅被调用一次（无重复创建）"

CREATE_COUNT=$(grep -acE "CreateContainer: pod=" /tmp/cri-multiplex.log 2>/dev/null || echo "0")
if [ "${CREATE_COUNT}" -eq 1 ]; then
    log_pass "CreateContainer 仅调用 1 次（hash 匹配，无重建）"
else
    log_fail "CreateContainer 被调用 ${CREATE_COUNT} 次（预期 1 次），存在重复创建"
    exit 1
fi

#==================== 验证 sandbox created 仅一次 ====================#
log_step "4.4 验证 sandbox created 仅一次"

SANDBOX_COUNT=$(grep -acE "sandbox created:" /tmp/cri-multiplex.log 2>/dev/null || echo "0")
if [ "${SANDBOX_COUNT}" -eq 1 ]; then
    log_pass "sandbox created 仅 1 次"
else
    log_fail "sandbox created ${SANDBOX_COUNT} 次（预期 1 次）"
    exit 1
fi

#==================== 验证删除 Pod 触发沙箱销毁 ====================#
log_step "5.1 删除 Pod 验证沙箱销毁"

log_info "kubectl delete pod ${POD_NAME} --force --grace-period=0"
kubectl delete pod "${POD_NAME}" --force --grace-period=0 >&2 || true

# 等待 RemovePodSandbox 被调用
REMOVED=0
for i in $(seq 1 15); do
    if grep -aqE "RemovePodSandbox:" /tmp/cri-multiplex.log 2>/dev/null; then
        REMOVED=1
        log_pass "RemovePodSandbox 已被调用（第 ${i} 次轮询）"
        break
    fi
    sleep 2
done

if [ "${REMOVED}" -ne 1 ]; then
    log_fail "15s 内未检测到 RemovePodSandbox 调用"
    exit 1
fi

#==================== 汇总 ====================#
print_summary

if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证通过：Pod 保持 Running，无 Stop 误调用，删除后沙箱销毁"
    exit 0
else
    exit 1
fi
