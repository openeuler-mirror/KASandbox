#!/bin/bash
###############################################################################
# 20_cleanup_orphan_recovery.sh -- 清理与孤儿资源回收黑盒验证
#
# 默认使用已经存在的 cri-multiplex 二进制和 E2B 模板，不调用
# build_prod.py 或 go build；E2B Pod 使用 07 号流程的 kubelet fixture。
# 如需刷新 build_id，可显式设置 E2B_SKIP_BUILD=0；该模式仍不会 go build。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "20 -- 清理与孤儿资源回收验证"

STATE_DIR="${STATE_DIR:-/tmp/cri-multiplex-cleanup-state}"
STATE_FILE="${STATE_DIR}/state.json"
ANDROID_STATE_DIR="${ANDROID_STATE_DIR:-/tmp/cri-multiplex-cleanup-android}"
ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR:-/home/fjq/cf17}"
E2B_POD_JSON="${E2B_POD_JSON:-/tmp/e2b-pod.json}"
POD_JSON="${E2B_POD_JSON}"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"
E2B_SKIP_BUILD="${E2B_SKIP_BUILD:-1}"
CRICTL="${CRICTL} --timeout ${E2B_CREATE_TIMEOUT:-180s}"
UNKNOWN_WORKDIR="${ANDROID_STATE_DIR}/android-similar-no-owner"
NONOWNER_COMMENT="cri-multiplex-similar-no-owner-20"
ORCH_PROTO="${MULTIPLEX_DIR}/proto/orchestrator.proto"
E2B_POD_TEMPLATE="${E2B_POD_TEMPLATE:-/tmp/e2b-kubelet-pod.yaml}"
ANDROID_WAIT_TIMEOUT="${ANDROID_WAIT_TIMEOUT:-360s}"
BROKEN_CNI_CONF_DIR="${BROKEN_CNI_CONF_DIR:-/tmp/cri-multiplex-20-empty-cni}"

cleanup_all() {
    local pids
    pids=$(cri_multiplex_pids)
    [ -z "${pids}" ] || kill ${pids} >/dev/null 2>&1 || true
    rm -f "${SOCKET}" || true
    kubectl delete pod -l cri-multiplex-test=20 --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
    rm -f /tmp/cri-multiplex-20-*.yaml /tmp/cri-multiplex-20-*.credentials.json || true
    iptables -t filter -D OUTPUT -p tcp -d 127.0.0.1 --dport 1 \
        -m comment --comment "${NONOWNER_COMMENT}" -j ACCEPT >/dev/null 2>&1 || true
    rm -rf "${STATE_DIR}" "${ANDROID_STATE_DIR}" "${BROKEN_CNI_CONF_DIR}" || true
}
trap cleanup_all EXIT

fail_context() {
    echo "--- state.json ---" >&2
    sed -n '1,260p' "${STATE_FILE}" >&2 2>/dev/null || true
    echo "--- cri-multiplex.log ---" >&2
    tail -n 160 /tmp/cri-multiplex.log >&2 2>/dev/null || true
}

start_cleanup_multiplex() {
    STATE_DIR="${STATE_DIR}" \
    ANDROID_STATE_DIR="${ANDROID_STATE_DIR}" \
    ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR}" \
    ANDROID_ENABLED=1 \
    CNI_ENABLED=1 \
    NODE_IP="${NODE_IP:-}" \
    E2B_SKIP_BUILD=1 \
    E2B_FORCE_RESTART=1 \
    E2B_NON_VALIDATION_STARTUP=1 \
    ORPHAN_RECONCILE_ENABLED=1 \
    ORPHAN_RECONCILE_INTERVAL=60s \
    ORPHAN_GRACE_PERIOD=0s \
    "${SCRIPT_DIR}/01_start_multiplex.sh"
}

stop_test_multiplex() {
    local pids
    pids=$(cri_multiplex_pids)
    [ -z "${pids}" ] || kill ${pids} >/dev/null 2>&1 || true
    for _ in $(seq 1 10); do
        [ -z "$(cri_multiplex_pids)" ] && break
        sleep 1
    done
    rm -f "${SOCKET}" || true
}

assert_empty_response() {
    echo "${1}" | grep -qE '^\{\}|^$'
}

wait_pod_ready() {
    local name="$1"
    if ! kubectl wait --for=condition=Ready "pod/${name}" --timeout="${ANDROID_WAIT_TIMEOUT}" >&2; then
        kubectl describe pod "${name}" >&2 || true
        tail -n 160 /tmp/cri-multiplex.log >&2 || true
        return 1
    fi
}

run_e2b_sandbox() {
    local name="$1"
    local yaml="/tmp/cri-multiplex-20-${name}.yaml" id
    local refresh_script="${REFRESH_SCRIPT}"

    # 与 07 号用例保持相同的 build_id/YAML 准备流程。20 号默认跳过
    # build_prod.py，但仍通过同一个 helper 校验并重置可复用 fixture。
    if [ "${E2B_SKIP_BUILD}" = "1" ]; then
        cp "${E2B_POD_TEMPLATE}" "${yaml}"
    fi
    if ! POD_YAML="${yaml}" refresh_or_reuse_e2b_yaml "${refresh_script}" "${name}" "${yaml}"; then
        echo "无法刷新或复用 E2B Pod YAML: ${yaml}" >&2
        rm -f "${yaml}"
        return 1
    fi
    if [ ! -f "${yaml}" ]; then
        log_fail "Pod YAML 未生成: ${yaml}"
        return 1
    fi
    local build_id
    build_id=$(grep -oP 'e2b\.dev/build-id:\s*"\K[^"]+' "${yaml}" | head -1 || true)
    if [ -z "${build_id}" ]; then
        log_fail "无法从 YAML 提取 build_id: ${yaml}"
        return 1
    fi
    log_pass "build_id 已准备: ${build_id}"
    if ! grep -q '^    e2b.dev/expose-ports:' "${yaml}"; then
        local annotated_yaml="${yaml}.annotated"
        awk -v ports="${E2B_EXPOSE_PORTS:-49983}" '
            /^  annotations:$/ {
                print
                print "    e2b.dev/expose-ports: \"" ports "\""
                next
            }
            { print }
        ' "${yaml}" > "${annotated_yaml}"
        mv "${annotated_yaml}" "${yaml}"
    fi
    if ! kubectl apply -f "${yaml}" >&2 2>&1; then
        log_fail "kubectl apply 失败: ${name}"
        return 1
    fi
    log_pass "Pod YAML 已提交: ${name}"
    kubectl label pod "${name}" cri-multiplex-test=20 --overwrite >/dev/null
    wait_pod_ready "${name}" || return 1
    id=$(kubectl get pod "${name}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
    [ -n "${id}" ] || return 1
    echo "${id}"
}

set_yaml_annotation() {
    local key="$1" value="$2" yaml="$3" tmp="${yaml}.tmp"

    awk -v key="${key}:" -v value="${value}" '
        $1 == key {
            print "    " key " \"" value "\""
            next
        }
        { print }
    ' "${yaml}" > "${tmp}"
    mv "${tmp}" "${yaml}"
}

run_e2b_create_failure_pod() {
    local name="$1"
    local yaml="/tmp/cri-multiplex-20-${name}.yaml" id
    local bad_build_id="missing-build-id-20-${RANDOM}-${RANDOM}"

    if [ "${E2B_SKIP_BUILD}" = "1" ]; then
        cp "${E2B_POD_TEMPLATE}" "${yaml}"
    fi
    if ! POD_YAML="${yaml}" refresh_or_reuse_e2b_yaml "${REFRESH_SCRIPT}" "${name}" "${yaml}"; then
        echo "无法刷新或复用 E2B Pod YAML: ${yaml}" >&2
        rm -f "${yaml}"
        return 1
    fi
    set_yaml_annotation "e2b.dev/build-id" "${bad_build_id}" "${yaml}"
    if ! kubectl apply -f "${yaml}" >&2 2>&1; then
        log_fail "创建失败回滚场景 kubectl apply 失败: ${name}"
        return 1
    fi
    kubectl label pod "${name}" cri-multiplex-test=20 --overwrite >/dev/null
    id=$(kubectl get pod "${name}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
    [ -n "${id}" ] || return 1
    echo "${id}"
}

submit_e2b_pod_no_wait() {
    local name="$1"
    local yaml="/tmp/cri-multiplex-20-${name}.yaml" id

    if [ "${E2B_SKIP_BUILD}" = "1" ]; then
        cp "${E2B_POD_TEMPLATE}" "${yaml}"
    fi
    if ! POD_YAML="${yaml}" refresh_or_reuse_e2b_yaml "${REFRESH_SCRIPT}" "${name}" "${yaml}"; then
        echo "无法刷新或复用 E2B Pod YAML: ${yaml}" >&2
        rm -f "${yaml}"
        return 1
    fi
    if ! kubectl apply -f "${yaml}" >&2 2>&1; then
        log_fail "kubectl apply 失败: ${name}"
        return 1
    fi
    kubectl label pod "${name}" cri-multiplex-test=20 --overwrite >/dev/null
    id=$(kubectl get pod "${name}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
    [ -n "${id}" ] || return 1
    echo "${id}"
}

e2b_resources() {
    python3 - "${STATE_FILE}" "$1" "$(effective_node_ip)" <<'PY'
import json, sys
path, sandbox_id, node_ip = sys.argv[1:]
try:
    state = json.load(open(path, encoding="utf-8"))
except (FileNotFoundError, json.JSONDecodeError):
    raise SystemExit(1)
for pod in (state.get("e2b") or {}).get("pods") or []:
    if pod.get("sandbox_id") != sandbox_id:
        continue
    record = pod.get("cni_record") or {}
    print(record.get("netns_path") or record.get("NetNSPath", ""))
    mappings = pod.get("port_mappings") or []
    if not mappings and pod.get("host_port", 0):
        mappings = [{"host_port": pod["host_port"], "sandbox_port": 49983}]
    node_ip = node_ip.replace(":", "_").replace(" ", "_")
    host_ip = (pod.get("host_ip") or "").replace(":", "_").replace(" ", "_")
    for mapping in mappings:
        print("cri-multiplex:hostport:%s:%d:%s:%d" %
              (node_ip, mapping.get("host_port", 0), host_ip,
               mapping.get("sandbox_port", 0)))
    raise SystemExit(0)
raise SystemExit(1)
PY
}

e2b_orchestrator_id() {
    python3 - "${STATE_FILE}" "$1" <<'PY'
import json, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
for pod in (state.get("e2b") or {}).get("pods") or []:
    if pod.get("sandbox_id") == sys.argv[2]:
        remote_id = pod.get("e2b_sandbox_id", "")
        if remote_id:
            print(remote_id)
            raise SystemExit(0)
raise SystemExit(1)
PY
}

effective_node_ip() {
    if [ -n "${NODE_IP:-}" ]; then
        echo "${NODE_IP}"
        return 0
    fi
    local detected
    detected=$(grep -aoP 'auto-detected node-ip: \K[^[:space:]]+' /tmp/cri-multiplex.log 2>/dev/null | tail -1 || true)
    if [ -n "${detected}" ]; then
        echo "${detected}"
        return 0
    fi
    hostname -I 2>/dev/null | awk '{print $1}'
}

run_android_sandbox() {
    local name="$1" adb_port="${2:-${ANDROID_ADB_PORT_START:-6520}}"
    local yaml="/tmp/cri-multiplex-20-${name}.yaml" id
    cat > "${yaml}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  labels:
    cri-multiplex-test: "20"
  annotations:
    android.dev/adb-port: "${adb_port}"
    android.dev/base-instance-num: "${ANDROID_BASE_INSTANCE_NUM:-1}"
spec:
  runtimeClassName: android
  restartPolicy: Never
  containers:
  - name: android
    image: android.dev/cvd:local
    imagePullPolicy: IfNotPresent
EOF
    kubectl apply -f "${yaml}" >&2
    wait_pod_ready "${name}" || return 1
    id=$(kubectl get pod "${name}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
    [ -n "${id}" ] || return 1
    echo "${id}"
}

android_resources() {
    python3 - "${STATE_FILE}" "$1" <<'PY'
import json, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
for pod in (state.get("android") or {}).get("pods") or []:
    if pod.get("sandbox_id") == sys.argv[2]:
        record = pod.get("cni_record") or {}
        print(pod.get("work_dir", ""))
        print(record.get("netns_path") or record.get("NetNSPath", ""))
        print(pod.get("launch_pid", 0))
        print(pod.get("launch_pgid", 0))
        print(pod.get("adb_port", 0))
        raise SystemExit(0)
raise SystemExit(1)
PY
}

wait_android_resources() {
    local id="$1" resources
    for _ in $(seq 1 180); do
        if resources=$(android_resources "${id}" 2>/dev/null); then
            [ -n "${resources}" ] && printf '%s\n' "${resources}" && return 0
        fi
        sleep 1
    done
    return 1
}

wait_android_gone() {
    local id="$1" workdir="$2" netns="$3"
    for _ in $(seq 1 180); do
        if ! android_resources "${id}" >/dev/null 2>&1 &&
           [ ! -e "${workdir}" ] && [ ! -e "${netns}" ]; then
            return 0
        fi
        sleep 1
    done
    return 1
}

state_has_no_e2b() {
    python3 - "${STATE_FILE}" "$1" <<'PY'
import json, sys
try:
    s = json.load(open(sys.argv[1], encoding="utf-8"))
except FileNotFoundError:
    raise SystemExit(0)
sandbox_id = sys.argv[2]
pods = (s.get("e2b") or {}).get("pods") or []
routes = s.get("routes") or []
if any(p.get("sandbox_id") == sandbox_id for p in pods):
    raise SystemExit(1)
if any(r.get("id") in (sandbox_id, sandbox_id + "-c") for r in routes):
    raise SystemExit(1)
PY
}

wait_e2b_create_failure_observed() {
    local name="$1" id="$2" describe_output log_output

    for _ in $(seq 1 120); do
        describe_output=$(kubectl describe pod "${name}" 2>&1 || true)
        log_output=$(tail -n 240 /tmp/cri-multiplex.log 2>/dev/null || true)
        if echo "${describe_output}" | grep -qiE "FailedCreatePodSandBox|failed to create sandbox|build|not found|invalid" &&
           echo "${log_output}" | grep -q "CNI ADD: sandbox=${id}" &&
           echo "${log_output}" | grep -q "RunPodSandbox: orchestrator.Create FAILED"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

wait_e2b_create_rollback() {
    local id="$1" netns="$2"

    for _ in $(seq 1 90); do
        if state_has_no_e2b "${id}" >/dev/null 2>&1 && [ ! -e "${netns}" ]; then
            return 0
        fi
        sleep 1
    done
    return 1
}

wait_pod_failure_diagnostic() {
    local name="$1" pattern="$2" timeout_seconds="${3:-120}"
    local describe_output

    for _ in $(seq 1 "${timeout_seconds}"); do
        describe_output=$(kubectl describe pod "${name}" 2>&1 || true)
        if echo "${describe_output}" | grep -qi "FailedCreatePodSandBox" &&
           echo "${describe_output}" | grep -qiE "${pattern}"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

owner_rule_exists() {
    local comment="$1"
    local rules
    rules=$(iptables-save 2>/dev/null || true)
    if grep -Eq -- "--comment (\"${comment}\"|${comment})( |$)" <<< "${rules}"; then
        return 0
    fi
    rules=$(nft -a list ruleset 2>/dev/null || true)
    grep -Fq "comment \"${comment}\"" <<< "${rules}"
}

wait_e2b_gone() {
    local id="$1" netns="$2" comments="$3" gone comment
    for _ in $(seq 1 90); do
        gone=1
        state_has_no_e2b "${id}" >/dev/null 2>&1 || gone=0
        [ -e "${netns}" ] && gone=0
        for comment in ${comments}; do
            owner_rule_exists "${comment}" && gone=0
        done
        [ "${gone}" = "1" ] && return 0
        sleep 1
    done
    return 1
}

check_owner_rules() {
    local comment
    [ -n "$1" ] || return 1
    for comment in $1; do
        owner_rule_exists "${comment}" || return 1
    done
}

remove_sandbox() {
    local output
    output=$(grpc_call "runtime.v1.RuntimeService/RemovePodSandbox" \
        "{\"pod_sandbox_id\":\"$1\"}") || true
    assert_empty_response "${output}"
}

delete_kube_pod() {
    local name="$1"
    kubectl delete pod "${name}" --wait=true --timeout=120s >&2 || true
    for _ in $(seq 1 60); do
        if ! kubectl get pod "${name}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

log_step "1. 前置检查（20 号默认不构建）"
if [ ! -x "${MULTIPLEX_DIR}/cri-multiplex" ]; then
    log_info "cri-multiplex 二进制不存在: ${MULTIPLEX_DIR}/cri-multiplex"
    exit 1
fi
if [ ! -f "${E2B_POD_JSON}" ] || ! grep -q 'e2b.dev/template-id' "${E2B_POD_JSON}"; then
    log_info "E2B 凭据 fixture 不存在或不完整: ${E2B_POD_JSON}"
    exit 1
fi
if ! require_refresh_script_quiet "${REFRESH_SCRIPT}"; then
    exit 1
fi
if [ "${E2B_SKIP_BUILD}" = "1" ] && ! validate_reusable_e2b_yaml_quiet "${E2B_POD_TEMPLATE}"; then
    log_info "E2B_SKIP_BUILD=1 时必须提供可复用的 kubelet Pod fixture"
    exit 1
fi
if [ ! -x "${ANDROID_ARTIFACTS_DIR}/bin/launch_cvd" ]; then
    log_info "launch_cvd 不存在或不可执行: ${ANDROID_ARTIFACTS_DIR}/bin/launch_cvd"
    exit 1
fi
if [ ! -e /dev/kvm ] || [ ! -e /dev/net/tun ]; then
    log_info "缺少 /dev/kvm 或 /dev/net/tun，无法运行 Android 清理场景"
    exit 1
fi
for required_command in grpcurl crictl kubectl iptables iptables-save nft python3; do
    if ! command -v "${required_command}" >/dev/null 2>&1; then
        log_info "缺少命令: ${required_command}"
        exit 1
    fi
done
if ! kubectl get runtimeclass e2b >/dev/null 2>&1; then
    log_info "RuntimeClass e2b 不存在"
    exit 1
fi
if ! kubectl get runtimeclass android >/dev/null 2>&1; then
    kubectl apply -f - >/dev/null <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: android
handler: android
EOF
fi
if ! kubectl get runtimeclass android >/dev/null 2>&1; then
    log_info "RuntimeClass android 不存在"
    exit 1
fi
rm -rf "${STATE_DIR}" "${ANDROID_STATE_DIR}"
mkdir -p "${STATE_DIR}" "${ANDROID_STATE_DIR}" "${UNKNOWN_WORKDIR}"
printf 'must-survive\n' > "${UNKNOWN_WORKDIR}/sentinel"
iptables -t filter -A OUTPUT -p tcp -d 127.0.0.1 --dport 1 \
    -m comment --comment "${NONOWNER_COMMENT}" -j ACCEPT
if [ "${E2B_SKIP_BUILD}" = "1" ]; then
    log_info "已有二进制和测试 fixture 就绪，使用 RuntimeClass 正常创建 Pod，跳过构建"
else
    log_info "已有二进制就绪，使用 07 号流程刷新 build_id 并通过 RuntimeClass 正常创建 Pod"
fi

log_step "2. Orchestrator 不可用错误诊断"
stop_test_multiplex
ORCH_DOWN_NAME="e2b-orch-down-20-${RANDOM}"
ORCH_DOWN_ADDRESS="${ORCH_DOWN_ADDRESS:-127.0.0.1:1}"
ORCHESTRATOR_ADDRESS="${ORCH_DOWN_ADDRESS}" start_cleanup_multiplex
ORCH_DOWN_ID=$(submit_e2b_pod_no_wait "${ORCH_DOWN_NAME}")
ORCH_DOWN_NETNS="${CNI_NETNS_DIR}/e2b-${ORCH_DOWN_ID:0:12}"
if ! wait_pod_failure_diagnostic "${ORCH_DOWN_NAME}" "orchestrator|connection|unavailable|refused|failed to create sandbox" 120; then
    fail_context
    kubectl describe pod "${ORCH_DOWN_NAME}" >&2 || true
    log_fail "Orchestrator 不可用时错误未通过 Pod 事件诊断"
    exit 1
fi
if state_has_no_e2b "${ORCH_DOWN_ID}" >/dev/null 2>&1 && [ ! -e "${ORCH_DOWN_NETNS}" ]; then
    log_pass "Orchestrator 不可用错误可诊断，且无 state/routes/netns 残留"
else
    fail_context
    log_fail "Orchestrator 不可用场景存在 state/routes 或 netns 残留: ${ORCH_DOWN_ID}"
    exit 1
fi
delete_kube_pod "${ORCH_DOWN_NAME}" || true

log_step "3. CNI 配置不可用错误诊断和无 netns 残留"
stop_test_multiplex
rm -rf "${BROKEN_CNI_CONF_DIR}"
mkdir -p "${BROKEN_CNI_CONF_DIR}"
CNI_DOWN_NAME="e2b-cni-down-20-${RANDOM}"
CNI_CONF_DIR="${BROKEN_CNI_CONF_DIR}" start_cleanup_multiplex
CNI_DOWN_ID=$(submit_e2b_pod_no_wait "${CNI_DOWN_NAME}")
CNI_DOWN_NETNS="${CNI_NETNS_DIR}/e2b-${CNI_DOWN_ID:0:12}"
if ! wait_pod_failure_diagnostic "${CNI_DOWN_NAME}" "cni|netns|network|failed to create sandbox|not initialized|no configs" 120; then
    fail_context
    kubectl describe pod "${CNI_DOWN_NAME}" >&2 || true
    log_fail "CNI 配置不可用时错误未通过 Pod 事件诊断"
    exit 1
fi
if state_has_no_e2b "${CNI_DOWN_ID}" >/dev/null 2>&1 && [ ! -e "${CNI_DOWN_NETNS}" ]; then
    log_pass "CNI 配置不可用错误可诊断，且无 state/routes/netns 残留"
else
    fail_context
    log_fail "CNI 配置不可用场景存在 state/routes 或 netns 残留: ${CNI_DOWN_ID}"
    exit 1
fi
delete_kube_pod "${CNI_DOWN_NAME}" || true

log_step "4. E2B 删除幂等清理"
start_cleanup_multiplex
E2B_NAME="e2b-cleanup-20-${RANDOM}"
E2B_ID=$(run_e2b_sandbox "${E2B_NAME}")
mapfile -t e2b_fields < <(e2b_resources "${E2B_ID}")
E2B_NETNS="${e2b_fields[0]:-}"
E2B_COMMENTS="${e2b_fields[*]:1}"
if [ -z "${E2B_NETNS}" ] || [ ! -e "${E2B_NETNS}" ] || ! check_owner_rules "${E2B_COMMENTS}"; then
    fail_context
    log_fail "E2B CNI netns 或 owner HostPort 规则未生成"
    exit 1
fi
log_pass "E2B sandbox、CNI netns、owner HostPort 规则已创建"
delete_kube_pod "${E2B_NAME}" || { fail_context; log_fail "E2B Pod 未按正常 kubelet 流程删除"; exit 1; }
remove_sandbox "${E2B_ID}" || { fail_context; log_fail "E2B 首次 RemovePodSandbox 失败"; exit 1; }
remove_sandbox "${E2B_ID}" || { fail_context; log_fail "E2B 重复 RemovePodSandbox 非幂等"; exit 1; }
if ! wait_e2b_gone "${E2B_ID}" "${E2B_NETNS}" "${E2B_COMMENTS}"; then
    fail_context
    log_fail "E2B state/routes、CNI netns 或 HostPort 未清理"
    exit 1
fi
log_pass "E2B 删除幂等：state/routes、CNI netns、HostPort 均已清理"

log_step "5. E2B 重启后回收 missing sandbox"
MISSING_NAME="e2b-missing-20-${RANDOM}"
MISSING_E2B_ID=$(run_e2b_sandbox "${MISSING_NAME}")
mapfile -t missing_fields < <(e2b_resources "${MISSING_E2B_ID}")
MISSING_NETNS="${missing_fields[0]:-}"
MISSING_COMMENTS="${missing_fields[*]:1}"
MISSING_ORCHESTRATOR_ID=$(e2b_orchestrator_id "${MISSING_E2B_ID}")
if [ -z "${MISSING_NETNS}" ] || [ ! -e "${MISSING_NETNS}" ]; then
    fail_context
    log_fail "missing E2B CNI state 未生成"
    exit 1
fi
stop_test_multiplex
orchestrator_delete=$(grpcurl -plaintext -proto "${ORCH_PROTO}" \
    -import-path "${MULTIPLEX_DIR}/proto" -import-path /usr/include \
    -d "{\"sandbox_id\":\"${MISSING_ORCHESTRATOR_ID}\"}" \
    "${ORCHESTRATOR_ADDRESS}" SandboxService/Delete 2>&1) || true
if ! assert_empty_response "${orchestrator_delete}"; then
    echo "${orchestrator_delete}" >&2
    log_fail "手工删除 Orchestrator sandbox 失败"
    exit 1
fi
kubectl delete pod "${MISSING_NAME}" --wait=false --grace-period=0 >&2 || true
start_cleanup_multiplex
if ! wait_e2b_gone "${MISSING_E2B_ID}" "${MISSING_NETNS}" "${MISSING_COMMENTS}"; then
    fail_context
    log_fail "重启后 missing E2B 未收敛"
    exit 1
fi
log_pass "重启后 missing E2B 的 state/routes、CNI netns、HostPort 已回收"

log_step "6. E2B 创建过程中异常资源回滚"
CREATE_FAIL_NAME="e2b-create-fail-20-${RANDOM}"
CREATE_FAIL_ID=$(run_e2b_create_failure_pod "${CREATE_FAIL_NAME}")
CREATE_FAIL_NETNS="${CNI_NETNS_DIR}/e2b-${CREATE_FAIL_ID:0:12}"
if ! wait_e2b_create_failure_observed "${CREATE_FAIL_NAME}" "${CREATE_FAIL_ID}"; then
    fail_context
    kubectl describe pod "${CREATE_FAIL_NAME}" >&2 || true
    log_fail "未观察到 E2B 创建失败前的 CNI ADD 和 Orchestrator Create 失败"
    exit 1
fi
delete_kube_pod "${CREATE_FAIL_NAME}" || true
if ! wait_e2b_create_rollback "${CREATE_FAIL_ID}" "${CREATE_FAIL_NETNS}"; then
    fail_context
    log_fail "E2B 创建失败后 state/routes 或 CNI netns 未回滚: id=${CREATE_FAIL_ID}, netns=${CREATE_FAIL_NETNS}"
    exit 1
fi
log_pass "E2B 创建失败后已回滚 state/routes 和 CNI netns"

log_step "7. Android 删除幂等清理和端口复用"
ANDROID_DELETE_NAME="android-delete-20-${RANDOM}"
ANDROID_DELETE_ID=$(run_android_sandbox "${ANDROID_DELETE_NAME}")
mapfile -t delete_fields < <(wait_android_resources "${ANDROID_DELETE_ID}")
DELETE_WORKDIR="${delete_fields[0]}"
DELETE_NETNS="${delete_fields[1]}"
if [ "${delete_fields[3]}" -le 0 ]; then
    fail_context
    log_fail "Android launch_cvd PGID 未记录"
    exit 1
fi
log_pass "Android CVD/PGID、workdir、CNI netns 已创建"
delete_kube_pod "${ANDROID_DELETE_NAME}" || { fail_context; log_fail "Android Pod 未按正常 kubelet 流程删除"; exit 1; }
remove_sandbox "${ANDROID_DELETE_ID}" || { fail_context; log_fail "Android 首次 RemovePodSandbox 失败"; exit 1; }
remove_sandbox "${ANDROID_DELETE_ID}" || { fail_context; log_fail "Android 重复 RemovePodSandbox 非幂等"; exit 1; }
if ! wait_android_gone "${ANDROID_DELETE_ID}" "${DELETE_WORKDIR}" "${DELETE_NETNS}"; then
    fail_context
    log_fail "Android PGID/workdir/netns 未清理"
    exit 1
fi
ANDROID_REUSE_NAME="android-reuse-20-${RANDOM}"
ANDROID_REUSE_ID=$(run_android_sandbox "${ANDROID_REUSE_NAME}")
mapfile -t reuse_fields < <(wait_android_resources "${ANDROID_REUSE_ID}")
if [ "${reuse_fields[4]}" != "${ANDROID_ADB_PORT_START:-6520}" ]; then
    fail_context
    log_fail "Android 删除后端口未复用：期望 ${ANDROID_ADB_PORT_START:-6520}，实际 ${reuse_fields[4]}"
    exit 1
fi
log_pass "Android 删除幂等，PGID/workdir/netns 已清理且 ADB 端口可复用"
delete_kube_pod "${ANDROID_REUSE_NAME}" || true
remove_sandbox "${ANDROID_REUSE_ID}" || true
wait_android_gone "${ANDROID_REUSE_ID}" "${reuse_fields[0]}" "${reuse_fields[1]}" || true

log_step "8. Android 进程异常退出"
ANDROID_EXIT_NAME="android-exit-20-${RANDOM}"
ANDROID_EXIT_ID=$(run_android_sandbox "${ANDROID_EXIT_NAME}")
mapfile -t exit_fields < <(wait_android_resources "${ANDROID_EXIT_ID}")
EXIT_WORKDIR="${exit_fields[0]}"
EXIT_NETNS="${exit_fields[1]}"
EXIT_PGID="${exit_fields[3]}"
if ! kill -0 -- "-${EXIT_PGID}" 2>/dev/null; then
    log_fail "Android launch_cvd PGID 不存在，无法执行异常退出场景"
    exit 1
fi
kill -KILL -- "-${EXIT_PGID}" || true
SAW_EXIT_STATE=0
for _ in $(seq 1 30); do
    if python3 - "${STATE_FILE}" "${ANDROID_EXIT_ID}" <<'PY' >/dev/null 2>&1
import json, sys
s = json.load(open(sys.argv[1], encoding="utf-8"))
for p in (s.get("android") or {}).get("pods") or []:
    if p.get("sandbox_id") == sys.argv[2] and p.get("state") == "Unknown" and p.get("container_state") == "Exited":
        raise SystemExit(0)
raise SystemExit(1)
PY
    then
        SAW_EXIT_STATE=1
        break
    fi
    sleep 0.2
done
if [ "${SAW_EXIT_STATE}" != "1" ]; then
    fail_context
    log_fail "异常退出未观察到 Unknown/Exited 状态"
    exit 1
fi
log_pass "kill launch_cvd PGID 后状态进入 Unknown/Exited"
if ! wait_android_gone "${ANDROID_EXIT_ID}" "${EXIT_WORKDIR}" "${EXIT_NETNS}"; then
    fail_context
    log_fail "异常退出后台资源未清理"
    exit 1
fi
log_pass "异常退出后台清理已完成"
delete_kube_pod "${ANDROID_EXIT_NAME}" || true

log_step "9. Android 重启后孤儿回收"
ANDROID_RESTART_NAME="android-restart-20-${RANDOM}"
ANDROID_RESTART_ID=$(run_android_sandbox "${ANDROID_RESTART_NAME}")
mapfile -t restart_fields < <(wait_android_resources "${ANDROID_RESTART_ID}")
RESTART_WORKDIR="${restart_fields[0]}"
RESTART_NETNS="${restart_fields[1]}"
RESTART_PGID="${restart_fields[3]}"
stop_test_multiplex
kill -KILL -- "-${RESTART_PGID}" 2>/dev/null || true
kubectl delete pod "${ANDROID_RESTART_NAME}" --wait=false --grace-period=0 >&2 || true
start_cleanup_multiplex
if ! wait_android_gone "${ANDROID_RESTART_ID}" "${RESTART_WORKDIR}" "${RESTART_NETNS}"; then
    fail_context
    log_fail "cri-multiplex 重启后 Android 孤儿未收敛"
    exit 1
fi
log_pass "重启后对账并回收 Android CVD、state、CNI netns"

log_step "10. 非 owner 资源保护"
if ! grep -q "skip unknown owner" /tmp/cri-multiplex.log; then
    fail_context
    log_fail "未输出 unknown owner warning"
    exit 1
fi
if [ ! -d "${UNKNOWN_WORKDIR}" ]; then
    fail_context
    log_fail "相似命名但无 owner 的目录被误删"
    exit 1
fi
if ! owner_rule_exists "${NONOWNER_COMMENT}"; then
    fail_context
    log_fail "无 owner 的 iptables 规则被误删"
    exit 1
fi
log_pass "相似命名但无 owner 的目录和 iptables 规则均受保护，并输出 warning"

print_summary
[ "${FAIL_COUNT}" -eq 0 ]
