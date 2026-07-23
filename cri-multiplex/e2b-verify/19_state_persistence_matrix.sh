#!/bin/bash
###############################################################################
# 19_state_persistence_matrix.sh — 状态持久化 kubelet 黑盒自动化验证
#
# 验证目标：
#   1. 正常路径：通过 kubectl 创建 E2B Pod，确认 state.json 持久化 Pod、
#      Container 路由和 E2B sandbox 状态。
#   2. 重启恢复：重启 cri-multiplex 后，通过 kubectl get / exec 验证 Pod
#      仍可查询、仍可执行命令。
#   3. 边界条件：空 state-dir、非法 JSON、单 Pod/单 Container 状态集合。
#   4. 异常处理：非法 Pod annotation、非法 state-dir 导致 kubelet/进程启动失败。
#   5. 并发安全：并发 kubectl get / exec 访问恢复后的 Pod，不死锁、不损坏状态。
#   6. 状态依赖：Running -> kubectl delete -> Removed，确认状态和路由清理。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "19 — 状态持久化 kubelet 黑盒自动化验证"

POD_NAME="${POD_NAME:-e2b-state-matrix-test}"
BAD_POD_NAME="${BAD_POD_NAME:-e2b-state-matrix-invalid-annotation}"
POD_YAML="${POD_YAML:-/tmp/e2b-kubelet-pod.yaml}"
WORK_POD_YAML="/tmp/e2b-state-matrix-pod.yaml"
BAD_POD_YAML="/tmp/e2b-state-matrix-invalid-annotation-pod.yaml"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"
STATE_DIR="${STATE_DIR:-/tmp/cri-multiplex-state-matrix}"
STATE_FILE="${STATE_DIR}/state.json"
BAD_STATE_PARENT="${BAD_STATE_PARENT:-/tmp/cri-multiplex-state-parent-file}"
BAD_STATE_DIR="${BAD_STATE_PARENT}/child"

cleanup_all() {
    kubectl delete pod "${POD_NAME}" --force --grace-period=0 >/dev/null 2>&1 || true
    kubectl delete pod "${BAD_POD_NAME}" --force --grace-period=0 >/dev/null 2>&1 || true
    rm -f "${WORK_POD_YAML}" "${BAD_POD_YAML}" || true
}
trap cleanup_all EXIT

start_cni_multiplex() {
    local desc="$1"
    if ! start_cni_android_multiplex "${desc}"; then
        return 1
    fi
}

wait_pod_deleted() {
    local pod_name="$1"
    for _ in $(seq 1 30); do
        if ! kubectl get pod "${pod_name}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

wait_remove_podsandbox_log() {
    local sandbox_id="$1"
    local timeout_seconds="${2:-90}"
    local log_file="${3:-/tmp/cri-multiplex.log}"

    for _ in $(seq 1 "${timeout_seconds}"); do
        if grep -aq "\\[GrpcE2BEngine\\] RemovePodSandbox: id=${sandbox_id}" "${log_file}" 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    return 1
}

wait_pod_ready() {
    local pod_name="$1"
    local timeout_seconds="${2:-90}"
    if kubectl wait --for=condition=Ready "pod/${pod_name}" --timeout="${timeout_seconds}s" >&2; then
        return 0
    fi
    kubectl describe pod "${pod_name}" >&2 || true
    return 1
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

state_json_matches() {
    python3 - "${STATE_FILE}" "$@" <<'PY'
import json
import sys

path = sys.argv[1]
check = sys.argv[2]
args = sys.argv[3:]

with open(path, "r", encoding="utf-8") as f:
    state = json.load(f)

routes = state.get("routes") or []
e2b = state.get("e2b") or {}
pods = e2b.get("pods") or []
images = e2b.get("images") or []
android = state.get("android") or {}
android_pods = android.get("pods") or []

def fail(msg):
    print(msg)
    sys.exit(1)

if check == "empty":
    if routes or pods or images or android_pods:
        fail(f"state is not empty: routes={len(routes)} e2b_pods={len(pods)} images={len(images)} android_pods={len(android_pods)}")
elif check == "k8s_active":
    pod_id, container_id = args
    route_ids = {r.get("id") for r in routes}
    if pod_id not in route_ids:
        fail(f"pod route missing: {pod_id}, routes={routes}")
    if container_id not in route_ids:
        fail(f"container route missing: {container_id}, routes={routes}")
    pod = next((p for p in pods if p.get("sandbox_id") == pod_id), None)
    if not pod:
        fail(f"e2b pod missing: {pod_id}, pods={pods}")
    if pod.get("state") != 0:
        fail(f"pod state should be Running(0), got {pod.get('state')}")
    if pod.get("container_state") not in (1, 2):
        fail(f"container state should be Running(1) or Exited(2), got {pod.get('container_state')}")
    if pod.get("container_name") == "":
        fail(f"container_name should be persisted: {pod}")
elif check == "removed":
    pod_id, container_id = args
    route_ids = {r.get("id") for r in routes}
    if pod_id in route_ids or container_id in route_ids:
        fail(f"removed routes still persisted: pod={pod_id} container={container_id} routes={routes}")
    if any(p.get("sandbox_id") == pod_id for p in pods):
        fail(f"removed e2b pod still persisted: {pod_id}, pods={pods}")
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

write_empty_state() {
    mkdir -p "${STATE_DIR}"
    cat > "${STATE_FILE}" <<'EOF'
{
  "version": 1,
  "e2b": {},
  "android": {}
}
EOF
}

reset_pod_yaml_name() {
    local src="$1"
    local dst="$2"
    local name="$3"
    cp "${src}" "${dst}"
    reset_e2b_yaml_metadata "${name}" "${dst}"
}

remove_yaml_annotation() {
    local annotation_key="$1"
    local yaml_file="$2"
    sed -i "\\#^[[:space:]]*${annotation_key}:#d" "${yaml_file}"
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

if [ ! -f "${MULTIPLEX_DIR}/pkg/engine/state_store.go" ]; then
    log_fail "状态持久化实现文件不存在: ${MULTIPLEX_DIR}/pkg/engine/state_store.go"
    exit 1
fi
log_pass "状态持久化实现文件存在"

if [ ! -d "${CNI_CONF_DIR}" ] || [ ! -d "${CNI_BIN_DIR}" ]; then
    log_fail "CNI 配置或二进制目录不存在: ${CNI_CONF_DIR}, ${CNI_BIN_DIR}"
    exit 1
fi
log_pass "CNI 配置目录和二进制目录存在"

log_step "1.2 清理旧 Pod 和旧状态"
kubectl delete pod "${POD_NAME}" --force --grace-period=0 >/dev/null 2>&1 || true
kubectl delete pod "${BAD_POD_NAME}" --force --grace-period=0 >/dev/null 2>&1 || true
wait_pod_deleted "${POD_NAME}" || true
wait_pod_deleted "${BAD_POD_NAME}" || true
rm -rf "${STATE_DIR}"
mkdir -p "${STATE_DIR}"
log_pass "旧 Pod 和测试 state-dir 已清理"

log_step "1.3 空状态目录启动"
start_cni_multiplex "空状态目录启动 cri-multiplex" || exit 1
write_empty_state
assert_state_json "空集合状态文件可解析" "empty" || exit 1

log_step "1.4 非法 JSON 启动容错"
printf '{"version":1,' > "${STATE_FILE}"
start_cni_multiplex "非法 JSON 后启动 cri-multiplex" || exit 1
if grep -q "failed to load ${STATE_FILE}" /tmp/cri-multiplex.log 2>/dev/null; then
    log_pass "非法 JSON 被识别并降级为空状态"
else
    log_fail "未发现非法 JSON 降级日志"
    exit 1
fi

log_step "2.1 刷新 build_id 并准备 Kubernetes Pod YAML"
if ! refresh_or_reuse_e2b_yaml "${REFRESH_SCRIPT}" "${POD_NAME}" "${POD_YAML}"; then
    log_fail "刷新或复用 build_id 失败"
    exit 1
fi
reset_pod_yaml_name "${POD_YAML}" "${WORK_POD_YAML}" "${POD_NAME}"
log_pass "Kubernetes Pod YAML 已准备完成: ${WORK_POD_YAML}"

log_step "2.2 通过 kubectl 创建 Pod"
CREATE_TIME=$(date +%s)
if ! kubectl apply -f "${WORK_POD_YAML}" >&2; then
    log_fail "kubectl apply 失败"
    exit 1
fi
log_pass "Pod YAML 已提交: ${POD_NAME}"

if ! wait_pod_ready "${POD_NAME}" 120; then
    log_fail "Pod 未在 120s 内 Ready"
    exit 1
fi
log_pass "Pod 已 Ready"

POD_UID=$(pod_uid "${POD_NAME}")
if [ -z "${POD_UID}" ]; then
    log_fail "无法读取 Pod UID"
    exit 1
fi
CONTAINER_ID=$(pod_container_id "${POD_NAME}" "${POD_UID}")
POD_IP=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
if [ -z "${POD_IP}" ]; then
    log_fail "无法读取 PodIP"
    exit 1
fi
log_pass "Pod UID/ContainerID/PodIP 已读取: ${POD_UID}, ${CONTAINER_ID}, ${POD_IP}"

assert_state_json "单 Pod/单 Container Running 状态已持久化" "k8s_active" "${POD_UID}" "${CONTAINER_ID}" || exit 1
assert_kubectl_exec "${POD_NAME}" "state_create_ok" echo "state_create_ok" || exit 1

log_step "3.1 重启 cri-multiplex 后通过 kubectl 查询和 exec 验证恢复"
start_cni_multiplex "重启 cri-multiplex 恢复 Kubernetes Pod 状态" || exit 1

PHASE=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
READY=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null || true)
if [ "${PHASE}" = "Running" ] && [ "${READY}" = "true" ]; then
    log_pass "重启后 kubectl get Pod 仍为 Running/Ready"
else
    log_fail "重启后 Pod 状态异常: phase=${PHASE}, ready=${READY}"
    kubectl describe pod "${POD_NAME}" >&2 || true
    exit 1
fi
assert_state_json "重启后 state.json 仍保留 Pod/Container/CNI 状态" "k8s_active" "${POD_UID}" "${CONTAINER_ID}" || exit 1
assert_kubectl_exec "${POD_NAME}" "state_restore_ok" echo "state_restore_ok" || exit 1

log_step "3.2 并发 kubectl get / exec 验证恢复后访问安全"
concurrent_log="/tmp/cri-multiplex-state-matrix-kubectl-concurrent.log"
: > "${concurrent_log}"
for i in $(seq 1 12); do
    (
        kubectl get pod "${POD_NAME}" -o jsonpath='{.status.phase}' >/dev/null 2>&1 &&
        kubectl get pod "${POD_NAME}" -o jsonpath='{.status.podIP}' >/dev/null 2>&1
    ) || echo "get-worker-${i} failed" >> "${concurrent_log}" &
done
for i in $(seq 1 5); do
    (
        kubectl exec "${POD_NAME}" -- echo "parallel-${i}" 2>/dev/null | grep -q "parallel-${i}"
    ) || echo "exec-worker-${i} failed" >> "${concurrent_log}" &
done
wait
if [ ! -s "${concurrent_log}" ]; then
    log_pass "并发 kubectl get/exec 全部成功"
else
    log_fail "并发 kubectl get/exec 存在失败: $(cat "${concurrent_log}")"
    exit 1
fi
assert_state_json "并发访问后 state.json 未损坏" "k8s_active" "${POD_UID}" "${CONTAINER_ID}" || exit 1

log_step "4.1 通过 kubectl delete 验证 Removed 状态清理"
if ! kubectl delete pod "${POD_NAME}" --wait=true --timeout=90s >&2; then
    log_fail "kubectl delete 未在 90s 内完成"
    kubectl describe pod "${POD_NAME}" >&2 || true
    exit 1
fi
if wait_pod_deleted "${POD_NAME}"; then
    log_pass "Pod 已从 Kubernetes 删除"
else
    log_fail "Pod 未在 30s 内删除"
    kubectl describe pod "${POD_NAME}" >&2 || true
    exit 1
fi

if wait_remove_podsandbox_log "${POD_UID}" 90; then
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
    log_pass "Remove 后 Pod/Container 状态和路由已删除"
else
    assert_state_json "Remove 后 Pod/Container 状态和路由已删除" "removed" "${POD_UID}" "${CONTAINER_ID}" || exit 1
fi

log_step "5.1 非法参数：缺少 template-id 时 kubelet 创建 Pod 失败应可观测"
reset_pod_yaml_name "${POD_YAML}" "${BAD_POD_YAML}" "${BAD_POD_NAME}"
remove_yaml_annotation "e2b.dev/template-id" "${BAD_POD_YAML}"

kubectl apply -f "${BAD_POD_YAML}" >&2 || {
    log_fail "非法参数场景 kubectl apply 失败，预期应提交成功后由 kubelet 报 sandbox 创建失败"
    exit 1
}
if kubectl wait --for=condition=Ready "pod/${BAD_POD_NAME}" --timeout=25s >/tmp/e2b-state-matrix-bad-wait.log 2>&1; then
    log_fail "缺少 template-id 时 Pod 不应 Ready"
    kubectl describe pod "${BAD_POD_NAME}" >&2 || true
    exit 1
fi
bad_describe=$(kubectl describe pod "${BAD_POD_NAME}" 2>&1 || true)
if echo "${bad_describe}" | grep -qiE "FailedCreatePodSandBox|missing required e2b annotations|InvalidArgument|template-id"; then
    log_pass "非法参数通过 kubectl describe 可观测"
else
    log_fail "非法参数未在 Pod 事件中体现: ${bad_describe}"
    exit 1
fi
kubectl delete pod "${BAD_POD_NAME}" --force --grace-period=0 >&2 || true
wait_pod_deleted "${BAD_POD_NAME}" || true

log_step "5.2 非法 state-dir 启动失败"
rm -rf "${BAD_STATE_PARENT}"
printf 'not-a-dir' > "${BAD_STATE_PARENT}"
bad_output_file="/tmp/cri-multiplex-state-matrix-bad-state-dir.log"
set +e
STATE_DIR="${BAD_STATE_DIR}" E2B_CNI_ENABLED=1 E2B_FORCE_RESTART=1 "${SCRIPT_DIR}/01_start_multiplex.sh" >"${bad_output_file}" 2>&1
bad_code=$?
set -e
bad_output=$(tr -d '\000' < "${bad_output_file}" 2>/dev/null || true)
if [ "${bad_code}" -ne 0 ] && echo "${bad_output}" | grep -qi "not a directory\\|create state dir\\|failed"; then
    log_pass "非法 state-dir 返回失败"
else
    log_fail "非法 state-dir 未按预期失败，code=${bad_code}, output=${bad_output}"
    exit 1
fi

log_step "6.1 清理测试状态并恢复 CNI 模式 cri-multiplex"
rm -rf "${STATE_DIR}"
mkdir -p "${STATE_DIR}"
write_empty_state
start_cni_multiplex "清理测试状态后重启 cri-multiplex" || exit 1
assert_state_json "测试结束后目标 Pod/Container 状态已清理" "removed" "${POD_UID}" "${CONTAINER_ID}" || exit 1

print_summary

if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证通过：状态持久化 kubelet 黑盒自动化场景全部通过"
    exit 0
else
    exit 1
fi
