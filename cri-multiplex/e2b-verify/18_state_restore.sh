#!/bin/bash
###############################################################################
# 18_state_restore.sh — cri-multiplex kubelet 重启恢复验证
#
# 验证目标：
#   1. 在 CNI+Android runtime 模式下，通过 kubelet 创建 E2B Pod。
#   2. cri-multiplex 重启后不丢失 E2B Pod/Container 路由和状态。
#   3. 重启后 kubectl get / kubectl exec 仍可正常工作。
#   4. 删除 Pod 后 kubelet 触发 RemovePodSandbox，并清理持久化状态。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "18 — cri-multiplex kubelet 重启恢复验证"

POD_NAME="${POD_NAME:-e2b-state-restore-test}"
POD_YAML="${POD_YAML:-/tmp/e2b-kubelet-pod.yaml}"
WORK_POD_YAML="/tmp/e2b-state-restore-pod.yaml"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"
STATE_DIR="${STATE_DIR:-/tmp/cri-multiplex-state-restore}"
STATE_FILE="${STATE_DIR}/state.json"
export STATE_DIR

cleanup_all() {
    kubectl delete pod "${POD_NAME}" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
    rm -f "${WORK_POD_YAML}" || true
}
trap cleanup_all EXIT

wait_pod_ready() {
    local pod_name="$1"
    local timeout_seconds="${2:-120}"
    if kubectl wait --for=condition=Ready "pod/${pod_name}" --timeout="${timeout_seconds}s" >&2; then
        return 0
    fi
    kubectl describe pod "${pod_name}" >&2 || true
    return 1
}

wait_pod_deleted() {
    local pod_name="$1"
    for _ in $(seq 1 60); do
        if ! kubectl get pod "${pod_name}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

wait_remove_podsandbox_log() {
    local sandbox_id="$1"
    for _ in $(seq 1 90); do
        if grep -aq "\\[GrpcE2BEngine\\] RemovePodSandbox: id=${sandbox_id}" /tmp/cri-multiplex.log 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    return 1
}

pod_uid() {
    kubectl get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null || true
}

pod_container_id() {
    local pod_name="$1"
    local uid="$2"
    local cid
    cid=$(kubectl get pod "${pod_name}" -o jsonpath='{.status.containerStatuses[0].containerID}' 2>/dev/null | sed -E 's#^[^:]+://##' || true)
    if [ -n "${cid}" ]; then
        echo "${cid}"
        return 0
    fi
    echo "${uid}-c"
}

assert_kubectl_exec() {
    local pod_name="$1"
    local expected="$2"
    shift 2

    local output
    output=$(kubectl exec "${pod_name}" -- "$@" 2>&1) || true
    if echo "${output}" | grep -q "${expected}"; then
        log_pass "kubectl exec 成功，输出包含 ${expected}"
        return 0
    fi
    log_fail "kubectl exec 失败或输出不匹配: ${output}"
    return 1
}

state_json_matches() {
    local check="$1"
    local pod_id="$2"
    local container_id="$3"

    python3 - "${STATE_FILE}" "${check}" "${pod_id}" "${container_id}" <<'PY'
import json
import sys

path, check, pod_id, container_id = sys.argv[1:5]
with open(path, "r", encoding="utf-8") as f:
    state = json.load(f)

routes = state.get("routes") or []
pods = (state.get("e2b") or {}).get("pods") or []
route_ids = {r.get("id") for r in routes}

def fail(msg):
    print(msg)
    sys.exit(1)

if check == "active":
    if pod_id not in route_ids:
        fail(f"pod route missing: {pod_id}")
    if container_id not in route_ids:
        fail(f"container route missing: {container_id}")
    pod = next((p for p in pods if p.get("sandbox_id") == pod_id), None)
    if not pod:
        fail(f"e2b pod missing: {pod_id}")
    if pod.get("state") != 0:
        fail(f"pod state should be Running(0), got {pod.get('state')}")
    if pod.get("container_state") not in (1, 2):
        fail(f"container state should be Running(1) or Exited(2), got {pod.get('container_state')}")
elif check == "removed":
    if pod_id in route_ids or container_id in route_ids:
        fail(f"removed route still persisted: pod={pod_id} container={container_id}")
    if any(p.get("sandbox_id") == pod_id for p in pods):
        fail(f"removed e2b pod still persisted: {pod_id}")
else:
    fail(f"unknown check {check}")
PY
}

assert_state_json() {
    local desc="$1"
    shift
    local output

    if output=$(state_json_matches "$@" 2>&1); then
        log_pass "${desc}"
        return 0
    fi
    if [ -n "${output}" ]; then
        echo "${output}" >&2
    fi
    log_fail "${desc}"
    return 1
}

log_step "1.1 前置检查"
require_refresh_script "${REFRESH_SCRIPT}" || exit 1
if ! command -v python3 >/dev/null 2>&1; then
    log_fail "python3 不可用，无法解析 state.json"
    exit 1
fi
log_pass "python3 可用"
if ! kubectl get runtimeclass e2b >/dev/null 2>&1; then
    log_fail "RuntimeClass e2b 不存在"
    exit 1
fi
log_pass "RuntimeClass e2b 存在"

log_step "1.2 切换 cri-multiplex 到 CNI+Android runtime 模式"
kubectl delete pod "${POD_NAME}" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
wait_pod_deleted "${POD_NAME}" || true
rm -rf "${STATE_DIR}"
mkdir -p "${STATE_DIR}"
start_cni_android_multiplex "启动 cri-multiplex CNI+Android runtime 模式" || exit 1

log_step "2.1 刷新 build_id 并准备 Kubernetes Pod YAML"
if ! refresh_or_reuse_e2b_yaml "${REFRESH_SCRIPT}" "${POD_NAME}" "${POD_YAML}"; then
    log_fail "刷新或复用 build_id 失败"
    exit 1
fi
cp "${POD_YAML}" "${WORK_POD_YAML}"
reset_e2b_yaml_metadata "${POD_NAME}" "${WORK_POD_YAML}"
log_pass "Kubernetes Pod YAML 已准备完成: ${WORK_POD_YAML}"

log_step "2.2 通过 kubelet 创建 Pod"
if ! kubectl apply -f "${WORK_POD_YAML}" >&2; then
    log_fail "kubectl apply 失败"
    exit 1
fi
wait_pod_ready "${POD_NAME}" 120 || exit 1
POD_UID=$(pod_uid "${POD_NAME}")
if [ -z "${POD_UID}" ]; then
    log_fail "无法读取 Pod UID"
    exit 1
fi
CONTAINER_ID=$(pod_container_id "${POD_NAME}" "${POD_UID}")
POD_IP=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
log_pass "Pod 已 Ready: uid=${POD_UID}, container=${CONTAINER_ID}, podIP=${POD_IP}"

log_step "2.3 重启前状态校验"
assert_state_json "重启前 state.json 已持久化目标 Pod/Container" "active" "${POD_UID}" "${CONTAINER_ID}" || exit 1
assert_kubectl_exec "${POD_NAME}" "state_restore_before" echo "state_restore_before" || exit 1

log_step "3.1 重启 cri-multiplex 并恢复状态"
start_cni_android_multiplex "重启 cri-multiplex CNI+Android runtime 模式" || exit 1

PHASE=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
READY=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null || true)
if [ "${PHASE}" = "Running" ] && [ "${READY}" = "true" ]; then
    log_pass "重启后 kubectl get Pod 仍为 Running/Ready"
else
    log_fail "重启后 Pod 状态异常: phase=${PHASE}, ready=${READY}"
    kubectl describe pod "${POD_NAME}" >&2 || true
    exit 1
fi
assert_state_json "重启后 state.json 仍保留目标 Pod/Container" "active" "${POD_UID}" "${CONTAINER_ID}" || exit 1
assert_kubectl_exec "${POD_NAME}" "state_restore_after" echo "state_restore_after" || exit 1

log_step "4.1 删除 Pod 并验证状态清理"
if ! kubectl delete pod "${POD_NAME}" --wait=true --timeout=90s >&2; then
    log_fail "kubectl delete 未在 90s 内完成"
    kubectl describe pod "${POD_NAME}" >&2 || true
    exit 1
fi
wait_pod_deleted "${POD_NAME}" || {
    log_fail "Pod 未在 60s 内删除"
    exit 1
}
log_pass "Pod 已从 Kubernetes 删除"

if wait_remove_podsandbox_log "${POD_UID}"; then
    log_pass "kubelet 已调用 RemovePodSandbox"
else
    log_fail "未在 90s 内观察到 kubelet 调用 RemovePodSandbox"
    tail -n 80 /tmp/cri-multiplex.log >&2 || true
    exit 1
fi

REMOVED=0
for _ in $(seq 1 90); do
    if state_json_matches "removed" "${POD_UID}" "${CONTAINER_ID}" >/dev/null 2>&1; then
        REMOVED=1
        break
    fi
    sleep 1
done
if [ "${REMOVED}" = "1" ]; then
    log_pass "删除后 state.json 已清理目标 Pod/Container"
else
    assert_state_json "删除后 state.json 已清理目标 Pod/Container" "removed" "${POD_UID}" "${CONTAINER_ID}" || exit 1
fi

print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证通过：CNI+Android runtime 模式下 cri-multiplex 重启后可恢复 E2B kubelet Pod 状态"
    exit 0
else
    exit 1
fi
