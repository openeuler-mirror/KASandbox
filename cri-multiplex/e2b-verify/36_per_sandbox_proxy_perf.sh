#!/usr/bin/env bash
###############################################################################
# 36 — per-sandbox 代理沙箱并发创建性能（SANDBOX_PROXY_APPLY=ready vs immediate）
#
# 测量对象：egress-mode=per-sandbox 的原生模式沙箱并发 RunPodSandbox 端到端耗时
# （ready=代理 spawn/装规则异步不进创建路径；immediate=同步进创建路径，语义对照）。
#
# 与 slot 预热池的关系（重要）：per-sandbox 代理沙箱走**独立的代理 slot 子池**
#（设计文档 §17 池化接入：SANDBOX_PROXY_POOL_SIZE=预热容量（缺省默认 200），
# 预热期即定形代理形态——无 tcpProxy、带 vrt SNAT、全协议防火墙基线；复用池
# 容量沿用 NETWORK_POOL_REUSED_SLOTS_SIZE）。本脚本按**全代理部署口径**测量：
# NETWORK_POOL_NEW_SLOTS_SIZE=0 关掉无用的普通预热池，
# SANDBOX_PROXY_POOL_SIZE=SANDBOX_PROXY_REUSED_POOL_SIZE=E2B36_PROXY_POOL_SIZE
#（默认 200）；每轮先等 proxyNewSlots 预热到满水位，冒烟后**先跑一把等并发
# 暖池轮**（创建即删，把 proxyReusedSlots 填起来，不计入结果），再等
# proxyNewSlots 补回满水位，最后才正式开火测量（正式轮命中的主要是复用池
# slot——连 CreateNetwork 都省掉的纯复用路径）。
#
# 用法：
#   sudo bash 36_per_sandbox_proxy_perf.sh                 # ready + immediate 各一轮，100 并发
#   E2B36_COUNT=50 bash 36_per_sandbox_proxy_perf.sh       # 自定义并发数
#   E2B36_MODES="ready" bash 36_per_sandbox_proxy_perf.sh  # 只测一个模式
#   E2B36_RUNP_TIMEOUT=1800s bash 36_...                   # immediate 高并发可调大单次 runp 超时
#   E2B36_PROXY_POOL_SIZE=200 bash 36_...                  # 代理 slot 子池预热容量（默认 200）
#
# 依赖：crictl/kubectl/jq/python3；orchestrator 镜像含 per-sandbox 代理特性且
# 预置 mitmdump + addon.py；宿主侧 CA 真源 /var/lib/cri-multiplex/egress-ca 已存在
# （缺则自建，与 35 号脚本同一真源）。结束时自动恢复 DaemonSet 基线 env 与
# cri-multiplex 基线（CNI 模式）。
###############################################################################
# shellcheck disable=SC2015
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "36 — per-sandbox 代理沙箱并发创建性能（ready vs immediate）"

#==================== 配置 ====================#
ORCH_NS="${ORCH_NS:-e2b}"
ORCH_DS="${ORCH_DS:-template-manager}"
ORCH_DS_SELECTOR="${ORCH_DS_SELECTOR:-app=template-manager}"

COUNT="${E2B36_COUNT:-100}"                       # 并发沙箱数
MODES="${E2B36_MODES:-ready immediate}"           # 待测 APPLY 模式
RUNP_TIMEOUT="${E2B36_RUNP_TIMEOUT:-1800s}"       # 单次 runp 超时（immediate 高并发需放宽）
PROXY_POOL_SIZE="${E2B36_PROXY_POOL_SIZE:-200}"   # 代理 slot 子池预热容量（全代理部署口径）

WORK="/tmp/e2b36"
CA_STAGE="${WORK}/ca"
CA_DIR_HOST="${E2B36_CA_DIR_HOST:-/var/lib/cri-multiplex/egress-ca}"   # 宿主侧 CA 唯一真源（同 35）
CA_DIR_POD="${E2B36_CA_DIR_POD:-/var/lib/cri-multiplex/egress-ca}"     # SANDBOX_PROXY_CONFDIR
LOG_DIR_POD="${E2B36_LOG_DIR_POD:-/var/log/e2b36-proxy}"               # SANDBOX_PROXY_LOG_DIR
MITM_JSON_POD="${CA_DIR_POD}/mitmproxy36.json"                         # MITMPROXY_CONFIG
PROXY_BINARY_POD="${E2B36_PROXY_BINARY:-/opt/mitmproxy/mitmdump}"
PROXY_ADDON_POD="${E2B36_PROXY_ADDON:-/opt/opensandbox-egress/addon.py}"
PERF_TEMPLATE="${WORK}/perf-pod-template.json"    # 喂给 32 号脚本的 --pod-json

MANAGED_KEYS=(
    SANDBOX_EGRESS_PROXY_MODE SANDBOX_PROXY_APPLY SANDBOX_PROXY_UPSTREAM
    SANDBOX_PROXY_EXEMPT_CIDRS SANDBOX_PROXY_CONFDIR SANDBOX_PROXY_LOG_DIR
    SANDBOX_PROXY_EXTRA_ARGS SANDBOX_PROXY_POOL_SIZE SANDBOX_PROXY_REUSED_POOL_SIZE
    NETWORK_POOL_NEW_SLOTS_SIZE
    SPOTBOX_STRATEGY_BASE_URL SPOTBOX_STRATEGY_POLL_INTERVAL SPOTBOX_PROFILE
    MITMPROXY_CONFIG
)

#==================== 状态 ====================#
declare -A BASE_ENV=()
declare -A RESULT_E2E=() RESULT_P50=() RESULT_P90=() RESULT_P99=() RESULT_MAX=() RESULT_OK=()
NODE_IP=""
ORCH_TOUCHED=0
MUX_RESTARTED=0
BASE_MITM_COUNT=0
BASE_CNI_ENABLED="${BASE_CNI_ENABLED:-1}"
BASE_CNI_POOL_ENABLED="${BASE_CNI_POOL_ENABLED:-1}"
BASE_CNI_POOL_SIZE="${BASE_CNI_POOL_SIZE:-200}"
BASE_HIDE_LABEL="${BASE_HIDE_LABEL:-flux-sandbox.io/direct=true}"

#==================== orchestrator DaemonSet 管理（与 35 同款） ====================#
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
    log_pass "已捕获 DaemonSet 基线 env（${#MANAGED_KEYS[@]} 个键）"

    local pod mark
    pod=$(tm_pod)
    [ -n "${pod}" ] || { log_fail "未找到 ${ORCH_DS_SELECTOR} pod"; return 1; }
    mark=$(kubectl -n "${ORCH_NS}" exec "${pod}" -- sh -c \
        'grep -ac "SANDBOX_EGRESS_PROXY_MODE" /usr/bin/orchestrator 2>/dev/null || echo 0' \
        2>/dev/null | tr -d '[:space:]' || true)
    [ "${mark:-0}" -ge 1 ] \
        && log_pass "orchestrator 镜像含 per-sandbox 代理特性（pod=${pod}）" \
        || { log_fail "orchestrator 镜像不含 SANDBOX_EGRESS_PROXY_MODE 特性，请先更新镜像"; return 1; }
    kubectl -n "${ORCH_NS}" exec "${pod}" -- sh -c \
        "test -x '${PROXY_BINARY_POD}' && test -f '${PROXY_ADDON_POD}'" 2>/dev/null \
        && log_pass "镜像预置 mitmdump（${PROXY_BINARY_POD}）与 addon.py（${PROXY_ADDON_POD}）" \
        || { log_fail "镜像缺少 ${PROXY_BINARY_POD} 或 ${PROXY_ADDON_POD}"; return 1; }
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

# 等待代理 slot 子池预热到位（proxyNewSlots len ≥ $2）。
# [Pool Status] 行的数值在 zap fields 里（"proxyNewSlots len": N），msg 本体是 %d 占位符。
wait_proxy_pool_warm() { # <mode> <want>
    local mode="$1"
    local want="$2"
    local t0 cur
    t0=$(date +%s)
    while true; do
        # [Pool Status] 的数值在下一行的 zap fields JSON 里（"proxyNewSlots len": N），
        # msg 本体是 %d 占位符——直接取日志尾部最后一次出现的字段值
        cur=$(kubectl -n "${ORCH_NS}" logs "$(tm_pod)" --tail=3000 2>/dev/null \
            | grep -aoP '"proxyNewSlots len":\s*\d+' | tail -1 | grep -oP '\d+' || true)
        if [ -z "${cur}" ]; then
            cur=$(kubectl -n "${ORCH_NS}" logs "$(tm_pod)" --tail=3000 2>/dev/null \
                | grep -aoP 'proxyNewSlots len[=:]\s*\d+' | tail -1 | grep -oP '\d+' || true)
        fi
        if [ -n "${cur}" ] && [ "${cur}" -ge "${want}" ]; then
            log_pass "[${mode}] 代理 slot 子池预热到位（proxyNewSlots=${cur} ≥ ${want}，耗时 $(( $(date +%s) - t0 ))s）"
            return 0
        fi
        if [ $(( $(date +%s) - t0 )) -ge 900 ]; then
            log_fail "[${mode}] 代理子池预热超时 900s（当前 ${cur:-NA}/${want}）"
            return 1
        fi
        sleep 5
    done
}

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
        E2B_FORCE_RESTART=1 bash "${SCRIPT_DIR}/01_start_multiplex.sh" > /tmp/36-restore-mux.log 2>&1 || true
}

#==================== 清理 ====================#
cleanup_36() {
    log_info "清理: 兜底删除残留沙箱 / 恢复基线（幂等）"
    # 32 号 --cleanup 已删除本轮沙箱；此处兜底按 id 清单文件清扫（含异常退出场景）
    local f pid
    for f in /tmp/p36*-pod-ids.txt; do
        [ -f "${f}" ] || continue
        while read -r pid; do
            [ -n "${pid}" ] && ${CRICTL} rmp -f "${pid}" >/dev/null 2>&1 || true
        done < "${f}"
        rm -f "${f}"
    done
    if [ "${ORCH_TOUCHED}" = "1" ]; then
        orch_restore_baseline && log_info "orchestrator 基线 env 已恢复" \
            || log_info "orchestrator 基线恢复失败，请手工检查 kubectl -n ${ORCH_NS} get ds ${ORCH_DS}"
    fi
    if [ "${MUX_RESTARTED}" = "1" ]; then
        restore_mux_baseline
    fi
}
trap cleanup_36 EXIT

#==================== 0. 前置检查 ====================#
log_step "0.1 前置检查"
for cmd in jq ip python3 kubectl openssl base64 pgrep; do
    command -v "${cmd}" >/dev/null 2>&1 || { log_fail "${cmd} 不存在"; exit 1; }
done
[ -f /tmp/e2b-pod.json ] || bash "${SCRIPT_DIR}/00_setup.sh" >/dev/null 2>&1 || true
[ -f /tmp/e2b-pod.json ] || { log_fail "基础 Pod JSON 准备失败"; exit 1; }
if [ ! -f /tmp/e2b-kubelet-pod.yaml ]; then
    E2B_SKIP_BUILD=0 E2B_YAML_COUNT=0 refresh_or_reuse_e2b_yaml \
        "${SCRIPT_DIR}/lib/refresh_build_id.sh" "e2b-kubelet-test" \
        "/tmp/e2b-kubelet-pod.yaml" || { log_fail "E2B fixture 准备失败"; exit 1; }
fi
export E2B_SKIP_BUILD=1 E2B_BASE_POD_YAML=/tmp/e2b-kubelet-pod.yaml
# 与 35 相同：build-id/envd token 必须与最新模板构建一致
sync_e2b_pod_json_from_kubelet_yaml /tmp/e2b-kubelet-pod.yaml /tmp/e2b-pod.json \
    && log_pass "e2b-pod.json 已同步最新模板凭证" \
    || { log_fail "e2b-pod.json 同步最新模板凭证失败"; exit 1; }

NODE_IP=$(ip -4 route get 1.1.1.1 2>/dev/null | grep -oP 'src \K[0-9.]+' | head -1)
[ -n "${NODE_IP}" ] || { log_fail "无法探测节点 IP"; exit 1; }
BASE_MITM_COUNT=$(pgrep -fc mitmdump 2>/dev/null || true)
log_pass "节点 IP=${NODE_IP}，mitmdump 基线进程数=${BASE_MITM_COUNT}"

capture_ds_baseline || exit 1

#==================== 1. cri-multiplex 切原生（非 CNI）模式 ====================#
log_step "1.1 切换 cri-multiplex 到非 CNI（原生）模式"
start_non_cni_multiplex "启动 cri-multiplex 非 CNI runtime 模式" || exit 1
MUX_RESTARTED=1

#==================== 2. CA staging + 性能模板 Pod JSON ====================#
log_step "2.1 测试 CA staging（真源同步，幂等）+ 最小 MITMPROXY_CONFIG"
mkdir -p "${CA_STAGE}"
if [ ! -s "${CA_DIR_HOST}/mitmproxy-ca.pem" ]; then
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
        -keyout "${CA_DIR_HOST}/mitmproxy-ca.key" -out "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" \
        -subj "/CN=e2b-egress-test-ca" >/dev/null 2>&1 \
        && cat "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" "${CA_DIR_HOST}/mitmproxy-ca.key" \
            > "${CA_DIR_HOST}/mitmproxy-ca.pem" \
        && chmod 600 "${CA_DIR_HOST}/mitmproxy-ca.key" "${CA_DIR_HOST}/mitmproxy-ca.pem" \
        || { log_fail "openssl 生成测试 CA 失败"; exit 1; }
fi
cp -f "${CA_DIR_HOST}/mitmproxy-ca.pem" "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" "${CA_STAGE}/"
# 性能测量不依赖名单语义：全空名单（外网透传直连、无注入），只为让 addon 正常加载
cat > "${CA_STAGE}/mitmproxy36.json" <<'EOF'
{
  "internal":         {"hosts": [], "domains": [], "nets": []},
  "header_whitelist": {"hosts": [], "domains": []},
  "header_blacklist": {"hosts": [], "domains": []}
}
EOF
log_pass "CA 与 MITMPROXY_CONFIG staging 就绪（${CA_STAGE}）"

log_step "2.2 生成性能模板 Pod JSON（带身份注解 → 推导 per-sandbox）"
# 32 号脚本只替换 uid/name 并加 label，注解从模板继承：带 sandbox-mis 即满足
# 推导 per-sandbox 条件（节点能力 + 身份注解）；sandbox-id 缺省按 Pod UID 派生，天然唯一。
jq '.annotations = ((.annotations // {}) + {"cri-multiplex.dev/sandbox-mis": "mis-perf-36"})' \
    /tmp/e2b-pod.json > "${PERF_TEMPLATE}" \
    && log_pass "性能模板就绪（${PERF_TEMPLATE}，sandbox-mis=mis-perf-36）" \
    || { log_fail "性能模板生成失败"; exit 1; }

#==================== 3. 逐模式测量 ====================#
run_round() { # <ready|immediate>
    local mode="$1"
    local prefix="p36${mode}" out="${WORK}/bulk-${mode}.log"
    log_step "3.x [${mode}] 设置 SANDBOX_PROXY_APPLY=${mode} 并等待滚动"
    orch_apply_env \
        "SANDBOX_EGRESS_PROXY_MODE=per-sandbox" \
        "SANDBOX_PROXY_APPLY=${mode}" \
        "SANDBOX_PROXY_UPSTREAM-" \
        "SANDBOX_PROXY_EXEMPT_CIDRS-" \
        "SANDBOX_PROXY_EXTRA_ARGS-" \
        "SANDBOX_PROXY_CONFDIR=${CA_DIR_POD}" \
        "SANDBOX_PROXY_LOG_DIR=${LOG_DIR_POD}" \
        "SANDBOX_PROXY_POOL_SIZE=${PROXY_POOL_SIZE}" \
        "SANDBOX_PROXY_REUSED_POOL_SIZE=${PROXY_POOL_SIZE}" \
        "NETWORK_POOL_NEW_SLOTS_SIZE=0" \
        "SPOTBOX_STRATEGY_BASE_URL-" \
        "SPOTBOX_STRATEGY_POLL_INTERVAL-" \
        "MITMPROXY_CONFIG=${MITM_JSON_POD}" || return 1

    # 滚动后 pod 容器文件系统重置，CA 材料与 MITMPROXY_CONFIG 需重推
    kubectl -n "${ORCH_NS}" exec "$(tm_pod)" -- mkdir -p "${CA_DIR_POD}" "${LOG_DIR_POD}" >&2
    push_pod_file "${CA_STAGE}/mitmproxy-ca.pem" "${CA_DIR_POD}/mitmproxy-ca.pem" \
        && push_pod_file "${CA_STAGE}/mitmproxy-ca-cert.pem" "${CA_DIR_POD}/mitmproxy-ca-cert.pem" \
        && push_pod_file "${CA_STAGE}/mitmproxy36.json" "${MITM_JSON_POD}" \
        && log_pass "[${mode}] CA（cert+key）与 MITMPROXY_CONFIG 已推入 pod" \
        || { log_fail "[${mode}] CA 材料推送失败"; return 1; }

    # 开火前先等代理 slot 子池预热到满水位（proxyNewSlots cap=PROXY_POOL_SIZE-1，
    # 一个在途）；池化后建网成本在预热期，预热不满开火会把补位等待混进测量尾巴
    local full_mark=$((PROXY_POOL_SIZE - 1))
    wait_proxy_pool_warm "${mode}" "${full_mark}" || return 1

    # 冒烟探针：单沙箱 runp/rmp 带重试，端到端证实 cri-multiplex→orchestrator
    # 链路稳定后再放大炮（滚动后 gRPC 就绪与编排面可服务之间存在过观测到的瞬时窗口）
    log_step "3.x [${mode}] 冒烟探针：单沙箱创建/删除（至多 5 次重试）"
    local smoke_json="${WORK}/smoke-${mode}.json" smoke_id="" si
    jq --arg uid "e2b36smoke${mode}$(date +%s)" --arg name "e2b36-smoke-${mode}" \
        '.metadata.uid = $uid | .metadata.name = $name' \
        "${PERF_TEMPLATE}" > "${smoke_json}"
    for si in 1 2 3 4 5; do
        smoke_id=$(POD_JSON="${smoke_json}" run_pod_sandbox 2>/dev/null) && break
        log_info "[${mode}] 冒烟第 ${si} 次失败，10s 后重试..."
        sleep 10
    done
    if [ -n "${smoke_id}" ]; then
        ${CRICTL} rmp -f "${smoke_id}" >/dev/null 2>&1 || true
        log_pass "[${mode}] 冒烟通过（编排面可服务）"
    else
        log_fail "[${mode}] 冒烟 5 次均失败，放弃本轮"
        return 1
    fi

    # 暖池一把：并发 ${COUNT} 个沙箱随即删除，把 proxyReusedSlots 填起来
    # （正式测量取池时复用池优先，命中的 slot 连 CreateNetwork 都省掉）；
    # 随后等 proxyNewSlots 补回满水位再正式开火
    log_step "3.x [${mode}] 暖池：并发 ${COUNT} 沙箱创建/删除填满代理复用池（不计入结果）"
    local warm_out="${WORK}/bulk-warm-${mode}.log" warm_line
    if ! python3 "${SCRIPT_DIR}/32_bulk_runp_concurrent.py" "${COUNT}" --cleanup \
            --pod-json "${PERF_TEMPLATE}" --prefix "p36warm${mode}" \
            --timeout "${RUNP_TIMEOUT}" 2> "${warm_out}"; then
        log_fail "[${mode}] 暖池轮存在失败沙箱（详见 ${warm_out}）"
        return 1
    fi
    warm_line=$(grep -aoE '\[BulkResult\].*' "${warm_out}" | tail -1 || true)
    log_info "[${mode}] 暖池 ${warm_line}"
    grep -qoP "ok=${COUNT}\b" <<< "${warm_line}" \
        && log_pass "[${mode}] 暖池 ${COUNT}/${COUNT} 成功" \
        || { log_fail "[${mode}] 暖池未全部成功：${warm_line}"; return 1; }
    wait_proxy_pool_warm "${mode}" "${full_mark}" || return 1

    log_step "3.x [${mode}] 并发创建 ${COUNT} 个 per-sandbox 代理沙箱（32 号脚本，--cleanup）"
    # 快照 cri-multiplex 日志（失败时定位 mux 侧是否崩溃/重启）
    cp /tmp/cri-multiplex.log "${WORK}/cri-log-before-${mode}.log" 2>/dev/null || true
    if ! python3 "${SCRIPT_DIR}/32_bulk_runp_concurrent.py" "${COUNT}" --cleanup \
            --pod-json "${PERF_TEMPLATE}" --prefix "${prefix}" \
            --timeout "${RUNP_TIMEOUT}" 2> "${out}"; then
        log_fail "[${mode}] 并发创建存在失败沙箱（详见 ${out}）"
        grep -a 'ERR' "${out}" | head -5 | while read -r l; do log_info "  ${l}"; done
        # 失败现场诊断：cri-multiplex socket 是否还活、进程是否换过
        if ${CRICTL} version >/dev/null 2>&1; then
            log_info "[${mode}] 诊断: cri-multiplex socket 当前可连接（失败是瞬时/队列问题）"
        else
            log_info "[${mode}] 诊断: cri-multiplex socket 当前不可连接（进程崩溃/重启过）"
        fi
        ps -eo pid,lstart,cmd | grep '[c]ri-multiplex' | head -3 | while read -r l; do log_info "  ps: ${l}"; done
        tail -30 /tmp/cri-multiplex.log 2>/dev/null | while read -r l; do log_info "  mux.log: ${l}"; done
        local tm restarts
        tm=$(tm_pod)
        restarts=$(kubectl -n "${ORCH_NS}" get pod "${tm}" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || true)
        log_info "[${mode}] 诊断: orchestrator pod=${tm} restarts=${restarts}"
        kubectl -n "${ORCH_NS}" logs "${tm}" --tail=15 2>/dev/null | while read -r l; do log_info "  orch: ${l}"; done
        kubectl -n "${ORCH_NS}" logs "${tm}" --previous --tail=15 2>/dev/null | while read -r l; do log_info "  orch(prev): ${l}"; done
    fi
    local line
    line=$(grep -aoE '\[BulkResult\].*' "${out}" | tail -1 || true)
    if [ -z "${line}" ]; then
        log_fail "[${mode}] 未采集到 [BulkResult]（${out}）"
        return 1
    fi
    log_info "[${mode}] ${line}"
    RESULT_OK[${mode}]=$(grep -oP 'ok=\K[0-9]+' <<< "${line}")
    RESULT_E2E[${mode}]=$(grep -oP 'e2e_ms=\K[0-9]+' <<< "${line}")
    RESULT_P50[${mode}]=$(grep -oP 'p50_ms=\K[0-9-]+' <<< "${line}")
    RESULT_P90[${mode}]=$(grep -oP 'p90_ms=\K[0-9-]+' <<< "${line}")
    RESULT_P99[${mode}]=$(grep -oP 'p99_ms=\K[0-9-]+' <<< "${line}")
    RESULT_MAX[${mode}]=$(grep -oP 'max_ms=\K[0-9-]+' <<< "${line}")
    [ "${RESULT_OK[${mode}]}" = "${COUNT}" ] \
        && log_pass "[${mode}] ${COUNT}/${COUNT} 创建成功" \
        || log_fail "[${mode}] 成功 ${RESULT_OK[${mode}]}/${COUNT}"

    # 代理收割检查：--cleanup 完成后 mitmdump 进程数应回到基线
    local i cur
    for i in $(seq 1 30); do
        cur=$(pgrep -fc mitmdump 2>/dev/null || true)
        [ "${cur}" -le "${BASE_MITM_COUNT}" ] && break
        sleep 2
    done
    [ "${cur}" -le "${BASE_MITM_COUNT}" ] \
        && log_pass "[${mode}] 代理进程已全部收割（mitmdump ${cur} ≤ 基线 ${BASE_MITM_COUNT}）" \
        || log_fail "[${mode}] 存在孤儿代理进程（mitmdump ${cur} > 基线 ${BASE_MITM_COUNT}）"
}

for m in ${MODES}; do
    run_round "${m}" || true
done

#==================== 4. 汇总 ====================#
log_step "4.1 结果汇总（并发 ${COUNT}，单次 runp 超时 ${RUNP_TIMEOUT}）"
printf '%-12s %8s %10s %10s %10s %10s %10s\n' "APPLY模式" "成功数" "e2e_ms" "p50_ms" "p90_ms" "p99_ms" "max_ms"
for m in ${MODES}; do
    printf '%-12s %8s %10s %10s %10s %10s %10s\n' "${m}" \
        "${RESULT_OK[${m}]:-NA}" "${RESULT_E2E[${m}]:-NA}" "${RESULT_P50[${m}]:-NA}" \
        "${RESULT_P90[${m}]:-NA}" "${RESULT_P99[${m}]:-NA}" "${RESULT_MAX[${m}]:-NA}"
done
if [ "${RESULT_OK[ready]:-}" = "${COUNT}" ] && [ "${RESULT_OK[immediate]:-}" = "${COUNT}" ]; then
    DELTA=$(( RESULT_E2E[immediate] - RESULT_E2E[ready] ))
    log_info "immediate 相对 ready 的端到端增量: ${DELTA}ms（= 代理 spawn+就绪探测+装规则进创建路径的代价）"
fi

print_summary
