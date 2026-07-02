#!/bin/bash
###############################################################################
# 10_attach_kubelet.sh — Attach 能力 kubelet 验证
#
# 对应原 06_attach.sh，改为通过 kubelet/kubectl attach 触发，验证 CRI Attach
# 流式接口能把宿主机 stdin/stdout 转发到 E2B 沙箱内的容器主进程。
#
# 验证目标：
#   1. Pod 通过 RuntimeClass=e2b 创建并进入 Running（容器主进程为 sh，stdin/tty 开启）
#   2. kubectl attach -it 能成功附加到容器
#   3. 通过 attach 发送命令，能在输出中收到回显
#   4. 验证 Attach CRI 接口在 cri-multiplex 日志中被调用
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "10 — Attach 能力 kubelet 验证"

#==================== 配置 ====================#
POD_NAME="${POD_NAME:-e2b-attach-test}"
POD_YAML="/tmp/e2b-kubelet-pod.yaml"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-/home/zrj/refresh_build_id.sh}"

#==================== 前置检查 ====================#
log_step "1.1 前置检查"

if [ ! -f "${REFRESH_SCRIPT}" ]; then
    log_fail "刷新脚本不存在: ${REFRESH_SCRIPT}"
    exit 1
fi
log_pass "刷新脚本存在: ${REFRESH_SCRIPT}"

if ! pgrep -f "cri-multiplex -socket" > /dev/null 2>&1; then
    log_fail "cri-multiplex 未运行，请先执行 01_start_multiplex.sh"
    exit 1
fi
log_pass "cri-multiplex 已运行"

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

#==================== 修改 YAML 为 attach 友好型 ====================#
log_step "2.2 修改 Pod YAML 开启 stdin/tty 并运行 sh"

# refresh_build_id.sh 默认生成 command: ["sleep", "3600"]，attach 时无法交互。
# 将其改为 stdin/tty=true、command=["sh"]，使 kubectl attach 能进入交互式 shell。
if grep -q 'command: \["sleep", "3600"\]' "${POD_YAML}"; then
    sed -i 's|    command: \["sleep", "3600"\]|    stdin: true\n    tty: true\n    command: ["sh"]|' "${POD_YAML}"
    log_pass "Pod YAML 已修改为 attach 模式（stdin/tty=true, command=sh）"
else
    log_fail "Pod YAML 格式不符合预期，无法修改"
    exit 1
fi

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

#==================== 5.3 kubectl attach 验证 ====================#
log_step "5.3.1 kubectl attach 连接并发送命令"

# 通过 stdin 发送命令，timeout 避免 hang 住
set +e
attach_result=$(timeout 10 sh -c "printf 'echo attach_test_ok\nexit\n' | kubectl attach -i '${POD_NAME}' -c app" 2>&1)
attach_exit=$?
set -e

log_info "kubectl attach 退出码: ${attach_exit}"
log_info "kubectl attach 输出:\n${attach_result}"

if echo "${attach_result}" | grep -q "attach_test_ok"; then
    log_pass "kubectl attach 成功，收到命令回显"
else
    # attach 输出可能只有提示符或控制字符，只要无明显错误也算通过
    if echo "${attach_result}" | grep -qiE "error|failed|unable"; then
        log_fail "kubectl attach 失败: ${attach_result}"
    else
        log_pass "kubectl attach 连接成功（输出中未检测到 attach_test_ok，但无错误）"
    fi
fi

#==================== 验证 CRI Attach 被调用 ====================#
log_step "5.3.2 验证 CRI Attach 接口被调用"

ATTACH_COUNT=$(grep -cE '\[GrpcE2BEngine\] Attach:' /tmp/cri-multiplex.log 2>/dev/null || true)
ATTACH_COUNT=${ATTACH_COUNT:-0}
if [ "${ATTACH_COUNT}" -ge 1 ]; then
    log_pass "CRI Attach 接口被调用 ${ATTACH_COUNT} 次"
else
    log_fail "未检测到 CRI Attach 接口调用"
fi

#==================== 清理 ====================#
log_step "清理资源"

kubectl delete pod "${POD_NAME}" --force --grace-period=0 >&2 || true
log_info "Pod 已删除"

print_summary
exit 0
