#!/bin/bash
###############################################################################
# 14_cni_service_dns.sh — E2B CNI Service/DNS 行为验证
#
# 验证目标：
#   1. RuntimeClass=e2b Pod 在 CNI 模式下 Ready，并获得 PodIP
#   2. Service EndpointSlice 包含 E2B PodIP
#   3. 普通 client Pod 可以直接访问 E2B PodIP:49983/health
#   4. 普通 client Pod 可以通过 Service DNS 访问 E2B envd health
#   5. E2B VM 内 Kubernetes DNS 行为做 best-effort 验证
#   6. E2B VM -> 宿主机（节点 IP）连通性保留（出向隔离不应误伤）
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/cni_behavior_common.sh"

log_section "14 — E2B CNI Service/DNS 行为验证"

POD_NAME="${POD_NAME:-e2b-cni-service-dns-test}"
APP_LABEL="${APP_LABEL:-e2b-cni-service-dns}"
SVC_NAME="${SVC_NAME:-e2b-cni-service-dns}"
CLIENT_POD="${CLIENT_POD:-cni-service-dns-client}"

cleanup() {
    cleanup_names "${SVC_NAME}" "${POD_NAME}" "${CLIENT_POD}"
}
trap cleanup EXIT

log_step "1.1 前置检查"
require_cni_behavior_prereqs || exit 1

if ! command -v curl >/dev/null 2>&1; then
    log_fail "宿主机 curl 不存在"
    exit 1
fi
log_pass "宿主机 curl 可用"

log_step "1.2 清理旧资源"
cleanup
log_pass "旧资源已清理"

log_step "2.1 刷新 build_id 并生成带 label 的 E2B Pod YAML"
prepare_e2b_cni_pod_yaml "${POD_NAME}" "${APP_LABEL}" || exit 1

log_step "2.2 创建 E2B CNI Pod"
apply_and_wait_pod_ready "${POD_NAME}" "${E2B_CNI_YAML}" "120s" || exit 1
POD_IP=$(get_pod_ip_or_fail "${POD_NAME}") || exit 1
log_pass "E2B PodIP: ${POD_IP}"

log_step "2.3 创建 Service 和 client Pod"
create_e2b_service "${SVC_NAME}" "${APP_LABEL}" "${ENVD_PORT}"
wait_endpoint_contains_ip "${SVC_NAME}" "${POD_IP}" || exit 1
create_curl_client_pod "${CLIENT_POD}" "role" "cni-service-dns-client" || exit 1

log_step "3.1 从宿主机验证 E2B PodIP health"
HOST_CODE=$(host_curl_code "http://${POD_IP}:${ENVD_PORT}/health")
if [ "${HOST_CODE}" = "204" ]; then
    log_pass "宿主机访问 PodIP:${ENVD_PORT}/health 成功，HTTP ${HOST_CODE}"
else
    log_fail "宿主机访问 PodIP:${ENVD_PORT}/health 失败，HTTP ${HOST_CODE}"
    cat /tmp/cni-host-curl.err >&2 || true
    exit 1
fi

log_step "3.2 从普通 Pod 直接访问 E2B PodIP"
expect_http_204_from_client "${CLIENT_POD}" "http://${POD_IP}:${ENVD_PORT}/health" "client Pod -> E2B PodIP" || exit 1

log_step "3.3 从普通 Pod 通过 Service DNS 访问 E2B"
expect_http_204_from_client "${CLIENT_POD}" "http://${SVC_NAME}:${ENVD_PORT}/health" "client Pod -> Service DNS" || exit 1

log_step "3.4 验证 Service DNS 解析"
DNS_OUTPUT=$(client_dns_lookup "${CLIENT_POD}" "${SVC_NAME}.default.svc.cluster.local" || true)
if grep -Eq '([0-9]{1,3}\.){3}[0-9]{1,3}' <<< "${DNS_OUTPUT}"; then
    log_pass "client Pod 可解析 Service FQDN"
else
    log_skip "client Pod DNS 工具不可用或未返回 IP；Service curl 已作为 DNS 行为主验证"
    echo "${DNS_OUTPUT}" >&2 || true
fi

log_step "3.5 E2B VM 内 Kubernetes DNS best-effort 验证"
# 注意：不能把 resolv.conf 一起抓回来做匹配——它必然含 nameserver IPv4，
# 会让这条断言恒真。这里只取"真正解析"的输出。
E2B_RESOLV=$(kubectl exec "${POD_NAME}" -- sh -c 'cat /etc/resolv.conf 2>/dev/null' 2>&1 || true)
E2B_DNS_OUTPUT=$(kubectl exec "${POD_NAME}" -- sh -c '
  getent hosts kubernetes.default.svc.cluster.local 2>/dev/null \
    || getent ahostsv4 kubernetes.default.svc.cluster.local 2>/dev/null \
    || (nslookup kubernetes.default.svc.cluster.local 2>/dev/null | sed -n "/Name:/,+1p") \
    || true
' 2>&1 || true)
log_info "E2B VM resolv.conf:"; printf '%s\n' "${E2B_RESOLV}" | sed 's/^/    /' >&2
log_info "E2B VM DNS 解析输出:"; printf '%s\n' "${E2B_DNS_OUTPUT}" | sed 's/^/    /' >&2

# 只认「IP + kubernetes.default...」这种真正的解析结果（getent 格式），
# 或 nslookup 的 Name/Address 两行组合。
if grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}[[:space:]].*kubernetes\.default' <<< "${E2B_DNS_OUTPUT}" ||
   { grep -q 'Name:.*kubernetes\.default' <<< "${E2B_DNS_OUTPUT}" && grep -Eq '([0-9]{1,3}\.){3}[0-9]{1,3}' <<< "${E2B_DNS_OUTPUT}"; }; then
    log_pass "E2B VM 内 Kubernetes DNS 解析成功"
else
    log_unsupported "E2B VM 内 Kubernetes DNS 未解析成功（当前 CNI POC 不承诺 guest DNS）"
fi

log_step "3.6 E2B VM -> 宿主机连通性保留验证（隔离不应误伤节点 IP）"
NODE_IP=$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)
if [ -z "${NODE_IP}" ]; then
    log_skip "无法读取节点 InternalIP，跳过宿主机连通性验证"
else
    GUEST_HOST_CODE=$(kubectl exec "${POD_NAME}" -- sh -c "if command -v curl >/dev/null 2>&1; then curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 3 --max-time 5 'http://${NODE_IP}:80/'; elif command -v wget >/dev/null 2>&1; then wget -q -T 5 -O /dev/null 'http://${NODE_IP}:80/' && echo 200 || echo 000; else echo NO_HTTP_CLIENT; fi" 2>/dev/null || true)
    if [ "${GUEST_HOST_CODE}" = "NO_HTTP_CLIENT" ]; then
        log_skip "E2B VM 内无 curl/wget，跳过宿主机连通性验证"
    elif [ "${GUEST_HOST_CODE}" != "000" ] && [ -n "${GUEST_HOST_CODE}" ]; then
        log_pass "E2B VM -> 宿主机 ${NODE_IP}:80 可达（HTTP ${GUEST_HOST_CODE}），出向隔离未误伤宿主机"
    else
        log_fail "E2B VM -> 宿主机 ${NODE_IP}:80 不可达（HTTP ${GUEST_HOST_CODE}）"
        exit 1
    fi
fi

log_step "4.1 删除资源"
cleanup
log_pass "资源删除请求已提交"

print_summary
if [ "${FAIL_COUNT}" -eq 0 ] && [ "${UNSUPPORTED_COUNT}" -eq 0 ]; then
    log_info "验证完成：E2B CNI Service/DNS 基础行为已检查"
fi
exit "$(summary_exit_code)"
