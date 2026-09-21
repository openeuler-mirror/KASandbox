#!/bin/bash
###############################################################################
# 35_sandbox_per_netns_proxy.sh — E2B 原生网络模式 per-sandbox 代理进程端到端验证
# （SANDBOX_EGRESS_PROXY_MODE=per-sandbox，orchestrator 侧；设计文档
#  《E2B原生网络沙箱按 netns 起 per-sandbox 代理进程详细设计与实现步骤.md》§12.2）
#
# ⚠️ orchestrator 运行于 template-manager DaemonSet（hostNetwork）中，本脚本
#    通过 kubectl set env 修改该 DaemonSet 的 SANDBOX_PROXY_* 系列 env 并等待
#    滚动重启完成来切换代理配置，结束（含失败兜底）时恢复基线 env 并再次等待
#    滚动完成。滚动重启会销毁该节点全部现存 E2B 沙箱，仅供专用测试节点运行。
#    与旧 34/35 号用例一致，本脚本自管理环境、不注册进 run_all.sh，需单独执行：
#        bash 35_sandbox_per_netns_proxy.sh
#
# 断言清单（§12.2 逐条对应，机读 PASS/FAIL）：
#   §6   断言1  带身份注解（不显式 egress-mode）→ 推导 per-sandbox：netns REDIRECT
#               规则 + mitmdump 进程存在且 /proc/<pid>/ns/net 与 slot netns 一致
#   §7   断言2  无身份注解 → 推导 off（无规则/无进程/直出）；
#               显式 per-sandbox 缺 sandbox-mis → InvalidArgument（§6.2）；
#               egress-profile 取非 internal 值 → InvalidArgument（仅支持 internal）
#   §8   断言3  显式 egress-mode=off（带身份）→ 不导流、直出
#   §4   断言4  节点未开能力 + 显式 per-sandbox → 创建失败 FailedPrecondition
#               （单独负向用例组，先于正式沙箱创建，避免滚动销毁在测沙箱）
#   §9   断言5  内网 mock HTTP 直连成功且代理日志出现 flow；外网 HTTPS（mitm=true）
#               证书 issuer=测试 CA 且 mock 站点收到 X-AI-* 注入 header
#   §10  断言6  guest 零配置：无 *_proxy env、无 profile.d 残留、普通 curl 仍被管控
#   §11  断言7  transparent 劫持：curl -v 直连真实目的 IP（无 CONNECT 到
#               169.254.0.22），代理日志出现 TLS flow 且 SNI 解析正确
#   §12  断言8  全程序覆盖：--noproxy / openssl s_client 同样被劫持；裸 IP 直连
#               host 退化为 IP、按 internal.nets IP 级规则处理（记录在案）
#   §13  断言9  mitm=false：TLS 端到端（issuer 非测试 CA）；domain 级 deny 命中时
#               代理返回 403（§7.2 降级语义）
#   §14  断言10 SANDBOX_PROXY_EXEMPT_CIDRS 内目的直连、代理日志无该 flow
#   §19  断言11 kill 代理进程 → 该沙箱出向 fail-close → 监督协程拉起后恢复
#   §15  断言12 代理上游出节点：mock 站点看到的源 IP = slot HostIP（vrt SNAT
#               §4.2-3 生效），且宿主 HostCIDR MASQUERADE 规则存在（宿主侧第二段
#               链路证据；mock 在节点本地，无法直接观测最终节点 IP，见末节说明）
#   §17  断言13 expose-ports 组合：同沙箱 49983:P 入向正常，与出向代理并存
#   §17b 断言19 expose-ports 写法①（原生模式）：49983 池内自动分配，经
#               PodSandboxStatus 注解（e2b.dev/host-port-49983）读回 + 入向可达
#   §17b 断言20 expose-ports 写法③（原生模式）：区间分配 + 读回端口落在区间
#               + 入向可达
#   §17b 断言21 expose-ports 负向（原生模式）：pinned 冲突 ResourceExhausted /
#               区间耗尽（外部占用）ResourceExhausted / malformed InvalidArgument
#   §18  断言14 管理面回归：envd /health 经 HostIP 可达；guest→192.0.2.1 豁免
#               （代理日志无该 flow）
#   §21  断言15 删除沙箱 → 代理进程消失、netns 删除（或池化保留但无 REDIRECT
#               残留）、无孤儿 mitmdump
#   §20  断言16 ready 模式创建耗时回归：per-sandbox 创建耗时相对 off 沙箱增量
#               低于阈值（代理不进创建关键路径）
#   §16  断言17 上游级联（§8.3）：节点默认上游 mock1 收到外网 CONNECT；内网直连
#               无记录；egress-upstream=off 直连无记录；注解覆盖 → 流向 mock2
#   §22  断言18 SANDBOX_PROXY_APPLY=immediate：runp 返回即导流（零等待单次检查
#               REDIRECT 规则/代理进程/规则顺序）+ 出向立即可用 + 删除收割
#
# mock 组件（§12.2 前置，全部由本脚本自建自清理）：
#   ① mock 统一代理 ×2（python CONNECT/HTTP forward proxy，*.test.local 静态
#      解析到 mock 站点 IP，记录 CONNECT/HTTP 日志）
#   ② mock 策略平台（python HTTP 服务，返回 addon.py 快照格式的 domain 级 deny
#      策略 + 接收 intercept-report）
#   ③ 内/外网 mock 站点（独立 netns e2b35-mockext 内：外网 HTTPS :39172、内网
#      HTTPS :39181、内网 HTTP :39180、豁免目标 HTTP :39183@198.51.100.9，
#      记录每请求源 IP / Host / X-AI-* header）
#   测试期 CA 唯一真源为宿主侧 /var/lib/cri-multiplex/egress-ca/（幂等生成），
#   模板构建（build_prod.py）内置同一份进 guest 信任库，运行期再推入 orchestrator
#   pod 的 SANDBOX_PROXY_CONFDIR——三处恒一致；mock 链路使用 198.51.100.0/24
#   （TEST-NET-2，避开 RFC1918 防火墙预置 deny）。
###############################################################################
# 本脚本（与既有 e2b-verify 用例同风格）大量使用 "[ 条件 ] && log_pass ... || log_fail ..."
# 断言惯用法；log_pass/log_fail 恒返回 0，该用法是安全的，故对 SC2015 整文件豁免。
# shellcheck disable=SC2015
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "35 — 原生模式 per-sandbox 代理（SANDBOX_EGRESS_PROXY_MODE=per-sandbox）"

#==================== 配置 ====================#
ORCH_NS="${ORCH_NS:-e2b}"
ORCH_DS="${ORCH_DS:-template-manager}"
ORCH_DS_SELECTOR="${ORCH_DS_SELECTOR:-app=template-manager}"
PROXY_PORT="${E2B35_PROXY_PORT:-15001}"
HOST_NETWORK_BASE="${HOST_NETWORK_BASE:-10.11.0.0}"   # native slot HostIP = base + idx
HOST_NETWORK_CIDR="${HOST_NETWORK_CIDR:-10.11.0.0/16}"

UP1_PORT="${E2B35_UP1_PORT:-39095}"     # mock 统一代理 1（节点默认上游）
UP2_PORT="${E2B35_UP2_PORT:-39096}"     # mock 统一代理 2（注解覆盖上游）
STRATEGY_PORT="${E2B35_STRATEGY_PORT:-39097}"
EXT_HTTPS_PORT=39172                    # 外网 mock HTTPS
INT_HTTP_PORT=39180                     # 内网 mock HTTP
INT_HTTPS_PORT=39181                    # 内网 mock HTTPS（策略 deny 用）
EXEMPT_HTTP_PORT=39183                  # EXEMPT_CIDRS 目标 HTTP

MOCK_NS="e2b35-mockext"
LINK_H="veth35h"
LINK_P="veth35p"
MOCK_HOST_IP="198.51.100.1"             # 宿主侧链路地址（mock 视角的"节点地址"）
SITE_IP="198.51.100.2"                  # 内/外网 mock 站点地址
EXEMPT_IP="198.51.100.9"                # 豁免目标地址（SANDBOX_PROXY_EXEMPT_CIDRS）

DOM_EXT="ext.test.local"                # 外网（whitelist → MITM + X-AI-* 注入）
DOM_EXT6="ext6.test.local"              # 断言6 专用
DOM_EXT7="ext7.test.local"              # 断言7 专用
DOM_EXT8A="ext8a.test.local"            # 断言8 --noproxy 专用
DOM_EXT8B="ext8b.test.local"            # 断言8 openssl s_client 专用
DOM_INT="int.test.local"                # 内网（internal.domains）
DOM_BLOCK="int-block.test.local"        # 内网 + 策略 domain 级 deny（断言9）

WORK="/tmp/e2b35"
CA_STAGE="${WORK}/ca"                   # 宿主侧 CA staging（真源同步，推入 pod）
CA_DIR_HOST="${E2B35_CA_DIR_HOST:-/var/lib/cri-multiplex/egress-ca}"   # 宿主侧 CA 唯一真源
CA_DIR_POD="${E2B35_CA_DIR_POD:-/var/lib/cri-multiplex/egress-ca}"   # SANDBOX_PROXY_CONFDIR
LOG_DIR_POD="${E2B35_LOG_DIR_POD:-/var/log/e2b35-proxy}"             # SANDBOX_PROXY_LOG_DIR
MITM_JSON_POD="${CA_DIR_POD}/mitmproxy35.json"                       # MITMPROXY_CONFIG
PROXY_BINARY_POD="${E2B35_PROXY_BINARY:-/opt/mitmproxy/mitmdump}"
PROXY_ADDON_POD="${E2B35_PROXY_ADDON:-/opt/opensandbox-egress/addon.py}"
ENVD_PORT="${ENVD_PORT:-49983}"
CREATE_SKEW_S="${E2B35_CREATE_SKEW_S:-10}"   # 断言16 ready 模式允许增量（秒）
CREATE_SKEW_MS=$((CREATE_SKEW_S * 1000))     # 同上，毫秒口径（断言16 实际使用）

SITES_LOG="${WORK}/sites.log"
UP1_LOG="${WORK}/upstream1.log"
UP2_LOG="${WORK}/upstream2.log"
STRAT_LOG="${WORK}/strategy.log"

TS="$(date +%s)$RANDOM"
SBX_A="e2b35a${TS}"    # 身份注解 + mitm 默认（true）+ expose-ports（推导 per-sandbox）
SBX_B="e2b35b${TS}"    # 无身份注解（推导 off）
SBX_C="e2b35c${TS}"    # 身份 + egress-mode=off
SBX_D="e2b35d${TS}"    # 身份 + mitm=false（显式关闭）
SBX_E="e2b35e${TS}"    # 身份 + egress-upstream=off（拦截后直连）
SBX_F="e2b35f${TS}"    # 身份 + egress-upstream=mock2（注解覆盖）
SBX_G="e2b35g${TS}"    # 身份（mitm 缺省）+ SANDBOX_PROXY_APPLY=immediate（断言18）
SBX_H="e2b35h${TS}"    # 无身份注解 + expose-ports 写法①（断言19，纯 HostPort 数据面）
SBX_I="e2b35i${TS}"    # 无身份注解 + expose-ports 写法③（断言20，纯 HostPort 数据面）
MIS_A="mis-a-${TS}"
MIS_C="mis-c-${TS}"
MIS_D="mis-d-${TS}"
MIS_E="mis-e-${TS}"
MIS_F="mis-f-${TS}"
MIS_G="mis-g-${TS}"

MANAGED_KEYS=(
    SANDBOX_EGRESS_PROXY_MODE SANDBOX_PROXY_APPLY SANDBOX_PROXY_UPSTREAM
    SANDBOX_PROXY_EXEMPT_CIDRS SANDBOX_PROXY_CONFDIR SANDBOX_PROXY_LOG_DIR
    SANDBOX_PROXY_EXTRA_ARGS
    SPOTBOX_STRATEGY_BASE_URL SPOTBOX_STRATEGY_POLL_INTERVAL MITMPROXY_CONFIG
)

#==================== 状态 ====================#
declare -A BASE_ENV=()
POD_A_ID="" POD_B_ID="" POD_C_ID="" POD_D_ID="" POD_E_ID="" POD_F_ID="" POD_G_ID=""
POD_H_ID="" POD_I_ID=""
CID_A="" CID_B="" CID_C="" CID_D="" CID_E="" CID_F="" CID_G=""
NS_A="" NS_B="" NS_C="" NS_E="" NS_G=""
HOSTIP_A="" HOSTIP_B="" HOSTIP_C="" HOSTIP_E=""
PIN_PORT=""
NODE_IP=""
ORCH_IP_IN_SANDBOX=""
T_CREATE_C=0 T_CREATE_D=0
ORCH_TOUCHED=0
MUX_RESTARTED=0
SITES_PID="" STRAT_PID="" UP1_PID="" UP2_PID=""
BASE_MITM_COUNT=0
GUEST_HAS_CURL=0 GUEST_HAS_OPENSSL=0
BASE_CNI_ENABLED="${BASE_CNI_ENABLED:-1}"
BASE_CNI_POOL_ENABLED="${BASE_CNI_POOL_ENABLED:-1}"
BASE_CNI_POOL_SIZE="${BASE_CNI_POOL_SIZE:-200}"
BASE_HIDE_LABEL="${BASE_HIDE_LABEL:-flux-sandbox.io/direct=true}"

#==================== 工具函数 ====================#
idx_from_hostip() { # <ip> → slot idx（HostIP = base + idx）
    python3 -c 'import ipaddress,sys
ip=int(ipaddress.IPv4Address(sys.argv[1])); base=int(ipaddress.IPv4Address(sys.argv[2]))
idx=ip-base
print(idx if idx >= 1 else "")' "$1" "${HOST_NETWORK_BASE}" 2>/dev/null || true
}

hostip_of() { # <pod_id>
    ${CRICTL} inspectp "$1" 2>/dev/null | jq -r '.status.network.ip // empty' 2>/dev/null || true
}

slot_ns_of() { # <pod_id> → 打印 ns-<idx>
    local ip idx
    ip=$(hostip_of "$1")
    [[ "${ip}" =~ ^10\.11\. ]] || return 1
    idx=$(idx_from_hostip "${ip}")
    [ -n "${idx}" ] || return 1
    echo "ns-${idx}"
}

# 打印 netns 内 nat 的 PREROUTING 链与 E2B_EGRESS_PROXY 自定义链（§17.3-2 链化
# 形态：PREROUTING 只挂一条 -i tap0 → E2B_EGRESS_PROXY jump，豁免与 catch-all
# REDIRECT 全在链内）。orchestrator 容器内 iptables 为 nf_tables 后端，宿主
# iptables-save 是 legacy 后端——两者互相不可见，故优先 nft 输出，为空再回退
# legacy（防未来容器镜像切回 legacy 后端时静默全绿）。
ns_nat_prerouting() { # <ns-name>
    local out
    out=$( { ip netns exec "$1" nft list chain ip nat PREROUTING; \
             ip netns exec "$1" nft list chain ip nat E2B_EGRESS_PROXY; } 2>/dev/null || true)
    if [ -n "${out}" ]; then
        printf '%s\n' "${out}"
    else
        ip netns exec "$1" iptables-save -t nat 2>/dev/null | grep -E "PREROUTING|E2B_EGRESS_PROXY" || true
    fi
}

# 指定 netns 内是否存在导流：PREROUTING 上 -i tap0 → E2B_EGRESS_PROXY jump 存在，
# 且链内有 catch-all REDIRECT → 15001（nft / legacy 两种渲染都匹配）
ns_has_redirect() { # <ns-name>
    local out
    out=$(ns_nat_prerouting "$1")
    grep -qE "iifname \"tap0\".*jump E2B_EGRESS_PROXY|-A PREROUTING -i tap0 .*-j E2B_EGRESS_PROXY" <<< "${out}" \
        && grep -qE "redirect to :${PROXY_PORT}|-j REDIRECT .*--to-ports ${PROXY_PORT}" <<< "${out}"
}

wait_ns_redirect() { # <ns-name> <present|absent> <timeout-s>
    local i
    for i in $(seq 1 "$((${3:-40} / 2))"); do
        if [ "$2" = "present" ] && ns_has_redirect "$1"; then return 0; fi
        if [ "$2" = "absent" ] && ! ns_has_redirect "$1"; then return 0; fi
        sleep 2
    done
    return 1
}

# 断言导流形态齐全且顺序正确：PREROUTING jump 存在；E2B_EGRESS_PROXY 链内
# 豁免在前（orchIP/链路本地/EXEMPT_CIDRS）、catch-all REDIRECT 兜底（§17.3-2）
# 同一行内同时匹配 nft（... return）与 legacy（... -j RETURN）渲染
check_ns_proxy_rules() { # <ns-name>
    local rules l_orch l_link l_exempt l_red
    rules=$(ns_nat_prerouting "$1")
    [ -n "${rules}" ] || return 1
    grep -qE "iifname \"tap0\".*jump E2B_EGRESS_PROXY|-A PREROUTING -i tap0 .*-j E2B_EGRESS_PROXY" <<< "${rules}" || return 1
    l_orch=$(grep -nE "${ORCH_IP_IN_SANDBOX}.*(return|RETURN)" <<< "${rules}" | head -1 | cut -d: -f1)
    l_link=$(grep -nE "169\.254\.0\.0/30.*(return|RETURN)" <<< "${rules}" | head -1 | cut -d: -f1)
    l_exempt=$(grep -nE "${EXEMPT_IP}.*(return|RETURN)" <<< "${rules}" | head -1 | cut -d: -f1)
    l_red=$(grep -nE "redirect to :${PROXY_PORT}|REDIRECT .*--to-ports ${PROXY_PORT}" <<< "${rules}" | head -1 | cut -d: -f1)
    [ -n "${l_orch}" ] && [ -n "${l_link}" ] && [ -n "${l_exempt}" ] && [ -n "${l_red}" ] \
        && [ "${l_orch}" -lt "${l_red}" ] && [ "${l_link}" -lt "${l_red}" ] && [ "${l_exempt}" -lt "${l_red}" ]
}

# 指定 netns 内的 mitmdump 进程 pid 列表（/proc/<pid>/ns/net inode 与 netns 一致）
proxy_pid_in_netns() { # <ns-name>
    local ns_inode pid p_inode
    ns_inode=$(stat -Lc '%i' "${CNI_NETNS_DIR}/$1" 2>/dev/null || true)
    [ -n "${ns_inode}" ] || return 1
    for pid in $(pgrep -f mitmdump 2>/dev/null); do
        p_inode=$(stat -Lc '%i' "/proc/${pid}/ns/net" 2>/dev/null || true)
        [ "${p_inode}" = "${ns_inode}" ] && echo "${pid}"
    done
    return 0
}

wait_proxy_pid() { # <ns-name> <timeout-s> [排除pid]
    local i pids
    for i in $(seq 1 "$((${2:-60} / 2))"); do
        pids=$(proxy_pid_in_netns "$1")
        if [ -n "${pids}" ]; then
            if [ -z "${3:-}" ] || ! grep -qw "$3" <<< "${pids}"; then
                echo "${pids}" | head -1
                return 0
            fi
        fi
        sleep 2
    done
    return 1
}

# per-sandbox 代理日志（SANDBOX_PROXY_LOG_DIR 落盘于 orchestrator pod 内；
# 兜底 kubectl logs 的 zapio 默认路径）
proxy_log_dump() { # <sandbox-id>
    local pod
    pod=$(tm_pod)
    [ -n "${pod}" ] || return 0
    kubectl -n "${ORCH_NS}" exec "${pod}" -- sh -c \
        "cat '${LOG_DIR_POD}/$1.log' 2>/dev/null" 2>/dev/null || true
}

wait_proxy_log() { # <sandbox-id> <pattern> <timeout-s>
    local i
    for i in $(seq 1 "${3:-10}"); do
        if proxy_log_dump "$1" | grep -qF "$2"; then return 0; fi
        sleep 3
    done
    # 兜底：zapio 并入 orchestrator 日志（service=egress-proxy-<sandboxID>）
    kubectl -n "${ORCH_NS}" logs "$(tm_pod)" --tail=3000 2>/dev/null \
        | grep "egress-proxy-$1" | grep -qF "$2"
}

proxy_log_absent() { # <sandbox-id> <pattern> —— 断言 flow 不存在
    ! proxy_log_dump "$1" | grep -qF "$2"
}

# FAIL 分支诊断：倒出代理日志中与给定域名相关的行（含 error/fail/route=）+ 原始 tail
proxy_log_diag() { # <sandbox-id> [pattern]
    local lines
    lines=$(proxy_log_dump "$1")
    if [ -z "${lines}" ]; then
        log_info "  proxy($1): <日志为空或不存在>"
        return 0
    fi
    if [ -n "${2:-}" ]; then
        grep -i "error\|fail\|warn\|route=\|$2" <<< "${lines}" | tail -10 | while read -r l; do log_info "  proxy($1): ${l}"; done
    fi
    tail -8 <<< "${lines}" | while read -r l; do log_info "  proxy($1)| ${l}"; done
}

wait_site_log() { # <grep-pattern> <timeout-s>
    local i
    for i in $(seq 1 "${2:-10}"); do
        grep -q "$1" "${SITES_LOG}" 2>/dev/null && return 0
        sleep 2
    done
    return 1
}

up_count() { # <log-file> <pattern>
    local n
    n=$(grep -c "$2" "$1" 2>/dev/null) || true
    echo "${n:-0}"
}

# guest 内执行（stderr 一并返回；失败不中断脚本）
guest_exec() { # <container_id> <cmd>
    ${CRICTL} exec "$1" sh -c "$2" 2>&1 || true
}

# 沙箱 Pod JSON 注解注入（在 prepare_direct_pod_json 产物上就地修改）
inject_annotations() { # <json-file> <extra-annotations-json>
    local tmp="$1.tmp"
    jq --argjson extra "$2" \
        '.annotations = ((.annotations // {}) + $extra)' "$1" > "${tmp}" \
        && mv "${tmp}" "$1"
}

# 从 guest_exec 输出提取 HTTP code：cri-multiplex 的 SPDY 流调试日志会混进
# exec 输出（帧边界交织，无法按行过滤），只能靠 curl -w 的唯一标记精确提取；
# 无标记 → 空串 → 断言失败（fail-closed 判定，优于污染误绿）
extract_http_code() { grep -oP 'E2B35_CODE:\K[0-9]{3}' | tail -1; }

# 期望失败的 runp（负向用例；不用 run_pod_sandbox 避免重试掩盖错误）
runp_expect_fail() { # <json-file> —— 输出 crictl 输出，返回其退出码
    sync_e2b_pod_json_from_kubelet_yaml /tmp/e2b-kubelet-pod.yaml "$1" || true
    ${CRICTL} runp -T 120s -r e2b "$1" 2>&1
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
    local k
    for k in "${MANAGED_KEYS[@]}"; do
        BASE_ENV[$k]=$(ds_env_get "${k}")
    done
    ORCH_IP_IN_SANDBOX=$(ds_env_get SANDBOX_ORCHESTRATOR_IP)
    ORCH_IP_IN_SANDBOX="${ORCH_IP_IN_SANDBOX:-192.0.2.1}"
    log_pass "已捕获 DaemonSet 基线 env（${#MANAGED_KEYS[@]} 个 SANDBOX_PROXY_* 键，沙箱管理面 IP=${ORCH_IP_IN_SANDBOX}）"

    # 特性 marker：镜像二进制内嵌 SANDBOX_EGRESS_PROXY_MODE 字符串（缺失说明镜像过旧）
    local pod mark
    pod=$(tm_pod)
    [ -n "${pod}" ] || { log_fail "未找到 ${ORCH_DS_SELECTOR} pod"; return 1; }
    mark=$(kubectl -n "${ORCH_NS}" exec "${pod}" -- sh -c \
        'grep -ac "SANDBOX_EGRESS_PROXY_MODE" /usr/bin/orchestrator 2>/dev/null || echo 0' \
        2>/dev/null | tr -d '[:space:]' || true)
    if [ "${mark:-0}" -ge 1 ]; then
        log_pass "orchestrator 镜像含 per-sandbox 代理特性（pod=${pod}）"
    else
        log_fail "orchestrator 镜像不含 SANDBOX_EGRESS_PROXY_MODE 特性，请先更新 ${ORCH_DS} DaemonSet 镜像"
        return 1
    fi
    # 节点（镜像）预置：mitmdump standalone 与 addon.py（§11.3）
    if kubectl -n "${ORCH_NS}" exec "${pod}" -- sh -c \
        "test -x '${PROXY_BINARY_POD}' && test -f '${PROXY_ADDON_POD}'" 2>/dev/null; then
        log_pass "镜像预置 mitmdump（${PROXY_BINARY_POD}）与 addon.py（${PROXY_ADDON_POD}）"
    else
        log_fail "镜像缺少 ${PROXY_BINARY_POD} 或 ${PROXY_ADDON_POD}（§11.3 节点预置未就绪）"
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

# 设置 env（参数为 KEY=VALUE / KEY- 列表）并等待滚动生效
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

# 推送文件到 orchestrator pod（滚动后容器文件系统重置，需重新推送）
push_pod_file() { # <local> <dest>
    local pod
    pod=$(tm_pod)
    if kubectl -n "${ORCH_NS}" cp "$1" "${pod}:$2" 2>/dev/null; then
        return 0
    fi
    local b64
    b64=$(base64 -w0 "$1")
    kubectl -n "${ORCH_NS}" exec "${pod}" -- sh -c "echo '${b64}' | base64 -d > '$2'" 2>/dev/null
}

restore_mux_baseline() {
    log_info "恢复 cri-multiplex 基线（CNI 模式，约 2-3 分钟）"
    CNI_ENABLED="${BASE_CNI_ENABLED}" CNI_POOL_ENABLED="${BASE_CNI_POOL_ENABLED}" \
        CNI_POOL_SIZE="${BASE_CNI_POOL_SIZE}" HIDE_SANDBOX_LABEL="${BASE_HIDE_LABEL}" \
        E2B_FORCE_RESTART=1 bash "${SCRIPT_DIR}/01_start_multiplex.sh" > /tmp/35-restore-mux.log 2>&1 || true
}

#==================== 清理 ====================#
cleanup_35() {
    log_info "清理: 删除用例沙箱 / mock 组件 / mock netns（幂等）"
    local pid
    for pid in "${POD_A_ID}" "${POD_B_ID}" "${POD_C_ID}" "${POD_D_ID}" "${POD_E_ID}" "${POD_F_ID}" "${POD_G_ID}" "${POD_H_ID}" "${POD_I_ID}"; do
        [ -n "${pid}" ] && ${CRICTL} rmp -f "${pid}" >/dev/null 2>&1 || true
    done
    for pid in "${SITES_PID}" "${STRAT_PID}" "${UP1_PID}" "${UP2_PID}"; do
        [ -n "${pid}" ] && kill "${pid}" 2>/dev/null || true
    done
    # 兜底清扫异常退出残留的 mock 进程（路径含本用例唯一前缀，不误伤）
    pkill -f "${WORK}/mock_fwd_proxy.py" 2>/dev/null || true
    pkill -f "${WORK}/mock_strategy.py" 2>/dev/null || true
    pkill -f "${WORK}/mock_sites.py" 2>/dev/null || true
    ip netns del "${MOCK_NS}" >/dev/null 2>&1 || true
    ip link del "${LINK_H}" >/dev/null 2>&1 || true
    rm -f /tmp/e2b-pod-e2b35*.json
    if [ "${ORCH_TOUCHED}" = "1" ]; then
        orch_restore_baseline && log_info "orchestrator 基线 env 已恢复" \
            || log_info "orchestrator 基线恢复失败，请手工检查 kubectl -n ${ORCH_NS} get ds ${ORCH_DS}"
    fi
    if [ "${MUX_RESTARTED}" = "1" ]; then
        restore_mux_baseline
    fi
}
trap cleanup_35 EXIT

#==================== 0. 前置检查 ====================#
log_step "0.1 前置检查"
for cmd in jq iptables iptables-save nft ip python3 kubectl openssl curl base64 stat pgrep sysctl; do
    command -v "${cmd}" >/dev/null 2>&1 || { log_fail "${cmd} 不存在"; exit 1; }
done
[ -f /tmp/cri-proto/api.proto ] || bash "${SCRIPT_DIR}/00_setup.sh" >/dev/null 2>&1 || true
[ -f /tmp/e2b-pod.json ] || bash "${SCRIPT_DIR}/00_setup.sh" >/dev/null 2>&1 || true
[ -f /tmp/e2b-pod.json ] || { log_fail "基础 Pod JSON 准备失败"; exit 1; }
BASE_POD_JSON="${POD_JSON}"
# 测试 CA 唯一真源（宿主侧）：模板构建（refresh_build_id → build_prod.py）与运行期
# mitmdump 都从该路径取 CA，二者恒一致。必须先于 refresh 确保存在——fixture 缺失触发
# 模板重建时 build_prod.py 会把真源 cert 内置进 guest 信任库。
mkdir -p "${CA_DIR_HOST}"
if [ ! -s "${CA_DIR_HOST}/mitmproxy-ca.pem" ]; then
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
        -keyout "${CA_DIR_HOST}/mitmproxy-ca.key" -out "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" \
        -subj "/CN=e2b-egress-test-ca" >/dev/null 2>&1 \
        || { log_fail "openssl 生成测试 CA 失败"; exit 1; }
    # mitmproxy confdir 约定格式：cert+key 合一
    cat "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" "${CA_DIR_HOST}/mitmproxy-ca.key" \
        > "${CA_DIR_HOST}/mitmproxy-ca.pem"
    chmod 600 "${CA_DIR_HOST}/mitmproxy-ca.key" "${CA_DIR_HOST}/mitmproxy-ca.pem"
    log_pass "测试 CA 已生成到真源（CN=e2b-egress-test-ca，${CA_DIR_HOST}）"
else
    log_pass "测试 CA 真源已存在，复用（与模板内置 CA 恒一致）"
fi
if [ ! -f /tmp/e2b-kubelet-pod.yaml ]; then
    E2B_SKIP_BUILD=0 E2B_YAML_COUNT=0 refresh_or_reuse_e2b_yaml \
        "${SCRIPT_DIR}/lib/refresh_build_id.sh" "e2b-kubelet-test" \
        "/tmp/e2b-kubelet-pod.yaml" || { log_fail "E2B fixture 准备失败"; exit 1; }
fi
export E2B_SKIP_BUILD=1 E2B_BASE_POD_YAML=/tmp/e2b-kubelet-pod.yaml
# /tmp/e2b-pod.json 的 build-id/execution-id/envd-access-token 必须与最新模板构建一致
# （模板重建后旧 build 的 envd token 失效、且旧 rootfs 无 curl/内置 CA）；
# 正常创建路径不经 sync（仅负向用例 runp_expect_fail 内部会同步），故在此统一同步基线
sync_e2b_pod_json_from_kubelet_yaml /tmp/e2b-kubelet-pod.yaml "${BASE_POD_JSON}" \
    && log_pass "e2b-pod.json 已同步最新模板凭证（$(grep -oP '"e2b.dev/build-id":\s*"\K[^"]+' "${BASE_POD_JSON}")" \
    || { log_fail "e2b-pod.json 同步最新模板凭证失败"; exit 1; }
log_pass "fixture 就绪（/tmp/e2b-pod.json + /tmp/e2b-kubelet-pod.yaml）"

NODE_IP=$(ip -4 route get 1.1.1.1 2>/dev/null | grep -oP 'src \K[0-9.]+' | head -1)
[ -n "${NODE_IP}" ] || { log_fail "无法探测节点 IP"; exit 1; }
BASE_MITM_COUNT=$(pgrep -fc mitmdump 2>/dev/null || true)   # pgrep -c 无匹配时输出 0 且退出码 1，不能再 || echo 0
log_pass "节点 IP=${NODE_IP}，mitmdump 基线进程数=${BASE_MITM_COUNT}"

capture_ds_baseline || exit 1

#==================== 1. cri-multiplex 切到原生（非 CNI）模式 ====================#
log_step "1.1 切换 cri-multiplex 到非 CNI（原生）模式"
start_non_cni_multiplex "启动 cri-multiplex 非 CNI runtime 模式" || exit 1
MUX_RESTARTED=1

#==================== 2. 测试 CA staging（真源同步）与 mock 站点证书（§7.3，幂等） ====================#
log_step "2.1 同步测试 CA 到 staging 并生成 mock 站点证书（openssl，幂等）"
mkdir -p "${CA_STAGE}"
cp -f "${CA_DIR_HOST}/mitmproxy-ca.pem" "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" "${CA_STAGE}/"
log_pass "测试 CA 已从真源同步到 staging（${CA_STAGE}）"
# mock 站点证书必须由测试 CA 签发且带 SAN——per-sandbox 代理 MITM 后作为 TLS
# 客户端验证上游证书：自签（issuer=自身）或缺 SAN 都会验证失败把 flow 打成 502。
# 存量自签证书（无 .ca-signed 哨兵）自动作废重签。
if [ ! -s "${CA_STAGE}/mock-site.crt" ] || [ ! -f "${CA_STAGE}/mock-site.crt.ca-signed" ]; then
    rm -f "${CA_STAGE}/mock-site.crt" "${CA_STAGE}/mock-site.key"
    cat > "${CA_STAGE}/mock-site.ext" <<EOF
subjectAltName=DNS:e2b35-mock-site,DNS:*.test.local,IP:${SITE_IP},IP:${EXEMPT_IP}
EOF
    openssl req -newkey rsa:2048 -nodes \
        -keyout "${CA_STAGE}/mock-site.key" -out "${CA_STAGE}/mock-site.csr" \
        -subj "/CN=e2b35-mock-site" >/dev/null 2>&1 \
        && openssl x509 -req -in "${CA_STAGE}/mock-site.csr" -days 30 \
            -CA "${CA_STAGE}/mitmproxy-ca-cert.pem" -CAkey "${CA_STAGE}/mitmproxy-ca.pem" \
            -CAcreateserial -extfile "${CA_STAGE}/mock-site.ext" \
            -out "${CA_STAGE}/mock-site.crt" >/dev/null 2>&1 \
        && : > "${CA_STAGE}/mock-site.crt.ca-signed" \
        || { log_fail "mock 站点证书生成失败"; exit 1; }
fi
# MITMPROXY_CONFIG：internal 名单（内网直连）+ header 白名单（X-AI-* 注入/MITM 触发）
cat > "${CA_STAGE}/mitmproxy35.json" <<EOF
{
  "internal":         {"hosts": [], "domains": ["${DOM_INT}", "${DOM_BLOCK}"], "nets": ["${SITE_IP}/32"]},
  "header_whitelist": {"hosts": [], "domains": ["${DOM_EXT}", "${DOM_EXT6}", "${DOM_EXT7}", "${DOM_EXT8A}", "${DOM_EXT8B}"]},
  "header_blacklist": {"hosts": [], "domains": []}
}
EOF
log_pass "MITMPROXY_CONFIG staging 就绪（internal: ${DOM_INT}/${DOM_BLOCK}/${SITE_IP}/32）"

#==================== 3. mock 组件（§12.2 前置①②③） ====================#
log_step "3.1 创建 mock 外部世界 netns（${MOCK_NS}：${MOCK_HOST_IP} <-> ${SITE_IP}/${EXEMPT_IP}）"
ip netns del "${MOCK_NS}" >/dev/null 2>&1 || true
ip link del "${LINK_H}" >/dev/null 2>&1 || true
ip netns add "${MOCK_NS}" || { log_fail "创建 netns ${MOCK_NS} 失败"; exit 1; }
ip link add "${LINK_H}" type veth peer name "${LINK_P}"
ip addr add "${MOCK_HOST_IP}/24" dev "${LINK_H}"
ip link set "${LINK_H}" up
ip link set "${LINK_P}" netns "${MOCK_NS}"
ip netns exec "${MOCK_NS}" ip addr add "${SITE_IP}/24" dev "${LINK_P}"
ip netns exec "${MOCK_NS}" ip addr add "${EXEMPT_IP}/24" dev "${LINK_P}"
ip netns exec "${MOCK_NS}" ip link set "${LINK_P}" up
ip netns exec "${MOCK_NS}" ip link set lo up
ip netns exec "${MOCK_NS}" ip route add default via "${MOCK_HOST_IP}"
sysctl -w "net.ipv4.conf.${LINK_H}.rp_filter=0" >/dev/null
ip netns exec "${MOCK_NS}" sysctl -w net.ipv4.conf.all.rp_filter=0 >/dev/null
log_pass "mock netns 就绪（${SITE_IP} 站点 / ${EXEMPT_IP} 豁免目标）"

log_step "3.2 写入 mock 组件脚本（统一代理 ×2 / 策略平台 / 内外网站点）"
mkdir -p "${WORK}"
: > "${SITES_LOG}"; : > "${UP1_LOG}"; : > "${UP2_LOG}"; : > "${STRAT_LOG}"

# ① mock 统一代理：CONNECT 隧道 + 明文 HTTP 正向代理；*.test.local 静态解析到 SITE_IP
cat > "${WORK}/mock_fwd_proxy.py" <<'PYEOF'
import socket, sys, threading
from urllib.parse import urlsplit

PORT = int(sys.argv[1]); LOG = sys.argv[2]; SITE_IP = sys.argv[3]

def log(line):
    with open(LOG, "a") as f:
        f.write(line + "\n")

def resolve(host):
    if host.endswith(".test.local"):
        return SITE_IP
    return host

def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d:
                break
            b.sendall(d)
    except OSError:
        pass
    finally:
        for s in (a, b):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass

def handle(conn, addr):
    try:
        conn.settimeout(30)
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = conn.recv(65536)
            if not chunk:
                conn.close(); return
            buf += chunk
            if len(buf) > 1 << 20:
                conn.close(); return
        head, _, _rest = buf.partition(b"\r\n\r\n")
        first = head.split(b"\r\n", 1)[0]
        method, target, _ver = first.split(b" ", 2)
        method = method.decode(); target = target.decode(errors="replace")
        if method.upper() == "CONNECT":
            host, _, port = target.partition(":")
            log(f"CONNECT src={addr[0]} dst={target}")
            upstream = socket.create_connection((resolve(host), int(port)), timeout=10)
            conn.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            t = threading.Thread(target=pipe, args=(conn, upstream), daemon=True)
            t.start()
            pipe(upstream, conn)
            t.join(timeout=5)
        else:
            u = urlsplit(target)
            host = u.hostname or ""
            port = u.port or 80
            log(f"HTTP src={addr[0]} {method} {target}")
            upstream = socket.create_connection((resolve(host), port), timeout=10)
            upstream.sendall(buf)
            pipe(upstream, conn)
    except (OSError, ValueError) as e:
        log(f"ERROR src={addr[0]} {e!r}")
    finally:
        try:
            conn.close()
        except OSError:
            pass

srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("0.0.0.0", PORT)); srv.listen(128)
log(f"LISTEN 0.0.0.0:{PORT}")
while True:
    c, a = srv.accept()
    threading.Thread(target=handle, args=(c, a), daemon=True).start()
PYEOF

# ② mock 策略平台：addon.py 快照格式（rules.intranet domain-only deny）+ intercept-report
cat > "${WORK}/mock_strategy.py" <<'PYEOF'
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit, parse_qs

PORT = int(sys.argv[1]); LOG = sys.argv[2]; DENY_DOMAIN = sys.argv[3]
ETAG = "35-v1"
SNAPSHOT = {
    "etag": ETAG,
    "ttl_seconds": 300,
    "identity": {},
    "rules": {
        "intranet": [{"id": "35-deny-block", "action": "deny", "priority": 100,
                      "match": {"domains": [DENY_DOMAIN]}}],
        "extranet": [],
    },
}

def log(line):
    with open(LOG, "a") as f:
        f.write(line + "\n")

class H(BaseHTTPRequestHandler):
    def do_GET(self):
        u = urlsplit(self.path)
        if u.path.endswith("/traffic/control/agent/strategy"):
            q = parse_qs(u.query)
            log(f"STRATEGY identity={q.get('identity', ['-'])[0]}")
            if q.get("etag", [""])[0] == ETAG:
                self.send_response(304); self.end_headers(); return
            body = json.dumps(SNAPSHOT).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body)
        else:
            self.send_response(404); self.end_headers()

    def do_POST(self):
        u = urlsplit(self.path)
        n = int(self.headers.get("Content-Length") or 0)
        if n:
            self.rfile.read(n)
        if u.path.endswith("/traffic/control/agent/intercept-report"):
            log(f"REPORT bytes={n}")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", "2")
            self.end_headers(); self.wfile.write(b"{}")
        else:
            self.send_response(404); self.end_headers()

    def log_message(self, *a):
        pass

ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
PYEOF

# ③ 内/外网 mock 站点：记录每请求源 IP / Host / X-AI-* header
cat > "${WORK}/mock_sites.py" <<'PYEOF'
import ssl, sys, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = sys.argv[1]; CERT = sys.argv[2]; KEY = sys.argv[3]
SITE_IP = sys.argv[4]; EXEMPT_IP = sys.argv[5]
EXT_PORT = int(sys.argv[6]); INT_HTTP_PORT = int(sys.argv[7])
INT_HTTPS_PORT = int(sys.argv[8]); EXEMPT_PORT = int(sys.argv[9])

SITES = [
    ("EXT", SITE_IP, EXT_PORT, True),
    ("INTTLS", SITE_IP, INT_HTTPS_PORT, True),
    ("INTHTTP", SITE_IP, INT_HTTP_PORT, False),
    ("EXEMPT", EXEMPT_IP, EXEMPT_PORT, False),
]

def log(line):
    with open(LOG, "a") as f:
        f.write(line + "\n")

def make_handler(tag):
    class H(BaseHTTPRequestHandler):
        def _ok(self):
            host = self.headers.get("Host", "-")
            xai = self.headers.get("x-ai-userid", "-")
            xais = self.headers.get("x-ai-source", "-")
            log(f"{tag} src={self.client_address[0]} host={host} "
                f"xai_userid={xai} xai_source={xais} path={self.path}")
            body = b"ok\n"
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        do_GET = _ok
        do_POST = _ok
        def log_message(self, *a):
            pass
    return H

for tag, ip, port, use_tls in SITES:
    srv = ThreadingHTTPServer((ip, port), make_handler(tag))
    if use_tls:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(CERT, KEY)
        srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    log(f"LISTEN {tag} {ip}:{port} tls={use_tls}")

threading.Event().wait()
PYEOF
log_pass "mock 脚本就绪（${WORK}）"

log_step "3.3 启动 mock 组件"
for p in "${UP1_PORT}" "${UP2_PORT}" "${STRATEGY_PORT}"; do
    command -v fuser >/dev/null 2>&1 && fuser -k "${p}/tcp" >/dev/null 2>&1 || true
done
sleep 1
ip netns exec "${MOCK_NS}" python3 "${WORK}/mock_sites.py" \
    "${SITES_LOG}" "${CA_STAGE}/mock-site.crt" "${CA_STAGE}/mock-site.key" \
    "${SITE_IP}" "${EXEMPT_IP}" \
    "${EXT_HTTPS_PORT}" "${INT_HTTP_PORT}" "${INT_HTTPS_PORT}" "${EXEMPT_HTTP_PORT}" &
SITES_PID=$!
python3 "${WORK}/mock_strategy.py" "${STRATEGY_PORT}" "${STRAT_LOG}" "${DOM_BLOCK}" &
STRAT_PID=$!
python3 "${WORK}/mock_fwd_proxy.py" "${UP1_PORT}" "${UP1_LOG}" "${SITE_IP}" &
UP1_PID=$!
python3 "${WORK}/mock_fwd_proxy.py" "${UP2_PORT}" "${UP2_LOG}" "${SITE_IP}" &
UP2_PID=$!
log_pass "mock 组件已拉起（sites=${SITES_PID} strategy=${STRAT_PID} up1=${UP1_PID} up2=${UP2_PID}）"
# 就绪等待：高负载节点上 python 进程启动可能超过数秒，固定 sleep 不可靠——
# 轮询直到端口可服务（同时监视进程存活），再进入自检
mock_wait_ready() { # <name> <pid> <url>
    local i CODE
    for i in $(seq 1 30); do
        kill -0 "$2" 2>/dev/null || { log_fail "mock $1 启动失败"; exit 1; }
        CODE=$(curl --noproxy '*' -sS -o /dev/null -w "%{http_code}" --max-time 3 "$3" 2>/dev/null || true)
        [ "${CODE}" = "200" ] && return 0
        sleep 2
    done
    return 1
}
# 自检：宿主经 mock 链路访问内网 HTTP 站点
if mock_wait_ready "站点" "${SITES_PID}" "http://${SITE_IP}:${INT_HTTP_PORT}/"; then
    log_pass "mock 站点链路自检通过（http://${SITE_IP}:${INT_HTTP_PORT} → 200）"
else
    log_fail "mock 站点链路自检失败（60s 内未就绪）"
    exit 1
fi
if mock_wait_ready "策略平台" "${STRAT_PID}" \
    "http://${MOCK_HOST_IP}:${STRATEGY_PORT}/traffic/control/agent/strategy?identity=selftest"; then
    log_pass "mock 策略平台自检通过"
else
    log_fail "mock 策略平台自检失败（60s 内未就绪）"
    exit 1
fi
for pair in "统一代理1:${UP1_PID}" "统一代理2:${UP2_PID}"; do
    name="${pair%%:*}"; pid="${pair##*:}"
    kill -0 "${pid}" 2>/dev/null || { log_fail "mock ${name} 启动失败"; exit 1; }
done
# mock 统一代理功能性自检（进程存活 ≠ 可服务）：HTTP 正向代理 + CONNECT 隧道各一次，
# 从宿主经 MOCK_HOST_IP 访问，验证监听与转发链路（会在 mock 日志留记录，级联断言用差值计数不受影响）。
# 注意：此处绝不能加 --noproxy '*'——它会令 curl 完全忽略 -x 的代理设置（自检 000 假失败）。
for pair in "统一代理1:${UP1_PORT}" "统一代理2:${UP2_PORT}"; do
    name="${pair%%:*}"; port="${pair##*:}"
    CODE=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 5 \
        -x "http://${MOCK_HOST_IP}:${port}" "http://${DOM_INT}:${INT_HTTP_PORT}/" 2>/dev/null || true)
    [ "${CODE}" = "200" ] && log_pass "mock ${name} HTTP 正向代理自检通过（${MOCK_HOST_IP}:${port}）" \
        || { log_fail "mock ${name} HTTP 正向代理自检失败（code=${CODE}）"; exit 1; }
    CODE=$(curl -sk -o /dev/null -w "%{http_code}" --max-time 8 \
        -x "http://${MOCK_HOST_IP}:${port}" "https://${DOM_EXT}:${EXT_HTTPS_PORT}/" 2>/dev/null || true)
    [ "${CODE}" = "200" ] && log_pass "mock ${name} CONNECT 隧道自检通过" \
        || { log_fail "mock ${name} CONNECT 隧道自检失败（code=${CODE}）"; exit 1; }
done

#==================== 4. 断言4：节点未开能力 + 显式 per-sandbox → FailedPrecondition ====================#
# 单独负向用例组：先于正式沙箱创建执行，避免后续滚动销毁在测沙箱
log_step "4.1 [断言4] 移除 SANDBOX_EGRESS_PROXY_MODE 并等待滚动（节点不具备能力）"
orch_apply_env "SANDBOX_EGRESS_PROXY_MODE-" || exit 1

log_step "4.2 [断言4] 显式 egress-mode=per-sandbox（带 sandbox-mis）→ 创建应失败 FailedPrecondition"
prepare_direct_pod_json "e2b35neg" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "e2b35neg${TS}" '{
    "e2b.dev/sandbox-id": $sid,
    "cri-multiplex.dev/egress-mode": "per-sandbox",
    "cri-multiplex.dev/sandbox-mis": "mis-neg"}')" || exit 1
NEG_OUT=$(runp_expect_fail "${POD_JSON}") && NEG_RC=0 || NEG_RC=$?
if [ "${NEG_RC}" -ne 0 ] && grep -qi "FailedPrecondition" <<< "${NEG_OUT}"; then
    log_pass "能力关闭时显式 per-sandbox 创建被拒绝（FailedPrecondition）"
elif [ "${NEG_RC}" -ne 0 ]; then
    log_fail "能力关闭时创建失败但错误码非 FailedPrecondition: ${NEG_OUT}"
else
    log_fail "能力关闭时显式 per-sandbox 竟创建成功（应 FailedPrecondition）: ${NEG_OUT}"
    ${CRICTL} rmp -f "${NEG_OUT}" >/dev/null 2>&1 || true
fi

#==================== 5. 开启 per-sandbox 能力并布设 CA ====================#
log_step "5.1 设置 per-sandbox 全套 env 并等待滚动"
orch_apply_env \
    "SANDBOX_EGRESS_PROXY_MODE=per-sandbox" \
    "SANDBOX_PROXY_APPLY=ready" \
    "SANDBOX_PROXY_UPSTREAM=http://${MOCK_HOST_IP}:${UP1_PORT}" \
    "SANDBOX_PROXY_EXEMPT_CIDRS=${EXEMPT_IP}/32" \
    "SANDBOX_PROXY_CONFDIR=${CA_DIR_POD}" \
    "SANDBOX_PROXY_LOG_DIR=${LOG_DIR_POD}" \
    "SANDBOX_PROXY_EXTRA_ARGS=--set ssl_verify_upstream_trusted_ca=${CA_DIR_POD}/mitmproxy-ca-cert.pem" \
    "SPOTBOX_STRATEGY_BASE_URL=http://${MOCK_HOST_IP}:${STRATEGY_PORT}" \
    "SPOTBOX_STRATEGY_POLL_INTERVAL=5" \
    "MITMPROXY_CONFIG=${MITM_JSON_POD}" || exit 1

log_step "5.2 推送 CA 材料与 MITMPROXY_CONFIG 到 orchestrator pod（§7.3）"
kubectl -n "${ORCH_NS}" exec "$(tm_pod)" -- mkdir -p "${CA_DIR_POD}" "${LOG_DIR_POD}" >&2
push_pod_file "${CA_STAGE}/mitmproxy-ca.pem" "${CA_DIR_POD}/mitmproxy-ca.pem" \
    && push_pod_file "${CA_STAGE}/mitmproxy-ca-cert.pem" "${CA_DIR_POD}/mitmproxy-ca-cert.pem" \
    && push_pod_file "${CA_STAGE}/mitmproxy35.json" "${MITM_JSON_POD}" \
    && log_pass "CA（cert+key）与 MITMPROXY_CONFIG 已推入 pod（${CA_DIR_POD}）" \
    || { log_fail "CA 推送失败"; exit 1; }

#==================== 6. 断言1：推导 per-sandbox（规则 + 进程 + netns 归属） ====================#
log_step "6.1 [断言1] 创建沙箱 A（身份注解 + mitm 缺省（默认 true）+ expose-ports，不显式 egress-mode）"
PIN_PORT=$(python3 - 28080 <<'PYEOF'
import socket, sys
p = int(sys.argv[1])
while p < 28200:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        s.bind(("0.0.0.0", p)); print(p); break
    except OSError:
        p += 1
    finally:
        s.close()
PYEOF
)
[ -n "${PIN_PORT}" ] || { log_fail "expose-ports 空闲端口探测失败"; exit 1; }
prepare_direct_pod_json "e2b35a" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "${SBX_A}" --arg mis "${MIS_A}" \
    --arg ports "${ENVD_PORT}:${PIN_PORT}" '{
    "e2b.dev/sandbox-id": $sid,
    "cri-multiplex.dev/sandbox-mis": $mis,
    "cri-multiplex.dev/egress-profile": "internal",
    "e2b.dev/expose-ports": $ports}')" || exit 1
POD_A_ID=$(run_pod_sandbox) || { log_fail "沙箱 A RunPodSandbox 失败"; print_summary; exit 1; }
log_pass "沙箱 A 创建成功: ${POD_A_ID}（sandbox-id=${SBX_A}，expose-ports=${ENVD_PORT}:${PIN_PORT}）"
CID_A=$(create_and_start_container "${POD_A_ID}") || { log_fail "沙箱 A 容器启动失败"; print_summary; exit 1; }
log_pass "沙箱 A 容器运行中: ${CID_A}"

log_step "6.2 [断言1] netns 内 REDIRECT 规则（ready 异步安装，轮询等待）"
NS_A=$(slot_ns_of "${POD_A_ID}") || { log_fail "无法从沙箱 A HostIP 反推 slot netns"; print_summary; exit 1; }
HOSTIP_A=$(hostip_of "${POD_A_ID}")
log_info "沙箱 A HostIP=${HOSTIP_A} → netns ${NS_A}"
if wait_ns_redirect "${NS_A}" present 40; then
    log_pass "${NS_A} 存在 catch-all REDIRECT → ${PROXY_PORT}（-i tap0）"
else
    log_fail "${NS_A} 40s 内未出现 REDIRECT 规则"
fi
if check_ns_proxy_rules "${NS_A}"; then
    log_pass "${NS_A} 规则齐全且顺序正确（orchIP/链路本地/EXEMPT_CIDRS 豁免在前，REDIRECT 兜底）"
else
    log_fail "${NS_A} 规则不完整或顺序错误: $(ns_nat_prerouting "${NS_A}" | tr '\n' ' ')"
fi

log_step "6.3 [断言1] mitmdump 进程存在且网络命名空间与 slot netns 一致"
if PROXY_PID_A=$(wait_proxy_pid "${NS_A}" 60); then
    log_pass "代理进程运行中 pid=${PROXY_PID_A}，/proc/${PROXY_PID_A}/ns/net 与 ${NS_A} inode 一致"
else
    log_fail "60s 内 ${NS_A} 内未出现 mitmdump 进程"
fi

# guest 工具探测（后续 guest 侧断言的可用性前提）
if guest_exec "${CID_A}" "command -v curl" | grep -q curl; then GUEST_HAS_CURL=1; fi
if guest_exec "${CID_A}" "command -v openssl" | grep -q openssl; then GUEST_HAS_OPENSSL=1; fi
[ "${GUEST_HAS_CURL}" = "1" ] && log_pass "guest curl 可用" || log_skip "guest 无 curl，后续出向行为断言将跳过"

# guest 内置 CA 验证（§7.3②：模板构建时已将真源 CA 内置进系统信任库，
# 后续 MITM 断言的 curl 一律不带 --cacert，直连系统信任库以端到端证明内置生效）
if [ "${GUEST_HAS_OPENSSL}" = "1" ]; then
    OUT=$(guest_exec "${CID_A}" "openssl x509 -in /etc/ssl/certs/e2b-egress-test-ca.pem -noout -subject 2>/dev/null || true")
    if grep -q "e2b-egress-test-ca" <<< "${OUT}"; then
        log_pass "guest 系统信任库已内置测试 CA（模板内置，无需 --cacert）"
    else
        log_fail "guest 信任库缺少内置测试 CA（模板未内置，请先重建模板）"
    fi
else
    log_fail "guest 无 openssl（模板未按预期装好工具链）"
fi

#==================== 7. 断言2：无身份注解 → 推导 off；缺 sandbox-mis → InvalidArgument ====================#
log_step "7.1 [断言2] 创建沙箱 B（无任何 cri-multiplex.dev/* 注解）→ 推导 off"
prepare_direct_pod_json "e2b35b" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "${SBX_B}" '{"e2b.dev/sandbox-id": $sid}')" || exit 1
POD_B_ID=$(run_pod_sandbox) || { log_fail "沙箱 B RunPodSandbox 失败"; print_summary; exit 1; }
CID_B=$(create_and_start_container "${POD_B_ID}") || { log_fail "沙箱 B 容器启动失败"; print_summary; exit 1; }
NS_B=$(slot_ns_of "${POD_B_ID}") || true
HOSTIP_B=$(hostip_of "${POD_B_ID}")
log_pass "沙箱 B 创建成功: ${POD_B_ID}（HostIP=${HOSTIP_B} netns=${NS_B:-?}）"

# ready 模式下规则为异步安装，off 判定需等待足够窗口确认"不会装"
if [ -n "${NS_B}" ]; then
    sleep 15
    if ns_has_redirect "${NS_B}"; then
        log_fail "沙箱 B（无身份注解）netns ${NS_B} 出现 REDIRECT 规则（应推导 off）"
    else
        log_pass "沙箱 B netns ${NS_B} 无 REDIRECT 规则（推导 off 生效）"
    fi
    if [ -n "$(proxy_pid_in_netns "${NS_B}")" ]; then
        log_fail "沙箱 B netns ${NS_B} 内存在 mitmdump 进程（应无代理）"
    else
        log_pass "沙箱 B 无代理进程"
    fi
else
    log_fail "无法反推沙箱 B 的 slot netns（HostIP=${HOSTIP_B}）"
fi
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    CODE_B=$(guest_exec "${CID_B}" "curl -sS -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --connect-timeout 5 --max-time 8 --resolve ${DOM_INT}:${INT_HTTP_PORT}:${SITE_IP} http://${DOM_INT}:${INT_HTTP_PORT}/" | extract_http_code)
    # 只断言直出成功 + 无代理介入（无规则/无进程已在上面断言）；站点观测到的 src
    # 取决于宿主网络栈对 slot 出向的 SNAT 行为（与本特性无关），仅记录不判定。
    SRC_B=$(grep "INTHTTP " "${SITES_LOG}" 2>/dev/null | tail -1 | grep -oE 'src=[0-9.]+' || true)
    if [ "${CODE_B}" = "200" ]; then
        log_pass "沙箱 B 流量直出（HTTP 200，未经代理；站点观测 src=${SRC_B:-?}，宿主 SNAT 行为仅记录）"
    else
        log_fail "沙箱 B 直出异常（code=${CODE_B}，见 ${SITES_LOG}）"
    fi
fi

log_step "7.2 [断言2] 显式 egress-mode=per-sandbox 但缺 sandbox-mis → InvalidArgument（§6.2）"
prepare_direct_pod_json "e2b35nomis" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "e2b35nomis${TS}" '{
    "e2b.dev/sandbox-id": $sid,
    "cri-multiplex.dev/egress-mode": "per-sandbox"}')" || exit 1
NEG_OUT=$(runp_expect_fail "${POD_JSON}") && NEG_RC=0 || NEG_RC=$?
if [ "${NEG_RC}" -ne 0 ] && grep -qi "InvalidArgument" <<< "${NEG_OUT}"; then
    log_pass "缺 sandbox-mis 被 cri-multiplex 拒绝（InvalidArgument）"
elif [ "${NEG_RC}" -ne 0 ]; then
    log_fail "缺 sandbox-mis 创建失败但错误码非 InvalidArgument: ${NEG_OUT}"
else
    log_fail "缺 sandbox-mis 竟创建成功（应 InvalidArgument，策略模块静默禁用防护失效）"
    ${CRICTL} rmp -f "${NEG_OUT}" >/dev/null 2>&1 || true
fi

log_step "7.3 [断言2] egress-profile 取非 internal 值 → InvalidArgument（当前仅支持 internal，§6.2）"
prepare_direct_pod_json "e2b35badprofile" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "e2b35badprofile${TS}" '{
    "e2b.dev/sandbox-id": $sid,
    "cri-multiplex.dev/sandbox-mis": "mis-badprofile",
    "cri-multiplex.dev/egress-profile": "personal"}')" || exit 1
NEG_OUT=$(runp_expect_fail "${POD_JSON}") && NEG_RC=0 || NEG_RC=$?
if [ "${NEG_RC}" -ne 0 ] && grep -qi "InvalidArgument" <<< "${NEG_OUT}"; then
    log_pass "egress-profile=personal 被 cri-multiplex 拒绝（InvalidArgument）"
elif [ "${NEG_RC}" -ne 0 ]; then
    log_fail "egress-profile=personal 创建失败但错误码非 InvalidArgument: ${NEG_OUT}"
else
    log_fail "egress-profile=personal 竟创建成功（应 InvalidArgument，仅支持 internal）"
    ${CRICTL} rmp -f "${NEG_OUT}" >/dev/null 2>&1 || true
fi

#==================== 8. 断言3：显式 egress-mode=off（带身份）→ 不导流 ====================#
log_step "8.1 [断言3] 创建沙箱 C（身份注解 + egress-mode=off）"
prepare_direct_pod_json "e2b35c" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "${SBX_C}" --arg mis "${MIS_C}" '{
    "e2b.dev/sandbox-id": $sid,
    "cri-multiplex.dev/sandbox-mis": $mis,
    "cri-multiplex.dev/egress-mode": "off"}')" || exit 1
T0=$(date +%s%3N)
POD_C_ID=$(run_pod_sandbox) || { log_fail "沙箱 C RunPodSandbox 失败"; print_summary; exit 1; }
T_CREATE_C=$(( $(date +%s%3N) - T0 ))
CID_C=$(create_and_start_container "${POD_C_ID}") || { log_fail "沙箱 C 容器启动失败"; print_summary; exit 1; }
NS_C=$(slot_ns_of "${POD_C_ID}") || true
HOSTIP_C=$(hostip_of "${POD_C_ID}")
log_pass "沙箱 C 创建成功: ${POD_C_ID}（HostIP=${HOSTIP_C} netns=${NS_C:-?} 创建耗时=${T_CREATE_C}ms）"

if [ -n "${NS_C}" ]; then
    sleep 15
    if ns_has_redirect "${NS_C}" || [ -n "$(proxy_pid_in_netns "${NS_C}")" ]; then
        log_fail "沙箱 C（egress-mode=off）出现导流规则或代理进程"
    else
        log_pass "沙箱 C 无 REDIRECT 规则、无代理进程（显式 off 生效）"
    fi
fi
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    CODE_C=$(guest_exec "${CID_C}" "curl -sS -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --connect-timeout 5 --max-time 8 --resolve ${DOM_INT}:${INT_HTTP_PORT}:${SITE_IP} http://${DOM_INT}:${INT_HTTP_PORT}/" | extract_http_code)
    [ "${CODE_C}" = "200" ] && log_pass "沙箱 C 直出正常（HTTP 200）" \
        || log_fail "沙箱 C 直出异常（code=${CODE_C}）"
fi

#==================== 9. 断言5：内网直连 + 外网 MITM（issuer=测试 CA + X-AI-* 注入） ====================#
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    log_step "9.1 [断言5] 沙箱 A 访问内网 mock HTTP（应直连成功 + 代理日志出现 flow）"
    CODE=$(guest_exec "${CID_A}" "curl -sS -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --connect-timeout 5 --max-time 8 --resolve ${DOM_INT}:${INT_HTTP_PORT}:${SITE_IP} http://${DOM_INT}:${INT_HTTP_PORT}/" | extract_http_code)
    [ "${CODE}" = "200" ] && log_pass "内网 mock 访问成功（HTTP 200）" \
        || log_fail "内网 mock 访问失败（code=${CODE}）"
    if wait_proxy_log "${SBX_A}" "${DOM_INT}" 10; then
        log_pass "沙箱 A 代理日志出现内网 flow（${DOM_INT}）"
    else
        log_fail "沙箱 A 代理日志无内网 flow（${DOM_INT}）"
    fi

    log_step "9.2 [断言5] 沙箱 A 访问外网 HTTPS（mitm=true）：subject=SNI 域名（MITM 落叶）+ X-AI-* 注入"
    # 注意：mock 站点证书本身由测试 CA 签发，issuer 在透传/MITM 下都是 e2b-egress-test-ca，
    # 无法区分——必须看 subject：MITM 落叶证书 subject=SNI 域名，透传 subject=e2b35-mock-site。
    OUT=$(guest_exec "${CID_A}" "curl -sv --resolve ${DOM_EXT}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT}:${EXT_HTTPS_PORT}/ -o /dev/null --max-time 15")
    if grep -qi "subject:.*CN\s*=\s*${DOM_EXT}" <<< "${OUT}"; then
        log_pass "外网 HTTPS 证书 subject=${DOM_EXT}（MITM 落叶证书，解密生效）"
    else
        log_fail "外网 HTTPS subject 非 SNI 域名（MITM 未生效）: $(grep -i 'subject\|issuer' <<< "${OUT}" | head -4)"
        proxy_log_diag "${SBX_A}" "${DOM_EXT}"
    fi
    if wait_site_log "EXT src=.* host=${DOM_EXT}:${EXT_HTTPS_PORT} xai_userid=${SBX_A} " 10; then
        log_pass "mock 外网站点收到 X-AI-* 注入 header（x-ai-userid=${SBX_A}，profile=internal 时 UserId 取 SANDBOX_ID）"
    else
        log_fail "mock 外网站点未见 X-AI-* 注入（见 ${SITES_LOG}）"
        proxy_log_diag "${SBX_A}" "${DOM_EXT}"
    fi
fi

#==================== 10. 断言6：guest 零配置 ====================#
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    log_step "10.1 [断言6] guest 无 proxy env / 无 profile.d 残留（§4.4）"
    OUT=$(guest_exec "${CID_A}" "env")
    if grep -qiE '^(http|https|all|no)_proxy=' <<< "${OUT}"; then
        log_fail "guest 存在 proxy env 注入: $(grep -iE '^(http|https|all|no)_proxy=' <<< "${OUT}")"
    else
        log_pass "guest 无任何 *_proxy env（零配置语义成立）"
    fi
    OUT=$(guest_exec "${CID_A}" "ls /etc/profile.d/ 2>/dev/null || true")
    if grep -qiE 'egress|proxy' <<< "${OUT}"; then
        log_fail "guest /etc/profile.d 存在代理残留: $(grep -iE 'egress|proxy' <<< "${OUT}")"
    else
        log_pass "guest /etc/profile.d 无代理残留"
    fi

    log_step "10.2 [断言6] 普通 curl（零代理配置）仍被代理管控"
    CODE=$(guest_exec "${CID_A}" "curl -sS -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --resolve ${DOM_EXT6}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT6}:${EXT_HTTPS_PORT}/ --max-time 15" | extract_http_code)
    if [ "${CODE}" = "200" ] && wait_proxy_log "${SBX_A}" "${DOM_EXT6}" 10; then
        log_pass "零配置 curl 被透明劫持管控（HTTP 200 + 代理日志出现 ${DOM_EXT6} flow）"
    else
        log_fail "零配置 curl 管控断言失败（code=${CODE}）"
        proxy_log_diag "${SBX_A}" "${DOM_EXT6}"
    fi
fi

#==================== 11. 断言7：transparent 劫持路径 ====================#
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    log_step "11.1 [断言7] curl -v 显示直连真实目的 IP（无 CONNECT 到 169.254.0.22）"
    OUT=$(guest_exec "${CID_A}" "curl -sv --resolve ${DOM_EXT7}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT7}:${EXT_HTTPS_PORT}/ -o /dev/null --max-time 15")
    if grep -q "Connected to ${DOM_EXT7} (${SITE_IP})" <<< "${OUT}" \
        && ! grep -q "CONNECT" <<< "${OUT}" && ! grep -q "169.254.0.22" <<< "${OUT}"; then
        log_pass "verbose 显示直连 ${SITE_IP}:${EXT_HTTPS_PORT}，无 CONNECT/无 169.254.0.22（transparent 而非显式代理）"
    else
        log_fail "transparent 路径断言失败: $(grep -E 'Connected to|CONNECT|169.254' <<< "${OUT}" | head -3)"
    fi
    if wait_proxy_log "${SBX_A}" "${DOM_EXT7}" 10; then
        log_pass "代理日志出现 TLS flow 且 SNI 解析出 ${DOM_EXT7}"
    else
        log_fail "代理日志未见 ${DOM_EXT7} flow（SNI 解析失败？）"
    fi
fi

#==================== 12. 断言8：全程序覆盖（无绕过面）+ 裸 IP 边界 ====================#
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    log_step "12.1 [断言8] curl --noproxy '*'（不读 proxy env）同样被劫持"
    CODE=$(guest_exec "${CID_A}" "curl -sS -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --noproxy '*' --resolve ${DOM_EXT8A}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT8A}:${EXT_HTTPS_PORT}/ --max-time 15" | extract_http_code)
    if [ "${CODE}" = "200" ] && wait_proxy_log "${SBX_A}" "${DOM_EXT8A}" 10; then
        log_pass "--noproxy 连接仍被 REDIRECT 劫持进代理（200 + flow 记录）"
    else
        log_fail "--noproxy 劫持断言失败（code=${CODE}）"
        proxy_log_diag "${SBX_A}" "${DOM_EXT8A}"
    fi
fi
if [ "${GUEST_HAS_OPENSSL}" = "1" ]; then
    log_step "12.2 [断言8] openssl s_client（不读 proxy env 的 TLS 客户端）同样被劫持"
    guest_exec "${CID_A}" "printf 'GET / HTTP/1.1\r\nHost: ${DOM_EXT8B}\r\nConnection: close\r\n\r\n' | openssl s_client -quiet -connect ${SITE_IP}:${EXT_HTTPS_PORT} -servername ${DOM_EXT8B} -verify_quiet 2>/dev/null" >/dev/null
    # MITM 解密 + X-AI-* 注入到达站点 = 经过代理的确定性证据（直连不会有注入 header）
    if wait_site_log "EXT src=.* host=${DOM_EXT8B} xai_userid=${SBX_A} " 10 \
        || wait_proxy_log "${SBX_A}" "${DOM_EXT8B}" 3; then
        log_pass "openssl s_client 连接被劫持管控（站点收到注入 header / 代理日志有 flow）"
    else
        log_fail "openssl s_client 连接未见代理管控证据"
    fi
else
    log_skip "guest 无 openssl，跳过 s_client 子断言"
fi
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    log_step "12.3 [断言8] 裸 IP 直连（无 SNI）：host 退化为 IP、按 internal.nets IP 级规则处理"
    # ${SITE_IP}/32 已列入 internal.nets → 裸 IP 访问按内网直连放行；
    # 域名级白/黑/策略名单对 IP 字面量天然不命中（§4.4 固有边界，断言记录在案而非拦截）
    CODE=$(guest_exec "${CID_A}" "curl -sk -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --connect-timeout 5 --max-time 10 https://${SITE_IP}:${EXT_HTTPS_PORT}/" | extract_http_code)
    if [ "${CODE}" = "200" ]; then
        log_pass "裸 IP 直连按 IP 级规则（internal.nets）直连放行（200）；域名级管控不命中——transparent 固有边界，记录在案"
    else
        log_fail "裸 IP 直连异常（code=${CODE}），internal.nets IP 级规则未生效"
    fi
    if proxy_log_dump "${SBX_A}" | grep -qF "${SITE_IP}:${EXT_HTTPS_PORT}"; then
        log_info "代理日志含 IP 字面量 flow（host 已退化为 IP）：$(proxy_log_dump "${SBX_A}" | grep -F "${SITE_IP}:${EXT_HTTPS_PORT}" | tail -1)"
    else
        log_info "代理日志无裸 IP flow 记录（TCPLayer 透传不产生 HTTP flow 日志，属预期边界）"
    fi
fi

#==================== 13. 断言9：mitm=false 透传 + domain 级 deny fail-close ====================#
log_step "13.1 [断言9] 创建沙箱 D（身份注解 + egress-mitm=false）"
prepare_direct_pod_json "e2b35d" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "${SBX_D}" --arg mis "${MIS_D}" '{
    "e2b.dev/sandbox-id": $sid,
    "cri-multiplex.dev/sandbox-mis": $mis,
    "cri-multiplex.dev/egress-mitm": "false"}')" || exit 1
T0=$(date +%s%3N)
POD_D_ID=$(run_pod_sandbox) || { log_fail "沙箱 D RunPodSandbox 失败"; print_summary; exit 1; }
T_CREATE_D=$(( $(date +%s%3N) - T0 ))
CID_D=$(create_and_start_container "${POD_D_ID}") || { log_fail "沙箱 D 容器启动失败"; print_summary; exit 1; }
log_pass "沙箱 D 创建成功: ${POD_D_ID}（创建耗时=${T_CREATE_D}ms）"
NS_D=$(slot_ns_of "${POD_D_ID}") || true
if [ -n "${NS_D}" ]; then
    if wait_ns_redirect "${NS_D}" present 40 && wait_proxy_pid "${NS_D}" 60 >/dev/null; then
        log_pass "沙箱 D 代理就绪（netns ${NS_D}）"
    else
        log_fail "沙箱 D 代理 60s 内未就绪"
    fi
fi

if [ "${GUEST_HAS_CURL}" = "1" ]; then
    log_step "13.2 [断言9] mitm=false：TLS 端到端透传（subject=站点证书，非 SNI 落叶）"
    # mock 站点证书由测试 CA 签发 → issuer 恒为 e2b-egress-test-ca，不能区分透传/MITM；
    # 看 subject：透传=站点证书 subject（e2b35-mock-site），MITM=落叶证书 subject（SNI 域名）。
    OUT=$(guest_exec "${CID_D}" "curl -skv --resolve ${DOM_EXT}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT}:${EXT_HTTPS_PORT}/ -o /dev/null --max-time 15")
    if grep -qi "subject:.*e2b35-mock-site" <<< "${OUT}" && ! grep -qi "subject:.*CN\s*=\s*${DOM_EXT}" <<< "${OUT}"; then
        log_pass "mitm=false 沙箱 TLS 端到端（subject=e2b35-mock-site，未解密透传）"
    else
        log_fail "mitm=false 透传断言失败: $(grep -i 'subject\|issuer' <<< "${OUT}" | head -4)"
        log_info "诊断: 沙箱 D 派生 no-MITM 配置与代理日志 tail 如下"
        kubectl -n "${ORCH_NS}" exec "$(tm_pod)" -- sh -c \
            "find /tmp /var/lib -name 'mitmproxy-config-nomitm.json' -exec sh -c 'echo == {}; cat {}' \; 2>/dev/null | head -30" 2>/dev/null || true
        proxy_log_dump "${SBX_D}" | tail -15 | while read -r l; do log_info "  proxyD: ${l}"; done
    fi

    log_step "13.3 [断言9] domain 级 deny 策略命中 → 403 可见（模板内置 CA，DEFER-MITM 成功）"
    # 等待沙箱 D 策略客户端拉取快照（mock 平台日志出现其 identity 轮询）
    STRAT_READY=0
    for i in $(seq 1 15); do
        grep -q "STRATEGY identity=${MIS_D}" "${STRAT_LOG}" 2>/dev/null && { STRAT_READY=1; break; }
        sleep 2
    done
    [ "${STRAT_READY}" = "1" ] && log_pass "沙箱 D 策略客户端已连接 mock 平台（identity=${MIS_D}）" \
        || log_fail "沙箱 D 策略客户端 30s 内未拉取策略（${STRAT_LOG}）"
    # 语义变化（模板内置 CA 后）：mitm=false 只关闭 header 注入触发的 MITM，
    # 策略维度 domain-only deny 的 DEFER-MITM（为让客户端看到 403 body）仍会解密，
    # 且 guest 信任测试 CA → 握手成功 → request 钩子返回 403（RC=0）。
    # 模板未内置 CA 时退化为握手失败（fail-close 式拒绝），两种都算 deny 生效。
    DENIED=0
    for i in $(seq 1 20); do
        CODE=$(guest_exec "${CID_D}" "curl -sk -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --connect-timeout 4 --max-time 8 --resolve ${DOM_BLOCK}:${INT_HTTPS_PORT}:${SITE_IP} https://${DOM_BLOCK}:${INT_HTTPS_PORT}/" | extract_http_code)
        if [ "${CODE}" = "403" ]; then DENIED=1; break; fi
        sleep 3
    done
    if [ "${DENIED}" = "1" ]; then
        log_pass "domain 级 deny 生效：DEFER-MITM 解密后 request 钩子返回 403（模板内置 CA 语义）"
    else
        log_fail "60s 内 domain 级 deny 未生效（${DOM_BLOCK} 未返回 403，最后一次 code=${CODE:-?}）"
    fi
fi

#==================== 14. 断言10：EXEMPT_CIDRS 直连、代理无感知 ====================#
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    log_step "14.1 [断言10] 沙箱 A 访问 EXEMPT_CIDRS 内目的（${EXEMPT_IP}/32）→ 直连、代理日志无 flow"
    CODE=$(guest_exec "${CID_A}" "curl -sS -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --connect-timeout 5 --max-time 8 http://${EXEMPT_IP}:${EXEMPT_HTTP_PORT}/" | extract_http_code)
    if [ "${CODE}" = "200" ]; then
        log_pass "EXEMPT_CIDRS 内目的直连成功（HTTP 200）"
    else
        log_fail "EXEMPT_CIDRS 内目的访问失败（code=${CODE}）"
    fi
    sleep 2
    if proxy_log_absent "${SBX_A}" "${EXEMPT_IP}"; then
        log_pass "代理日志无 ${EXEMPT_IP} flow（iptables 层豁免，未经代理）"
    else
        log_fail "代理日志出现 ${EXEMPT_IP} flow（豁免未生效）"
    fi
fi

#==================== 15. 断言12+17c：egress-upstream=off 直连 + 出节点源 IP ====================#
log_step "15.1 [断言12/17c] 创建沙箱 E（身份注解 + egress-upstream=off，拦截后直连）"
prepare_direct_pod_json "e2b35e" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "${SBX_E}" --arg mis "${MIS_E}" '{
    "e2b.dev/sandbox-id": $sid,
    "cri-multiplex.dev/sandbox-mis": $mis,
    "cri-multiplex.dev/egress-upstream": "off"}')" || exit 1
POD_E_ID=$(run_pod_sandbox) || { log_fail "沙箱 E RunPodSandbox 失败"; print_summary; exit 1; }
CID_E=$(create_and_start_container "${POD_E_ID}") || { log_fail "沙箱 E 容器启动失败"; print_summary; exit 1; }
NS_E=$(slot_ns_of "${POD_E_ID}") || true
HOSTIP_E=$(hostip_of "${POD_E_ID}")
log_pass "沙箱 E 创建成功: ${POD_E_ID}（HostIP=${HOSTIP_E} netns=${NS_E:-?}）"
if [ -n "${NS_E}" ]; then
    if wait_ns_redirect "${NS_E}" present 40 && wait_proxy_pid "${NS_E}" 60 >/dev/null; then
        log_pass "沙箱 E 代理就绪（netns ${NS_E}）"
    else
        log_fail "沙箱 E 代理 60s 内未就绪"
    fi
fi

if [ "${GUEST_HAS_CURL}" = "1" ] && [ -n "${HOSTIP_E}" ]; then
    log_step "15.2 [断言12] 代理上游出节点：mock 站点观测源 IP（vrt SNAT → HostIP 证据链）"
    UP1_BEFORE=$(up_count "${UP1_LOG}" "${EXT_HTTPS_PORT}")
    OUT=$(guest_exec "${CID_E}" "curl -sk -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --resolve ${DOM_EXT}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT}:${EXT_HTTPS_PORT}/ --max-time 15")
    CODE_E=$(extract_http_code <<< "${OUT}")
    log_info "沙箱 E 外网访问 code=${CODE_E:-?}（upstream=off 应直连成功 200）"
    # 直连路径：代理进程（netns 内）→ vrt SNAT → HostIP → 宿主路由 → mock 站点。
    # mock 站点在节点本地，宿主 MASQUERADE（-o 默认网关）不命中本链路，故站点观测
    # 到的是 slot HostIP（证明 §4.2-3 vrt SNAT 生效）；宿主 MASQUERADE 规则存在性
    # 单独断言（证明出真实外网时源 IP 会被改写为节点 IP 的第二段链路在位）。
    if wait_site_log "EXT src=${HOSTIP_E} " 10; then
        log_pass "mock 站点观测源 IP = slot HostIP ${HOSTIP_E}（vrt SNAT 生效；若源为 10.12.x.x 则 SNAT 缺失）"
    else
        SRC_SEEN=$(grep "EXT " "${SITES_LOG}" 2>/dev/null | tail -3 || true)
        log_fail "mock 站点未观测到源 IP=${HOSTIP_E} 的请求（最近记录: ${SRC_SEEN:-无}）"
        log_info "沙箱 E curl code=${CODE_E:-?}"
        proxy_log_diag "${SBX_E}" "${DOM_EXT}"
    fi
    # orchestrator 在 pod 内以 nf_tables 后端安装宿主侧规则（per-slot /32 MASQUERADE），
    # 宿主 iptables-save 是 legacy 后端不可见；优先 nft，回退 legacy
    if nft list chain ip nat POSTROUTING 2>/dev/null | grep -E "ip saddr ${HOSTIP_E}[ /].*masquerade" | grep -q . \
        || iptables-save -t nat 2>/dev/null | grep -q -- "-A POSTROUTING -s ${HOSTIP_E}.*-j MASQUERADE"; then
        log_pass "宿主 ${HOSTIP_E}（${HOST_NETWORK_CIDR} 段）MASQUERADE 规则在位（出真实外网时源 IP → 节点 IP）"
    else
        log_fail "宿主缺少 ${HOSTIP_E} MASQUERADE 规则"
    fi

    log_step "15.3 [断言17c] egress-upstream=off → 外网直连，统一代理无记录"
    sleep 2
    UP1_AFTER=$(up_count "${UP1_LOG}" "${EXT_HTTPS_PORT}")
    if [ "${UP1_AFTER}" = "${UP1_BEFORE}" ]; then
        log_pass "沙箱 E 外网访问未经统一代理（mock1 计数不变 ${UP1_BEFORE}）"
    else
        log_fail "沙箱 E（upstream=off）外网流量疑似经过统一代理（mock1 计数 ${UP1_BEFORE} -> ${UP1_AFTER}）"
    fi
fi

#==================== 16. 断言17a/b/d：上游级联 ====================#
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    log_step "16.1 [断言17a] 沙箱 A（节点默认上游）访问外网 → 本地代理 flow + mock1 收到 CONNECT"
    UP1_BEFORE=$(up_count "${UP1_LOG}" "CONNECT")
    OUT=$(guest_exec "${CID_A}" "curl -sS -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --resolve ${DOM_EXT}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT}:${EXT_HTTPS_PORT}/ --max-time 15")
    CODE=$(extract_http_code <<< "${OUT}")
    sleep 2
    UP1_AFTER=$(up_count "${UP1_LOG}" "CONNECT")
    if [ "${UP1_AFTER}" -gt "${UP1_BEFORE}" ] && proxy_log_dump "${SBX_A}" | grep -qF "${DOM_EXT}"; then
        log_pass "级联生效：本地代理有 ${DOM_EXT} flow 且 mock1 收到 CONNECT（${UP1_BEFORE} -> ${UP1_AFTER}）"
        grep "CONNECT" "${UP1_LOG}" | tail -2 | while read -r l; do log_info "  mock1: ${l}"; done
    else
        log_fail "级联断言失败（mock1 CONNECT 计数 ${UP1_BEFORE} -> ${UP1_AFTER}，沙箱侧 code=${CODE:-?}）"
        # 诊断：代理日志 route label + 从沙箱 netns 内直连 mock1 的连通性
        proxy_log_dump "${SBX_A}" | grep -i "route=\|error\|${DOM_EXT}" | tail -8 | while read -r l; do log_info "  proxyA: ${l}"; done
        PPID_A=$(proxy_pid_in_netns "${NS_A}" | head -1 || true)
        if [ -n "${PPID_A}" ]; then
            nsenter -t "${PPID_A}" -n curl -sS -o /dev/null -w 'netns->mock1 HTTP code=%{http_code}\n' --max-time 5 \
                -x "http://${MOCK_HOST_IP}:${UP1_PORT}" "http://${DOM_INT}:${INT_HTTP_PORT}/" 2>&1 \
                | tail -2 | while read -r l; do log_info "  diag: ${l}"; done
        fi
    fi

    log_step "16.2 [断言17b] 沙箱 A 访问内网 mock → 直连，统一代理无记录"
    UP1_BEFORE=$(up_count "${UP1_LOG}" "${INT_HTTP_PORT}")
    guest_exec "${CID_A}" "curl -sS -o /dev/null --resolve ${DOM_INT}:${INT_HTTP_PORT}:${SITE_IP} http://${DOM_INT}:${INT_HTTP_PORT}/ --max-time 8" >/dev/null
    sleep 2
    UP1_AFTER=$(up_count "${UP1_LOG}" "${INT_HTTP_PORT}")
    if [ "${UP1_AFTER}" = "${UP1_BEFORE}" ]; then
        log_pass "内网 flow 直连，统一代理无记录（计数不变 ${UP1_BEFORE}）"
    else
        log_fail "内网 flow 疑似经过统一代理（计数 ${UP1_BEFORE} -> ${UP1_AFTER}）"
    fi
fi

log_step "16.3 [断言17d] 注解覆盖上游：沙箱 F（egress-upstream=mock2）→ 流向覆盖后的地址"
prepare_direct_pod_json "e2b35f" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "${SBX_F}" --arg mis "${MIS_F}" \
    --arg up "http://${MOCK_HOST_IP}:${UP2_PORT}" '{
    "e2b.dev/sandbox-id": $sid,
    "cri-multiplex.dev/sandbox-mis": $mis,
    "cri-multiplex.dev/egress-upstream": $up}')" || exit 1
POD_F_ID=$(run_pod_sandbox) || { log_fail "沙箱 F RunPodSandbox 失败"; print_summary; exit 1; }
CID_F=$(create_and_start_container "${POD_F_ID}") || { log_fail "沙箱 F 容器启动失败"; print_summary; exit 1; }
NS_F=$(slot_ns_of "${POD_F_ID}") || true
if [ -n "${NS_F}" ]; then
    wait_ns_redirect "${NS_F}" present 40 >/dev/null 2>&1 && wait_proxy_pid "${NS_F}" 60 >/dev/null \
        && log_pass "沙箱 F 代理就绪" || log_fail "沙箱 F 代理 60s 内未就绪"
fi
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    UP1_BEFORE=$(up_count "${UP1_LOG}" "CONNECT")
    UP2_BEFORE=$(up_count "${UP2_LOG}" "CONNECT")
    OUT=$(guest_exec "${CID_F}" "curl -sk -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --resolve ${DOM_EXT}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT}:${EXT_HTTPS_PORT}/ --max-time 15")
    CODE_F=$(extract_http_code <<< "${OUT}")
    sleep 2
    UP1_AFTER=$(up_count "${UP1_LOG}" "CONNECT")
    UP2_AFTER=$(up_count "${UP2_LOG}" "CONNECT")
    if [ "${UP2_AFTER}" -gt "${UP2_BEFORE}" ] && [ "${UP1_AFTER}" = "${UP1_BEFORE}" ]; then
        log_pass "注解覆盖生效：沙箱 F 外网流向 mock2（${UP2_BEFORE} -> ${UP2_AFTER}），mock1 无新增（${UP1_BEFORE}）"
    else
        log_fail "注解覆盖断言失败（mock1 ${UP1_BEFORE}->${UP1_AFTER}，mock2 ${UP2_BEFORE}->${UP2_AFTER}，沙箱侧 code=${CODE_F:-?}）"
        proxy_log_dump "${SBX_F}" | grep -i "route=\|error\|${DOM_EXT}" | tail -8 | while read -r l; do log_info "  proxyF: ${l}"; done
    fi
fi

#==================== 17. 断言13：expose-ports 与出向代理并存 ====================#
log_step "17.1 [断言13] 沙箱 A expose-ports（${ENVD_PORT}:${PIN_PORT}）入向访问"
CODE=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 5 "http://${NODE_IP}:${PIN_PORT}/health" 2>/dev/null || true)
if [ "${CODE}" = "200" ] || [ "${CODE}" = "204" ]; then
    log_pass "宿主经 ${NODE_IP}:${PIN_PORT} 入向访问正常（HTTP ${CODE}）"
else
    log_fail "expose-ports 入向访问失败（HTTP ${CODE}）"
fi
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    log_step "17.2 [断言13] 同沙箱出向代理并存无干扰"
    CODE=$(guest_exec "${CID_A}" "curl -sS -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --resolve ${DOM_EXT}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT}:${EXT_HTTPS_PORT}/ --max-time 15" | extract_http_code)
    if [ "${CODE}" = "200" ]; then
        log_pass "出向代理与入向 HostPort 并存无干扰（200）"
    else
        log_fail "expose-ports 组合下出向异常（code=${CODE}）"
        proxy_log_diag "${SBX_A}" "${DOM_EXT}"
    fi
fi

#==================== 17b. 断言19/20/21：expose-ports 三写法原生模式矩阵 ====================#
# 场景 1（HostPort 访问沙箱内 49983）的原生模式完整看护。写法②（指定宿主端口
# 49983:P）由断言13（沙箱 A）覆盖；此处补写法①（池内自动分配）/写法③（区间
# 分配）正向 + pinned 冲突/区间耗尽/malformed 负向。H/I 不带身份注解（推导
# off、不起代理）——本组断言只看 HostPort 数据面；分配结果经 cri-multiplex
# PodSandboxStatus 注解 e2b.dev/host-port-49983 读回。

log_step "17b.1 [断言19] 写法①：expose-ports=49983（池内自动分配）→ 读回端口入向可达"
prepare_direct_pod_json "e2b35h" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "${SBX_H}" '{
    "e2b.dev/sandbox-id": $sid,
    "e2b.dev/expose-ports": "49983"}')" || exit 1
POD_H_ID=$(run_pod_sandbox) || { log_fail "沙箱 H（写法①）RunPodSandbox 失败"; print_summary; exit 1; }
HP_H=$(${CRICTL} inspectp "${POD_H_ID}" 2>/dev/null | jq -r '.status.annotations["e2b.dev/host-port-49983"] // empty')
if [ -z "${HP_H}" ]; then
    log_fail "沙箱 H 未读回自动分配的宿主端口（e2b.dev/host-port-49983 注解缺失）"
else
    CODE=""
    for i in $(seq 1 10); do
        CODE=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 3 "http://${NODE_IP}:${HP_H}/health" 2>/dev/null || true)
        if [ "${CODE}" = "200" ] || [ "${CODE}" = "204" ]; then break; fi
        sleep 3
    done
    if [ "${CODE}" = "200" ] || [ "${CODE}" = "204" ]; then
        log_pass "写法① 自动分配宿主端口 ${HP_H}，入向访问正常（HTTP ${CODE}）"
    else
        log_fail "写法① 入向访问失败（${NODE_IP}:${HP_H} HTTP ${CODE}）"
    fi
fi

log_step "17b.2 [断言20] 写法③：expose-ports=49983:H1-H2（区间分配）→ 读回端口落区间、入向可达"
RANGE_BASE=$(python3 - 28300 <<'PYEOF'
import socket, sys
p = int(sys.argv[1])
while p < 28500:
    socks = []
    try:
        for q in (p, p + 1, p + 2):
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.bind(("0.0.0.0", q))
            socks.append(s)
        print(p)
        break
    except OSError:
        p += 1
    finally:
        for s in socks:
            s.close()
PYEOF
)
[ -n "${RANGE_BASE}" ] || { log_fail "写法③ 连续空闲区间探测失败"; print_summary; exit 1; }
RANGE_END=$((RANGE_BASE + 2))
prepare_direct_pod_json "e2b35i" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "${SBX_I}" --arg ports "${ENVD_PORT}:${RANGE_BASE}-${RANGE_END}" '{
    "e2b.dev/sandbox-id": $sid,
    "e2b.dev/expose-ports": $ports}')" || exit 1
POD_I_ID=$(run_pod_sandbox) || { log_fail "沙箱 I（写法③）RunPodSandbox 失败"; print_summary; exit 1; }
HP_I=$(${CRICTL} inspectp "${POD_I_ID}" 2>/dev/null | jq -r '.status.annotations["e2b.dev/host-port-49983"] // empty')
if [ -z "${HP_I}" ]; then
    log_fail "沙箱 I 未读回区间分配的宿主端口（e2b.dev/host-port-49983 注解缺失）"
elif [ "${HP_I}" -lt "${RANGE_BASE}" ] || [ "${HP_I}" -gt "${RANGE_END}" ]; then
    log_fail "写法③ 读回端口 ${HP_I} 不在声明区间 ${RANGE_BASE}-${RANGE_END} 内"
else
    CODE=""
    for i in $(seq 1 10); do
        CODE=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 3 "http://${NODE_IP}:${HP_I}/health" 2>/dev/null || true)
        if [ "${CODE}" = "200" ] || [ "${CODE}" = "204" ]; then break; fi
        sleep 3
    done
    if [ "${CODE}" = "200" ] || [ "${CODE}" = "204" ]; then
        log_pass "写法③ 区间 ${RANGE_BASE}-${RANGE_END} 分配到 ${HP_I}，入向访问正常（HTTP ${CODE}）"
    else
        log_fail "写法③ 入向访问失败（${NODE_IP}:${HP_I} HTTP ${CODE}）"
    fi
fi

log_step "17b.3 [断言21] 负向：pinned 冲突（沙箱 A 占用 ${PIN_PORT}）→ ResourceExhausted"
prepare_direct_pod_json "e2b35hpc" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "e2b35hpc${TS}" --arg ports "${ENVD_PORT}:${PIN_PORT}" '{
    "e2b.dev/sandbox-id": $sid,
    "e2b.dev/expose-ports": $ports}')" || exit 1
NEG_OUT=$(runp_expect_fail "${POD_JSON}") && NEG_RC=0 || NEG_RC=$?
if [ "${NEG_RC}" -ne 0 ] && grep -qi "ResourceExhausted" <<< "${NEG_OUT}"; then
    log_pass "pinned 冲突被拒绝（ResourceExhausted）"
elif [ "${NEG_RC}" -ne 0 ]; then
    log_fail "pinned 冲突创建失败但错误码非 ResourceExhausted: ${NEG_OUT}"
else
    log_fail "pinned 冲突竟创建成功（应 ResourceExhausted）"
    ${CRICTL} rmp -f "${NEG_OUT}" >/dev/null 2>&1 || true
fi

log_step "17b.4 [断言21] 负向：区间耗尽（外部占用整个区间）→ ResourceExhausted"
XRANGE_BASE=$(python3 - 28600 <<'PYEOF'
import socket, sys
p = int(sys.argv[1])
while p < 28800:
    socks = []
    try:
        for q in (p, p + 1):
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.bind(("0.0.0.0", q))
            socks.append(s)
        print(p)
        break
    except OSError:
        p += 1
    finally:
        for s in socks:
            s.close()
PYEOF
)
[ -n "${XRANGE_BASE}" ] || { log_fail "区间耗尽用例空闲区间探测失败"; print_summary; exit 1; }
python3 - "${XRANGE_BASE}" >/dev/null 2>&1 <<'PYEOF' &
import socket, sys, time
base = int(sys.argv[1])
socks = []
for q in (base, base + 1):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("0.0.0.0", q))
    s.listen(1)
    socks.append(s)
time.sleep(120)
PYEOF
HOLDER_PID=$!
sleep 1
prepare_direct_pod_json "e2b35hxf" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "e2b35hxf${TS}" --arg ports "${ENVD_PORT}:${XRANGE_BASE}-$((XRANGE_BASE + 1))" '{
    "e2b.dev/sandbox-id": $sid,
    "e2b.dev/expose-ports": $ports}')" || exit 1
NEG_OUT=$(runp_expect_fail "${POD_JSON}") && NEG_RC=0 || NEG_RC=$?
kill "${HOLDER_PID}" 2>/dev/null || true
if [ "${NEG_RC}" -ne 0 ] && grep -qi "ResourceExhausted" <<< "${NEG_OUT}"; then
    log_pass "区间耗尽被拒绝（ResourceExhausted，外部占用 ${XRANGE_BASE}-$((XRANGE_BASE + 1))）"
elif [ "${NEG_RC}" -ne 0 ]; then
    log_fail "区间耗尽创建失败但错误码非 ResourceExhausted: ${NEG_OUT}"
else
    log_fail "区间耗尽竟创建成功（应 ResourceExhausted，bindProbe 失效？）"
    ${CRICTL} rmp -f "${NEG_OUT}" >/dev/null 2>&1 || true
fi

log_step "17b.5 [断言21] 负向：malformed（49983:80 撞保留端口）→ InvalidArgument"
prepare_direct_pod_json "e2b35hbad" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "e2b35hbad${TS}" '{
    "e2b.dev/sandbox-id": $sid,
    "e2b.dev/expose-ports": "49983:80"}')" || exit 1
NEG_OUT=$(runp_expect_fail "${POD_JSON}") && NEG_RC=0 || NEG_RC=$?
if [ "${NEG_RC}" -ne 0 ] && grep -qi "InvalidArgument" <<< "${NEG_OUT}"; then
    log_pass "malformed expose-ports 被拒绝（InvalidArgument）"
elif [ "${NEG_RC}" -ne 0 ]; then
    log_fail "malformed 创建失败但错误码非 InvalidArgument: ${NEG_OUT}"
else
    log_fail "malformed expose-ports 竟创建成功（应 InvalidArgument）"
    ${CRICTL} rmp -f "${NEG_OUT}" >/dev/null 2>&1 || true
fi

#==================== 18. 断言14：管理面回归 ====================#
log_step "18.1 [断言14] 管理面访问：envd /health 经 HostIP 可达"
if [ -n "${HOSTIP_A}" ]; then
    CODE=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 5 "http://${HOSTIP_A}:${ENVD_PORT}/health" 2>/dev/null || true)
    if [ "${CODE}" = "200" ] || [ "${CODE}" = "204" ]; then
        log_pass "envd 管理面可达（http://${HOSTIP_A}:${ENVD_PORT}/health → ${CODE}）"
    else
        log_fail "envd 管理面不可达（HTTP ${CODE}）"
    fi
else
    log_fail "沙箱 A HostIP 为空，无法验证管理面"
fi
if [ "${GUEST_HAS_CURL}" = "1" ]; then
    log_step "18.2 [断言14] guest → ${ORCH_IP_IN_SANDBOX}（管理面）豁免导流"
    guest_exec "${CID_A}" "curl -sS -o /dev/null --connect-timeout 3 --max-time 5 http://${ORCH_IP_IN_SANDBOX}:80/ ; true" >/dev/null
    sleep 2
    if proxy_log_absent "${SBX_A}" "${ORCH_IP_IN_SANDBOX}"; then
        log_pass "管理面流量被 netns 豁免规则 RETURN（代理日志无 ${ORCH_IP_IN_SANDBOX} flow）"
    else
        log_fail "管理面流量疑似经过代理（代理日志出现 ${ORCH_IP_IN_SANDBOX}）"
    fi
fi

#==================== 19. 断言11：kill 代理 → fail-close → 监督重启恢复 ====================#
if [ "${GUEST_HAS_CURL}" = "1" ] && [ -n "${NS_A}" ]; then
    log_step "19.1 [断言11] kill 代理进程 → 沙箱出向 fail-close → 监督拉起后恢复"
    PIDS_A=$(proxy_pid_in_netns "${NS_A}" || true)
    PID_A=$(head -1 <<< "${PIDS_A}")
    if [ -z "${PID_A}" ]; then
        log_fail "未找到沙箱 A 代理进程，无法执行 kill 测试"
    else
        # mitmdump 是 PyInstaller 双进程（bootloader 父 + python 子，同进程组）：
        # 只杀一个另一个仍持有 15001 监听 → 必须全部 kill -9 才是真实的崩溃场景
        log_info "沙箱 A 代理进程组：$(tr '\n' ' ' <<< "${PIDS_A}")"
        for p in ${PIDS_A}; do kill -9 "${p}" 2>/dev/null || true; done
        # 立即测试（无 sleep）：监督 backoff(1s)+mitmdump spawn(数秒) 期间 15001 无监听，
        # REDIRECT 目标不存在 → RST → curl 失败。sleep 会错过该窗口（重启完成则 curl 成功，误判 fail-close 破坏）。
        OUT=$(guest_exec "${CID_A}" "curl -sS -o /dev/null --resolve ${DOM_INT}:${INT_HTTP_PORT}:${SITE_IP} http://${DOM_INT}:${INT_HTTP_PORT}/ --connect-timeout 3 --max-time 4 ; echo RC=\$?")
        if grep -q "RC=[1-9]" <<< "${OUT}"; then
            log_pass "代理被杀后出向 fail-close（curl 失败，REDIRECT 无监听 → RST）"
        else
            log_fail "代理被杀后出向仍可用（fail-close 语义破坏）"
        fi
        if NEW_PID_A=$(wait_proxy_pid "${NS_A}" 120 "${PID_A}"); then
            log_pass "监督协程已拉起新代理进程（pid=${NEW_PID_A}）"
            # 恢复检查用 internal 目标（直连路径，不受 external 上游/证书验证影响）
            RECOVERED=0
            for i in $(seq 1 20); do
                CODE=$(guest_exec "${CID_A}" "curl -sS -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --resolve ${DOM_INT}:${INT_HTTP_PORT}:${SITE_IP} http://${DOM_INT}:${INT_HTTP_PORT}/ --connect-timeout 4 --max-time 8" | extract_http_code)
                [ "${CODE}" = "200" ] && { RECOVERED=1; break; }
                sleep 3
            done
            if [ "${RECOVERED}" = "1" ]; then
                log_pass "代理重启后出向恢复（规则无需重装，~$((i*3))s）"
            else
                log_fail "代理重启后 60s 内出向未恢复"
            fi
        else
            log_fail "120s 内监督协程未拉起新代理（SANDBOX_PROXY_RESTART=on-crash 失效？）"
        fi
    fi
fi

#==================== 20. 断言16：ready 模式创建耗时回归 ====================#
log_step "20.1 [断言16] ready 模式创建耗时对比（off=${T_CREATE_C}ms vs per-sandbox=${T_CREATE_D}ms）"
# ready 语义：spawn/就绪探测/装规则全部后台异步，不进创建关键路径。
# 以同批次 off 沙箱 C 为基线（两者均非首个沙箱，规避模板预热一阶偏移）；
# 精确分位数基准请对照 32/33 号脚本（[PerfTrace] 口径）。
if [ "${T_CREATE_C}" -gt 0 ] && [ "${T_CREATE_D}" -gt 0 ]; then
    DELTA=$((T_CREATE_D - T_CREATE_C))
    if [ "${DELTA}" -le "${CREATE_SKEW_MS}" ]; then
        log_pass "per-sandbox 创建增量 ${DELTA}ms ≤ 阈值 ${CREATE_SKEW_MS}ms（代理未进创建关键路径）"
    else
        log_fail "per-sandbox 创建增量 ${DELTA}ms 超阈值 ${CREATE_SKEW_MS}ms（ready 异步语义疑似破坏）"
    fi
else
    log_skip "缺少创建耗时基线（C=${T_CREATE_C}ms D=${T_CREATE_D}ms）"
fi
# 打点观测（O7）：orchestrator 日志 proxy_*_ms，软断言（仅记录）
PROXY_TS=$(kubectl -n "${ORCH_NS}" logs "$(tm_pod)" --tail=3000 2>/dev/null \
    | grep -oE "proxy_(spawn|ready|rules)_ms=[0-9]+" | tail -3 || true)
[ -n "${PROXY_TS}" ] && log_info "orchestrator 代理打点: $(tr '\n' ' ' <<< "${PROXY_TS}")"

#==================== 21. 断言15：删除沙箱 → 资源回收 ====================#
log_step "21.1 [断言15] 删除沙箱 A：代理进程 / netns / 规则回收"
PID_A=$(proxy_pid_in_netns "${NS_A}" | head -1 || true)
${CRICTL} rmp -f "${POD_A_ID}" >/dev/null 2>&1 || true
wait_cri_pod_absent "${POD_A_ID}" 30 && log_pass "沙箱 A 已删除" || log_fail "沙箱 A 删除超时"
POD_A_ID=""
GONE=0
for i in $(seq 1 15); do
    if [ -z "${PID_A}" ] || ! kill -0 "${PID_A}" 2>/dev/null; then GONE=1; break; fi
    sleep 2
done
[ "${GONE}" = "1" ] && log_pass "代理进程已随删除收割（pid=${PID_A:-未知} 不存在）" \
    || { log_fail "删除 30s 后代理进程仍存活（pid=${PID_A}，孤儿泄漏）"
         # 诊断：orchestrator 日志里该代理服务的停止/重启痕迹 + 代理自身日志 + 进程状态
         kubectl -n "${ORCH_NS}" logs "$(tm_pod)" --tail=6000 2>/dev/null \
             | grep -i "egress-proxy-${SBX_A}" | tail -15 \
             | while read -r l; do log_info "  orch: ${l}"; done
         proxy_log_dump "${SBX_A}" | tail -12 | while read -r l; do log_info "  proxyA: ${l}"; done
         ps -eo pid,ppid,pgid,stat,cmd 2>/dev/null | grep -E 'mitmdump' | grep -v grep \
             | while read -r l; do log_info "  ps: ${l}"; done; }
if [ -n "${NS_A}" ]; then
    if [ -e "${CNI_NETNS_DIR}/${NS_A}" ]; then
        # 代理 slot 池化复用（§17）：netns 按设计随代理子池留存，导流链
        # （E2B_EGRESS_PROXY）由归还路径异步剥离——轮询等待而非单次检查
        if wait_ns_redirect "${NS_A}" absent 30; then
            log_pass "netns ${NS_A} 随 slot 池留存（设计行为），E2B_EGRESS_PROXY 导流链已清除"
        else
            log_fail "netns ${NS_A} 池化保留但 30s 后导流规则仍残留"
        fi
    else
        log_pass "netns ${NS_A} 已删除（规则随之消失，宿主无残留）"
    fi
    [ -z "$(proxy_pid_in_netns "${NS_A}" 2>/dev/null)" ] && log_pass "${NS_A} 内无残留代理进程" \
        || log_fail "${NS_A} 内仍有代理进程"
fi

log_step "21.2 [断言15] 删除其余沙箱（B/C/D/E/F）并检查孤儿进程"
for pid in "${POD_B_ID}" "${POD_C_ID}" "${POD_D_ID}" "${POD_E_ID}" "${POD_F_ID}"; do
    [ -n "${pid}" ] && ${CRICTL} rmp -f "${pid}" >/dev/null 2>&1 || true
done
POD_B_ID="" POD_C_ID="" POD_D_ID="" POD_E_ID="" POD_F_ID=""
# 代理收割挂在 orchestrator 异步 Delete 的 cleanup 链上（LIFO，晚于 CRI 返回），
# 固定 sleep 在高负载下不可靠——轮询直到 mitmdump 进程数回落到基线
ORPHAN_WAIT_OK=0
for i in $(seq 1 30); do
    NOW_MITM_COUNT=$(pgrep -fc mitmdump 2>/dev/null || true)
    if [ "${NOW_MITM_COUNT}" -le "${BASE_MITM_COUNT}" ]; then ORPHAN_WAIT_OK=1; break; fi
    sleep 2
done
NOW_MITM_COUNT=$(pgrep -fc mitmdump 2>/dev/null || true)
if [ "${ORPHAN_WAIT_OK}" = "1" ]; then
    log_pass "无孤儿 mitmdump 进程（基线 ${BASE_MITM_COUNT} → 当前 ${NOW_MITM_COUNT}）"
else
    log_fail "60s 后 mitmdump 进程数 ${BASE_MITM_COUNT} → ${NOW_MITM_COUNT}，存在孤儿: $(pgrep -af mitmdump || true)"
fi

#==================== 22. 断言18：SANDBOX_PROXY_APPLY=immediate（创建路径同步导流） ====================#
# 滚动重启会销毁在测沙箱，必须放在 §21 全部用例沙箱删除之后
log_step "22.1 [断言18] 切 SANDBOX_PROXY_APPLY=immediate 并等待滚动"
orch_apply_env "SANDBOX_PROXY_APPLY=immediate" || exit 1
# 滚动后 pod 容器文件系统重置：重推 CA 材料与 MITMPROXY_CONFIG
kubectl -n "${ORCH_NS}" exec "$(tm_pod)" -- mkdir -p "${CA_DIR_POD}" "${LOG_DIR_POD}" >&2
push_pod_file "${CA_STAGE}/mitmproxy-ca.pem" "${CA_DIR_POD}/mitmproxy-ca.pem" \
    && push_pod_file "${CA_STAGE}/mitmproxy-ca-cert.pem" "${CA_DIR_POD}/mitmproxy-ca-cert.pem" \
    && push_pod_file "${CA_STAGE}/mitmproxy35.json" "${MITM_JSON_POD}" \
    && log_pass "CA（cert+key）与 MITMPROXY_CONFIG 已重推入 pod" \
    || { log_fail "CA 重推失败"; exit 1; }

log_step "22.2 [断言18] 创建沙箱 G（immediate：runp 返回即导流，零等待单次检查）"
prepare_direct_pod_json "e2b35g" "${BASE_POD_JSON}" || exit 1
inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "${SBX_G}" --arg mis "${MIS_G}" '{
    "e2b.dev/sandbox-id": $sid,
    "cri-multiplex.dev/sandbox-mis": $mis}')" || exit 1
T0=${EPOCHSECONDS}
POD_G_ID=$(run_pod_sandbox) || { log_fail "沙箱 G RunPodSandbox 失败"; print_summary; exit 1; }
log_pass "沙箱 G 创建成功: ${POD_G_ID}（耗时 $((EPOCHSECONDS - T0))s，immediate 含同步导流）"
NS_G=$(slot_ns_of "${POD_G_ID}") || { log_fail "无法反推沙箱 G 的 slot netns"; print_summary; exit 1; }
# immediate 语义：spawn/装规则在 CreateNetwork 事务内同步完成——单次检查，不轮询；
# 与 §6.2 ready 模式的 wait_ns_redirect 轮询形成语义对照
if ns_has_redirect "${NS_G}"; then
    log_pass "runp 返回时 ${NS_G} REDIRECT 规则已在（immediate 同步语义，零等待单次检查）"
else
    log_fail "runp 返回时 ${NS_G} 无 REDIRECT 规则（immediate 同步语义破坏）"
fi
PID_G=$(proxy_pid_in_netns "${NS_G}" | head -1 || true)
[ -n "${PID_G}" ] && log_pass "runp 返回时代理进程已在（pid=${PID_G}）" \
    || log_fail "runp 返回时 ${NS_G} 内无代理进程（immediate 同步语义破坏）"
if check_ns_proxy_rules "${NS_G}"; then
    log_pass "${NS_G} 规则齐全且顺序正确（immediate）"
else
    log_fail "${NS_G} 规则不完整或顺序错误: $(ns_nat_prerouting "${NS_G}" | tr '\n' ' ')"
fi

log_step "22.3 [断言18] immediate 沙箱出向立即可用（内网 mock 200 + 代理 flow）"
CID_G=$(create_and_start_container "${POD_G_ID}") || { log_fail "沙箱 G 容器启动失败"; print_summary; exit 1; }
CODE=$(guest_exec "${CID_G}" "curl -sS -o /dev/null -w '\nE2B35_CODE:%{http_code}\n' --connect-timeout 5 --max-time 8 --resolve ${DOM_INT}:${INT_HTTP_PORT}:${SITE_IP} http://${DOM_INT}:${INT_HTTP_PORT}/" | extract_http_code)
if [ "${CODE}" = "200" ] && wait_proxy_log "${SBX_G}" "${DOM_INT}" 10; then
    log_pass "immediate 沙箱出向立即可用（HTTP 200 + 代理日志 flow）"
else
    log_fail "immediate 沙箱出向异常（code=${CODE}）"
fi

log_step "22.4 [断言18] 删除沙箱 G → 代理收割"
PID_G=$(proxy_pid_in_netns "${NS_G}" | head -1 || true)
${CRICTL} rmp -f "${POD_G_ID}" >/dev/null 2>&1 || true
wait_cri_pod_absent "${POD_G_ID}" 30 && log_pass "沙箱 G 已删除" || log_fail "沙箱 G 删除超时"
POD_G_ID=""
GONE=0
for i in $(seq 1 15); do
    if [ -z "${PID_G}" ] || ! kill -0 "${PID_G}" 2>/dev/null; then GONE=1; break; fi
    sleep 2
done
[ "${GONE}" = "1" ] && log_pass "沙箱 G 代理进程已随删除收割" \
    || log_fail "沙箱 G 删除 30s 后代理进程仍存活（pid=${PID_G}）"
# 恢复 ready 基线交由 cleanup_35 的 orch_restore_baseline 统一兜底

#==================== 收尾 ====================#
trap - EXIT
cleanup_35
print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    exit 0
fi
exit 1
