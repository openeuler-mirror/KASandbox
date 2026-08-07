#!/bin/bash
###############################################################################
# 19_state_persistence_matrix.sh — 状态持久化 kubelet 黑盒自动化验证
#
# 验证目标：
#   1. 正常路径：通过 kubectl 创建 E2B Pod，确认 state.json 持久化 Pod、
#      Container 路由和 E2B sandbox 状态。
#   2. 重启恢复：重启 cri-multiplex 后，通过 kubectl get / exec 验证 Pod
#      仍可查询、仍可执行命令。
#   3. 边界条件：非法 JSON、单 Pod/单 Container 状态集合。
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
CRASH_POD_NAME="${CRASH_POD_NAME:-e2b-state-crash-19}"
CONCURRENT_E2B_PREFIX="${CONCURRENT_E2B_PREFIX:-e2b-state-burst-19}"
MIXED_E2B_PREFIX="${MIXED_E2B_PREFIX:-e2b-state-mixed-e2b-19}"
MIXED_DEFAULT_PREFIX="${MIXED_DEFAULT_PREFIX:-e2b-state-mixed-default-19}"
POD_YAML="${POD_YAML:-/tmp/e2b-kubelet-pod.yaml}"
WORK_POD_YAML="/tmp/e2b-state-matrix-pod.yaml"
BAD_POD_YAML="/tmp/e2b-state-matrix-invalid-annotation-pod.yaml"
CRASH_POD_YAML="/tmp/e2b-state-crash-19.yaml"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"
STATE_DIR="${STATE_DIR:-/tmp/cri-multiplex-state-matrix}"
STATE_FILE="${STATE_DIR}/state.json"
BAD_STATE_PARENT="${BAD_STATE_PARENT:-/tmp/cri-multiplex-state-parent-file}"
BAD_STATE_DIR="${BAD_STATE_PARENT}/child"
TEST_LABEL_KEY="cri-multiplex-test"
TEST_LABEL_VALUE="19"
DEFAULT_CLIENT_IMAGE="${DEFAULT_CLIENT_IMAGE:-docker.io/library/busybox:latest}"

cleanup_all() {
    kubectl delete pod "${POD_NAME}" "${BAD_POD_NAME}" "${CRASH_POD_NAME}" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete pod -l "${TEST_LABEL_KEY}=${TEST_LABEL_VALUE}" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
    rm -f "${WORK_POD_YAML}" "${BAD_POD_YAML}" "${CRASH_POD_YAML}" /tmp/e2b-state-burst-19-*.yaml /tmp/e2b-state-mixed-*-19-*.yaml || true
}
trap cleanup_all EXIT

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

assert_kubectl_exec() {
    local pod_name="$1"
    local expected="$2"
    shift 2

    local output
    output=$(kubectl_exec_output_with_retry "${pod_name}" 45 "$@" 2>&1) || true
    if echo "${output}" | grep -q "${expected}"; then
        log_pass "kubectl exec 成功，输出包含 ${expected}"
        return 0
    fi
    log_fail "kubectl exec 失败或输出不匹配: ${output}"
    return 1
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
elif check == "routes_active":
    route_ids = {r.get("id") for r in routes}
    missing = [rid for rid in args if rid and rid not in route_ids]
    if missing:
        fail(f"active routes missing: {missing}, routes={routes}")
elif check == "routes_removed":
    route_ids = {r.get("id") for r in routes}
    remaining = [rid for rid in args if rid and rid in route_ids]
    if remaining:
        fail(f"removed routes still persisted: {remaining}, routes={routes}")
elif check == "e2b_active":
    pod_ids = {p.get("sandbox_id") for p in pods}
    missing = [pid for pid in args if pid and pid not in pod_ids]
    if missing:
        fail(f"active e2b pods missing: {missing}, pods={pods}")
    bad = [p for p in pods if p.get("sandbox_id") in args and p.get("state") != 0]
    if bad:
        fail(f"e2b pods should be Running(0): {bad}")
elif check == "e2b_removed":
    pod_ids = {p.get("sandbox_id") for p in pods}
    remaining = [pid for pid in args if pid and pid in pod_ids]
    if remaining:
        fail(f"removed e2b pods still persisted: {remaining}, pods={pods}")
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

add_metadata_label() {
    local yaml="$1"
    local key="$2"
    local value="$3"
    local tmp="${yaml}.tmp"

    awk -v key="${key}" -v value="${value}" '
        /^metadata:/ {
            in_metadata=1
            inserted=0
            print
            next
        }
        in_metadata == 1 && /^  annotations:/ && inserted != 1 {
            print "  labels:"
            print "    " key ": \"" value "\""
            inserted=1
            print
            next
        }
        in_metadata == 1 && /^spec:/ {
            if (inserted != 1) {
                print "  labels:"
                print "    " key ": \"" value "\""
                inserted=1
            }
            in_metadata=0
            print
            next
        }
        { print }
    ' "${yaml}" > "${tmp}"
    mv "${tmp}" "${yaml}"
}

create_e2b_test_yaml() {
    local name="$1"
    local yaml="$2"

    reset_pod_yaml_name "${POD_YAML}" "${yaml}" "${name}"
    add_metadata_label "${yaml}" "${TEST_LABEL_KEY}" "${TEST_LABEL_VALUE}"
}

create_default_test_yaml() {
    local name="$1"
    local yaml="$2"

    cat > "${yaml}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  labels:
    ${TEST_LABEL_KEY}: "${TEST_LABEL_VALUE}"
spec:
  restartPolicy: Never
  containers:
    - name: app
      image: ${DEFAULT_CLIENT_IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["sleep", "3600"]
EOF
}

remove_yaml_annotation() {
    local annotation_key="$1"
    local yaml_file="$2"
    sed -i "\\#^[[:space:]]*${annotation_key}:#d" "${yaml_file}"
}

pod_cri_sandbox_id() {
    local pod_name="$1"
    ${CRICTL} pods --name "${pod_name}" -q 2>/dev/null | head -1 || true
}

collect_route_ids_for_pods() {
    local name sid cid runtime_class

    for name in "$@"; do
        sid=$(pod_cri_sandbox_id "${name}")
        runtime_class=$(kubectl get pod "${name}" -o jsonpath='{.spec.runtimeClassName}' 2>/dev/null || true)
        if [ -z "${sid}" ] && [ "${runtime_class}" = "e2b" ]; then
            sid=$(pod_uid "${name}")
        fi
        cid=$(pod_container_id "${name}" "${sid}")
        [ "${cid}" = "-c" ] && cid=""
        [ -n "${sid}" ] && printf '%s\n' "${sid}"
        [ -n "${cid}" ] && printf '%s\n' "${cid}"
    done
}

collect_e2b_sandbox_ids() {
    local name sid

    for name in "$@"; do
        sid=$(pod_uid "${name}")
        [ -n "${sid}" ] && printf '%s\n' "${sid}"
    done
}

apply_yaml_concurrently() {
    local log_file="$1"
    shift
    local yaml rc

    : > "${log_file}"
    for yaml in "$@"; do
        kubectl apply -f "${yaml}" >>"${log_file}" 2>&1 &
    done
    set +e
    wait
    rc=$?
    set -e
    if [ "${rc}" -ne 0 ] || { [ -s "${log_file}" ] && grep -qiE "error|failed|forbidden|invalid" "${log_file}"; }; then
        log_fail "并发 kubectl apply 存在失败: $(cat "${log_file}")"
        return 1
    fi
}

wait_pods_ready() {
    local timeout_seconds="$1"
    shift
    local name

    for name in "$@"; do
        wait_pod_ready "${name}" "${timeout_seconds}" || return 1
    done
}

delete_pods_concurrently() {
    local log_file="$1"
    shift
    local name rc

    : > "${log_file}"
    for name in "$@"; do
        kubectl delete pod "${name}" --wait=false --grace-period=0 --ignore-not-found >>"${log_file}" 2>&1 &
    done
    set +e
    wait
    rc=$?
    set -e
    if [ "${rc}" -ne 0 ] || { [ -s "${log_file}" ] && grep -qiE "error|failed|forbidden|invalid" "${log_file}"; }; then
        log_fail "并发 kubectl delete 存在失败: $(cat "${log_file}")"
        return 1
    fi
}

wait_pods_deleted() {
    local name

    for name in "$@"; do
        wait_pod_deleted "${name}" || return 1
    done
}

wait_state_routes_removed() {
    local timeout_seconds="$1"
    shift

    for _ in $(seq 1 "${timeout_seconds}"); do
        if state_json_matches "routes_removed" "$@" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

wait_state_e2b_removed() {
    local timeout_seconds="$1"
    shift

    for _ in $(seq 1 "${timeout_seconds}"); do
        if state_json_matches "e2b_removed" "$@" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

log_step "1.1 前置检查"
require_refresh_script_quiet "${REFRESH_SCRIPT}" || exit 1
if ! command -v python3 >/dev/null 2>&1; then
    log_info "python3 不可用，无法解析 state.json"
    exit 1
fi
log_info "python3 可用"

if ! kubectl get runtimeclass e2b >/dev/null 2>&1; then
    log_info "RuntimeClass e2b 不存在"
    exit 1
fi
log_info "RuntimeClass e2b 存在"

if [ ! -f "${MULTIPLEX_DIR}/pkg/engine/state_store.go" ]; then
    log_info "状态持久化实现文件不存在: ${MULTIPLEX_DIR}/pkg/engine/state_store.go"
    exit 1
fi
log_info "状态持久化实现文件存在"

if [ ! -d "${CNI_CONF_DIR}" ] || [ ! -d "${CNI_BIN_DIR}" ]; then
    log_info "CNI 配置或二进制目录不存在: ${CNI_CONF_DIR}, ${CNI_BIN_DIR}"
    exit 1
fi
log_info "CNI 配置目录和二进制目录存在"

log_step "1.2 清理旧 Pod 和旧状态"
kubectl delete pod "${POD_NAME}" "${BAD_POD_NAME}" "${CRASH_POD_NAME}" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
kubectl delete pod -l "${TEST_LABEL_KEY}=${TEST_LABEL_VALUE}" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
wait_pod_deleted "${POD_NAME}" || true
wait_pod_deleted "${BAD_POD_NAME}" || true
wait_pod_deleted "${CRASH_POD_NAME}" || true
rm -rf "${STATE_DIR}"
mkdir -p "${STATE_DIR}"
log_pass "旧 Pod 和测试 state-dir 已清理"

log_step "1.4 非法 JSON 启动容错"
printf '{"version":1,' > "${STATE_FILE}"
start_cni_android_multiplex "非法 JSON 后启动 cri-multiplex" || exit 1
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
start_cni_android_multiplex "重启 cri-multiplex 恢复 Kubernetes Pod 状态" || exit 1

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
        kubectl_exec_output_with_retry "${POD_NAME}" 45 echo "parallel-${i}" 2>/dev/null | grep -q "parallel-${i}"
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

log_step "4.2 并发创建/删除 10 个 E2B Pod 验证状态持久化并发安全"
burst_names=()
burst_yamls=()
for i in $(seq 1 10); do
    name="${CONCURRENT_E2B_PREFIX}-${i}"
    yaml="/tmp/${name}.yaml"
    burst_names+=("${name}")
    burst_yamls+=("${yaml}")
    create_e2b_test_yaml "${name}" "${yaml}"
done
apply_yaml_concurrently "/tmp/e2b-state-burst-19-apply.log" "${burst_yamls[@]}" || exit 1
wait_pods_ready 240 "${burst_names[@]}" || exit 1
mapfile -t burst_e2b_ids < <(collect_e2b_sandbox_ids "${burst_names[@]}")
mapfile -t burst_route_ids < <(collect_route_ids_for_pods "${burst_names[@]}")
if [ "${#burst_e2b_ids[@]}" -eq 10 ]; then
    log_pass "10 个 E2B Pod UID 已收集"
else
    log_fail "E2B Pod UID 数量不符合预期: ${#burst_e2b_ids[@]}"
    exit 1
fi
assert_state_json "10 个 E2B Pod Running 状态已持久化" "e2b_active" "${burst_e2b_ids[@]}" || exit 1
assert_state_json "10 个 E2B Pod/Container 路由已持久化" "routes_active" "${burst_route_ids[@]}" || exit 1
delete_pods_concurrently "/tmp/e2b-state-burst-19-delete.log" "${burst_names[@]}" || exit 1
wait_pods_deleted "${burst_names[@]}" || exit 1
wait_state_routes_removed 120 "${burst_route_ids[@]}" || {
    assert_state_json "10 个 E2B Pod/Container 路由已清理" "routes_removed" "${burst_route_ids[@]}" || exit 1
}
wait_state_e2b_removed 120 "${burst_e2b_ids[@]}" || {
    assert_state_json "10 个 E2B Pod 状态已清理" "e2b_removed" "${burst_e2b_ids[@]}" || exit 1
}
log_pass "10 个 E2B Pod 并发创建/删除后状态已收敛"

log_step "4.3 混合 5 个 E2B Pod + 5 个普通 Pod 并发创建/删除验证路由互不干扰"
mixed_names=()
mixed_e2b_names=()
mixed_yamls=()
for i in $(seq 1 5); do
    e2b_name="${MIXED_E2B_PREFIX}-${i}"
    default_name="${MIXED_DEFAULT_PREFIX}-${i}"
    e2b_yaml="/tmp/${e2b_name}.yaml"
    default_yaml="/tmp/${default_name}.yaml"
    mixed_names+=("${e2b_name}" "${default_name}")
    mixed_e2b_names+=("${e2b_name}")
    mixed_yamls+=("${e2b_yaml}" "${default_yaml}")
    create_e2b_test_yaml "${e2b_name}" "${e2b_yaml}"
    create_default_test_yaml "${default_name}" "${default_yaml}"
done
apply_yaml_concurrently "/tmp/e2b-state-mixed-19-apply.log" "${mixed_yamls[@]}" || exit 1
wait_pods_ready 240 "${mixed_names[@]}" || exit 1
mapfile -t mixed_e2b_ids < <(collect_e2b_sandbox_ids "${mixed_e2b_names[@]}")
mapfile -t mixed_route_ids < <(collect_route_ids_for_pods "${mixed_names[@]}")
if [ "${#mixed_e2b_ids[@]}" -eq 5 ]; then
    log_pass "5 个混合场景 E2B Pod UID 已收集"
else
    log_fail "混合场景 E2B Pod UID 数量不符合预期: ${#mixed_e2b_ids[@]}"
    exit 1
fi
assert_state_json "混合场景 E2B Pod Running 状态已持久化" "e2b_active" "${mixed_e2b_ids[@]}" || exit 1
assert_state_json "混合场景 E2B/containerd 路由已持久化" "routes_active" "${mixed_route_ids[@]}" || exit 1
delete_pods_concurrently "/tmp/e2b-state-mixed-19-delete.log" "${mixed_names[@]}" || exit 1
wait_pods_deleted "${mixed_names[@]}" || exit 1
wait_state_routes_removed 120 "${mixed_route_ids[@]}" || {
    assert_state_json "混合场景 E2B/containerd 路由已清理" "routes_removed" "${mixed_route_ids[@]}" || exit 1
}
wait_state_e2b_removed 120 "${mixed_e2b_ids[@]}" || {
    assert_state_json "混合场景 E2B Pod 状态已清理" "e2b_removed" "${mixed_e2b_ids[@]}" || exit 1
}
log_pass "混合 5+5 并发创建/删除后状态已收敛"

log_step "4.4 kill -9 cri-multiplex 后验证状态恢复和后续删除"
create_e2b_test_yaml "${CRASH_POD_NAME}" "${CRASH_POD_YAML}"
kubectl apply -f "${CRASH_POD_YAML}" >&2
wait_pod_ready "${CRASH_POD_NAME}" 120 || exit 1
CRASH_UID=$(pod_uid "${CRASH_POD_NAME}")
CRASH_CONTAINER_ID=$(pod_container_id "${CRASH_POD_NAME}" "${CRASH_UID}")
if [ -z "${CRASH_UID}" ] || [ -z "${CRASH_CONTAINER_ID}" ]; then
    log_fail "无法读取崩溃恢复场景 Pod UID/ContainerID: uid=${CRASH_UID}, cid=${CRASH_CONTAINER_ID}"
    exit 1
fi
assert_state_json "崩溃前 Pod/Container 状态已持久化" "k8s_active" "${CRASH_UID}" "${CRASH_CONTAINER_ID}" || exit 1
crash_pids=$(cri_multiplex_pids)
if [ -z "${crash_pids}" ]; then
    log_fail "未找到待 kill -9 的 cri-multiplex 进程"
    exit 1
fi
log_info "kill -9 cri-multiplex 进程: ${crash_pids}"
kill -9 ${crash_pids} 2>/dev/null || true
sleep 2
start_cni_android_multiplex "kill -9 后重启 cri-multiplex 恢复状态" || exit 1
assert_state_json "kill -9 重启后 Pod/Container 状态已恢复" "k8s_active" "${CRASH_UID}" "${CRASH_CONTAINER_ID}" || exit 1
assert_kubectl_exec "${CRASH_POD_NAME}" "crash_restore_ok" echo "crash_restore_ok" || exit 1
kubectl delete pod "${CRASH_POD_NAME}" --wait=true --timeout=90s >&2 || {
    log_fail "kill -9 恢复后的 Pod 删除失败"
    kubectl describe pod "${CRASH_POD_NAME}" >&2 || true
    exit 1
}
wait_pod_deleted "${CRASH_POD_NAME}" || exit 1
if wait_state_routes_removed 120 "${CRASH_UID}" "${CRASH_CONTAINER_ID}" &&
   wait_state_e2b_removed 120 "${CRASH_UID}"; then
    log_pass "kill -9 恢复后 Pod 删除与状态清理成功"
else
    assert_state_json "kill -9 恢复后 Pod/Container 路由已清理" "removed" "${CRASH_UID}" "${CRASH_CONTAINER_ID}" || exit 1
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
STATE_DIR="${BAD_STATE_DIR}" CNI_ENABLED=1 E2B_FORCE_RESTART=1 "${SCRIPT_DIR}/01_start_multiplex.sh" >"${bad_output_file}" 2>&1
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
start_cni_android_multiplex "清理测试状态后重启 cri-multiplex" || exit 1
assert_state_json "测试结束后目标 Pod/Container 状态已清理" "removed" "${POD_UID}" "${CONTAINER_ID}" || exit 1

print_summary

if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证通过：状态持久化 kubelet 黑盒自动化场景全部通过"
    exit 0
else
    exit 1
fi
