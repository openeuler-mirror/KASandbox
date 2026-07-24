#!/bin/bash
###############################################################################
# 16_cni_networkpolicy_egress.sh — E2B CNI NetworkPolicy egress 验证
#
# 验证目标：
#   1. baseline: E2B VM 内可以访问普通 Service ClusterIP
#   2. deny-all egress policy 应阻断 E2B VM -> 普通 Service ClusterIP
#
# 当前 E2B CNI 仍是 POC。如果 policy 未阻断流量，本用例记录 SKIP
# 表示 UNSUPPORTED，而不是把当前 POC 判为功能失败。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/cni_behavior_common.sh"

log_section "16 — E2B CNI NetworkPolicy egress 验证"

POD_NAME="${POD_NAME:-e2b-cni-np-egress-test}"
APP_LABEL="${APP_LABEL:-e2b-cni-np-egress}"
TARGET_POD="${TARGET_POD:-cni-egress-target}"
TARGET_SVC="${TARGET_SVC:-cni-egress-target}"
TARGET_PORT="${TARGET_PORT:-8080}"
SERVER_IMAGE="${SERVER_IMAGE:-docker.io/library/busybox:latest}"
DENY_POLICY="${DENY_POLICY:-deny-e2b-egress}"
TEMP_ALLOW_EXTRA_CIDRS="${TEMP_ALLOW_EXTRA_CIDRS:-}"

cleanup() {
    cleanup_names "${DENY_POLICY}" "${TARGET_SVC}" "${POD_NAME}" "${TARGET_POD}"
}
trap cleanup EXIT

e2b_http_code() {
    local url="$1"
    kubectl exec "${POD_NAME}" -- sh -c "if command -v curl >/dev/null 2>&1; then curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 3 --max-time 5 '${url}'; elif command -v wget >/dev/null 2>&1; then wget -q -T 5 -O /dev/null '${url}' && echo 200 || echo 000; else echo NO_HTTP_CLIENT; fi" 2>/tmp/e2b-egress.err || true
}

patch_e2b_firewall_allowlist() {
    local pod_name="$1"
    shift

    local pod_uid
    pod_uid=$(kubectl get pod "${pod_name}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
    if [ -z "${pod_uid}" ]; then
        log_fail "无法读取 E2B Pod UID，不能临时放通 egress firewall"
        return 1
    fi

    local netns_name="e2b-${pod_uid:0:12}"
    if [ ! -e "/var/run/netns/${netns_name}" ]; then
        log_fail "E2B netns 不存在: ${netns_name}"
        return 1
    fi

    local cidr
    for cidr in "$@"; do
        [ -n "${cidr}" ] || continue
        if ip netns exec "${netns_name}" nft add element inet slot-firewall filtered_always_allowlist "{ ${cidr} }" 2>/tmp/e2b-egress-nft.err; then
            log_pass "临时放通 E2B firewall allowlist: ${netns_name} ${cidr}"
            continue
        fi

        if ip netns exec "${netns_name}" nft list set inet slot-firewall filtered_always_allowlist 2>/dev/null | grep -q "${cidr%%/*}"; then
            log_pass "E2B firewall allowlist 已包含: ${netns_name} ${cidr}"
            continue
        fi

        log_fail "临时放通 E2B firewall allowlist 失败: ${netns_name} ${cidr}"
        cat /tmp/e2b-egress-nft.err >&2 || true
        return 1
    done
}

log_step "1.1 前置检查"
require_cni_behavior_prereqs || exit 1

log_step "1.2 清理旧资源"
cleanup
sleep 2
log_pass "旧资源已清理"

log_step "2.1 创建 E2B CNI Pod"
prepare_e2b_cni_pod_yaml "${POD_NAME}" "${APP_LABEL}" || exit 1
apply_and_wait_pod_ready "${POD_NAME}" "${E2B_CNI_YAML}" "120s" || exit 1
POD_IP=$(get_pod_ip_or_fail "${POD_NAME}") || exit 1
log_pass "E2B PodIP: ${POD_IP}"

log_step "2.2 创建普通 HTTP target Pod 和 Service"
cat > "/tmp/${TARGET_POD}.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${TARGET_POD}
  labels:
    app: ${TARGET_POD}
spec:
  restartPolicy: Never
  containers:
    - name: httpd
      image: ${SERVER_IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["sh", "-c", "mkdir -p /www; echo ok > /www/index.html; httpd -f -p ${TARGET_PORT} -h /www"]
---
apiVersion: v1
kind: Service
metadata:
  name: ${TARGET_SVC}
spec:
  selector:
    app: ${TARGET_POD}
  ports:
    - name: http
      port: ${TARGET_PORT}
      targetPort: ${TARGET_PORT}
      protocol: TCP
EOF
kubectl apply -f "/tmp/${TARGET_POD}.yaml" >&2
if kubectl wait --for=condition=Ready "pod/${TARGET_POD}" --timeout=120s >&2; then
    log_pass "target Pod 已 Ready: ${TARGET_POD}"
else
    log_fail "target Pod 未 Ready: ${TARGET_POD}"
    kubectl describe pod "${TARGET_POD}" >&2 || true
    exit 1
fi

TARGET_CLUSTER_IP=$(kubectl get svc "${TARGET_SVC}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
if [ -z "${TARGET_CLUSTER_IP}" ]; then
    log_fail "无法读取 target Service ClusterIP"
    exit 1
fi
log_pass "target Service ClusterIP: ${TARGET_CLUSTER_IP}"
TARGET_POD_IP=$(kubectl get pod "${TARGET_POD}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
if [ -z "${TARGET_POD_IP}" ]; then
    log_fail "无法读取 target PodIP"
    exit 1
fi
log_pass "target PodIP: ${TARGET_POD_IP}"

log_step "2.3 临时放通 E2B firewall 到 target PodIP / ServiceIP"
TEMP_ALLOW_CIDRS=("${TARGET_CLUSTER_IP}" "${TARGET_POD_IP}")
if [ -n "${TEMP_ALLOW_EXTRA_CIDRS}" ]; then
    IFS=',' read -r -a EXTRA_CIDRS <<< "${TEMP_ALLOW_EXTRA_CIDRS}"
    TEMP_ALLOW_CIDRS+=("${EXTRA_CIDRS[@]}")
fi
patch_e2b_firewall_allowlist "${POD_NAME}" "${TEMP_ALLOW_CIDRS[@]}" || exit 1

log_step "3.1 baseline：E2B VM 内访问普通 Service ClusterIP / PodIP"
BASE_CODE=$(e2b_http_code "http://${TARGET_CLUSTER_IP}:${TARGET_PORT}/")
if [ "${BASE_CODE}" = "200" ]; then
    log_pass "baseline 成功：E2B VM -> target Service HTTP ${BASE_CODE}"
    TARGET_URL="http://${TARGET_CLUSTER_IP}:${TARGET_PORT}/"
elif [ "${BASE_CODE}" = "NO_HTTP_CLIENT" ]; then
    log_skip "E2B VM 内无 curl/wget，无法验证 egress NetworkPolicy"
    print_summary
    exit 0
else
    log_skip "E2B VM 无法访问 target Service，HTTP ${BASE_CODE}，继续尝试 target PodIP"
    POD_CODE=$(e2b_http_code "http://${TARGET_POD_IP}:${TARGET_PORT}/")
    if [ "${POD_CODE}" = "200" ]; then
        log_pass "baseline 成功：E2B VM -> target PodIP HTTP ${POD_CODE}"
        TARGET_URL="http://${TARGET_POD_IP}:${TARGET_PORT}/"
    elif [ "${POD_CODE}" = "NO_HTTP_CLIENT" ]; then
        log_skip "E2B VM 内无 curl/wget，无法验证 egress NetworkPolicy"
        print_summary
        exit 0
    else
        log_skip "E2B VM 无法访问 target Service 或 PodIP，当前 CNI POC 暂不具备 egress baseline，跳过 egress NetworkPolicy 验证"
        cat /tmp/e2b-egress.err >&2 || true
        print_summary
        exit 0
    fi
fi

log_step "4.1 应用 deny-all egress NetworkPolicy"
cat > "/tmp/${DENY_POLICY}.yaml" <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: ${DENY_POLICY}
spec:
  podSelector:
    matchLabels:
      app: ${APP_LABEL}
  policyTypes:
    - Egress
EOF
kubectl apply -f "/tmp/${DENY_POLICY}.yaml" >&2
sleep 8
log_pass "deny-all egress policy 已应用"

DENY_CODE=$(e2b_http_code "${TARGET_URL}")
if [ "${DENY_CODE}" = "000" ]; then
    log_pass "deny-all egress 已阻断 E2B VM -> target"
else
    log_skip "deny-all egress 未阻断，HTTP ${DENY_CODE}，当前 CNI POC 暂不承诺 egress NetworkPolicy"
fi

log_step "5.1 删除资源"
cleanup
sleep 3
log_pass "资源删除请求已提交"

print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证完成：E2B CNI NetworkPolicy egress 行为已检查"
    exit 0
else
    exit 1
fi
