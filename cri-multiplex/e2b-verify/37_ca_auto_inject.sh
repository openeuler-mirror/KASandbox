#!/bin/bash
###############################################################################
# 37_ca_auto_inject.sh — per-sandbox 代理 CA 自动生成 + guest 自动注入端到端验证
# （SANDBOX_PROXY_CA_AUTO=true，orchestrator 侧；设计文档
#  《E2B原生网络沙箱按 netns 起 per-sandbox 代理进程详细设计与实现步骤.md》§7.4）
#
# ⚠️ 与 35 号脚本同款 DS 自管理模式：本脚本通过 kubectl set env 修改
#    template-manager DaemonSet 的 SANDBOX_PROXY_* / SANDBOX_PROXY_CA_AUTO env
#    并等待滚动重启，结束（含失败兜底）时恢复基线 env 并再次等待滚动完成。
#    滚动重启会销毁该节点全部现存 E2B 沙箱，仅供专用测试节点运行。
#    不注册进 run_all.sh，需单独执行：
#        bash 37_ca_auto_inject.sh
#
# 断言清单（§7.4 逐条对应，机读 PASS/FAIL）：
#   §5  断言1  删除宿主 confdir CA + SANDBOX_PROXY_CA_AUTO=true 滚动重启后，
#              orchestrator 启动路径自动生成 CA（mitmproxy-ca.pem /
#              mitmproxy-ca-cert.pem 存在、openssl 可解析、CN=e2b-egress-auto-ca、
#              cert 0644 / 合并 0600）；二次滚动幂等（产物不变）
#   §6  断言2  默认沙箱（egress-mitm 缺省=true）：guest 内
#              /etc/ssl/certs/e2b-egress-ca.pem 存在且 subject=自动 CA；
#              /usr/local/share/ca-certificates/e2b-egress-ca.crt 的 SHA256
#              指纹与宿主 confdir cert 一致；curl 不带 --cacert 访问 mock
#              HTTPS 站点成功（200），对端证书 issuer=自动 CA 且 subject
#              CN=目标域名（mitmproxy 伪造落叶证书 → MITM 生效）
#   §7  断言3  egress-mitm=false 沙箱：guest 内无注入（两个路径均不存在），
#              HTTPS 透传正常（curl -k 200，对端证书 subject CN=mock 站点
#              原始证书 CN，即端到端未被 MITM）
#
# mock 组件（本脚本自建自清理，35 号同款最简形态）：
#   独立 netns e2b37-mockext 内的外网 HTTPS mock 站点（198.51.100.2:39272），
#   站点证书由自动 CA 签发（ mitmproxy 上游验证经
#   SANDBOX_PROXY_EXTRA_ARGS=--set ssl_verify_upstream_trusted_ca 信任自动 CA）。
#   mock 链路使用 198.51.100.0/24（TEST-NET-2，避开 RFC1918 防火墙预置 deny）。
###############################################################################
# 与既有 e2b-verify 用例同风格使用 "[ 条件 ] && log_pass ... || log_fail ..."
# 断言惯用法；log_pass/log_fail 恒返回 0，该用法是安全的，故对 SC2015 整文件豁免。
# shellcheck disable=SC2015
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "37 — per-sandbox 代理 CA 自动生成 + guest 自动注入（SANDBOX_PROXY_CA_AUTO）"

#==================== 配置 ====================#
ORCH_NS="${ORCH_NS:-e2b}"
ORCH_DS="${ORCH_DS:-template-manager}"
ORCH_DS_SELECTOR="${ORCH_DS_SELECTOR:-app=template-manager}"

CA_DIR_HOST="${E2B37_CA_DIR_HOST:-/var/lib/cri-multiplex/egress-ca}"   # 宿主侧 confdir（hostPath 挂载，pod 内同路径）
CA_DIR_POD="${E2B37_CA_DIR_POD:-${CA_DIR_HOST}}"                       # SANDBOX_PROXY_CONFDIR（容器内路径）
MITM_JSON_NAME="mitmproxy37.json"                                      # MITMPROXY_CONFIG（confdir 内，hostPath 共享）
LOG_DIR_POD="${E2B37_LOG_DIR_POD:-/var/log/e2b35-proxy}"               # SANDBOX_PROXY_LOG_DIR（DS 已 hostPath 挂载，宿主可读代理日志）

MOCK_NS="e2b37-mockext"
LINK_H="veth37h"
LINK_P="veth37p"
MOCK_HOST_IP="198.51.100.1"             # 宿主侧链路地址
SITE_IP="198.51.100.2"                  # mock 站点地址
EXT_HTTPS_PORT=39272                    # 外网 mock HTTPS
DOM_EXT="ext37.test.local"              # 外网（header_whitelist → MITM）

WORK="/tmp/e2b37"
SITES_LOG="${WORK}/sites.log"

TS="$(date +%s)$RANDOM"
SBX_A="e2b37a${TS}"    # 身份注解 + mitm 缺省（true）→ 注入 CA
SBX_B="e2b37b${TS}"    # 身份注解 + egress-mitm=false → 不注入
MIS_A="mis-a-${TS}"
MIS_B="mis-b-${TS}"

MANAGED_KEYS=(
    SANDBOX_EGRESS_PROXY_MODE SANDBOX_PROXY_APPLY SANDBOX_PROXY_CA_AUTO
    SANDBOX_PROXY_CONFDIR SANDBOX_PROXY_LOG_DIR SANDBOX_PROXY_EXTRA_ARGS
    MITMPROXY_CONFIG
)

#==================== 状态 ====================#
declare -A BASE_ENV=()
POD_A_ID="" POD_B_ID=""
CID_A="" CID_B=""
ORCH_TOUCHED=0
MUX_RESTARTED=0
SITES_PID=""
CA_BACKED_UP=0
BASE_CNI_ENABLED="${BASE_CNI_ENABLED:-1}"
BASE_CNI_POOL_ENABLED="${BASE_CNI_POOL_ENABLED:-1}"
BASE_CNI_POOL_SIZE="${BASE_CNI_POOL_SIZE:-200}"
BASE_HIDE_LABEL="${BASE_HIDE_LABEL:-flux-sandbox.io/direct=true}"

#==================== 工具函数 ====================#
# guest 内执行（stderr 一并返回；失败不中断脚本）。
# cri-multiplex 的 exec 转发会把自身的 HTTP/2 frame 调试行（"Create stream" 等）
# 混进输出，统一过滤，避免污染断言取值。
guest_exec() { # <container_id> <cmd>
    ${CRICTL} exec "$1" sh -c "$2" 2>&1 \
        | grep -vE "^[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9:]+ \(0x[0-9a-f]+\)" || true
}

# 沙箱 Pod JSON 注解注入（在 prepare_direct_pod_json 产物上就地修改）
inject_annotations() { # <json-file> <extra-annotations-json>
    local tmp="$1.tmp"
    jq --argjson extra "$2" \
        '.annotations = ((.annotations // {}) + $extra)' "$1" > "${tmp}" \
        && mv "${tmp}" "$1"
}

#==================== orchestrator DaemonSet 管理 ====================#
ds_env_get() { # <env-name>
    kubectl -n "${ORCH_NS}" get ds "${ORCH_DS}" \
        -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name=='$1')].value}" 2>/dev/null
}

tm_pod() {
    kubectl -n "${ORCH_NS}" get pod -l "${ORCH_DS_SELECTOR}" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

capture_ds_baseline() {
    kubectl -n "${ORCH_NS}" get ds "${ORCH_DS}" >/dev/null 2>&1 \
        || { log_fail "DaemonSet ${ORCH_NS}/${ORCH_DS} 不存在"; return 1; }
    # 等待 pod Ready 再做 exec 探测：上一轮脚本清理可能刚触发 DS 滚动，
    # 直接 exec 会命中 terminating/not-ready pod 秒败（实测两轮误报）
    kubectl -n "${ORCH_NS}" wait --for=condition=ready pod -l "${ORCH_DS_SELECTOR}" \
        --timeout=120s >/dev/null 2>&1 || true
    local k
    for k in "${MANAGED_KEYS[@]}"; do
        BASE_ENV[$k]=$(ds_env_get "${k}")
    done
    log_pass "已捕获 DaemonSet 基线 env（${#MANAGED_KEYS[@]} 个受管键）"

    # 特性 marker：镜像二进制内嵌 SANDBOX_PROXY_CA_AUTO 字符串（缺失说明镜像过旧）
    local pod mark
    pod=$(tm_pod)
    [ -n "${pod}" ] || { log_fail "未找到 ${ORCH_DS_SELECTOR} pod"; return 1; }
    mark=$(kubectl -n "${ORCH_NS}" exec "${pod}" -- sh -c \
        'grep -ac "SANDBOX_PROXY_CA_AUTO" /usr/bin/orchestrator 2>/dev/null || echo 0' \
        2>/dev/null | tr -d '[:space:]' || true)
    if [ "${mark:-0}" -ge 1 ]; then
        log_pass "orchestrator 镜像含 CA 自动注入特性（pod=${pod}）"
    else
        log_fail "orchestrator 镜像不含 SANDBOX_PROXY_CA_AUTO 特性，请先更新 ${ORCH_DS} DaemonSet 镜像"
        return 1
    fi
}

orch_wait_ready() {
    log_info "等待 ${ORCH_DS} 滚动重启完成 ..."
    if ! kubectl -n "${ORCH_NS}" rollout status "ds/${ORCH_DS}" --timeout=300s >&2; then
        log_fail "DaemonSet ${ORCH_DS} 滚动超时（300s）"
        return 1
    fi
    wait_tcp_connect 127.0.0.1 5008 180 "orchestrator gRPC :5008"
}

orch_apply_env() {
    log_info "设置 ${ORCH_DS} env: $*"
    kubectl -n "${ORCH_NS}" set env "ds/${ORCH_DS}" "$@" >&2
    ORCH_TOUCHED=1
    orch_wait_ready
}

orch_restore_baseline() {
    local args=() k
    for k in "${MANAGED_KEYS[@]}"; do
        if [ -n "${BASE_ENV[$k]}" ]; then args+=("${k}=${BASE_ENV[$k]}"); else args+=("${k}-"); fi
    done
    log_info "恢复 ${ORCH_DS} 基线 env: ${args[*]}"
    kubectl -n "${ORCH_NS}" set env "ds/${ORCH_DS}" "${args[@]}" >&2
    orch_wait_ready
}

restore_mux_baseline() {
    log_info "恢复 cri-multiplex 基线（CNI 模式，约 2-3 分钟）"
    CNI_ENABLED="${BASE_CNI_ENABLED}" CNI_POOL_ENABLED="${BASE_CNI_POOL_ENABLED}" \
        CNI_POOL_SIZE="${BASE_CNI_POOL_SIZE}" HIDE_SANDBOX_LABEL="${BASE_HIDE_LABEL}" \
        E2B_FORCE_RESTART=1 bash "${SCRIPT_DIR}/01_start_multiplex.sh" > /tmp/37-restore-mux.log 2>&1 || true
}

#==================== 清理 ====================#
cleanup_37() {
    log_info "清理: 删除用例沙箱 / mock 组件 / mock netns，恢复 confdir 与 DS 基线（幂等）"
    for pid in "${POD_A_ID}" "${POD_B_ID}"; do
        [ -n "${pid}" ] && ${CRICTL} rmp -f "${pid}" >/dev/null 2>&1 || true
    done
    [ -n "${SITES_PID}" ] && kill "${SITES_PID}" 2>/dev/null || true
    pkill -f "${WORK}/mock_sites.py" 2>/dev/null || true
    ip netns del "${MOCK_NS}" >/dev/null 2>&1 || true
    ip link del "${LINK_H}" >/dev/null 2>&1 || true
    rm -f /tmp/e2b-pod-e2b37*.json
    rm -f "${CA_DIR_HOST}/${MITM_JSON_NAME}" 2>/dev/null || true
    # 恢复 confdir 基线：有备份则还原，无备份（原本为空）则删除自动生成的 CA，
    # 避免基线 env 恢复后（CA_AUTO 关闭）残留自动生成产物混淆后续手工验证
    if [ "${CA_BACKED_UP}" = "1" ]; then
        cp -f "${WORK}/ca-backup/"* "${CA_DIR_HOST}/" 2>/dev/null || true
    else
        rm -f "${CA_DIR_HOST}/mitmproxy-ca.pem" "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" 2>/dev/null || true
    fi
    if [ "${ORCH_TOUCHED}" = "1" ]; then
        orch_restore_baseline && log_info "orchestrator 基线 env 已恢复" \
            || log_info "orchestrator 基线恢复失败，请手工检查 kubectl -n ${ORCH_NS} get ds ${ORCH_DS}"
    fi
    if [ "${MUX_RESTARTED}" = "1" ]; then
        restore_mux_baseline
    fi
}
trap cleanup_37 EXIT

#==================== 0. 前置检查 ====================#
log_step "0.1 前置检查"
for cmd in jq ip python3 kubectl openssl curl base64 sha256sum; do
    command -v "${cmd}" >/dev/null 2>&1 || { log_fail "${cmd} 不存在"; exit 1; }
done
[ -f /tmp/cri-proto/api.proto ] || bash "${SCRIPT_DIR}/00_setup.sh" >/dev/null 2>&1 || true
[ -f /tmp/e2b-pod.json ] || bash "${SCRIPT_DIR}/00_setup.sh" >/dev/null 2>&1 || true
[ -f /tmp/e2b-pod.json ] || { log_fail "基础 Pod JSON 准备失败"; exit 1; }
BASE_POD_JSON="${POD_JSON}"
if [ ! -f /tmp/e2b-kubelet-pod.yaml ]; then
    E2B_SKIP_BUILD=0 E2B_YAML_COUNT=0 refresh_or_reuse_e2b_yaml \
        "${SCRIPT_DIR}/lib/refresh_build_id.sh" "e2b-kubelet-test" \
        "/tmp/e2b-kubelet-pod.yaml" || { log_fail "E2B fixture 准备失败"; exit 1; }
fi
export E2B_SKIP_BUILD=1 E2B_BASE_POD_YAML=/tmp/e2b-kubelet-pod.yaml
sync_e2b_pod_json_from_kubelet_yaml /tmp/e2b-kubelet-pod.yaml "${BASE_POD_JSON}" \
    && log_pass "e2b-pod.json 已同步最新模板凭证" \
    || { log_fail "e2b-pod.json 同步最新模板凭证失败"; exit 1; }

capture_ds_baseline || exit 1

#==================== 1. cri-multiplex 切到原生（非 CNI）模式 ====================#
log_step "1.1 切换 cri-multiplex 到非 CNI（原生）模式"
start_non_cni_multiplex "启动 cri-multiplex 非 CNI runtime 模式" || exit 1
MUX_RESTARTED=1

#==================== 2. 备份并删除宿主 confdir CA，开启 CA_AUTO ====================#
log_step "2.1 备份并清空宿主 confdir CA（${CA_DIR_HOST}）"
mkdir -p "${CA_DIR_HOST}" "${WORK}/ca-backup"
if [ -s "${CA_DIR_HOST}/mitmproxy-ca.pem" ] || [ -s "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" ]; then
    cp -f "${CA_DIR_HOST}/mitmproxy-ca.pem" "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" \
        "${CA_DIR_HOST}/mitmproxy-ca.key" "${WORK}/ca-backup/" 2>/dev/null || true
    CA_BACKED_UP=1
    log_info "已备份既有 CA 到 ${WORK}/ca-backup"
fi
rm -f "${CA_DIR_HOST}/mitmproxy-ca.pem" "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" "${CA_DIR_HOST}/mitmproxy-ca.key"
[ ! -e "${CA_DIR_HOST}/mitmproxy-ca.pem" ] && [ ! -e "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" ] \
    && log_pass "宿主 confdir CA 已清空" || { log_fail "宿主 confdir CA 清空失败"; exit 1; }

log_step "2.2 写入 MITMPROXY_CONFIG（header_whitelist=${DOM_EXT}）"
cat > "${CA_DIR_HOST}/${MITM_JSON_NAME}" <<EOF
{
  "internal":         {"hosts": [], "domains": [], "nets": []},
  "header_whitelist": {"hosts": [], "domains": ["${DOM_EXT}"]},
  "header_blacklist": {"hosts": [], "domains": []}
}
EOF
log_pass "MITMPROXY_CONFIG 就绪（${CA_DIR_HOST}/${MITM_JSON_NAME}，hostPath 与 pod 共享）"

log_step "2.3 开启 SANDBOX_PROXY_CA_AUTO=true 并等待滚动（APPLY=ready，验证注入强制 immediate 语义）"
orch_apply_env \
    "SANDBOX_EGRESS_PROXY_MODE=per-sandbox" \
    "SANDBOX_PROXY_CA_AUTO=true" \
    "SANDBOX_PROXY_APPLY=ready" \
    "SANDBOX_PROXY_CONFDIR=${CA_DIR_POD}" \
    "SANDBOX_PROXY_LOG_DIR=${LOG_DIR_POD}" \
    "SANDBOX_PROXY_EXTRA_ARGS=--set ssl_verify_upstream_trusted_ca=${CA_DIR_POD}/mitmproxy-ca-cert.pem" \
    "MITMPROXY_CONFIG=${CA_DIR_POD}/${MITM_JSON_NAME}" || exit 1

#==================== 5. 断言1：CA 自动生成 ====================#
log_step "5.1 [断言1] 滚动后宿主 confdir CA 已由 orchestrator 自动生成"
AUTO_CA_OK=1
if [ -s "${CA_DIR_HOST}/mitmproxy-ca.pem" ] && [ -s "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" ]; then
    log_pass "mitmproxy-ca.pem 与 mitmproxy-ca-cert.pem 均已生成"
else
    log_fail "confdir CA 未自动生成（${CA_DIR_HOST}）"
    AUTO_CA_OK=0
fi
if [ "${AUTO_CA_OK}" = "1" ]; then
    if openssl x509 -in "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" -noout -subject 2>/dev/null \
        | grep -q "e2b-egress-auto-ca"; then
        log_pass "自动 CA openssl 可解析且 CN=e2b-egress-auto-ca"
    else
        log_fail "自动 CA 解析失败或 CN 不符: $(openssl x509 -in "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" -noout -subject 2>&1)"
    fi
    grep -q "PRIVATE KEY" "${CA_DIR_HOST}/mitmproxy-ca.pem" \
        && log_pass "合并 PEM 内含私钥（mitmproxy confdir 约定格式）" \
        || log_fail "合并 PEM 不含私钥"
    P_CERT=$(stat -c '%a' "${CA_DIR_HOST}/mitmproxy-ca-cert.pem")
    P_COMB=$(stat -c '%a' "${CA_DIR_HOST}/mitmproxy-ca.pem")
    [ "${P_CERT}" = "644" ] && [ "${P_COMB}" = "600" ] \
        && log_pass "文件权限正确（cert=${P_CERT} combined=${P_COMB}）" \
        || log_fail "文件权限不符（cert=${P_CERT} 期望 644，combined=${P_COMB} 期望 600）"
fi

log_step "5.2 [断言1] 幂等：再次滚动重启后 CA 产物不变"
FP_BEFORE=$(sha256sum "${CA_DIR_HOST}/mitmproxy-ca.pem" "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" 2>/dev/null || true)
orch_apply_env "SANDBOX_PROXY_CA_AUTO=true" || exit 1   # 同值 set env 触发新一轮滚动
FP_AFTER=$(sha256sum "${CA_DIR_HOST}/mitmproxy-ca.pem" "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" 2>/dev/null || true)
[ -n "${FP_BEFORE}" ] && [ "${FP_BEFORE}" = "${FP_AFTER}" ] \
    && log_pass "二次滚动后 CA 逐字节不变（幂等跳过）" \
    || log_fail "二次滚动后 CA 发生变化（幂等被破坏）"

#==================== 3. mock 站点（证书由自动 CA 签发） ====================#
log_step "3.1 创建 mock 外部 netns（${MOCK_NS}：${MOCK_HOST_IP} <-> ${SITE_IP}）"
ip netns del "${MOCK_NS}" >/dev/null 2>&1 || true
ip link del "${LINK_H}" >/dev/null 2>&1 || true
ip netns add "${MOCK_NS}" || { log_fail "创建 netns ${MOCK_NS} 失败"; exit 1; }
ip link add "${LINK_H}" type veth peer name "${LINK_P}"
ip addr add "${MOCK_HOST_IP}/24" dev "${LINK_H}"
ip link set "${LINK_H}" up
ip link set "${LINK_P}" netns "${MOCK_NS}"
ip netns exec "${MOCK_NS}" ip addr add "${SITE_IP}/24" dev "${LINK_P}"
ip netns exec "${MOCK_NS}" ip link set "${LINK_P}" up
ip netns exec "${MOCK_NS}" ip link set lo up
ip netns exec "${MOCK_NS}" ip route add default via "${MOCK_HOST_IP}"
sysctl -w "net.ipv4.conf.${LINK_H}.rp_filter=0" >/dev/null
ip netns exec "${MOCK_NS}" sysctl -w net.ipv4.conf.all.rp_filter=0 >/dev/null
log_pass "mock netns 就绪（站点 ${SITE_IP}:${EXT_HTTPS_PORT}）"

log_step "3.2 用自动 CA 签发 mock 站点证书（带 SAN，幂等）"
mkdir -p "${WORK}"
cat > "${WORK}/mock-site.ext" <<EOF
subjectAltName=DNS:${DOM_EXT},IP:${SITE_IP}
EOF
openssl req -newkey rsa:2048 -nodes \
    -keyout "${WORK}/mock-site.key" -out "${WORK}/mock-site.csr" \
    -subj "/CN=e2b37-mock-site" >/dev/null 2>&1 \
    && openssl x509 -req -in "${WORK}/mock-site.csr" -days 30 \
        -CA "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" -CAkey "${CA_DIR_HOST}/mitmproxy-ca.pem" \
        -CAcreateserial -extfile "${WORK}/mock-site.ext" \
        -out "${WORK}/mock-site.crt" >/dev/null 2>&1 \
    || { log_fail "mock 站点证书签发失败"; exit 1; }
log_pass "mock 站点证书已由自动 CA 签发（CN=e2b37-mock-site，SAN=${DOM_EXT}）"

log_step "3.3 启动 mock HTTPS 站点"
: > "${SITES_LOG}"
cat > "${WORK}/mock_sites.py" <<'PYEOF'
import ssl, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = sys.argv[1]; CERT = sys.argv[2]; KEY = sys.argv[3]
IP = sys.argv[4]; PORT = int(sys.argv[5])

class H(BaseHTTPRequestHandler):
    def _ok(self):
        with open(LOG, "a") as f:
            f.write(f"REQ src={self.client_address[0]} host={self.headers.get('Host','-')} path={self.path}\n")
        body = b"ok\n"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    do_GET = _ok
    do_POST = _ok
    def log_message(self, *a):
        pass

srv = ThreadingHTTPServer((IP, PORT), H)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(CERT, KEY)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
with open(LOG, "a") as f:
    f.write(f"LISTEN {IP}:{PORT}\n")
srv.serve_forever()
PYEOF
command -v fuser >/dev/null 2>&1 && fuser -k "${EXT_HTTPS_PORT}/tcp" >/dev/null 2>&1 || true
sleep 1
ip netns exec "${MOCK_NS}" python3 "${WORK}/mock_sites.py" \
    "${SITES_LOG}" "${WORK}/mock-site.crt" "${WORK}/mock-site.key" \
    "${SITE_IP}" "${EXT_HTTPS_PORT}" &
SITES_PID=$!
CODE=""
for i in $(seq 1 30); do
    kill -0 "${SITES_PID}" 2>/dev/null || { log_fail "mock 站点启动失败"; exit 1; }
    CODE=$(curl --noproxy '*' -sk -o /dev/null -w "%{http_code}" --max-time 3 \
        "https://${SITE_IP}:${EXT_HTTPS_PORT}/" 2>/dev/null || true)
    [ "${CODE}" = "200" ] && break
    sleep 2
done
[ "${CODE}" = "200" ] && log_pass "mock HTTPS 站点自检通过（https://${SITE_IP}:${EXT_HTTPS_PORT} → 200）" \
    || { log_fail "mock HTTPS 站点自检失败（60s 内未就绪）"; exit 1; }

#==================== 6. 断言2：默认沙箱（mitm 缺省 true）→ 注入 + MITM 生效 ====================#
log_step "6.1 [断言2] 创建沙箱 A（身份注解 + egress-mitm 缺省=true）"
prepare_direct_pod_json "e2b37a" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "${SBX_A}" --arg mis "${MIS_A}" '{
    "e2b.dev/sandbox-id": $sid,
    "cri-multiplex.dev/sandbox-mis": $mis,
    "cri-multiplex.dev/egress-profile": "internal"}')" || exit 1
POD_A_ID=$(run_pod_sandbox) || { log_fail "沙箱 A RunPodSandbox 失败（CA 注入强制 immediate，失败即创建失败）"; print_summary; exit 1; }
log_pass "沙箱 A 创建成功: ${POD_A_ID}（CA 注入已同步完成，创建成功即注入成功）"
CID_A=$(create_and_start_container "${POD_A_ID}") || { log_fail "沙箱 A 容器启动失败"; print_summary; exit 1; }
log_pass "沙箱 A 容器运行中: ${CID_A}"

guest_exec "${CID_A}" "command -v curl" | grep -q curl || { log_fail "guest 无 curl（模板未按预期装好工具链）"; print_summary; exit 1; }
guest_exec "${CID_A}" "command -v openssl" | grep -q openssl || { log_fail "guest 无 openssl"; print_summary; exit 1; }

log_step "6.2 [断言2] guest 信任库已注入自动 CA"
OUT=$(guest_exec "${CID_A}" "openssl x509 -in /etc/ssl/certs/e2b-egress-ca.pem -noout -subject 2>/dev/null || true")
if grep -q "e2b-egress-auto-ca" <<< "${OUT}"; then
    log_pass "guest /etc/ssl/certs/e2b-egress-ca.pem 可解析且 subject=自动 CA"
else
    log_fail "guest 信任库缺少注入的自动 CA: ${OUT}"
fi

log_step "6.3 [断言2] guest 内 cert 指纹与宿主 confdir cert 一致"
HOST_FP=$(sha256sum "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" | awk '{print $1}')
GUEST_FP=$(guest_exec "${CID_A}" "sha256sum /usr/local/share/ca-certificates/e2b-egress-ca.crt 2>/dev/null | awk '{print \$1}'" | grep -oE '^[0-9a-f]{64}' | head -1)
if [ -n "${HOST_FP}" ] && [ "${HOST_FP}" = "${GUEST_FP}" ]; then
    log_pass "指纹一致（${HOST_FP:0:16}…）"
else
    log_fail "指纹不一致：host=${HOST_FP:-<空>} guest=${GUEST_FP:-<空>}"
fi

log_step "6.4 [断言2] MITM 请求成功：curl 直连系统信任库（不带 --cacert/-k）→ 200"
CODE=$(guest_exec "${CID_A}" "curl -sS -o /dev/null -w '\nE2B37_CODE:%{http_code}\n' --connect-timeout 5 --max-time 15 --resolve ${DOM_EXT}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT}:${EXT_HTTPS_PORT}/" | grep -oP 'E2B37_CODE:\K[0-9]{3}' | tail -1)
if [ "${CODE}" = "200" ]; then
    log_pass "MITM 请求成功（https://${DOM_EXT}:${EXT_HTTPS_PORT} → 200，系统信任库直接信任伪造证书）"
else
    log_fail "MITM 请求失败（code=${CODE:-<空>}）"
    log_info "guest 侧 curl -v 详情："
    guest_exec "${CID_A}" "curl -v --connect-timeout 5 --max-time 15 --resolve ${DOM_EXT}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT}:${EXT_HTTPS_PORT}/ 2>&1 | tail -25" || true
    log_info "per-sandbox 代理日志（${LOG_DIR_POD}/${POD_A_ID}.log）尾部："
    tail -40 "${LOG_DIR_POD}/${POD_A_ID}.log" 2>/dev/null || log_info "（代理日志不存在）"
fi

log_step "6.5 [断言2] 对端证书 issuer=自动 CA 且 subject CN=目标域名（mitmproxy 伪造落叶证书）"
OUT=$(guest_exec "${CID_A}" "printf 'GET / HTTP/1.1\r\nHost: ${DOM_EXT}\r\nConnection: close\r\n\r\n' | openssl s_client -connect ${SITE_IP}:${EXT_HTTPS_PORT} -servername ${DOM_EXT} 2>/dev/null | openssl x509 -noout -issuer -subject 2>/dev/null || true")
if grep -q "issuer.*e2b-egress-auto-ca" <<< "${OUT}" && grep -q "subject.*CN *= *${DOM_EXT}" <<< "${OUT}"; then
    log_pass "MITM 证据链完整（issuer=e2b-egress-auto-ca，subject CN=${DOM_EXT}）"
else
    log_fail "对端证书不符预期（应 issuer=自动 CA、subject CN=${DOM_EXT}）: ${OUT}"
fi

#==================== 7. 断言3：egress-mitm=false → 不注入 + 透传 ====================#
log_step "7.1 [断言3] 创建沙箱 B（身份注解 + egress-mitm=false）"
prepare_direct_pod_json "e2b37b" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "${SBX_B}" --arg mis "${MIS_B}" '{
    "e2b.dev/sandbox-id": $sid,
    "cri-multiplex.dev/sandbox-mis": $mis,
    "cri-multiplex.dev/egress-profile": "internal",
    "cri-multiplex.dev/egress-mitm": "false"}')" || exit 1
POD_B_ID=$(run_pod_sandbox) || { log_fail "沙箱 B RunPodSandbox 失败"; print_summary; exit 1; }
CID_B=$(create_and_start_container "${POD_B_ID}") || { log_fail "沙箱 B 容器启动失败"; print_summary; exit 1; }
log_pass "沙箱 B 创建成功: ${POD_B_ID}（mitm=false）"

log_step "7.2 [断言3] guest 内无 CA 注入"
OUT=$(guest_exec "${CID_B}" "ls /usr/local/share/ca-certificates/e2b-egress-ca.crt /etc/ssl/certs/e2b-egress-ca.pem 2>&1 || true")
if grep -qi "No such file" <<< "${OUT}" && ! grep -q "e2b-egress-ca.pem$" <<< "${OUT}"; then
    log_pass "guest 内两个 CA 路径均不存在（mitm=false 短路跳过注入）"
else
    log_fail "mitm=false 沙箱竟被注入 CA: ${OUT}"
fi

log_step "7.3 [断言3] HTTPS 透传正常（端到端，未被 MITM）"
CODE=$(guest_exec "${CID_B}" "curl -sk -o /dev/null -w '\nE2B37_CODE:%{http_code}\n' --connect-timeout 5 --max-time 15 --resolve ${DOM_EXT}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT}:${EXT_HTTPS_PORT}/" | grep -oP 'E2B37_CODE:\K[0-9]{3}' | tail -1)
[ "${CODE}" = "200" ] && log_pass "透传请求成功（curl -k → 200）" || log_fail "透传请求失败（code=${CODE:-<空>}）"
OUT=$(guest_exec "${CID_B}" "printf 'GET / HTTP/1.1\r\nHost: ${DOM_EXT}\r\nConnection: close\r\n\r\n' | openssl s_client -connect ${SITE_IP}:${EXT_HTTPS_PORT} -servername ${DOM_EXT} 2>/dev/null | openssl x509 -noout -subject 2>/dev/null || true")
if grep -q "subject.*CN *= *e2b37-mock-site" <<< "${OUT}"; then
    log_pass "对端证书为 mock 站点原始证书（CN=e2b37-mock-site，端到端未被 MITM）"
else
    log_fail "对端证书 subject 非 mock 站点原始证书（疑被 MITM）: ${OUT}"
fi

print_summary
