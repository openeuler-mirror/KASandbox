#!/bin/bash
###############################################################################
# 09_execsync_kubelet.sh — ExecSync 能力 kubelet 验证
#
# 对应原 05_execsync.sh，改为通过 kubelet/kubectl 触发，验证 CRI Exec
#（kubectl exec 在 kubelet 侧走流式 Exec 接口）同步执行结果。
#
# 验证目标：
#   1. Pod 通过 RuntimeClass=e2b 创建并进入 Running
#   2. kubectl exec 执行简单命令，stdout 正确返回
#   3. kubectl exec 执行多行命令，输出完整
#   4. kubectl exec 执行返回非零退出码的命令，退出码可正确传递
#   5. kubectl exec 读取沙箱内文件内容
#   6. 验证 Exec CRI 接口在 cri-multiplex 日志中被调用
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "09 — ExecSync 能力 kubelet 验证"

#==================== 配置 ====================#
POD_NAME="${POD_NAME:-e2b-execsync-test}"
POD_YAML="/tmp/e2b-kubelet-pod.yaml"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"

#==================== 前置检查 ====================#
log_step "1.1 前置检查"

require_refresh_script "${REFRESH_SCRIPT}" || exit 1

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

if ! refresh_or_reuse_e2b_yaml "${REFRESH_SCRIPT}" "${POD_NAME}" "${POD_YAML}"; then
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

#==================== ExecSync / kubectl exec 验证 ====================#

log_step "5.1.1 kubectl exec — echo hello"
output=$(kubectl exec "${POD_NAME}" -- echo "hello" 2>&1) || true
if echo "${output}" | grep -qiE "error stream protocol|internal error|unable to upgrade"; then
    log_fail "kubectl exec 流式协议错误: ${output}"
elif echo "${output}" | grep -q "hello"; then
    log_pass "kubectl exec echo hello 成功，输出: ${output}"
else
    log_fail "kubectl exec echo hello 失败: ${output}"
fi

log_step "5.1.2 kubectl exec — 多条命令"
output=$(kubectl exec "${POD_NAME}" -- sh -c "echo line1; echo line2" 2>&1) || true
if echo "${output}" | grep -qiE "error stream protocol|internal error|unable to upgrade"; then
    log_fail "kubectl exec 流式协议错误: ${output}"
elif echo "${output}" | grep -q "line1" && echo "${output}" | grep -q "line2"; then
    log_pass "kubectl exec 多条命令成功，输出: ${output}"
else
    log_fail "kubectl exec 多条命令失败: ${output}"
fi

log_step "5.1.3 kubectl exec — 检查退出码"
set +e
output=$(kubectl exec "${POD_NAME}" -- sh -c "exit 42" 2>&1)
exit_code=$?
set -e
if echo "${output}" | grep -qiE "error stream protocol|internal error|unable to upgrade"; then
    log_fail "kubectl exec 流式协议错误: ${output}"
elif [ "${exit_code}" -eq 42 ]; then
    log_pass "kubectl exec 退出码正确，值为: ${exit_code}"
elif echo "${output}" | grep -qiE "exit code 42|exited with 42"; then
    log_pass "kubectl exec 返回退出码 42 信息: ${output}"
else
    log_fail "kubectl exec 退出码不正确（exit_code=${exit_code}, 输出: ${output}）"
fi

log_step "5.1.4 kubectl exec — cat /etc/os-release"
output=$(kubectl exec "${POD_NAME}" -- cat /etc/os-release 2>&1) || true
if echo "${output}" | grep -qiE "error stream protocol|internal error|unable to upgrade"; then
    log_fail "kubectl exec 流式协议错误: ${output}"
elif echo "${output}" | grep -qE "Ubuntu|NAME="; then
    log_pass "kubectl exec cat /etc/os-release 成功"
else
    log_fail "kubectl exec cat /etc/os-release 失败: ${output}"
fi

#==================== 验证 CRI Exec 被调用 ====================#
log_step "5.1.5 验证 CRI Exec 接口被调用"

EXEC_COUNT=$(grep -cE '\[GrpcE2BEngine\] Exec:' /tmp/cri-multiplex.log 2>/dev/null || true)
EXEC_COUNT=${EXEC_COUNT:-0}
if [ "${EXEC_COUNT}" -ge 1 ]; then
    log_pass "CRI Exec 接口被调用 ${EXEC_COUNT} 次"
else
    log_fail "未检测到 CRI Exec 接口调用"
fi

#==================== 清理 ====================#
log_step "清理资源"

kubectl delete pod "${POD_NAME}" --force --grace-period=0 >&2 || true
log_info "Pod 已删除"

print_summary
exit 0
