#!/bin/bash
###############################################################################
# 17_android_cni_podip.sh — Android CNI PodIP/Netns 访问验证
#
# 验证目标：
#   1. Android RuntimeClass 通过 CNI 模式启动
#   2. PodSandboxStatus 返回 PodIP / cni-netns / adb-url
#   3. launch_cvd 在 Android Pod netns 内监听
#   4. ADB 端口可通过 PodIP 访问
#   5. Android guest eth1 通过 Cuttlefish tap 接入 Pod netns
#   6. Android guest 可通过 CNI eth0 访问集群内 TCP 服务
#   7. PodIP 可通过 DNAT 访问 Android guest 内服务
#   8. 删除 Pod 后 netns 清理
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
ANDROID_GUEST_SERVICE_PORT="${ANDROID_GUEST_SERVICE_PORT:-18080}"
ANDROID_DNS_TEST_HOST="${ANDROID_DNS_TEST_HOST:-kubernetes.default.svc.cluster.local}"
ADB_BIN="${ADB_BIN:-${ANDROID_ARTIFACTS_DIR}/bin/adb}"

KUBE_API_CLUSTER_IP=$(kubectl get svc kubernetes -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
KUBE_DNS_CLUSTER_IP=$(kubectl get svc kube-dns -n kube-system -o jsonpath='{.spec.clusterIP}' 2>/dev/null || \
    kubectl get svc coredns -n kube-system -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
ANDROID_EGRESS_TEST_HOST="${ANDROID_EGRESS_TEST_HOST:-${KUBE_API_CLUSTER_IP}}"
ANDROID_EGRESS_TEST_PORT="${ANDROID_EGRESS_TEST_PORT:-443}"
ANDROID_GUEST_DNS_SERVERS="${ANDROID_GUEST_DNS_SERVERS:-${KUBE_DNS_CLUSTER_IP}}"

cleanup() {
    kubectl delete pod "${POD_NAME}" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

log_step "1.1 前置检查"
if [ ! -x "${ANDROID_ARTIFACTS_DIR}/bin/launch_cvd" ]; then
    log_info "launch_cvd 不存在或不可执行: ${ANDROID_ARTIFACTS_DIR}/bin/launch_cvd"
    exit 1
fi
log_info "launch_cvd 可执行: ${ANDROID_ARTIFACTS_DIR}/bin/launch_cvd"

log_step "1.2 启动 cri-multiplex CNI+Android runtime 模式"
if ! START_CNI_ANDROID_COUNT=0 start_cni_android_multiplex "启动 cri-multiplex CNI+Android runtime 模式"; then
    log_info "启动 cri-multiplex CNI+Android runtime 模式失败"
    exit 1
fi
log_info "cri-multiplex 已启用 AndroidEngine"

log_step "1.3 创建 RuntimeClass android"
cat > "${RUNTIMECLASS_YAML}" <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: android
handler: android
EOF
kubectl apply -f "${RUNTIMECLASS_YAML}" >&2
log_info "RuntimeClass android 已创建/更新"

log_step "1.4 清理旧 Pod"
cleanup
sleep 3
log_info "旧 Android Pod 已清理"

log_step "1.5 清理旧 CVD 实例"
if [ -x "${ANDROID_ARTIFACTS_DIR}/bin/cvd" ]; then
    if HOME="${ANDROID_ARTIFACTS_DIR}" timeout 30 "${ANDROID_ARTIFACTS_DIR}/bin/cvd" stop >/tmp/android-cni-preclean.log 2>&1; then
        log_info "旧 CVD 实例已清理"
    else
        log_info "cvd stop 未成功或无旧实例，继续执行；日志: /tmp/android-cni-preclean.log"
    fi
else
    log_info "cvd 命令不存在，跳过旧 CVD 实例清理"
fi

log_step "2.1 提交 Android Pod"
cat > "${POD_YAML}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  annotations:
    android.dev/adb-port: "${ANDROID_ADB_PORT}"
    android.dev/base-instance-num: "${ANDROID_BASE_INSTANCE_NUM}"
    android.dev/guest-service-ports: "${ANDROID_GUEST_SERVICE_PORT}"
    android.dev/guest-dns-servers: "${ANDROID_GUEST_DNS_SERVERS}"
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
   grep -q '"android.dev/guest-ip":' <<< "${INSPECT_OUTPUT}" &&
   grep -q '"android.dev/guest-gateway":' <<< "${INSPECT_OUTPUT}" &&
   grep -q '"android.dev/guest-tap":' <<< "${INSPECT_OUTPUT}" &&
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
if [ -x "${ADB_BIN}" ]; then
    log_info "执行 adb connect ${ADB_URL}"
    if "${ADB_BIN}" connect "${ADB_URL}" >&2; then
        log_pass "adb connect 成功: ${ADB_URL}"
    else
        log_fail "adb connect 失败: ${ADB_URL}"
        exit 1
    fi
else
    log_fail "adb 命令不存在或不可执行: ${ADB_BIN}"
    exit 1
fi

log_step "3.5 验证 Android guest 原生网络配置"
GUEST_IP=$(grep -oP '"android.dev/guest-ip":\s*"\K[^"]+' <<< "${INSPECT_OUTPUT}" | head -1 || true)
GUEST_GW=$(grep -oP '"android.dev/guest-gateway":\s*"\K[^"]+' <<< "${INSPECT_OUTPUT}" | head -1 || true)
GUEST_TAP=$(grep -oP '"android.dev/guest-tap":\s*"\K[^"]+' <<< "${INSPECT_OUTPUT}" | head -1 || true)
if [ -z "${GUEST_IP}" ] || [ -z "${GUEST_GW}" ] || [ -z "${GUEST_TAP}" ]; then
    log_fail "无法提取 guest 网络状态: guest_ip=${GUEST_IP}, gateway=${GUEST_GW}, tap=${GUEST_TAP}"
    exit 1
fi
if ip netns exec "${NETNS_NAME}" ip addr show "${GUEST_TAP}" | grep -q "${GUEST_GW}/30"; then
    log_pass "Pod netns tap 已配置: ${GUEST_TAP} ${GUEST_GW}/30"
else
    log_fail "Pod netns tap 未正确配置: ${GUEST_TAP}"
    ip netns exec "${NETNS_NAME}" ip addr show "${GUEST_TAP}" >&2 || true
    exit 1
fi
if "${ADB_BIN}" -s "${ADB_URL}" shell "su 0 ip addr show eth1" | grep -q "${GUEST_IP}/30"; then
    log_pass "Android guest eth1 已配置: ${GUEST_IP}/30"
else
    log_fail "Android guest eth1 未正确配置: ${GUEST_IP}/30"
    "${ADB_BIN}" -s "${ADB_URL}" shell "su 0 ip addr show eth1" >&2 || true
    exit 1
fi

log_step "3.6 验证 Android guest 通过 CNI 访问集群 TCP 服务"
if [ -z "${ANDROID_EGRESS_TEST_HOST}" ]; then
    log_fail "无法确定 Android guest 出向测试目标"
    exit 1
fi
if "${ADB_BIN}" -s "${ADB_URL}" shell "su 0 sh -c 'toybox nc -w 5 ${ANDROID_EGRESS_TEST_HOST} ${ANDROID_EGRESS_TEST_PORT} </dev/null >/dev/null 2>&1'" >&2; then
    log_pass "Android guest 可经 Pod netns/CNI eth0 访问 ${ANDROID_EGRESS_TEST_HOST}:${ANDROID_EGRESS_TEST_PORT}"
else
    log_fail "Android guest 无法经 CNI 访问 ${ANDROID_EGRESS_TEST_HOST}:${ANDROID_EGRESS_TEST_PORT}"
    "${ADB_BIN}" -s "${ADB_URL}" shell "su 0 ip route show table local_network; su 0 ip rule show" >&2 || true
    ip netns exec "${NETNS_NAME}" iptables -t nat -S >&2 || true
    ip netns exec "${NETNS_NAME}" iptables -S >&2 || true
    exit 1
fi

log_step "3.7 验证 PodIP 到 Android guest 内服务"
"${ADB_BIN}" -s "${ADB_URL}" shell "su 0 pkill -f 'toybox nc.*${ANDROID_GUEST_SERVICE_PORT}' 2>/dev/null || true" >&2 || true
"${ADB_BIN}" -s "${ADB_URL}" shell "su 0 sh -c 'toybox nc -L -p ${ANDROID_GUEST_SERVICE_PORT} sh -c '\''printf \"HTTP/1.1 200 OK\r\nContent-Length: 15\r\n\r\nANDROID-CNI-OK\n\"'\'' >/data/local/tmp/cni-guest-http.log 2>&1 &'" >&2
HTTP_BODY=$(curl -sS --max-time 5 "http://${POD_IP}:${ANDROID_GUEST_SERVICE_PORT}/" 2>/tmp/android-cni-guest-curl.err || true)
if grep -q "ANDROID-CNI-OK" <<< "${HTTP_BODY}"; then
    log_pass "PodIP:${ANDROID_GUEST_SERVICE_PORT} 可访问 Android guest 内服务"
else
    log_fail "PodIP:${ANDROID_GUEST_SERVICE_PORT} 无法访问 Android guest 内服务"
    cat /tmp/android-cni-guest-curl.err >&2 || true
    echo "${HTTP_BODY}" >&2
    ip netns exec "${NETNS_NAME}" iptables -t nat -S >&2 || true
    ip netns exec "${NETNS_NAME}" iptables -S >&2 || true
    exit 1
fi

log_step "3.8 验证 Android guest DNS 当前状态"
DNS_OUTPUT=$("${ADB_BIN}" -s "${ADB_URL}" shell "getent hosts ${ANDROID_DNS_TEST_HOST} 2>/dev/null || toybox nslookup ${ANDROID_DNS_TEST_HOST} 2>/dev/null | head -n 8 || ping -c 1 -W 1 ${ANDROID_DNS_TEST_HOST} 2>&1 | head -n 1" 2>&1 || true)
echo "${DNS_OUTPUT}" >&2
if grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}[[:space:]]|Address[[:space:]]+[0-9]+:|PING .*\(([0-9]{1,3}\.){3}[0-9]{1,3}\)' <<< "${DNS_OUTPUT}"; then
    log_pass "Android guest hostname DNS 解析可用: ${ANDROID_DNS_TEST_HOST}"
elif DNS_DUMPSYS=$("${ADB_BIN}" -s "${ADB_URL}" shell "su 0 dumpsys dnsresolver | sed -n '1,180p'" 2>&1 || true) &&
     grep -Eq 'NOERROR:[1-9][0-9]*' <<< "${DNS_DUMPSYS}"; then
    echo "${DNS_DUMPSYS}" >&2
    log_pass "Android guest hostname DNS 解析可用: ${ANDROID_DNS_TEST_HOST}"
else
    log_fail "Android guest hostname DNS 解析不可用: ${ANDROID_DNS_TEST_HOST}"
    "${ADB_BIN}" -s "${ADB_URL}" shell "su 0 dumpsys ethernet | sed -n '1,120p'; su 0 dumpsys dnsresolver | sed -n '1,180p'" >&2 || true
    exit 1
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
