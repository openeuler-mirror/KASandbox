#!/bin/bash
###############################################################################
# 09_execsync_kubelet.sh — ExecSync 能力 kubelet 验证
#
# 通过 kubelet 原生会调用 CRI ExecSync 的场景验证同步执行链路：
#   - exec startup/readiness/liveness probe
#   - lifecycle postStart / preStop exec hook
#
# 同时保留 kubectl exec 非交互命令验证。注意：kubectl exec 在 kubelet
# 侧走 CRI Exec streaming 接口，不是 CRI ExecSync。
#
# 验证目标：
#   1. Pod 通过 RuntimeClass=e2b 创建并进入 Running
#   2. kubelet 通过 startup/readiness/liveness exec probe 触发 CRI ExecSync
#   3. kubelet 通过 postStart/preStop exec hook 触发 CRI ExecSync
#   4. kubectl exec 非交互命令语义正常（CRI Exec streaming 链路）
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "09 — ExecSync 能力 kubelet 验证"

#==================== 配置 ====================#
POD_NAME="${POD_NAME:-e2b-execsync-test}"
BASE_POD_YAML="${POD_YAML:-/tmp/e2b-kubelet-pod.yaml}"
POD_YAML="${WORK_POD_YAML:-/tmp/e2b-execsync-kubelet-pod.yaml}"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"
STARTUP_PROBE_MARK="startup_probe_execsync_ok"
READINESS_PROBE_MARK="readiness_probe_execsync_ok"
LIVENESS_PROBE_MARK="liveness_probe_execsync_ok"
POSTSTART_MARK="poststart_execsync_ok"
PRESTOP_MARK="prestop_execsync_ok"

count_execsync_logs() {
    local mark="$1"
    grep -cE "\\[GrpcE2BEngine\\] ExecSync: .*${mark}" /tmp/cri-multiplex.log 2>/dev/null || true
}

wait_execsync_log() {
    local mark="$1"
    local desc="$2"
    local timeout_seconds="${3:-90}"
    local deadline=$((SECONDS + timeout_seconds))
    local count

    while [ "${SECONDS}" -lt "${deadline}" ]; do
        count=$(count_execsync_logs "${mark}")
        count=${count:-0}
        if [ "${count}" -ge 1 ]; then
            log_pass "${desc} 已触发 CRI ExecSync（${mark}, count=${count}）"
            return 0
        fi
        sleep 2
    done

    log_fail "${desc} 未在 ${timeout_seconds}s 内触发 CRI ExecSync（${mark}）"
    tail -n 120 /tmp/cri-multiplex.log >&2 2>/dev/null || true
    return 1
}

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
    delete_pod_and_wait_gone "${POD_NAME}" 90 || {
        log_fail "旧 Pod 未在 90s 内删除: ${POD_NAME}"
        exit 1
    }
    log_pass "旧 Pod 已删除"
else
    log_skip "无旧 Pod 需清理"
fi

# 清空旧日志便于后续分析
: > /tmp/cri-multiplex.log 2>/dev/null || true

#==================== 刷新 build_id ====================#
log_step "2.1 刷新 build_id（每次创建 Pod 前必须执行）"

if ! E2B_BASE_POD_YAML="${BASE_POD_YAML}" prepare_e2b_pod_yaml "${POD_NAME}" "${POD_YAML}"; then
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

#==================== 注入 kubelet ExecSync 场景 ====================#
log_step "2.2 注入 exec probe 和 lifecycle hook"

tmp_yaml="${POD_YAML}.execsync.tmp"
if ! awk -v startup_mark="${STARTUP_PROBE_MARK}" \
    -v readiness_mark="${READINESS_PROBE_MARK}" \
    -v liveness_mark="${LIVENESS_PROBE_MARK}" \
    -v poststart_mark="${POSTSTART_MARK}" \
    -v prestop_mark="${PRESTOP_MARK}" '
    /^  containers:/ {
        print "  terminationGracePeriodSeconds: 30"
        in_containers=1
        print
        next
    }
    in_containers == 1 && /^  - name:/ && first_container_seen != 1 {
        first_container_seen=1
        in_first_container=1
        print
        next
    }
    in_first_container == 1 && /^  - name:/ {
        if (inserted != 1) {
            print_execsync_block()
            inserted=1
        }
        in_first_container=0
        print
        next
    }
    in_first_container == 1 && /^    (lifecycle|startupProbe|readinessProbe|livenessProbe):/ {
        skip_block=1
        next
    }
    skip_block == 1 && /^    [A-Za-z0-9_-]+:/ {
        skip_block=0
    }
    skip_block == 1 {
        next
    }
    in_first_container == 1 && /^    imagePullPolicy:/ {
        print
        if (inserted != 1) {
            print_execsync_block()
            inserted=1
        }
        next
    }
    { print }
    END {
        if (first_container_seen != 1 || inserted != 1) {
            exit 2
        }
    }
    function print_execsync_block() {
        print "    lifecycle:"
        print "      postStart:"
        print "        exec:"
        print "          command: [\"sh\", \"-c\", \"echo " poststart_mark "\"]"
        print "      preStop:"
        print "        exec:"
        print "          command: [\"sh\", \"-c\", \"echo " prestop_mark "\"]"
        print "    startupProbe:"
        print "      exec:"
        print "        command: [\"sh\", \"-c\", \"echo " startup_mark "\"]"
        print "      periodSeconds: 2"
        print "      timeoutSeconds: 5"
        print "      failureThreshold: 30"
        print "    readinessProbe:"
        print "      exec:"
        print "        command: [\"sh\", \"-c\", \"echo " readiness_mark "\"]"
        print "      periodSeconds: 2"
        print "      timeoutSeconds: 5"
        print "      failureThreshold: 3"
        print "    livenessProbe:"
        print "      exec:"
        print "        command: [\"sh\", \"-c\", \"echo " liveness_mark "\"]"
        print "      periodSeconds: 2"
        print "      timeoutSeconds: 5"
        print "      failureThreshold: 3"
    }
' "${POD_YAML}" > "${tmp_yaml}"; then
    log_fail "注入 exec probe/lifecycle hook 失败"
    exit 1
fi
mv "${tmp_yaml}" "${POD_YAML}"
log_pass "Pod YAML 已注入 startup/readiness/liveness exec probe 和 postStart/preStop exec hook"

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

#==================== kubelet ExecSync 验证 ====================#
log_step "4.1 验证 exec startup/readiness/liveness probe 触发 CRI ExecSync"

wait_execsync_log "${STARTUP_PROBE_MARK}" "startupProbe exec" 90 || exit 1
wait_execsync_log "${READINESS_PROBE_MARK}" "readinessProbe exec" 90 || exit 1
wait_execsync_log "${LIVENESS_PROBE_MARK}" "livenessProbe exec" 90 || exit 1

log_step "4.2 验证 lifecycle postStart 触发 CRI ExecSync"

wait_execsync_log "${POSTSTART_MARK}" "postStart exec hook" 60 || exit 1

log_step "4.3 验证 probe 未产生失败事件"

describe_output=$(kubectl describe pod "${POD_NAME}" 2>&1 || true)
if echo "${describe_output}" | grep -qiE "Liveness probe failed|Readiness probe failed|Startup probe failed|PostStartHookError"; then
    log_fail "exec probe/lifecycle hook 出现失败事件: ${describe_output}"
else
    log_pass "exec probe/lifecycle hook 无失败事件"
fi

#==================== kubectl exec 非交互命令验证 ====================#

log_step "5.1.1 kubectl exec — echo hello（CRI Exec streaming）"
output=$(kubectl exec "${POD_NAME}" -- echo "hello" 2>&1) || true
if echo "${output}" | grep -qiE "error stream protocol|internal error|unable to upgrade"; then
    log_fail "kubectl exec 流式协议错误: ${output}"
elif echo "${output}" | grep -q "hello"; then
    log_pass "kubectl exec echo hello 成功，输出: ${output}"
else
    log_fail "kubectl exec echo hello 失败: ${output}"
fi

log_step "5.1.2 kubectl exec — 多条命令（CRI Exec streaming）"
output=$(kubectl exec "${POD_NAME}" -- sh -c "echo line1; echo line2" 2>&1) || true
if echo "${output}" | grep -qiE "error stream protocol|internal error|unable to upgrade"; then
    log_fail "kubectl exec 流式协议错误: ${output}"
elif echo "${output}" | grep -q "line1" && echo "${output}" | grep -q "line2"; then
    log_pass "kubectl exec 多条命令成功，输出: ${output}"
else
    log_fail "kubectl exec 多条命令失败: ${output}"
fi

log_step "5.1.3 kubectl exec — 检查退出码（CRI Exec streaming）"
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

log_step "5.1.4 kubectl exec — cat /etc/os-release（CRI Exec streaming）"
output=$(kubectl exec "${POD_NAME}" -- cat /etc/os-release 2>&1) || true
if echo "${output}" | grep -qiE "error stream protocol|internal error|unable to upgrade"; then
    log_fail "kubectl exec 流式协议错误: ${output}"
elif echo "${output}" | grep -qE "Ubuntu|NAME="; then
    log_pass "kubectl exec cat /etc/os-release 成功"
else
    log_fail "kubectl exec cat /etc/os-release 失败: ${output}"
fi

#==================== 清理 ====================#
log_step "清理资源"

if kubectl delete pod "${POD_NAME}" --wait=true --timeout=90s >&2; then
    log_pass "Pod 已删除"
else
    log_fail "Pod 未在 90s 内完成删除"
    kubectl delete pod "${POD_NAME}" --force --grace-period=0 --ignore-not-found >&2 || true
fi

log_step "6.1 验证 lifecycle preStop 触发 CRI ExecSync"

wait_execsync_log "${PRESTOP_MARK}" "preStop exec hook" 60 || exit 1

print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    exit 0
fi
exit 1
