#!/bin/bash
###############################################################################
# 13_android_multi_sandbox.sh — Android 多实例 kubelet 创建验证
#
# 验证目标：
#   1. CNI 模式下启用 AndroidEngine
#   2. 创建两个 RuntimeClass=android Pod
#   3. Pod1 使用 base_instance_num=1 / ADB 6520
#   4. Pod2 使用 base_instance_num=2 / ADB 6521
#   5. 两个 Pod 均 Ready，且两个 ADB 端口均可连接
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "13 — Android 多实例 kubelet 沙箱创建验证"

ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR:-/home/fjq/cf17}"
ANDROID_WAIT_TIMEOUT="${ANDROID_WAIT_TIMEOUT:-360s}"
ANDROID_ADB_WAIT_TIMEOUT="${ANDROID_ADB_WAIT_TIMEOUT:-30}"
POD1="${POD1:-android-cvd-multi-1}"
POD2="${POD2:-android-cvd-multi-2}"
YAML1="/tmp/${POD1}.yaml"
YAML2="/tmp/${POD2}.yaml"
RUNTIMECLASS_YAML="${RUNTIMECLASS_YAML:-/tmp/runtimeclass-android.yaml}"

cleanup() {
    kubectl delete pod "${POD1}" "${POD2}" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

require_file_exec() {
    local path="$1"
    if [ ! -x "${path}" ]; then
        log_fail "文件不存在或不可执行: ${path}"
        exit 1
    fi
    log_pass "文件可执行: ${path}"
}

create_android_pod_yaml() {
    local pod_name="$1"
    local base_instance_num="$2"
    local adb_port="$3"
    local yaml="$4"
    cat > "${yaml}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  annotations:
    android.dev/base-instance-num: "${base_instance_num}"
    android.dev/adb-port: "${adb_port}"
spec:
  runtimeClassName: android
  restartPolicy: Never
  containers:
    - name: android
      image: android.dev/cvd:local
      imagePullPolicy: IfNotPresent
EOF
}

wait_ready() {
    local pod_name="$1"
    if kubectl wait --for=condition=Ready "pod/${pod_name}" --timeout="${ANDROID_WAIT_TIMEOUT}" >&2; then
        log_pass "Pod 已 Ready: ${pod_name}"
    else
        log_fail "Pod 未在 ${ANDROID_WAIT_TIMEOUT} 内 Ready: ${pod_name}"
        kubectl describe pod "${pod_name}" >&2 || true
        tail -n 160 /tmp/cri-multiplex.log >&2 || true
        exit 1
    fi
}

verify_android_status() {
    local pod_name="$1"
    local expected_base="$2"
    local expected_port="$3"

    local uid inspect adb_url adb_host adb_port pod_ip netns_path
    uid=$(kubectl get pod "${pod_name}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
    pod_ip=$(kubectl get pod "${pod_name}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
    if [ -z "${uid}" ]; then
        log_fail "无法读取 Pod UID: ${pod_name}"
        exit 1
    fi
    if [ -z "${pod_ip}" ]; then
        log_fail "无法读取 PodIP: ${pod_name}"
        exit 1
    fi
    netns_path="/var/run/netns/android-${uid:0:12}"
    inspect=$(${CRICTL} inspectp "${uid}" 2>&1) || true
    if grep -q "not found\|NotFound\|level=fatal" <<< "${inspect}"; then
        log_fail "无法 inspect Android PodSandbox: ${pod_name}/${uid}"
        echo "${inspect}" >&2
        exit 1
    fi
    if grep -q "\"ip\": \"${pod_ip}\"" <<< "${inspect}" &&
       grep -q '"android.dev/cni-enabled": "true"' <<< "${inspect}" &&
       grep -q "\"android.dev/pod-ip\": \"${pod_ip}\"" <<< "${inspect}" &&
       grep -q "\"android.dev/cni-netns\": \"${netns_path}\"" <<< "${inspect}" &&
       grep -q "\"android.dev/base-instance-num\": \"${expected_base}\"" <<< "${inspect}" &&
       grep -q "\"android.dev/adb-port\": \"${expected_port}\"" <<< "${inspect}"; then
        log_pass "CRI status 匹配 ${pod_name}: podIP=${pod_ip}, base_instance_num=${expected_base}, adb=${expected_port}"
    else
        log_fail "CRI status 不匹配 ${pod_name}: 预期 podIP=${pod_ip}, base=${expected_base}, adb=${expected_port}"
        echo "${inspect}" >&2
        exit 1
    fi

    adb_url=$(grep -oP '"android.dev/adb-url":\s*"\K[^"]+' <<< "${inspect}" | head -1 || true)
    adb_host="${adb_url%:*}"
    adb_port="${adb_url##*:}"
    if [ -z "${adb_url}" ] || [ "${adb_host}" != "${pod_ip}" ] || [ "${adb_port}" != "${expected_port}" ]; then
        log_fail "ADB URL 异常: pod=${pod_name}, url=${adb_url}, expected_port=${expected_port}"
        exit 1
    fi
    wait_tcp_connect "${adb_host}" "${adb_port}" "${ANDROID_ADB_WAIT_TIMEOUT}" "ADB 端口 ${pod_name} ${adb_url}" || exit 1
}

log_step "1.1 前置检查"
require_file_exec "${ANDROID_ARTIFACTS_DIR}/bin/launch_cvd"
if [ -x "${ANDROID_ARTIFACTS_DIR}/bin/cvd_internal_stop" ]; then
    log_pass "cvd_internal_stop 可执行: ${ANDROID_ARTIFACTS_DIR}/bin/cvd_internal_stop"
else
    log_skip "cvd_internal_stop 不存在，清理将依赖进程组 kill"
fi

log_step "1.2 启动 cri-multiplex CNI+Android runtime 模式"
if ! ANDROID_ENABLED=1 E2B_CNI_ENABLED=1 ANDROID_CNI_ENABLED=1 ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR}" ANDROID_ADB_PORT_START=6520 ANDROID_BASE_INSTANCE_NUM_START=1 E2B_FORCE_RESTART=1 "${SCRIPT_DIR}/01_start_multiplex.sh" >&2; then
    log_fail "启动 cri-multiplex CNI+Android runtime 模式失败"
    exit 1
fi
require_cri_multiplex_cni_enabled || exit 1
require_cri_multiplex_android_cni_enabled || exit 1
if ! cri_multiplex_cmdline | grep -q -- "-android-enabled"; then
    log_fail "cri-multiplex 未启用 -android-enabled"
    exit 1
fi
log_pass "cri-multiplex 已启用 AndroidEngine"

log_step "1.3 清理旧 Pod"
cleanup
sleep 3
log_pass "旧 Android 多实例 Pod 已清理"

log_step "2.1 创建 RuntimeClass android"
cat > "${RUNTIMECLASS_YAML}" <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: android
handler: android
EOF
kubectl apply -f "${RUNTIMECLASS_YAML}" >&2
log_pass "RuntimeClass android 已创建/更新"

log_step "3.1 提交两个 Android Pod"
create_android_pod_yaml "${POD1}" 1 6520 "${YAML1}"
create_android_pod_yaml "${POD2}" 2 6521 "${YAML2}"
kubectl apply -f "${YAML1}" >&2
kubectl apply -f "${YAML2}" >&2
log_pass "两个 Android Pod 已提交: ${POD1}, ${POD2}"

log_step "3.2 等待两个 Pod Ready"
wait_ready "${POD1}"
wait_ready "${POD2}"

log_step "4.1 验证两个 Android sandbox 的 CRI status 和 ADB 端口"
verify_android_status "${POD1}" 1 6520
verify_android_status "${POD2}" 2 6521

log_step "5.1 删除 Pod 验证清理"
cleanup
sleep 5
log_pass "删除请求已提交"

print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证通过：Android 多实例 Pod 可通过不同 base_instance_num 创建"
    exit 0
else
    exit 1
fi
