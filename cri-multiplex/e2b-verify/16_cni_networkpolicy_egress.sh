#!/bin/bash
###############################################################################
# 16_cni_networkpolicy_egress.sh — E2B CNI 沙箱出向隔离 / NetworkPolicy egress 验证
#
# 验证目标：
#   1. baseline: E2B VM 内可以访问普通 Service ClusterIP（不经 Pod CIDR，恒通）
#   2. 沙箱间隔离: E2B VM A -> E2B Pod B PodIP 被 predefinedDenySet(Pod CIDR) 全协议阻断
#   3. Pod CIDR 内普通 Pod 默认同样被阻断；filtered_always_allowlist 例外放通后恢复可达
#   4. allow-internet=false 的用户级 deny 覆盖 TCP（CRI 模式修复验证）
#   5. deny-all egress NetworkPolicy best-effort（bridge 数据面不生效则记 SKIP）
#
# 前置：orchestrator 以 SANDBOX_DENIED_POD_CIDR=<podCIDR> 启动（ helm
#       sandboxDeniedPodCIDR ）。未注入时 2/3 项记录 SKIP（向后兼容）。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/cni_behavior_common.sh"

log_section "16 — E2B CNI 沙箱出向隔离 / NetworkPolicy egress 验证"

POD_NAME="${POD_NAME:-e2b-cni-np-egress-test}"
POD_NAME_B="${POD_NAME_B:-e2b-cni-np-egress-peer}"
POD_NAME_C="${POD_NAME_C:-e2b-cni-np-egress-noinet}"
APP_LABEL="${APP_LABEL:-e2b-cni-np-egress}"
TARGET_POD="${TARGET_POD:-cni-egress-target}"
TARGET_SVC="${TARGET_SVC:-cni-egress-target}"
TARGET_PORT="${TARGET_PORT:-8080}"
SERVER_IMAGE="${SERVER_IMAGE:-docker.io/library/busybox:latest}"
DENY_POLICY="${DENY_POLICY:-deny-e2b-egress}"
POD_CIDR="${POD_CIDR:-10.233.64.0/18}"

cleanup() {
    cleanup_names "${DENY_POLICY}" "${TARGET_SVC}" "${POD_NAME}" "${POD_NAME_B}" "${POD_NAME_C}" "${TARGET_POD}"
}
trap cleanup EXIT

# e2b_http_code <pod> <url> — 在 E2B guest 内执行 HTTP 请求，输出状态码
e2b_http_code() {
    local pod="$1"
    local url="$2"
    kubectl exec "${pod}" -- sh -c "if command -v curl >/dev/null 2>&1; then curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 3 --max-time 5 '${url}'; elif command -v wget >/dev/null 2>&1; then wget -q -T 5 -O /dev/null '${url}' && echo 200 || echo 000; else echo NO_HTTP_CLIENT; fi" 2>/tmp/e2b-egress.err || true
}

# e2b_netns_name <pod> — 解析 cri-multiplex CNI netns 名
# 优先从 cri-multiplex state 文件读取 cni_record.NetNSName（预热池命中时
# netns 保持 e2b-pool* 命名，无法从 UID 推导）；state 不可用时回退到
# 直建路径的推导规则：prefix("e2b-") + shortID(裸 Pod UID)，
# 长度 >12 时 shortID = uid[:6] + sha256(uid)[:6]（cni_manager.go）
e2b_netns_name() {
    local pod="$1"
    local pod_uid
    pod_uid=$(kubectl get pod "${pod}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
    if [ -z "${pod_uid}" ]; then
        return 1
    fi

    local state_file="${CRI_MULTIPLEX_STATE:-/var/lib/cri-multiplex/state/state.json}"
    local from_state
    if [ -f "${state_file}" ]; then
        from_state=$(python3 - "${state_file}" "${pod_uid}" <<'EOF'
import json, sys
state_path, uid = sys.argv[1], sys.argv[2]
try:
    with open(state_path) as f:
        d = json.load(f)
    for p in (d.get("e2b") or {}).get("pods") or []:
        if uid in (p.get("pod_uid"), p.get("sandbox_id")):
            name = (p.get("cni_record") or {}).get("NetNSName") or ""
            if name:
                print(name)
                sys.exit(0)
except Exception:
    pass
sys.exit(1)
EOF
) || true
        if [ -n "${from_state}" ]; then
            echo "${from_state}"
            return 0
        fi
    fi

    if [ "${#pod_uid}" -le 12 ]; then
        echo "e2b-${pod_uid}"
    else
        echo "e2b-${pod_uid:0:6}$(printf '%s' "${pod_uid}" | sha256sum | cut -c1-6)"
    fi
}

# e2b_deny_set_contains <pod> <cidr> — 检查沙箱 netns 的预定义 deny 集合
e2b_deny_set_contains() {
    local pod="$1"
    local cidr="$2"
    local netns_name
    netns_name=$(e2b_netns_name "${pod}") || return 1
    ip netns exec "${netns_name}" nft list set inet slot-firewall filtered_always_denylist 2>/dev/null | grep -q "${cidr}"
}

patch_e2b_firewall_allowlist() {
    local pod_name="$1"
    shift

    local netns_name
    if ! netns_name=$(e2b_netns_name "${pod_name}"); then
        log_fail "无法读取 E2B Pod UID，不能临时放通 egress firewall"
        return 1
    fi
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
log_pass "旧资源已清理"

log_step "2.1 创建 E2B CNI Pod A"
prepare_e2b_cni_pod_yaml "${POD_NAME}" "${APP_LABEL}" || exit 1
apply_and_wait_pod_ready "${POD_NAME}" "${E2B_CNI_YAML}" "120s" || exit 1
POD_IP=$(get_pod_ip_or_fail "${POD_NAME}") || exit 1
log_pass "E2B Pod A PodIP: ${POD_IP}"

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

log_step "2.3 创建 E2B CNI Pod B（沙箱互访对端）"
E2B_CNI_YAML=/tmp/e2b-np-egress-peer.yaml POD_YAML=/tmp/e2b-np-egress-peer.yaml prepare_e2b_cni_pod_yaml "${POD_NAME_B}" "${APP_LABEL}-peer" || exit 1
apply_and_wait_pod_ready "${POD_NAME_B}" "/tmp/e2b-np-egress-peer.yaml" "120s" || exit 1
POD_B_IP=$(get_pod_ip_or_fail "${POD_NAME_B}") || exit 1
log_pass "E2B Pod B PodIP: ${POD_B_IP}"

log_step "3.1 baseline：E2B VM A 访问普通 Service ClusterIP"
BASE_CODE=$(e2b_http_code "${POD_NAME}" "http://${TARGET_CLUSTER_IP}:${TARGET_PORT}/")
if [ "${BASE_CODE}" = "200" ]; then
    log_pass "baseline 成功：E2B VM -> target Service HTTP ${BASE_CODE}"
elif [ "${BASE_CODE}" = "NO_HTTP_CLIENT" ]; then
    log_skip "E2B VM 内无 curl/wget，无法验证出向行为"
    print_summary
    exit 0
else
    log_fail "baseline 失败：E2B VM -> target Service HTTP ${BASE_CODE}（ClusterIP 经宿主机 DNAT，不受沙箱隔离影响，应恒通）"
    cat /tmp/e2b-egress.err >&2 || true
    exit 1
fi

log_step "3.2 检测沙箱隔离是否启用（predefinedDenySet 含 ${POD_CIDR}）"
ISOLATION=0
if e2b_deny_set_contains "${POD_NAME}" "${POD_CIDR}"; then
    ISOLATION=1
    log_pass "沙箱 A netns 的 filtered_always_denylist 包含 Pod CIDR ${POD_CIDR}"
else
    log_skip "沙箱 A netns 的 predefinedDenySet 不含 ${POD_CIDR}，orchestrator 未注入 SANDBOX_DENIED_POD_CIDR，3.3/3.4 记录为 SKIP"
fi

log_step "3.3 沙箱间隔离：E2B VM A -> E2B Pod B PodIP（TCP）"
if [ "${ISOLATION}" = "1" ]; then
    PEER_CODE=$(e2b_http_code "${POD_NAME}" "http://${POD_B_IP}:${ENVD_PORT}/health")
    if [ "${PEER_CODE}" = "000" ]; then
        log_pass "沙箱 A -> 沙箱 B 已被阻断（HTTP ${PEER_CODE}），沙箱间隔离生效"
    else
        log_fail "沙箱 A -> 沙箱 B 未被阻断（HTTP ${PEER_CODE}），隔离未生效"
        exit 1
    fi
else
    log_skip "沙箱隔离未启用，跳过沙箱互访断言"
fi

log_step "3.4 Pod CIDR 默认阻断 + allowlist 例外放通"
if [ "${ISOLATION}" = "1" ]; then
    DIRECT_CODE=$(e2b_http_code "${POD_NAME}" "http://${TARGET_POD_IP}:${TARGET_PORT}/")
    if [ "${DIRECT_CODE}" = "000" ]; then
        log_pass "E2B VM -> target PodIP（Pod CIDR 内普通 Pod）默认被阻断"
    else
        log_fail "E2B VM -> target PodIP 未被阻断（HTTP ${DIRECT_CODE}），Pod CIDR deny 未生效"
        exit 1
    fi

    patch_e2b_firewall_allowlist "${POD_NAME}" "${TARGET_POD_IP}" || exit 1
    ALLOWED_CODE=$(e2b_http_code "${POD_NAME}" "http://${TARGET_POD_IP}:${TARGET_PORT}/")
    if [ "${ALLOWED_CODE}" = "200" ]; then
        log_pass "allowlist 例外生效：放通后 E2B VM -> target PodIP HTTP ${ALLOWED_CODE}"
    else
        log_fail "allowlist 例外未生效：放通后 E2B VM -> target PodIP HTTP ${ALLOWED_CODE}"
        exit 1
    fi
else
    log_skip "沙箱隔离未启用，跳过默认阻断断言，仅验证 baseline 直连"
    patch_e2b_firewall_allowlist "${POD_NAME}" "${TARGET_CLUSTER_IP}" "${TARGET_POD_IP}" || exit 1
    DIRECT_CODE=$(e2b_http_code "${POD_NAME}" "http://${TARGET_POD_IP}:${TARGET_PORT}/")
    if [ "${DIRECT_CODE}" = "200" ]; then
        log_pass "baseline 成功：E2B VM -> target PodIP HTTP ${DIRECT_CODE}"
    else
        log_skip "E2B VM 无法访问 target PodIP，HTTP ${DIRECT_CODE}，当前 CNI POC 暂不具备 egress baseline"
    fi
fi

log_step "3.5 allow-internet=false 用户级 deny 覆盖 TCP"
E2B_CNI_YAML=/tmp/e2b-np-egress-noinet.yaml POD_YAML=/tmp/e2b-np-egress-noinet.yaml prepare_e2b_cni_pod_yaml "${POD_NAME_C}" "${APP_LABEL}-noinet" || exit 1
sed -i 's|e2b.dev/allow-internet: "true"|e2b.dev/allow-internet: "false"|' /tmp/e2b-np-egress-noinet.yaml
apply_and_wait_pod_ready "${POD_NAME_C}" "/tmp/e2b-np-egress-noinet.yaml" "120s" || exit 1
NOINET_CODE=$(e2b_http_code "${POD_NAME_C}" "http://${TARGET_CLUSTER_IP}:${TARGET_PORT}/")
if [ "${NOINET_CODE}" = "000" ]; then
    log_pass "allow-internet=false 沙箱 TCP 出向已被阻断（HTTP ${NOINET_CODE}）"
elif [ "${NOINET_CODE}" = "NO_HTTP_CLIENT" ]; then
    log_skip "E2B VM 内无 curl/wget，跳过 allow-internet=false 断言"
else
    log_fail "allow-internet=false 沙箱 TCP 出向未被阻断（HTTP ${NOINET_CODE}），用户级 deny 未覆盖 TCP"
    exit 1
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
log_pass "deny-all egress policy 已应用"

DENY_CODE=""
DENY_BLOCKED=0
for _ in $(seq 1 30); do
    DENY_CODE=$(e2b_http_code "${POD_NAME}" "http://${TARGET_CLUSTER_IP}:${TARGET_PORT}/")
    if [ "${DENY_CODE}" = "000" ]; then
        DENY_BLOCKED=1
        break
    fi
    sleep 1
done
if [ "${DENY_BLOCKED}" = "1" ]; then
    log_pass "deny-all egress 已阻断 E2B VM -> target"
else
    log_skip "deny-all egress 未阻断，HTTP ${DENY_CODE}，bridge 数据面不支持 NetworkPolicy，记为 UNSUPPORTED"
fi

log_step "5.1 删除资源"
cleanup
log_pass "资源删除请求已提交"

print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证完成：E2B CNI 沙箱出向隔离 / NetworkPolicy egress 行为已检查"
    exit 0
else
    exit 1
fi
