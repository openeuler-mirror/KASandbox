#!/bin/bash
###############################################################################
# 17_android_cni_podip.sh — Android CNI PodIP/Netns 访问验证
#
# 验证目标：
#   1. Android RuntimeClass 通过 CNI 模式启动
#   2. PodSandboxStatus 返回 PodIP / cni-netns / adb-url
#   3. launch_cvd 在 Android Pod netns 内监听
#   4. ADB 端口可通过 PodIP 访问
#   5. 删除 Pod 后 netns 清理
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "17 — Android CNI PodIP/Netns 访问验证"

POD_NAME="${POD_NAME:-android-cni-podip-test}"
POD_YAML="${POD_YAML:-/tmp/android-cni-podip.yaml}"
RUNTIMECLASS_YAML="${RUNTIMECLASS_YAML:-/tmp/runtimeclass-android.yaml}"
ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR:-/home/fjq/cf17}"
ANDROID_ADB_PORT="${ANDROID_ADB_PORT:-6520}"
ANDROID_BASE_INSTANCE_NUM="${ANDROID_BASE_INSTANCE_NUM:-1}"
ANDROID_WAIT_TIMEOUT="${ANDROID_WAIT_TIMEOUT:-240s}"
ANDROID_ADB_WAIT_TIMEOUT="${ANDROID_ADB_WAIT_TIMEOUT:-30}"
ANDROID_CLEANUP_WAIT_TIMEOUT="${ANDROID_CLEANUP_WAIT_TIMEOUT:-150}"

cleanup() {
    kubectl delete pod "${POD_NAME}" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

log_step "1.1 前置检查"
if [ ! -x "${ANDROID_ARTIFACTS_DIR}/bin/launch_cvd" ]; then
    log_fail "launch_cvd 不存在或不可执行: ${ANDROID_ARTIFACTS_DIR}/bin/launch_cvd"
    exit 1
fi
log_pass "launch_cvd 可执行: ${ANDROID_ARTIFACTS_DIR}/bin/launch_cvd"

log_step "1.2 启动 cri-multiplex CNI+Android runtime 模式"
if ! ANDROID_ENABLED=1 E2B_CNI_ENABLED=1 ANDROID_CNI_ENABLED=1 ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR}" ANDROID_ADB_PORT_START="${ANDROID_ADB_PORT}" ANDROID_BASE_INSTANCE_NUM_START="${ANDROID_BASE_INSTANCE_NUM}" E2B_FORCE_RESTART=1 "${SCRIPT_DIR}/01_start_multiplex.sh" >&2; then
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

log_step "1.3 创建 RuntimeClass android"
cat > "${RUNTIMECLASS_YAML}" <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: android
handler: android
EOF
kubectl apply -f "${RUNTIMECLASS_YAML}" >&2
log_pass "RuntimeClass android 已创建/更新"

log_step "1.4 清理旧 Pod"
cleanup
sleep 3
log_pass "旧 Android Pod 已清理"

log_step "2.1 提交 Android Pod"
cat > "${POD_YAML}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  annotations:
    android.dev/adb-port: "${ANDROID_ADB_PORT}"
    android.dev/base-instance-num: "${ANDROID_BASE_INSTANCE_NUM}"
spec:
  runtimeClassName: android
  restartPolicy: Never
  containers:
    - name: android
      image: android.dev/cvd:local
      imagePullPolicy: IfNotPresent
EOF

kubectl apply -f "${POD_YAML}" >&2
log_pass "Android Pod 已提交: ${POD_NAME}"

log_step "2.2 等待 Android Pod Ready"
if kubectl wait --for=condition=Ready "pod/${POD_NAME}" --timeout="${ANDROID_WAIT_TIMEOUT}" >&2; then
    log_pass "Android Pod 已 Ready"
else
    log_fail "Android Pod 未在 ${ANDROID_WAIT_TIMEOUT} 内 Ready"
    kubectl describe pod "${POD_NAME}" >&2 || true
    tail -n 160 /tmp/cri-multiplex.log >&2 || true
    exit 1
fi

POD_UID=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
POD_IP=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
if [ -z "${POD_UID}" ] || [ -z "${POD_IP}" ]; then
    log_fail "无法读取 Android Pod UID 或 PodIP: uid=${POD_UID}, ip=${POD_IP}"
    exit 1
fi
NETNS_NAME="android-${POD_UID:0:12}"
NETNS_PATH="/var/run/netns/${NETNS_NAME}"
log_pass "Android Pod UID=${POD_UID}, PodIP=${POD_IP}"

log_step "3.1 验证 CRI status / annotations"
INSPECT_OUTPUT=$(${CRICTL} inspectp "${POD_UID}" 2>&1) || true
if grep -q "not found\|NotFound\|level=fatal" <<< "${INSPECT_OUTPUT}"; then
    log_fail "无法 inspect Android PodSandbox: ${POD_UID}"
    echo "${INSPECT_OUTPUT}" >&2
    exit 1
fi

if grep -q "\"ip\": \"${POD_IP}\"" <<< "${INSPECT_OUTPUT}" &&
   grep -q '"android.dev/cni-enabled": "true"' <<< "${INSPECT_OUTPUT}" &&
   grep -q "\"android.dev/pod-ip\": \"${POD_IP}\"" <<< "${INSPECT_OUTPUT}" &&
   grep -q "\"android.dev/cni-netns\": \"${NETNS_PATH}\"" <<< "${INSPECT_OUTPUT}" &&
   grep -q "\"android.dev/adb-url\": \"${POD_IP}:${ANDROID_ADB_PORT}\"" <<< "${INSPECT_OUTPUT}"; then
    log_pass "CRI status 返回 Android CNI 关键信息"
else
    log_fail "CRI status 缺少 Android CNI 关键信息"
    echo "${INSPECT_OUTPUT}" >&2
    exit 1
fi

log_step "3.2 验证 Android CNI netns"
if [ -e "${NETNS_PATH}" ]; then
    log_pass "Android CNI netns 存在: ${NETNS_PATH}"
else
    log_fail "Android CNI netns 不存在: ${NETNS_PATH}"
    exit 1
fi

log_step "3.3 验证 ADB 端口可通过 PodIP 访问"
wait_tcp_connect "${POD_IP}" "${ANDROID_ADB_PORT}" "${ANDROID_ADB_WAIT_TIMEOUT}" "PodIP:${ANDROID_ADB_PORT}" || exit 1

log_step "3.4 验证 netns 内 ADB 监听"
if command -v ss >/dev/null 2>&1; then
    if NETNS_SS=$(ip netns exec "${NETNS_NAME}" ss -lntp 2>/dev/null || true); then
        if grep -q ":${ANDROID_ADB_PORT}" <<< "${NETNS_SS}"; then
            log_pass "netns 内已监听 ADB 端口: ${ANDROID_ADB_PORT}"
        else
            log_fail "netns 内未监听 ADB 端口: ${ANDROID_ADB_PORT}"
            echo "${NETNS_SS}" >&2
            exit 1
        fi
    fi
else
    log_skip "ss 命令不存在，跳过 netns 监听检查"
fi

ADB_URL="${POD_IP}:${ANDROID_ADB_PORT}"
if command -v adb >/dev/null 2>&1; then
    log_info "执行 adb connect ${ADB_URL}"
    if adb connect "${ADB_URL}" >&2; then
        log_pass "adb connect 成功: ${ADB_URL}"
    else
        log_fail "adb connect 失败: ${ADB_URL}"
        exit 1
    fi
else
    log_skip "adb 命令不存在，跳过 adb connect"
fi

log_step "4.1 删除 Pod 验证清理"
kubectl delete pod "${POD_NAME}" --force --grace-period=0 >&2 || true
NETNS_REMOVED=0
for i in $(seq 1 "${ANDROID_CLEANUP_WAIT_TIMEOUT}"); do
    if [ ! -e "${NETNS_PATH}" ]; then
        NETNS_REMOVED=1
        log_pass "Android CNI netns 已清理（第 ${i} 次轮询）"
        break
    fi
    sleep 1
done
if [ "${NETNS_REMOVED}" -ne 1 ]; then
    if [ ! -e "${NETNS_PATH}" ]; then
        log_pass "Android CNI netns 已清理（最终检查）"
    else
        log_fail "Android CNI netns 未在 ${ANDROID_CLEANUP_WAIT_TIMEOUT}s 内清理: ${NETNS_PATH}"
    fi
fi

print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证通过：Android 已接入 CNI 网络并可通过 PodIP 访问"
    exit 0
else
    exit 1
fi
