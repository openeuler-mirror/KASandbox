#!/bin/bash
###############################################################################
# 12_android_kubelet_sandbox.sh — Android RuntimeClass kubelet 创建沙箱验证
#
# 验证目标：
#   1. cri-multiplex 启用 AndroidEngine
#   2. RuntimeClass=android Pod 通过 kubelet 创建
#   3. StartContainer 启动宿主机 Cuttlefish VM
#   4. Pod 进入 Ready
#   5. CRI PodSandboxStatus 返回 android.dev/adb-url
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "12 — Android RuntimeClass kubelet 沙箱创建验证"

POD_NAME="${POD_NAME:-android-cvd-kubelet-test}"
POD_YAML="${POD_YAML:-/tmp/android-cvd-kubelet-pod.yaml}"
RUNTIMECLASS_YAML="${RUNTIMECLASS_YAML:-/tmp/runtimeclass-android.yaml}"
ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR:-/home/fjq/cf17}"
ANDROID_ADB_PORT="${ANDROID_ADB_PORT:-6520}"
ANDROID_BASE_INSTANCE_NUM="${ANDROID_BASE_INSTANCE_NUM:-1}"
ANDROID_WAIT_TIMEOUT="${ANDROID_WAIT_TIMEOUT:-240s}"
ANDROID_CLEANUP_EXISTING="${ANDROID_CLEANUP_EXISTING:-1}"

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

if [ -x "${ANDROID_ARTIFACTS_DIR}/bin/cvd" ]; then
    log_pass "cvd 可执行: ${ANDROID_ARTIFACTS_DIR}/bin/cvd"
elif [ -x "${ANDROID_ARTIFACTS_DIR}/bin/cvd_internal_stop" ]; then
    log_pass "cvd_internal_stop 可执行: ${ANDROID_ARTIFACTS_DIR}/bin/cvd_internal_stop"
else
    log_fail "cvd 或 cvd_internal_stop 不存在或不可执行: ${ANDROID_ARTIFACTS_DIR}/bin"
    exit 1
fi

if [ ! -e /dev/kvm ]; then
    log_fail "/dev/kvm 不存在，无法启动 Android Cuttlefish VM"
    exit 1
fi
log_pass "/dev/kvm 存在"

if [ ! -e /dev/net/tun ]; then
    log_fail "/dev/net/tun 不存在，无法启动 Android Cuttlefish VM"
    exit 1
fi
log_pass "/dev/net/tun 存在"

log_step "1.2 启动 cri-multiplex CNI+Android runtime 模式"

if ! ANDROID_ENABLED=1 E2B_CNI_ENABLED=1 ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR}" ANDROID_ADB_PORT_START="${ANDROID_ADB_PORT}" E2B_FORCE_RESTART=1 "${SCRIPT_DIR}/01_start_multiplex.sh" >&2; then
    log_fail "启动 cri-multiplex CNI+Android runtime 模式失败"
    exit 1
fi
log_pass "cri-multiplex CNI+Android runtime 模式已启动"

require_cri_multiplex_cni_enabled || exit 1

if ! cri_multiplex_cmdline | grep -q -- "-android-enabled"; then
    log_fail "cri-multiplex 未启用 -android-enabled"
    exit 1
fi
log_pass "cri-multiplex 已启用 AndroidEngine"

log_step "1.3 清理旧 CVD 实例，避免 ADB 端口残留误判"

if [ "${ANDROID_CLEANUP_EXISTING}" = "1" ]; then
    if HOME="${ANDROID_ARTIFACTS_DIR}" timeout 30 "${ANDROID_ARTIFACTS_DIR}/bin/cvd" stop >/tmp/android-cvd-preclean.log 2>&1; then
        log_pass "旧 CVD 实例已清理"
    else
        log_skip "cvd stop 未成功或无旧实例，继续执行；日志: /tmp/android-cvd-preclean.log"
    fi
else
    log_skip "ANDROID_CLEANUP_EXISTING=0，跳过旧 CVD 清理"
fi

log_step "2.1 创建 RuntimeClass android"

cat > "${RUNTIMECLASS_YAML}" <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: android
handler: android
EOF

cat ${RUNTIMECLASS_YAML}

kubectl apply -f "${RUNTIMECLASS_YAML}" >&2
log_pass "RuntimeClass android 已创建/更新"

log_step "2.2 清理旧 Android Pod"

if kubectl get pod "${POD_NAME}" >/dev/null 2>&1; then
    kubectl delete pod "${POD_NAME}" --force --grace-period=0 >&2 || true
    sleep 3
    log_pass "旧 Pod 已删除: ${POD_NAME}"
else
    log_skip "无旧 Pod 需清理"
fi

: > /tmp/cri-multiplex.log 2>/dev/null || true

log_step "3.1 生成并提交 Android Pod"

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

cat ${POD_YAML}
kubectl apply -f "${POD_YAML}" >&2
log_pass "Android Pod 已提交: ${POD_NAME}"

log_step "3.2 等待 Android Pod Ready"

if kubectl wait --for=condition=Ready "pod/${POD_NAME}" --timeout="${ANDROID_WAIT_TIMEOUT}" >&2; then
    log_pass "Android Pod 已 Ready"
else
    log_fail "Android Pod 未在 ${ANDROID_WAIT_TIMEOUT} 内 Ready"
    kubectl describe pod "${POD_NAME}" >&2 || true
    tail -n 120 /tmp/cri-multiplex.log >&2 || true
    exit 1
fi

POD_UID=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
POD_IP=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
if [ -z "${POD_UID}" ]; then
    log_fail "无法读取 Android Pod UID"
    exit 1
fi
log_pass "Android Pod UID: ${POD_UID}"

log_step "4.1 验证 CRI PodSandboxStatus Android annotations"

INSPECT_OUTPUT=$(${CRICTL} inspectp "${POD_UID}" 2>&1) || true
if grep -q "not found\|NotFound\|level=fatal" <<< "${INSPECT_OUTPUT}"; then
    log_fail "无法 inspect Android PodSandbox: ${POD_UID}"
    echo "${INSPECT_OUTPUT}" >&2
    exit 1
fi

if grep -q '"android.dev/adb-url"' <<< "${INSPECT_OUTPUT}" &&
   grep -q "\"android.dev/adb-port\": \"${ANDROID_ADB_PORT}\"" <<< "${INSPECT_OUTPUT}"; then
    log_pass "CRI status 返回 Android ADB annotations"
else
    log_fail "CRI status 缺少 Android ADB annotations"
    echo "${INSPECT_OUTPUT}" >&2
    exit 1
fi

log_step "4.2 验证 ADB 端口可连接"

ADB_URL=$(grep -oP '"android.dev/adb-url":\s*"\K[^"]+' <<< "${INSPECT_OUTPUT}" | head -1 || true)
if [ -z "${ADB_URL}" ]; then
    log_fail "无法从 CRI status 提取 android.dev/adb-url"
    exit 1
fi
ADB_HOST="${ADB_URL%:*}"
ADB_PORT="${ADB_URL##*:}"

if timeout 5 bash -c "cat < /dev/null > /dev/tcp/${ADB_HOST}/${ADB_PORT}" 2>/dev/null; then
    log_pass "ADB 端口可连接: ${ADB_URL}"
else
    log_fail "ADB 端口不可连接: ${ADB_URL}"
    tail -n 120 /tmp/cri-multiplex.log >&2 || true
    exit 1
fi

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

log_step "5.1 删除 Pod 验证清理"

kubectl delete pod "${POD_NAME}" --force --grace-period=0 >&2 || true
sleep 5

if pgrep -af "launch_cvd.*${ANDROID_ARTIFACTS_DIR}|cvd_internal_start" >/tmp/android-cvd-pgrep.out 2>/dev/null; then
    log_skip "仍观察到 CVD 相关进程，可能是其他手工实例；请确认是否属于本用例"
    cat /tmp/android-cvd-pgrep.out >&2 || true
else
    log_pass "未观察到残留 CVD 进程"
fi

print_summary

if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证通过：kubelet 已能通过 RuntimeClass=android 创建 Android 沙箱"
    exit 0
else
    exit 1
fi
