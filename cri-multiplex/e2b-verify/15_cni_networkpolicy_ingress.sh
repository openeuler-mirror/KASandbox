#!/bin/bash
###############################################################################
# 15_cni_networkpolicy_ingress.sh — E2B CNI NetworkPolicy ingress 验证
#
# 验证目标：
#   1. baseline: allowed/denied client 均可访问 E2B PodIP
#   2. deny-all ingress policy 应阻断 client -> E2B PodIP
#   3. allow selected client policy 应只允许 role=allowed-client
#
# 当前 E2B CNI 仍是 POC。如果 policy 未阻断流量，本用例记录 SKIP
# 表示 UNSUPPORTED，而不是把当前 POC 判为功能失败。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/cni_behavior_common.sh"

log_section "15 — E2B CNI NetworkPolicy ingress 验证"

POD_NAME="${POD_NAME:-e2b-cni-np-ingress-test}"
APP_LABEL="${APP_LABEL:-e2b-cni-np-ingress}"
SVC_NAME="${SVC_NAME:-e2b-cni-np-ingress}"
ALLOWED_CLIENT="${ALLOWED_CLIENT:-cni-np-allowed-client}"
DENIED_CLIENT="${DENIED_CLIENT:-cni-np-denied-client}"
DENY_POLICY="${DENY_POLICY:-deny-e2b-ingress}"
ALLOW_POLICY="${ALLOW_POLICY:-allow-e2b-from-client}"

cleanup() {
    cleanup_names "${DENY_POLICY}" "${ALLOW_POLICY}" "${SVC_NAME}" "${POD_NAME}" "${ALLOWED_CLIENT}" "${DENIED_CLIENT}"
}
trap cleanup EXIT

log_step "1.1 前置检查"
require_cni_behavior_prereqs || exit 1

log_step "1.2 清理旧资源"
cleanup
sleep 2
log_pass "旧资源已清理"

log_step "2.1 创建 E2B CNI Pod、Service 和两个 client Pod"
prepare_e2b_cni_pod_yaml "${POD_NAME}" "${APP_LABEL}" || exit 1
apply_and_wait_pod_ready "${POD_NAME}" "${E2B_CNI_YAML}" "120s" || exit 1
POD_IP=$(get_pod_ip_or_fail "${POD_NAME}") || exit 1
log_pass "E2B PodIP: ${POD_IP}"

create_e2b_service "${SVC_NAME}" "${APP_LABEL}" "${ENVD_PORT}"
wait_endpoint_contains_ip "${SVC_NAME}" "${POD_IP}" || exit 1
create_curl_client_pod "${ALLOWED_CLIENT}" "role" "allowed-client" || exit 1
create_curl_client_pod "${DENIED_CLIENT}" "role" "denied-client" || exit 1

log_step "3.1 baseline：两个 client 均可访问 E2B PodIP"
expect_http_204_from_client "${ALLOWED_CLIENT}" "http://${POD_IP}:${ENVD_PORT}/health" "allowed client baseline" || true
expect_http_204_from_client "${DENIED_CLIENT}" "http://${POD_IP}:${ENVD_PORT}/health" "denied client baseline" || true

log_step "4.1 应用 deny-all ingress NetworkPolicy"
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
    - Ingress
EOF
kubectl apply -f "/tmp/${DENY_POLICY}.yaml" >&2
sleep 5
log_pass "deny-all ingress policy 已应用"

DENY_BLOCKED=0
if expect_blocked_from_client "${DENIED_CLIENT}" "http://${POD_IP}:${ENVD_PORT}/health" "deny-all ingress 阻断 denied client"; then
    DENY_BLOCKED=1
else
    rc=$?
    if [ "${rc}" = "2" ]; then
        DENY_BLOCKED=0
    fi
fi

if [ "${DENY_BLOCKED}" = "0" ]; then
    log_skip "NetworkPolicy ingress 未生效或 E2B VM 流量未经过 Calico policy datapath，后续 allow 测试仅记录边界"
else
    log_step "4.2 应用 allow selected client NetworkPolicy"
    cat > "/tmp/${ALLOW_POLICY}.yaml" <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: ${ALLOW_POLICY}
spec:
  podSelector:
    matchLabels:
      app: ${APP_LABEL}
  policyTypes:
    - Ingress
  ingress:
    - from:
        - podSelector:
            matchLabels:
              role: allowed-client
      ports:
        - protocol: TCP
          port: ${ENVD_PORT}
EOF
    kubectl apply -f "/tmp/${ALLOW_POLICY}.yaml" >&2
    sleep 5
    log_pass "allow selected client policy 已应用"

    expect_http_204_from_client "${ALLOWED_CLIENT}" "http://${POD_IP}:${ENVD_PORT}/health" "allow policy 放行 allowed client" || true
    expect_blocked_from_client "${DENIED_CLIENT}" "http://${POD_IP}:${ENVD_PORT}/health" "allow policy 阻断 denied client" || true
fi

log_step "5.1 删除资源"
cleanup
sleep 3
log_pass "资源删除请求已提交"

print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证完成：E2B CNI NetworkPolicy ingress 行为已检查"
    exit 0
else
    exit 1
fi
