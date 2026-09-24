#!/bin/bash
###############################################################################
# 34_cni_public_egress.sh — E2B CNI 沙箱出公网（default 路由）正向验证
#
# 验证目标（针对 external netns 缺 default 路由导致出网全断的缺陷回归）：
#   1. E2B CNI Pod Ready 并取得 PodIP
#   2. 沙箱 netns 路由表包含 default 路由（CNI routes 段或 orchestrator 补装，
#      二者居其一即可；都没有则出向流量在路由查找阶段被丢弃，含 DNS）
#   3. netns 视角出公网：curl 探针返回非 000（覆盖 DNS 解析 + 默认路由 + SNAT）
#   4. VM 内视角出公网 best-effort（guest 无 curl/wget 时记 SKIP）
#
# 探针地址经 EGRESS_PROBE_URL 可配（默认 https://api.deepseek.com）。
# 断言"非 000"而非具体状态码：链路通即可（200/401/403 都算通），000 = 断。
# 宿主机本身不可达探针时（离线环境）出网断言记 SKIP，default 路由断言
# 与环境无关，始终执行。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/cni_behavior_common.sh"

log_section "34 — E2B CNI 沙箱出公网（default 路由）验证"

POD_NAME="${POD_NAME:-e2b-cni-public-egress-test}"
APP_LABEL="${APP_LABEL:-e2b-cni-public-egress}"
EGRESS_PROBE_URL="${EGRESS_PROBE_URL:-https://api.deepseek.com}"
EGRESS_PROBE_MAX_TIME="${EGRESS_PROBE_MAX_TIME:-10}"

cleanup() {
    cleanup_names "${POD_NAME}"
}
trap cleanup EXIT

# e2b_guest_http_code <pod> <url> — 在 E2B guest 内执行 HTTP 请求，输出状态码
e2b_guest_http_code() {
    local pod="$1"
    local url="$2"
    kubectl exec "${pod}" -- sh -c "if command -v curl >/dev/null 2>&1; then curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time ${EGRESS_PROBE_MAX_TIME} '${url}'; elif command -v wget >/dev/null 2>&1; then wget -q -T ${EGRESS_PROBE_MAX_TIME} -O /dev/null '${url}' && echo 200 || echo 000; else echo NO_HTTP_CLIENT; fi" 2>/tmp/e2b-public-egress.err || true
}

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

log_step "2.1 创建 E2B CNI Pod"
prepare_e2b_cni_pod_yaml "${POD_NAME}" "${APP_LABEL}" || exit 1
apply_and_wait_pod_ready "${POD_NAME}" "${E2B_CNI_YAML}" "120s" || exit 1
POD_IP=$(get_pod_ip_or_fail "${POD_NAME}") || exit 1
log_pass "E2B PodIP: ${POD_IP}"

log_step "2.2 解析沙箱 netns"
NETNS_NAME=$(e2b_netns_name "${POD_NAME}") || {
    log_fail "无法解析 E2B Pod 的 netns 名: ${POD_NAME}"
    exit 1
}
if [ ! -e "/var/run/netns/${NETNS_NAME}" ]; then
    log_fail "E2B netns 不存在: ${NETNS_NAME}"
    exit 1
fi
log_pass "沙箱 netns: ${NETNS_NAME}"

log_step "3.1 netns 路由表须含 default 路由（缺陷核心断言）"
NETNS_ROUTES=$(ip netns exec "${NETNS_NAME}" ip route 2>&1 || true)
echo "${NETNS_ROUTES}" >&2
if grep -q '^default ' <<< "${NETNS_ROUTES}"; then
    DEFAULT_ROUTE=$(grep '^default ' <<< "${NETNS_ROUTES}" | head -1)
    log_pass "netns 含 default 路由: ${DEFAULT_ROUTE}"
else
    log_fail "netns 缺 default 路由：VM/netns 出向流量（含 DNS）会在路由查找阶段被丢弃"
    log_fail "修复方向：orchestrator CreateExternalNetNSNetwork 按 CNI gateway 补 default 路由，或 CNI IPAM 配置 routes 段"
    exit 1
fi

log_step "3.2 宿主机探针可达性预检（离线环境降级）"
HOST_PROBE_CODE=$(host_curl_code "${EGRESS_PROBE_URL}")
if [ "${HOST_PROBE_CODE}" = "000" ] || [ -z "${HOST_PROBE_CODE}" ]; then
    log_skip "宿主机不可达探针 ${EGRESS_PROBE_URL}（HTTP ${HOST_PROBE_CODE:-空}），跳过出网断言；default 路由断言已通过"
    print_summary
    exit 0
fi
log_pass "宿主机可达探针 ${EGRESS_PROBE_URL}（HTTP ${HOST_PROBE_CODE}）"

log_step "3.3 netns 视角出公网"
NETNS_CODE=""
NETNS_DEADLINE=$(( $(date +%s) + 30 ))
while true; do
    NETNS_CODE=$(ip netns exec "${NETNS_NAME}" curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time "${EGRESS_PROBE_MAX_TIME}" "${EGRESS_PROBE_URL}" 2>/tmp/e2b-netns-egress.err || true)
    if [ -n "${NETNS_CODE}" ] && [ "${NETNS_CODE}" != "000" ]; then
        break
    fi
    if [ "$(date +%s)" -ge "${NETNS_DEADLINE}" ]; then
        break
    fi
    sleep 2
done
if [ -n "${NETNS_CODE}" ] && [ "${NETNS_CODE}" != "000" ]; then
    log_pass "netns 出公网成功: ${NETNS_NAME} -> ${EGRESS_PROBE_URL} HTTP ${NETNS_CODE}"
else
    log_fail "netns 出公网失败: ${NETNS_NAME} -> ${EGRESS_PROBE_URL} HTTP ${NETNS_CODE:-空}（宿主机可达，沙箱 netns 不可达）"
    cat /tmp/e2b-netns-egress.err >&2 || true
    exit 1
fi

log_step "3.4 VM 内视角出公网（best-effort）"
GUEST_CODE=""
GUEST_DEADLINE=$(( $(date +%s) + 60 ))
while true; do
    GUEST_CODE=$(e2b_guest_http_code "${POD_NAME}" "${EGRESS_PROBE_URL}")
    if [ "${GUEST_CODE}" = "NO_HTTP_CLIENT" ]; then
        break
    fi
    if [ -n "${GUEST_CODE}" ] && [ "${GUEST_CODE}" != "000" ]; then
        break
    fi
    if [ "$(date +%s)" -ge "${GUEST_DEADLINE}" ]; then
        break
    fi
    sleep 2
done
if [ "${GUEST_CODE}" = "NO_HTTP_CLIENT" ]; then
    log_skip "E2B VM 内无 curl/wget，跳过 guest 出网断言（netns 视角已通过）"
elif [ -n "${GUEST_CODE}" ] && [ "${GUEST_CODE}" != "000" ]; then
    log_pass "VM 内出公网成功: HTTP ${GUEST_CODE}"
else
    log_fail "VM 内出公网失败: HTTP ${GUEST_CODE:-空}（netns 可达但 guest 不可达）"
    cat /tmp/e2b-public-egress.err >&2 || true
    exit 1
fi

log_step "4.1 删除资源"
cleanup
log_pass "资源删除请求已提交"

print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证完成：E2B CNI 沙箱出公网（default 路由）行为已检查"
    exit 0
else
    exit 1
fi
