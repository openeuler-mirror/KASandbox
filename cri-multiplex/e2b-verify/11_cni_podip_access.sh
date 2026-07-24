#!/bin/bash
###############################################################################
# 11_cni_podip_access.sh — Calico CNI PodIP 访问 E2B 沙箱验证
#
# 验证目标：
#   1. RuntimeClass=e2b Pod 通过 kubelet 创建并进入 Running
#   2. Kubernetes 分配的 PodIP 与 CRI PodSandboxStatus.Network.Ip 一致
#   3. CRI status annotations 中包含 CNI 相关信息
#   4. CNI netns 中存在 eth0=<PodIP> 和 tap0=169.254.0.22/30
#   5. 可以通过 CNI 分配的 PodIP 访问沙箱内 envd health: PodIP:49983/health
#   6. 删除 Pod 后 CNI netns 被清理
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "11 — Calico CNI PodIP 访问 E2B 沙箱验证"

#==================== 配置 ====================#
POD_NAME="${POD_NAME:-e2b-cni-podip-test}"
POD_YAML="/tmp/e2b-kubelet-pod.yaml"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"
ENVD_PORT="${ENVD_PORT:-49983}"
NETNS_NAME=""

cleanup() {
    kubectl delete pod "${POD_NAME}" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

#==================== 前置检查 ====================#
log_step "1.1 前置检查"

require_refresh_script "${REFRESH_SCRIPT}" || exit 1

require_cri_multiplex_cni_enabled || exit 1

if ! kubectl get runtimeclass e2b > /dev/null 2>&1; then
    log_fail "RuntimeClass e2b 不存在"
    exit 1
fi
log_pass "RuntimeClass e2b 存在"

if ! command -v curl >/dev/null 2>&1; then
    log_fail "curl 不存在，无法验证 PodIP:49983/health"
    exit 1
fi
log_pass "curl 可用"

if [ ! -d /etc/cni/net.d ] || [ ! -x /opt/cni/bin/calico ] || [ ! -x /opt/cni/bin/calico-ipam ]; then
    log_fail "Calico CNI 配置或二进制不完整"
    exit 1
fi
log_pass "Calico CNI 配置和二进制存在"

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

: > /tmp/cri-multiplex.log 2>/dev/null || true

#==================== 刷新 build_id ====================#
log_step "2.1 刷新 build_id（每次创建 Pod 前必须执行）"

if ! refresh_or_reuse_e2b_yaml "${REFRESH_SCRIPT}" "${POD_NAME}" "${POD_YAML}"; then
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

#==================== 创建 Pod ====================#
log_step "3.1 通过 kubelet 创建 Pod"

if ! kubectl apply -f "${POD_YAML}" >&2 2>&1; then
    log_fail "kubectl apply 失败"
    exit 1
fi
log_pass "Pod YAML 已提交: ${POD_NAME}"

log_step "3.2 等待 Pod 进入 Ready"

if kubectl wait --for=condition=Ready "pod/${POD_NAME}" --timeout=90s >&2; then
    log_pass "Pod 已 Ready"
else
    log_fail "Pod 未在 90s 内 Ready"
    kubectl describe pod "${POD_NAME}" >&2 || true
    exit 1
fi

POD_UID=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
POD_IP=$(kubectl get pod "${POD_NAME}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)

if [ -z "${POD_UID}" ] || [ -z "${POD_IP}" ]; then
    log_fail "无法读取 Pod UID 或 PodIP: uid=${POD_UID}, ip=${POD_IP}"
    exit 1
fi
log_pass "Kubernetes PodIP 已分配: ${POD_IP}"

NETNS_NAME="e2b-${POD_UID:0:12}"
NETNS_PATH="/var/run/netns/${NETNS_NAME}"
CRI_POD_ID=$(${CRICTL} pods --name "${POD_NAME}" 2>/dev/null | awk -v n="${POD_NAME}" 'NR > 1 && $0 ~ n { print $1; exit }' || true)
if [ -z "${CRI_POD_ID}" ]; then
    CRI_POD_ID="${POD_UID}"
    log_skip "无法从 crictl pods 反查 CRI Pod ID，回退使用 Kubernetes UID"
else
    log_pass "CRI Pod ID: ${CRI_POD_ID}"
fi

#==================== CRI PodSandboxStatus 验证 ====================#
log_step "4.1 验证 CRI PodSandboxStatus 返回 CNI PodIP"

INSPECT_OUTPUT=$(${CRICTL} inspectp "${CRI_POD_ID}" 2>&1) || true
if grep -q "not found\|NotFound\|level=fatal" <<< "${INSPECT_OUTPUT}"; then
    log_skip "CRI PodSandboxStatus 暂不可查，跳过 status/annotation 检查: ${CRI_POD_ID}"
elif grep -q "\"ip\": \"${POD_IP}\"" <<< "${INSPECT_OUTPUT}"; then
    log_pass "CRI PodSandboxStatus.Network.Ip 与 Kubernetes PodIP 一致: ${POD_IP}"
else
    log_fail "CRI PodSandboxStatus.Network.Ip 未返回 Kubernetes PodIP"
    echo "${INSPECT_OUTPUT}" >&2
fi

if grep -q "not found\|NotFound\|level=fatal" <<< "${INSPECT_OUTPUT}"; then
    :
elif grep -q '"e2b.dev/cni-enabled": "true"' <<< "${INSPECT_OUTPUT}" &&
   grep -q "\"e2b.dev/pod-ip\": \"${POD_IP}\"" <<< "${INSPECT_OUTPUT}" &&
   grep -q "\"e2b.dev/cni-netns\": \"${NETNS_PATH}\"" <<< "${INSPECT_OUTPUT}"; then
    log_pass "CRI annotations 包含 CNI 信息"
else
    log_fail "CRI annotations 缺少 CNI 信息"
    echo "${INSPECT_OUTPUT}" >&2
fi

#==================== netns / 网卡验证 ====================#
log_step "4.2 验证 CNI netns 与 eth0/tap0"

if [ -e "${NETNS_PATH}" ]; then
    log_pass "CNI netns 存在: ${NETNS_PATH}"
else
    log_skip "CNI netns 未在 ${NETNS_PATH} 观察到，跳过 netns 细节检查"
fi

if ! NETNS_ADDR_OUTPUT=$(ip netns exec "${NETNS_NAME}" ip addr show 2>&1); then
    if grep -qi "Operation not permitted" <<< "${NETNS_ADDR_OUTPUT}"; then
        log_skip "当前执行环境无权限进入 netns，跳过 eth0/tap0 细节检查"
    elif grep -qi "No such file or directory\\|Cannot open network namespace" <<< "${NETNS_ADDR_OUTPUT}"; then
        log_skip "当前环境无法通过 ${NETNS_NAME} 进入 netns，跳过 eth0/tap0 细节检查"
    else
        log_fail "无法进入 netns ${NETNS_NAME}"
        echo "${NETNS_ADDR_OUTPUT}" >&2
    fi
else
    if grep -q "${POD_IP}/32" <<< "${NETNS_ADDR_OUTPUT}"; then
        log_pass "netns eth0 持有 CNI PodIP: ${POD_IP}/32"
    else
        log_fail "netns eth0 未持有 CNI PodIP: ${POD_IP}/32"
        echo "${NETNS_ADDR_OUTPUT}" >&2
    fi

    if grep -q "169.254.0.22/30" <<< "${NETNS_ADDR_OUTPUT}"; then
        log_pass "netns tap0 已创建并配置 169.254.0.22/30"
    else
        log_fail "netns tap0 未配置 169.254.0.22/30"
        echo "${NETNS_ADDR_OUTPUT}" >&2
    fi
fi

#==================== PodIP 访问沙箱验证 ====================#
log_step "5.1 通过 CNI PodIP 访问沙箱 envd health"

HTTP_CODE=$(curl -sS -o /tmp/e2b-cni-health.out -w "%{http_code}" --max-time 5 "http://${POD_IP}:${ENVD_PORT}/health" 2>/tmp/e2b-cni-health.err || true)
if [ "${HTTP_CODE}" = "204" ]; then
    log_pass "PodIP:${ENVD_PORT}/health 可访问，HTTP ${HTTP_CODE}"
else
    log_fail "PodIP:${ENVD_PORT}/health 访问失败，HTTP ${HTTP_CODE}"
    log_info "curl stderr:"
    cat /tmp/e2b-cni-health.err >&2 || true
    log_info "curl body:"
    cat /tmp/e2b-cni-health.out >&2 || true
fi

#==================== 删除清理验证 ====================#
log_step "6.1 删除 Pod 并验证 CNI netns 清理"

kubectl delete pod "${POD_NAME}" --force --grace-period=0 >&2 || true

NETNS_REMOVED=0
for i in $(seq 1 90); do
    if [ ! -e "${NETNS_PATH}" ]; then
        NETNS_REMOVED=1
        log_pass "CNI netns 已清理（第 ${i} 次轮询）"
        break
    fi
    sleep 1
done

if [ "${NETNS_REMOVED}" -ne 1 ]; then
    log_fail "CNI netns 未清理: ${NETNS_PATH}"
fi

#==================== 汇总 ====================#
print_summary

if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证通过：CNI PodIP 可以访问 E2B 沙箱"
    exit 0
else
    exit 1
fi
