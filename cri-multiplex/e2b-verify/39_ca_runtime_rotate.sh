#!/bin/bash
###############################################################################
# 39_ca_runtime_rotate.sh — per-sandbox 代理 CA 运行时轮转端到端验证
# （SANDBOX_PROXY_CA_AUTO=true 代际化布局下的 ROTATE 触发文件 / SIGHUP 轮转、
#   回滚、代际 GC 与存量沙箱隔离；设计文档
#   《E2B原生网络沙箱按 netns 起 per-sandbox 代理进程详细设计与实现步骤.md》§7.5.7）
#
# ⚠️ 与 37 号脚本同款 DS 自管理模式：本脚本通过 kubectl set env 修改
#    template-manager DaemonSet 的 SANDBOX_PROXY_* / SANDBOX_PROXY_CA_AUTO env
#    并等待滚动重启，结束（含失败兜底）时恢复基线 env 并再次等待滚动完成。
#    滚动重启会销毁该节点全部现存 E2B 沙箱，仅供专用测试节点运行。
#    不注册进 run_all.sh，需单独执行：
#        bash 39_ca_runtime_rotate.sh
#
# 断言清单（§7.5.7 逐条对应，机读 PASS/FAIL）：
#   1. 空 confdir 起步滚动 → current symlink 与 gen-1 目录生成（记录指纹 FP1）
#   2. 沙箱 A（MITM on）：guest 注入 CA 指纹=FP1，MITM 请求 200，
#      伪造落叶证书 issuer=自动 CA 且宿主侧 verify 经 gen-1 cert 通过
#   3. touch ROTATE → rotate-status.json success → current 切换、FP2≠FP1、
#      status.new_fingerprint 与 openssl DER 指纹口径一致、previous→gen-1、
#      gen-1 目录保留
#   4. 沙箱 B：注入/签发指纹=FP2（轮转只影响新建沙箱）
#   5. 沙箱 A 未删：重发 MITM 请求仍 200，落叶证书经 gen-1 verify 通过、
#      经 gen-2 verify 失败（存量沙箱持有旧 CA 不受轮转影响）
#   6. 删除 A → 再次 ROTATE（gen-3）→ gen-1 被 GC（引用已释放且非
#      current/previous）、gen-2 保留（B 持有 + previous）、B 仍可用（=FP2）
#   7. 回滚：echo <gen-2> > ROTATE → status rollback=true、current 指回 gen-2、
#      previous→gen-3 → 沙箱 C 指纹=FP2
#   8. SIGHUP 兜底路径：pod 内向 orchestrator 进程发 SIGHUP → 新轮转成功
#      （gen-4 成为 current、previous→gen-2、无引用的 gen-3 被 GC）。
#      注意：本节点 template-manager 以 hostPID=true 运行，容器内 PID 1 是
#      宿主 systemd 而非 orchestrator，故 SIGHUP 目标用 pgrep -x orchestrator
#      动态定位，不能写死 kill -HUP 1
#   9. rollout restart → current 指向与指纹不变（不重新生成）；watcher 仍工作
#      （再 touch ROTATE 能轮转）
#  10. 负向：echo current > ROTATE 被拒绝（success=false、current 不动、
#      gen 目录一个不删）
#  11. 清理全部测试沙箱，恢复 DS 基线 env 与 confdir 基线
#
# 指纹口径：一律 DER 字节 SHA-256（openssl x509 -fingerprint -sha256），与
#   rotate-status.json / orchestrator 日志对账口径一致；不是 PEM 字节哈希。
# GC 语义（§7.5.5）：保留 current + previous（两个 symlink 显式标记）+
#   进程内引用计数 >0 的代际，其余整目录删除；只在启动（main.go）与
#   轮转/回滚成功后触发。
#
# 与 37 的差异说明：
#   - SANDBOX_PROXY_EXTRA_ARGS 的 ssl_verify_upstream_trusted_ca 指向脚本自维护的
#     ${CONFDIR}/upstream-ca-bundle.pem（轮转后追加新代际 cert 的累积 bundle），
#     而非 current/ 下单 cert：per-sandbox mitmdump 只在 spawn 时读一次该文件，
#     若指向 current/ 单 cert，轮转后旧沙箱（持旧 CA）对新签发站点证书的上游
#     校验会失败；bundle 让各代际沙箱的上游校验全程可用且站点无需重启。
#   - mock 站点证书用 gen-1 CA 签发一次，全程不换（bundle 始终含 gen-1）。
#
# mock 组件（本脚本自建自清理，37 同款最简形态）：
#   独立 netns e2b39-mockext 内的外网 HTTPS mock 站点（198.51.100.12:39282），
#   mock 链路使用 198.51.100.0/24（TEST-NET-2，避开 RFC1918 防火墙预置 deny）。
###############################################################################
# 与既有 e2b-verify 用例同风格使用 "[ 条件 ] && log_pass ... || log_fail ..."
# 断言惯用法；log_pass/log_fail 恒返回 0，该用法是安全的，故对 SC2015 整文件豁免。
# shellcheck disable=SC2015
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "39 — per-sandbox 代理 CA 运行时轮转（ROTATE / SIGHUP / 回滚 / 代际 GC）"

#==================== 配置 ====================#
ORCH_NS="${ORCH_NS:-e2b}"
ORCH_DS="${ORCH_DS:-template-manager}"
ORCH_DS_SELECTOR="${ORCH_DS_SELECTOR:-app=template-manager}"

CA_DIR_HOST="${E2B39_CA_DIR_HOST:-/var/lib/cri-multiplex/egress-ca}"   # 宿主侧 confdir（hostPath 挂载，pod 内同路径）
CA_DIR_POD="${E2B39_CA_DIR_POD:-${CA_DIR_HOST}}"                       # SANDBOX_PROXY_CONFDIR（容器内路径）
MITM_JSON_NAME="mitmproxy39.json"                                      # MITMPROXY_CONFIG（confdir 内，hostPath 共享）
BUNDLE_NAME="upstream-ca-bundle.pem"                                   # 脚本自维护的上游校验 CA bundle（confdir 内）
STATUS_NAME="rotate-status.json"
LOG_DIR_POD="${E2B39_LOG_DIR_POD:-/var/log/e2b35-proxy}"               # SANDBOX_PROXY_LOG_DIR（DS 已 hostPath 挂载，宿主可读代理日志）

MOCK_NS="e2b39-mockext"
LINK_H="veth39h"
LINK_P="veth39p"
MOCK_HOST_IP="198.51.100.11"            # 宿主侧链路地址
SITE_IP="198.51.100.12"                 # mock 站点地址
EXT_HTTPS_PORT=39282                    # 外网 mock HTTPS
DOM_EXT="ext39.test.local"              # 外网（header_whitelist → MITM）

WORK="/tmp/e2b39"
SITES_LOG="${WORK}/sites.log"

TS="$(date +%s)$RANDOM"
SBX_A="e2b39a${TS}"
SBX_B="e2b39b${TS}"
SBX_C="e2b39c${TS}"
MIS_A="mis-a-${TS}"
MIS_B="mis-b-${TS}"
MIS_C="mis-c-${TS}"

MANAGED_KEYS=(
    SANDBOX_EGRESS_PROXY_MODE SANDBOX_PROXY_APPLY SANDBOX_PROXY_CA_AUTO
    SANDBOX_PROXY_CONFDIR SANDBOX_PROXY_LOG_DIR SANDBOX_PROXY_EXTRA_ARGS
    MITMPROXY_CONFIG
)

#==================== 状态 ====================#
declare -A BASE_ENV=()
POD_A_ID="" POD_B_ID="" POD_C_ID=""
CID_A="" CID_B="" CID_C=""
ORCH_TOUCHED=0
MUX_RESTARTED=0
SITES_PID=""
CA_BACKED_UP=0
CONFDIR_TOUCHED=0        # confdir 已被本脚本改动标记：备份完成且即将清空前置位；
                         # cleanup 仅在置位后才碰 confdir（trap 注册后、备份完成前
                         # 任何 exit 都不会误删 confdir 里的 CA 私钥）
GEN1="" GEN2="" GEN3="" GEN4=""
FP1="" FP2="" FP4=""
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

# 解析 current symlink 指向的 gen 目录（宿主视角；失败返回空串）
ca_gen_dir() {
    readlink -f "${CA_DIR_HOST}/current" 2>/dev/null || true
}

ca_gen_name() {
    local d
    d=$(ca_gen_dir)
    [ -n "${d}" ] && basename "${d}" || true
}

# 解析 previous symlink 指向的 gen 目录名（不存在返回空串）
ca_previous_name() {
    readlink "${CA_DIR_HOST}/previous" 2>/dev/null || true
}

# 从 openssl -fingerprint 输出中提取纯 hex 指纹（小写无冒号）
fp_normalize() {
    sed 's/.*=//' | tr -d ':' | tr 'A-F' 'a-f' | grep -oE '[0-9a-f]{64}' | head -1
}

# current 指向的纯 cert 的 DER SHA-256 指纹，与 openssl x509 -fingerprint -sha256
# / rotate-status.json 的 new_fingerprint 同口径
ca_current_fp() {
    openssl x509 -in "${CA_DIR_HOST}/current/mitmproxy-ca-cert.pem" \
        -noout -fingerprint -sha256 2>/dev/null | fp_normalize
}

# 列出 confdir 内全部 gen 目录名（字典序输出，仅用于展示/对比清单）
ca_list_gens() {
    ls -d "${CA_DIR_HOST}"/gen-* 2>/dev/null | xargs -rn1 basename 2>/dev/null | sort || true
}

# 清空 confdir 全部内容
ca_confdir_clear() {
    find "${CA_DIR_HOST}" -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true
}

# 把 current 代际 cert 追加进上游校验 bundle（轮转后调用，保持各代际 mitmdump
# 上游校验全程可用——见头部「与 37 的差异说明」）
bundle_add_current() {
    cat "${CA_DIR_HOST}/current/mitmproxy-ca-cert.pem" >> "${CA_DIR_HOST}/${BUNDLE_NAME}"
}

# 触发一次轮转并等待 rotate-status.json 落定（先删旧 status 再触发，避免读到
# 上一轮残留）。<target: ""=正常轮转 | gen 目录名=回滚> <timeout秒>；成功时
# 回显 status JSON，超时返回非 0。
trigger_rotate() {
    local target="$1" timeout="${2:-30}" i
    rm -f "${CA_DIR_HOST}/${STATUS_NAME}"
    if [ -n "${target}" ]; then
        # 带内容的回滚触发：同目录 tmp + mv 原子写入，消除 watcher 2s 轮询
        # 读到空/半截内容的窗口（touch 空文件本身原子，无需此处理）
        local tmp="${CA_DIR_HOST}/.ROTATE.tmp-$$"
        echo "${target}" > "${tmp}" \
            && mv "${tmp}" "${CA_DIR_HOST}/ROTATE" \
            || { log_info "写 ROTATE 触发文件失败"; return 1; }
    else
        : > "${CA_DIR_HOST}/ROTATE"
    fi
    for i in $(seq 1 "${timeout}"); do
        if [ -s "${CA_DIR_HOST}/${STATUS_NAME}" ]; then
            cat "${CA_DIR_HOST}/${STATUS_NAME}"
            return 0
        fi
        sleep 1
    done
    log_info "等待 rotate-status.json 超时（${timeout}s），orchestrator 日志尾部："
    kubectl -n "${ORCH_NS}" logs "$(tm_pod)" --tail=30 2>/dev/null | grep -i -E "proxy CA|rotat" || true
    return 1
}

# 从 guest 抓取 MITM 伪造的落叶证书 PEM（s_client 输出的第一个 cert 块）
fetch_leaf() { # <container_id> <out-file>
    guest_exec "$1" "printf 'GET / HTTP/1.1\r\nHost: ${DOM_EXT}\r\nConnection: close\r\n\r\n' | openssl s_client -connect ${SITE_IP}:${EXT_HTTPS_PORT} -servername ${DOM_EXT} 2>/dev/null" \
        | awk '/-----BEGIN CERTIFICATE-----/{f=1} f{print} /-----END CERTIFICATE-----/{exit}' > "$2"
    [ -s "$2" ]
}

# 沙箱 CA 断言组：<label> <container_id> <期望 DER 指纹> [期望 gen 目录（宿主侧，
# 做落叶证书 verify 对账；缺省跳过）]
assert_sandbox_fp() {
    local label="$1" cid="$2" exp_fp="$3" gen_dir="${4:-}"
    local gfp code out leaf="${WORK}/leaf-${label}.pem"

    gfp=$(guest_exec "${cid}" "openssl x509 -in /usr/local/share/ca-certificates/e2b-egress-ca.crt -noout -fingerprint -sha256 2>/dev/null" \
        | fp_normalize)
    if [ -n "${exp_fp}" ] && [ "${gfp}" = "${exp_fp}" ]; then
        log_pass "${label}: guest 注入 CA 指纹匹配期望代际（${exp_fp:0:16}…）"
    else
        log_fail "${label}: guest 注入 CA 指纹不符（expect=${exp_fp:-<空>} actual=${gfp:-<空>}）"
    fi

    code=$(guest_exec "${cid}" "curl -sS -o /dev/null -w '\nE2B39_CODE:%{http_code}\n' --connect-timeout 5 --max-time 15 --resolve ${DOM_EXT}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT}:${EXT_HTTPS_PORT}/" \
        | grep -oP 'E2B39_CODE:\K[0-9]{3}' | tail -1)
    if [ "${code}" = "200" ]; then
        log_pass "${label}: MITM 请求成功（系统信任库直连 → 200，伪造证书被信任）"
    else
        log_fail "${label}: MITM 请求失败（code=${code:-<空>}）"
        log_info "${label} guest 侧 curl -v 详情："
        guest_exec "${cid}" "curl -v --connect-timeout 5 --max-time 15 --resolve ${DOM_EXT}:${EXT_HTTPS_PORT}:${SITE_IP} https://${DOM_EXT}:${EXT_HTTPS_PORT}/ 2>&1 | tail -20" || true
    fi

    out=$(guest_exec "${cid}" "printf 'GET / HTTP/1.1\r\nHost: ${DOM_EXT}\r\nConnection: close\r\n\r\n' | openssl s_client -connect ${SITE_IP}:${EXT_HTTPS_PORT} -servername ${DOM_EXT} 2>/dev/null | openssl x509 -noout -issuer -subject 2>/dev/null || true")
    if grep -q "issuer.*e2b-egress-auto-ca" <<< "${out}" && grep -q "subject.*CN *= *${DOM_EXT}" <<< "${out}"; then
        log_pass "${label}: 落叶证书 issuer=e2b-egress-auto-ca、subject CN=${DOM_EXT}"
    else
        log_fail "${label}: 落叶证书不符预期: ${out}"
    fi

    if [ -n "${gen_dir}" ] && [ -d "${gen_dir}" ]; then
        if fetch_leaf "${cid}" "${leaf}"; then
            if openssl verify -CAfile "${gen_dir}/mitmproxy-ca-cert.pem" "${leaf}" >/dev/null 2>&1; then
                log_pass "${label}: 宿主侧 verify 通过——落叶证书确由 $(basename "${gen_dir}") CA 签发"
            else
                log_fail "${label}: 落叶证书 verify 失败（CA=$(basename "${gen_dir}")）: $(openssl verify -CAfile "${gen_dir}/mitmproxy-ca-cert.pem" "${leaf}" 2>&1)"
            fi
        else
            log_fail "${label}: 抓取落叶证书失败"
        fi
    fi
}

# 创建 MITM 全开沙箱（身份注解 + egress-profile=internal，mitm 缺省 true）；
# 回显 "<pod_id> <container_id>"，失败返回非 0。
create_mitm_sandbox() { # <name-prefix> <sandbox-id> <mis>
    prepare_direct_pod_json "$1" "${BASE_POD_JSON}" || return 1
    inject_annotations "${POD_JSON}" "$(jq -nc --arg sid "$2" --arg mis "$3" '{
        "e2b.dev/sandbox-id": $sid,
        "cri-multiplex.dev/sandbox-mis": $mis,
        "cri-multiplex.dev/egress-profile": "internal"}')" || return 1
    local pid cid
    pid=$(run_pod_sandbox) || return 1
    cid=$(create_and_start_container "${pid}") || { ${CRICTL} rmp -f "${pid}" >/dev/null 2>&1 || true; return 1; }
    echo "${pid} ${cid}"
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
    # 直接 exec 会命中 terminating/not-ready pod 秒败
    kubectl -n "${ORCH_NS}" wait --for=condition=ready pod -l "${ORCH_DS_SELECTOR}" \
        --timeout=120s >/dev/null 2>&1 || true
    local k
    for k in "${MANAGED_KEYS[@]}"; do
        BASE_ENV[$k]=$(ds_env_get "${k}")
    done
    log_pass "已捕获 DaemonSet 基线 env（${#MANAGED_KEYS[@]} 个受管键）"

    # 特性 marker：镜像二进制内嵌 rotate-status.json 字符串（缺失说明镜像过旧，
    # 运行时轮转特性未编入）
    local pod mark
    pod=$(tm_pod)
    [ -n "${pod}" ] || { log_fail "未找到 ${ORCH_DS_SELECTOR} pod"; return 1; }
    mark=$(kubectl -n "${ORCH_NS}" exec "${pod}" -- sh -c \
        'grep -ac "rotate-status.json" /usr/bin/orchestrator 2>/dev/null || echo 0' \
        2>/dev/null | tr -d '[:space:]' || true)
    if [ "${mark:-0}" -ge 1 ]; then
        log_pass "orchestrator 镜像含运行时 CA 轮转特性（pod=${pod}）"
    else
        log_fail "orchestrator 镜像不含运行时轮转特性（rotate-status.json marker 缺失），请先更新 ${ORCH_DS} DaemonSet 镜像"
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
        E2B_FORCE_RESTART=1 bash "${SCRIPT_DIR}/01_start_multiplex.sh" > /tmp/39-restore-mux.log 2>&1 || true
}

#==================== 清理 ====================#
cleanup_39() {
    log_info "清理: 删除用例沙箱 / mock 组件 / mock netns，恢复 confdir 与 DS 基线（幂等）"
    for pid in "${POD_A_ID}" "${POD_B_ID}" "${POD_C_ID}"; do
        [ -n "${pid}" ] && ${CRICTL} rmp -f "${pid}" >/dev/null 2>&1 || true
    done
    [ -n "${SITES_PID}" ] && kill "${SITES_PID}" 2>/dev/null || true
    pkill -f "${WORK}/mock_sites.py" 2>/dev/null || true
    ip netns del "${MOCK_NS}" >/dev/null 2>&1 || true
    ip link del "${LINK_H}" >/dev/null 2>&1 || true
    rm -f /tmp/e2b-pod-e2b39*.json
    # confdir 仅在本脚本改动过（CONFDIR_TOUCHED 置位）后才恢复/清理：
    # trap 注册后、备份完成前任何 exit 都不会碰 confdir（含其中的 CA 私钥）
    if [ "${CONFDIR_TOUCHED}" = "1" ]; then
        if [ "${CA_BACKED_UP}" = "1" ]; then
            ca_confdir_clear
            if cp -a "${WORK}/ca-backup/." "${CA_DIR_HOST}/" 2>/dev/null; then
                log_info "confdir 基线已还原"
            else
                log_fail "confdir 基线还原失败，请手工检查 ${CA_DIR_HOST}（备份在 ${WORK}/ca-backup）"
            fi
        else
            ca_confdir_clear
        fi
    fi
    if [ "${ORCH_TOUCHED}" = "1" ]; then
        orch_restore_baseline && log_info "orchestrator 基线 env 已恢复" \
            || log_info "orchestrator 基线恢复失败，请手工检查 kubectl -n ${ORCH_NS} get ds ${ORCH_DS}"
    fi
    if [ "${MUX_RESTARTED}" = "1" ]; then
        restore_mux_baseline
    fi
}
trap cleanup_39 EXIT

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

#==================== 2. 备份并清空宿主 confdir，开启 CA_AUTO ====================#
log_step "2.1 备份并清空宿主 confdir（${CA_DIR_HOST}）"
mkdir -p "${CA_DIR_HOST}" "${WORK}/ca-backup"
if [ -n "$(ls -A "${CA_DIR_HOST}" 2>/dev/null)" ]; then
    # 备份失败必须直接退出：不能置 CA_BACKED_UP 继续清空（否则无备份删私钥）
    if cp -a "${CA_DIR_HOST}/." "${WORK}/ca-backup/"; then
        CA_BACKED_UP=1
        log_info "已备份既有 confdir 内容到 ${WORK}/ca-backup"
    else
        log_fail "confdir 备份失败，拒绝继续（避免无备份清空 ${CA_DIR_HOST}）"
        exit 1
    fi
fi
CONFDIR_TOUCHED=1
ca_confdir_clear
[ -z "$(ls -A "${CA_DIR_HOST}" 2>/dev/null)" ] \
    && log_pass "宿主 confdir 已清空（空 confdir 起步）" || { log_fail "宿主 confdir 清空失败: $(ls -la "${CA_DIR_HOST}")"; exit 1; }

log_step "2.2 写入 MITMPROXY_CONFIG（header_whitelist=${DOM_EXT}）"
cat > "${CA_DIR_HOST}/${MITM_JSON_NAME}" <<EOF
{
  "internal":         {"hosts": [], "domains": [], "nets": []},
  "header_whitelist": {"hosts": [], "domains": ["${DOM_EXT}"]},
  "header_blacklist": {"hosts": [], "domains": []}
}
EOF
log_pass "MITMPROXY_CONFIG 就绪（${CA_DIR_HOST}/${MITM_JSON_NAME}，hostPath 与 pod 共享）"

log_step "2.3 开启 SANDBOX_PROXY_CA_AUTO=true 并等待滚动（APPLY=ready）"
orch_apply_env \
    "SANDBOX_EGRESS_PROXY_MODE=per-sandbox" \
    "SANDBOX_PROXY_CA_AUTO=true" \
    "SANDBOX_PROXY_APPLY=ready" \
    "SANDBOX_PROXY_CONFDIR=${CA_DIR_POD}" \
    "SANDBOX_PROXY_LOG_DIR=${LOG_DIR_POD}" \
    "SANDBOX_PROXY_EXTRA_ARGS=--set ssl_verify_upstream_trusted_ca=${CA_DIR_POD}/${BUNDLE_NAME}" \
    "MITMPROXY_CONFIG=${CA_DIR_POD}/${MITM_JSON_NAME}" || exit 1

#==================== 3. 断言1：空 confdir 起步 → current + gen-1 生成 ====================#
log_step "3.1 [断言1] 滚动后 current symlink 与 gen-1 目录生成"
GEN1=$(ca_gen_name)
if [ -n "${GEN1}" ] && [[ "${GEN1}" == gen-* ]] \
    && [ -s "${CA_DIR_HOST}/${GEN1}/mitmproxy-ca.pem" ] && [ -s "${CA_DIR_HOST}/${GEN1}/mitmproxy-ca-cert.pem" ]; then
    log_pass "current -> ${GEN1}/ 已建立，两 PEM 齐备"
else
    log_fail "代际化 CA 未自动生成: $(ls -la "${CA_DIR_HOST}" 2>&1 | tr '\n' ' ')"
    print_summary; exit 1
fi
FP1=$(ca_current_fp)
[ -n "${FP1}" ] && log_pass "gen-1 指纹 FP1=${FP1:0:16}…（DER SHA-256 口径）" \
    || { log_fail "FP1 读取失败"; print_summary; exit 1; }

#==================== 4. mock 站点（证书由 gen-1 CA 签发，全程不换） ====================#
log_step "4.1 初始化上游校验 bundle（gen-1 cert）并创建 mock 外部 netns"
cp "${CA_DIR_HOST}/${GEN1}/mitmproxy-ca-cert.pem" "${CA_DIR_HOST}/${BUNDLE_NAME}"
ip netns del "${MOCK_NS}" >/dev/null 2>&1 || true
ip link del "${LINK_H}" >/dev/null 2>&1 || true
ip netns add "${MOCK_NS}" || { log_fail "创建 netns ${MOCK_NS} 失败"; print_summary; exit 1; }
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

log_step "4.2 用 gen-1 CA 签发 mock 站点证书（带 SAN）"
mkdir -p "${WORK}"
cat > "${WORK}/mock-site.ext" <<EOF
subjectAltName=DNS:${DOM_EXT},IP:${SITE_IP}
EOF
openssl req -newkey rsa:2048 -nodes \
    -keyout "${WORK}/mock-site.key" -out "${WORK}/mock-site.csr" \
    -subj "/CN=e2b39-mock-site" >/dev/null 2>&1 \
    && openssl x509 -req -in "${WORK}/mock-site.csr" -days 30 \
        -CA "${CA_DIR_HOST}/${GEN1}/mitmproxy-ca-cert.pem" -CAkey "${CA_DIR_HOST}/${GEN1}/mitmproxy-ca.pem" \
        -CAcreateserial -extfile "${WORK}/mock-site.ext" \
        -out "${WORK}/mock-site.crt" >/dev/null 2>&1 \
    || { log_fail "mock 站点证书签发失败"; print_summary; exit 1; }
log_pass "mock 站点证书已由 gen-1 CA 签发（CN=e2b39-mock-site，SAN=${DOM_EXT}）"

log_step "4.3 启动 mock HTTPS 站点"
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
    kill -0 "${SITES_PID}" 2>/dev/null || { log_fail "mock 站点启动失败"; print_summary; exit 1; }
    CODE=$(curl --noproxy '*' -sk -o /dev/null -w "%{http_code}" --max-time 3 \
        "https://${SITE_IP}:${EXT_HTTPS_PORT}/" 2>/dev/null || true)
    [ "${CODE}" = "200" ] && break
    sleep 2
done
[ "${CODE}" = "200" ] && log_pass "mock HTTPS 站点自检通过（https://${SITE_IP}:${EXT_HTTPS_PORT} → 200）" \
    || { log_fail "mock HTTPS 站点自检失败（60s 内未就绪）"; print_summary; exit 1; }

#==================== 5. 断言2：沙箱 A → 指纹 FP1 ====================#
log_step "5.1 [断言2] 创建沙箱 A（MITM on）→ 签发/注入指纹应为 FP1"
read -r POD_A_ID CID_A <<< "$(create_mitm_sandbox "e2b39a" "${SBX_A}" "${MIS_A}")" \
    || { log_fail "沙箱 A 创建失败"; print_summary; exit 1; }
[ -n "${POD_A_ID}" ] && [ -n "${CID_A}" ] || { log_fail "沙箱 A 创建返回为空"; print_summary; exit 1; }
log_pass "沙箱 A 创建成功: ${POD_A_ID}"
assert_sandbox_fp "A" "${CID_A}" "${FP1}" "${CA_DIR_HOST}/${GEN1}"

#==================== 6. 断言3：touch ROTATE → 轮转 1（gen-2） ====================#
log_step "6.1 [断言3] 宿主 touch ROTATE → 轮转成功、current 切换、gen-1 保留"
STATUS_JSON=$(trigger_rotate "" 30) \
    || { log_fail "ROTATE 触发后 ${STATUS_NAME} 未落定"; print_summary; exit 1; }
echo "${STATUS_JSON}" | jq -e '.success == true' >/dev/null \
    && log_pass "rotate-status.json success=true（gen=$(echo "${STATUS_JSON}" | jq -r .generation)）" \
    || log_fail "rotate-status.json 未记成功: ${STATUS_JSON}"
GEN2=$(ca_gen_name)
FP2=$(ca_current_fp)
[ -n "${GEN2}" ] && [ "${GEN2}" != "${GEN1}" ] \
    && log_pass "current 已切换：${GEN1} -> ${GEN2}" \
    || log_fail "current 未切换（仍为 ${GEN2:-<空>}）"
[ -n "${FP2}" ] && [ "${FP2}" != "${FP1}" ] \
    && log_pass "新指纹 FP2=${FP2:0:16}… ≠ FP1" \
    || log_fail "轮转后指纹未变化（FP=${FP2:-<空>}）"
S_FP=$(echo "${STATUS_JSON}" | jq -r '.new_fingerprint // empty')
[ -n "${S_FP}" ] && [ "${S_FP}" = "${FP2}" ] \
    && log_pass "status.new_fingerprint 与 openssl DER 指纹口径一致（${S_FP:0:16}…）" \
    || log_fail "status.new_fingerprint 与 DER 口径不符（status=${S_FP:-<空>} openssl=${FP2:-<空>}）"
[ "$(ca_previous_name)" = "${GEN1}" ] \
    && log_pass "previous -> ${GEN1}（误轮转回滚余地）" \
    || log_fail "previous 未指向 gen-1（现为 $(ca_previous_name || echo '<空>')）"
[ -d "${CA_DIR_HOST}/${GEN1}" ] \
    && log_pass "gen-1 目录仍保留（previous 保留语义）" \
    || log_fail "gen-1 目录被误删（previous 指向的代际应保留）"
bundle_add_current

#==================== 7. 断言4：沙箱 B → 指纹 FP2 ====================#
log_step "7.1 [断言4] 创建沙箱 B → 签发/注入指纹应为 FP2（轮转只影响新建沙箱）"
read -r POD_B_ID CID_B <<< "$(create_mitm_sandbox "e2b39b" "${SBX_B}" "${MIS_B}")" \
    || { log_fail "沙箱 B 创建失败"; print_summary; exit 1; }
[ -n "${POD_B_ID}" ] && [ -n "${CID_B}" ] || { log_fail "沙箱 B 创建返回为空"; print_summary; exit 1; }
log_pass "沙箱 B 创建成功: ${POD_B_ID}"
assert_sandbox_fp "B" "${CID_B}" "${FP2}" "${CA_DIR_HOST}/${GEN2}"

#==================== 8. 断言5：存量沙箱 A 不受轮转影响 ====================#
log_step "8.1 [断言5] 沙箱 A 重发 MITM 请求 → 指纹仍为 FP1（mitmdump 内存持有旧 CA）"
assert_sandbox_fp "A复测" "${CID_A}" "${FP1}" "${CA_DIR_HOST}/${GEN1}"
if fetch_leaf "${CID_A}" "${WORK}/leaf-a-recheck.pem"; then
    if openssl verify -CAfile "${CA_DIR_HOST}/${GEN2}/mitmproxy-ca-cert.pem" "${WORK}/leaf-a-recheck.pem" >/dev/null 2>&1; then
        log_fail "A复测: 落叶证书竟能被 gen-2 CA 验证（存量沙箱被轮转波及）"
    else
        log_pass "A复测: 落叶证书无法被 gen-2 CA 验证（确为 gen-1 签发，轮转未波及存量）"
    fi
else
    log_fail "A复测: 抓取落叶证书失败"
fi

#==================== 9. 断言6：删 A → 轮转 2（gen-3）→ gen-1 被 GC ====================#
log_step "9.1 [断言6] 删除沙箱 A，等待引用释放"
${CRICTL} rmp -f "${POD_A_ID}" >/dev/null 2>&1 || true
wait_cri_pod_absent "${POD_A_ID}" 60 \
    && log_pass "沙箱 A 已删除（orchestrator 侧不可见，gen-1 引用计数应已释放）" \
    || log_fail "沙箱 A 删除后 60s 仍可见"
POD_A_ID="" CID_A=""
sleep 3   # Delete 异步处理兜底：确保 ReleaseProxyCA 已执行

log_step "9.2 [断言6] 再次 touch ROTATE → gen-3 生成，gen-1 被 GC、gen-2 保留"
STATUS_JSON=$(trigger_rotate "" 30) \
    || { log_fail "轮转 2 后 ${STATUS_NAME} 未落定"; print_summary; exit 1; }
echo "${STATUS_JSON}" | jq -e '.success == true' >/dev/null \
    && log_pass "轮转 2 success=true（gen=$(echo "${STATUS_JSON}" | jq -r .generation)）" \
    || log_fail "轮转 2 未记成功: ${STATUS_JSON}"
GEN3=$(ca_gen_name)
[ -n "${GEN3}" ] && [ "${GEN3}" != "${GEN2}" ] \
    && log_pass "current 已切换：${GEN2} -> ${GEN3}" \
    || log_fail "轮转 2 后 current 未切换（仍为 ${GEN3:-<空>}）"
[ "$(ca_previous_name)" = "${GEN2}" ] \
    && log_pass "previous 已推进：-> ${GEN2}" \
    || log_fail "previous 未推进到 gen-2（现为 $(ca_previous_name || echo '<空>')）"
[ ! -d "${CA_DIR_HOST}/${GEN1}" ] \
    && log_pass "gen-1 目录已被 GC（A 的引用已释放，且非 current/previous）" \
    || log_fail "gen-1 目录未被 GC（引用泄漏？）: $(ca_list_gens | tr '\n' ' ')"
[ -d "${CA_DIR_HOST}/${GEN2}" ] \
    && log_pass "gen-2 目录保留（B 持有引用 + previous 语义）" \
    || log_fail "gen-2 目录被误删（B 仍在用）: $(ca_list_gens | tr '\n' ' ')"
bundle_add_current

log_step "9.3 [断言6] 沙箱 B 复测 → 指纹仍为 FP2"
assert_sandbox_fp "B复测" "${CID_B}" "${FP2}" "${CA_DIR_HOST}/${GEN2}"

#==================== 10. 断言7：回滚到 gen-2 → 沙箱 C 指纹 FP2 ====================#
log_step "10.1 [断言7] echo ${GEN2} > ROTATE（tmp+mv 原子写）→ 回滚成功、current 指回 gen-2"
STATUS_JSON=$(trigger_rotate "${GEN2}" 30) \
    || { log_fail "回滚触发后 ${STATUS_NAME} 未落定"; print_summary; exit 1; }
if echo "${STATUS_JSON}" | jq -e '.success == true and .rollback == true' >/dev/null; then
    log_pass "rotate-status.json success=true 且 rollback=true"
else
    log_fail "回滚 status 不符预期: ${STATUS_JSON}"
fi
[ "$(ca_gen_name)" = "${GEN2}" ] \
    && log_pass "current 已指回 ${GEN2}" \
    || log_fail "current 未指回 ${GEN2}（现为 $(ca_gen_name)）"
[ "$(ca_previous_name)" = "${GEN3}" ] \
    && log_pass "previous 已指向被替换的 ${GEN3}" \
    || log_fail "previous 未指向 gen-3（现为 $(ca_previous_name || echo '<空>')）"

log_step "10.2 [断言7] 创建沙箱 C → 签发/注入指纹应为 FP2"
read -r POD_C_ID CID_C <<< "$(create_mitm_sandbox "e2b39c" "${SBX_C}" "${MIS_C}")" \
    || { log_fail "沙箱 C 创建失败"; print_summary; exit 1; }
[ -n "${POD_C_ID}" ] && [ -n "${CID_C}" ] || { log_fail "沙箱 C 创建返回为空"; print_summary; exit 1; }
log_pass "沙箱 C 创建成功: ${POD_C_ID}"
assert_sandbox_fp "C" "${CID_C}" "${FP2}" "${CA_DIR_HOST}/${GEN2}"

#==================== 11. 断言8：SIGHUP 轮转 ====================#
log_step "11.1 [断言8] pod 内 SIGHUP orchestrator → 新轮转成功（gen-4，gen-3 被 GC）"
rm -f "${CA_DIR_HOST}/${STATUS_NAME}"
# hostPID=true 部署下容器内 PID 1 是宿主 systemd，SIGHUP 目标须动态定位 orchestrator
ORCH_PID=$(kubectl -n "${ORCH_NS}" exec "$(tm_pod)" -- sh -c 'pgrep -x orchestrator | head -1' 2>/dev/null | tr -d '[:space:]' || true)
[ -n "${ORCH_PID}" ] || { log_fail "pod 内未找到 orchestrator 进程"; print_summary; exit 1; }
kubectl -n "${ORCH_NS}" exec "$(tm_pod)" -- kill -HUP "${ORCH_PID}" \
    || { log_fail "SIGHUP 发送失败（pid=${ORCH_PID}）"; print_summary; exit 1; }
SIGHUP_OK=0
for i in $(seq 1 30); do
    if [ -s "${CA_DIR_HOST}/${STATUS_NAME}" ]; then SIGHUP_OK=1; break; fi
    sleep 1
done
if [ "${SIGHUP_OK}" = "1" ] && jq -e '.success == true' "${CA_DIR_HOST}/${STATUS_NAME}" >/dev/null; then
    log_pass "SIGHUP 轮转 success=true（gen=$(jq -r .generation "${CA_DIR_HOST}/${STATUS_NAME}")）"
else
    log_fail "SIGHUP 轮转未成功: $(cat "${CA_DIR_HOST}/${STATUS_NAME}" 2>/dev/null || echo '<status 未生成>')"
fi
GEN4=$(ca_gen_name)
FP4=$(ca_current_fp)
[ -n "${GEN4}" ] && [ "${GEN4}" != "${GEN2}" ] && [ "${GEN4}" != "${GEN3}" ] \
    && log_pass "current 已切换到新代际 ${GEN4}" \
    || log_fail "SIGHUP 后 current 未切换到新代际（现为 ${GEN4:-<空>}）"
# GC 语义（current + previous + 引用计数）：本轮 previous=gen-2（B/C 持有引用），
# gen-3 无引用且非 current/previous → 被回收
[ "$(ca_previous_name)" = "${GEN2}" ] \
    && log_pass "previous -> ${GEN2}（B/C 引用的代际，双重保留）" \
    || log_fail "previous 未指向 gen-2（现为 $(ca_previous_name || echo '<空>')）"
[ -d "${CA_DIR_HOST}/${GEN2}" ] \
    && log_pass "gen-2 目录保留（B/C 引用 + previous）" \
    || log_fail "gen-2 目录被误删: $(ca_list_gens | tr '\n' ' ')"
[ ! -d "${CA_DIR_HOST}/${GEN3}" ] \
    && log_pass "gen-3 目录已被 GC（无引用、非 current/previous）" \
    || log_fail "gen-3 目录未被 GC: $(ca_list_gens | tr '\n' ' ')"
bundle_add_current

#==================== 12. 断言9：滚动重启幂等 + watcher 仍工作 ====================#
log_step "12.1 [断言9] rollout restart → current 指向与指纹不变（不重新生成）"
kubectl -n "${ORCH_NS}" rollout restart "ds/${ORCH_DS}" >&2
orch_wait_ready || { print_summary; exit 1; }
[ "$(ca_gen_name)" = "${GEN4}" ] \
    && log_pass "重启后 current 仍指向 ${GEN4}（幂等，未重新生成）" \
    || log_fail "重启后 current 变化：${GEN4} -> $(ca_gen_name)"
[ "$(ca_current_fp)" = "${FP4}" ] \
    && log_pass "重启后指纹不变（${FP4:0:16}…）" \
    || log_fail "重启后指纹变化"

log_step "12.2 [断言9] 重启后 watcher 仍工作（再 touch ROTATE 能轮转）"
STATUS_JSON=$(trigger_rotate "" 30) \
    || { log_fail "重启后 ROTATE 未落定"; print_summary; exit 1; }
echo "${STATUS_JSON}" | jq -e '.success == true' >/dev/null \
    && log_pass "重启后 watcher 轮转 success=true（gen=$(echo "${STATUS_JSON}" | jq -r .generation)）" \
    || log_fail "重启后 watcher 轮转未成功: ${STATUS_JSON}"

#==================== 12.3 断言10（负向）：非法回滚目标被拒绝 ====================#
log_step "12.3 [断言10] 负向：echo current > ROTATE 被拒绝（不切换、不 GC）"
NEG_CUR_BEFORE=$(ca_gen_name)
NEG_GENS_BEFORE=$(ca_list_gens | tr '\n' ' ')
STATUS_JSON=$(trigger_rotate "current" 30) \
    || { log_fail "负向回滚后 ${STATUS_NAME} 未落定"; print_summary; exit 1; }
if echo "${STATUS_JSON}" | jq -e '.success == false' >/dev/null; then
    log_pass "非法回滚目标被拒绝（success=false，error=$(echo "${STATUS_JSON}" | jq -r .error)）"
else
    log_fail "非法回滚目标竟被接受: ${STATUS_JSON}"
fi
[ "$(ca_gen_name)" = "${NEG_CUR_BEFORE}" ] \
    && log_pass "current 未动（仍 ${NEG_CUR_BEFORE}）" \
    || log_fail "current 被非法回滚改动：${NEG_CUR_BEFORE} -> $(ca_gen_name)"
[ "$(ca_list_gens | tr '\n' ' ')" = "${NEG_GENS_BEFORE}" ] \
    && log_pass "gen 目录一个不删（${NEG_GENS_BEFORE}）" \
    || log_fail "gen 目录集合变化：${NEG_GENS_BEFORE} -> $(ca_list_gens | tr '\n' ' ')"

#==================== 13. 收尾 ====================#
log_step "13.1 清理测试沙箱（B/C；A 已在断言6 删除）"
for pid in "${POD_B_ID}" "${POD_C_ID}"; do
    [ -n "${pid}" ] && ${CRICTL} rmp -f "${pid}" >/dev/null 2>&1 || true
done
POD_B_ID="" POD_C_ID="" CID_B="" CID_C=""
log_pass "测试沙箱已清理（DS 基线 env 与 confdir 由 cleanup 恢复）"

trap - EXIT
cleanup_39
print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    exit 0
fi
exit 1
