#!/bin/bash
###############################################################################
# 26_admin_sandbox_service.sh — E2BSandboxService（admin socket）生命周期验证
#
# 验证 admin unix socket 上新增的 E2BSandboxService
# （crimultiplex.admin.v1，Create/Update/List/Delete/ListCachedBuilds）：
#   1. Create 参数校验（缺 sandbox_id → InvalidArgument）
#   2. Create 完整生命周期（CNI/HostPort/tracker/stateStore 继承）
#   3. Create 幂等重试（同 sandbox_id 重复调用直接返回）
#   4. List 可见（orchestrator 视图）+ CRI ListPodSandbox 可见（本地同构）
#   5. GetSandboxRuntime 运行事实（Running / CNI PodIP / HostPort 映射）
#   6. PodIP:49983/health 与 nodeIP:hostPort/health 可达
#   7. Update / ListCachedBuilds 转发可用
#   8. Delete 一次完成全部清理（netns/stateStore/HostPort），且幂等
#   9. expose-ports 三种写法（4.3 节）：
#      - 写法② P:H 指定宿主端口：映射成功且可访问；端口被占用时 Create 失败（ResourceExhausted）且完整回滚
#      - 写法③ P:H1-H2 区间：区间内分配成功；区间被外部进程全占用时 Create 失败（bindProbe 探测）
#      - malformed（保留端口 / 非法格式）：fail-fast 返回 InvalidArgument
#
# 前置：cri-multiplex 以 CNI 模式运行（CNI_ENABLED=1，见 01_start_multiplex.sh），
# /tmp/e2b-pod.json 存在（提供 template/build/team/envd token）。
# 不加入 run_all.sh（依赖新版二进制的 admin 接口）。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "26 — E2BSandboxService admin 接口生命周期验证"

ADMIN_SOCK="${ADMIN_SOCK:-/run/cri-multiplex/admin.sock}"
ADMIN_PROTO_DIR="${MULTIPLEX_DIR}/proto"
ENVD_PORT="${ENVD_PORT:-49983}"
# NODE_IP 不可靠猜（cri-multiplex 的 -node-ip 可省略、自动探测结果可能与 hostname -I 首地址不同），
# 创建沙箱后从 HostPort iptables 规则注释（cri-multiplex:hostport:<nodeIP>:...）中提取
NODE_IP="${NODE_IP:-}"
GRPCURL_BIN="$(command -v grpcurl || echo /root/go/bin/grpcurl)"

SBX_ID="e2badmin$(date +%s)$RANDOM"
# 4.3 节 expose-ports 三种写法验证用的附加沙箱（提前定义，保证 cleanup 在 set -u 下可用）
SBX_PIN="${SBX_ID}-pin"
SBX_PIN2="${SBX_ID}-pin2"
SBX_RANGE="${SBX_ID}-rng"
SBX_FULL="${SBX_ID}-full"
SBX_BAD1="${SBX_ID}-bad1"
SBX_BAD2="${SBX_ID}-bad2"
OCC_PID=""
HOST_IP=""
HOST_PORT=""

# envd /health 在该版本返回 204 No Content（与 11 号用例口径一致），200 也视为成功
health_ok() {
    [ "$1" = "200" ] || [ "$1" = "204" ]
}

# grpcurl 调 admin.sock（本机 grpcurl -unix flag 有 bug，必须用 unix:/// scheme + -plaintext）
# 用法: gadmin <service> <method> [json_data]；响应写 stdout，日志走 stderr
gadmin() {
    local svc="$1" method="$2" data="${3:-}"
    echo "  [gRPC] --> ${svc}/${method} request:" >&2
    [ -n "${data}" ] && echo "  [gRPC]     ${data}" >&2
    local out rc
    if [ -n "${data}" ]; then
        out=$("${GRPCURL_BIN}" -plaintext \
            -import-path "${ADMIN_PROTO_DIR}" -proto admin.proto \
            -d "${data}" "unix://${ADMIN_SOCK}" \
            "crimultiplex.admin.v1.${svc}/${method}" 2>&1) && rc=0 || rc=$?
    else
        out=$("${GRPCURL_BIN}" -plaintext \
            -import-path "${ADMIN_PROTO_DIR}" -proto admin.proto \
            "unix://${ADMIN_SOCK}" \
            "crimultiplex.admin.v1.${svc}/${method}" 2>&1) && rc=0 || rc=$?
    fi
    echo "  [gRPC] <-- ${out}" >&2
    printf '%s' "${out}"
    return ${rc}
}

# 构造 Create 请求：annotation 全量映射，expose-ports 走 metadata
build_create_req() {
    local sid="$1" alias="$2" ports="$3"
    jq -nc --arg sid "${sid}" --arg alias "${alias}" --arg ports "${ports}" --slurpfile pod "${POD_JSON}" '
      ($pod[0].annotations) as $a |
      {sandbox: {
        sandbox_id: $sid, template_id: $a["e2b.dev/template-id"], build_id: $a["e2b.dev/build-id"],
        team_id: $a["e2b.dev/team-id"], alias: $alias,
        envd_access_token: ($a["e2b.dev/envd-access-token"] // ""),
        vcpu: ($a["e2b.dev/vcpu"] // "1" | tonumber),
        ram_mb: ($a["e2b.dev/ram-mb"] // "2048" | tonumber),
        total_disk_size_mb: ($a["e2b.dev/total-disk-size-mb"] // "0" | tonumber),
        max_sandbox_length: ($a["e2b.dev/max-sandbox-length"] // "0" | tonumber),
        huge_pages: (($a["e2b.dev/huge-pages"] // "false") == "true"),
        auto_pause: (($a["e2b.dev/auto-pause"] // "false") == "true"),
        snapshot: (($a["e2b.dev/snapshot"] // "false") == "true"),
        allow_internet_access: (($a["e2b.dev/allow-internet"] // "true") == "true"),
        envd_version: ($a["e2b.dev/envd-version"] // ""),
        kernel_version: ($a["e2b.dev/kernel-version"] // ""),
        firecracker_version: ($a["e2b.dev/firecracker-version"] // ""),
        base_template_id: ($a["e2b.dev/base_template_id"] // ""),
        env_vars: ($a["e2b.dev/env-vars"] // "{}" | fromjson),
        network: ($a["e2b.dev/network"] // "{}" | fromjson),
        volumeMounts: ($a["e2b.dev/volume-mounts"] // "[]" | fromjson),
        auto_resume: ($a["e2b.dev/auto-resume"] // "{}" | fromjson),
        metadata: {"e2b.dev/expose-ports": $ports}
      }}'
}

# 从 GetSandboxRuntime 提取指定 sandboxPort 的宿主端口
get_hostport() {
    local out
    out=$(gadmin "E2BSandboxAdminService" "GetSandboxRuntime" "{\"e2b_sandbox_id\": \"$1\"}") || true
    echo "${out}" | jq -r --argjson p "$2" '.hostports[]? | select(.sandboxPort == $p) | .hostPort' 2>/dev/null | head -1
}

# netns 名与 cni_manager.shortID 一致：>12 字符取 前6 + sha256前6
netns_name_of() {
    local id="$1"
    if [ ${#id} -le 12 ]; then echo "e2b-${id}"; else echo "e2b-${id:0:6}$(echo -n "${id}" | sha256sum | cut -c1-6)"; fi
}

# 从给定起点找首个可 bind 的 IPv4 空闲端口（跳过 5008 与 NodePort 段 30000-32767）
probe_free_port() {
    python3 - "$1" <<'PYEOF'
import socket, sys
p = int(sys.argv[1])
while p < 60000:
    if p == 5008 or 30000 <= p <= 32767: p += 1; continue
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        s.bind(("0.0.0.0", p)); print(p); break
    except OSError: p += 1
    finally: s.close()
PYEOF
}

# 找两个连续可 bind 空闲端口，输出 "p1 p2"
probe_free_port_pair() {
    python3 - "$1" <<'PYEOF'
import socket, sys
p = int(sys.argv[1])
while p < 59999:
    if p == 5008 or p+1 == 5008 or 30000 <= p <= 32767 or 30000 <= p+1 <= 32767: p += 1; continue
    s1 = socket.socket(); s2 = socket.socket()
    try:
        s1.bind(("0.0.0.0", p)); s2.bind(("0.0.0.0", p+1)); print(p, p+1); break
    except OSError: p += 1
    finally: s1.close(); s2.close()
PYEOF
}

# 等待 envd /health 就绪（最多 ~30s）
wait_health() {
    local i code
    for i in $(seq 1 10); do
        code=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 5 "$1" 2>/dev/null || true)
        if health_ok "${code}"; then return 0; fi
        sleep 3
    done
    return 1
}

cleanup_26() {
    log_info "清理: Delete 全部用例沙箱（兜底，幂等）"
    local sid
    for sid in "${SBX_ID}" "${SBX_PIN}" "${SBX_PIN2}" "${SBX_RANGE}" "${SBX_FULL}" "${SBX_BAD1}" "${SBX_BAD2}"; do
        gadmin "E2BSandboxService" "Delete" "{\"sandbox_id\": \"${sid}\"}" >/dev/null 2>&1 || true
        ${CRICTL} rmp -f "${sid}" >/dev/null 2>&1 || true
    done
    [ -n "${OCC_PID}" ] && kill "${OCC_PID}" 2>/dev/null || true
}
trap cleanup_26 EXIT

#==================== 1. 前置检查 ====================#
log_step "1.1 前置检查"

require_cri_multiplex_cni_enabled_quiet || {
    log_fail "cri-multiplex 未以 CNI 模式运行，先执行: CNI_ENABLED=1 bash e2b-verify/01_start_multiplex.sh"
    exit 1
}
log_pass "cri-multiplex CNI 模式运行中"

[ -S "${ADMIN_SOCK}" ] || {
    log_fail "admin socket 不存在: ${ADMIN_SOCK}（cri-multiplex 版本过旧或未启动）"
    exit 1
}
log_pass "admin socket 存在: ${ADMIN_SOCK}"

[ -f "${POD_JSON}" ] || {
    log_fail "${POD_JSON} 不存在（先跑 02_lifecycle.sh 生成，或手工准备）"
    exit 1
}
# 同步 kubelet 最新模板注解（build_id 可能过期），与 run_pod_sandbox 保持一致
sync_e2b_pod_json_from_kubelet_yaml /tmp/e2b-kubelet-pod.yaml "${POD_JSON}" || true

for cmd in jq curl sha256sum; do
    command -v "${cmd}" >/dev/null 2>&1 || { log_fail "${cmd} 不存在"; exit 1; }
done
log_pass "jq / curl / sha256sum 可用；grpcurl=${GRPCURL_BIN}"

# grpc_call 依赖 00_setup.sh 下载的 CRI proto（/tmp 可能被清理，缺失时自动补齐）
if [ ! -f "${PROTO_FILE}" ]; then
    log_info "CRI proto 缺失，执行 00_setup.sh 准备"
    bash "${SCRIPT_DIR}/00_setup.sh" >/dev/null 2>&1 || true
fi
[ -f "${PROTO_FILE}" ] || { log_fail "CRI proto 准备失败: ${PROTO_FILE}"; exit 1; }
log_pass "CRI proto 就绪: ${PROTO_FILE}"

TEMPLATE_ID=$(jq -r '.annotations["e2b.dev/template-id"] // empty' "${POD_JSON}")
BUILD_ID=$(jq -r '.annotations["e2b.dev/build-id"] // empty' "${POD_JSON}")
TEAM_ID=$(jq -r '.annotations["e2b.dev/team-id"] // empty' "${POD_JSON}")
ENVD_TOKEN=$(jq -r '.annotations["e2b.dev/envd-access-token"] // empty' "${POD_JSON}")
if [ -z "${TEMPLATE_ID}" ] || [ -z "${BUILD_ID}" ] || [ -z "${TEAM_ID}" ]; then
    log_fail "无法从 ${POD_JSON} 提取 template-id/build-id/team-id"
    exit 1
fi
log_info "sandbox_id=${SBX_ID} template=${TEMPLATE_ID} build=${BUILD_ID} team=${TEAM_ID}"

#==================== 2. Create ====================#
log_step "2.1 Create 参数校验：缺 sandbox_id 应返回 InvalidArgument"
out=$(gadmin "E2BSandboxService" "Create" \
    "{\"sandbox\": {\"template_id\": \"${TEMPLATE_ID}\", \"build_id\": \"${BUILD_ID}\", \"team_id\": \"${TEAM_ID}\"}}") || true
if grep -q "InvalidArgument" <<< "${out}"; then
    log_pass "缺 sandbox_id 返回 InvalidArgument"
else
    log_fail "缺 sandbox_id 未返回 InvalidArgument: ${out}"
fi

log_step "2.2 Create 完整生命周期（annotation 全量映射到 SandboxConfig，expose-ports 走 metadata）"
# 与 CRI 路径 annotationsToSandboxConfig 对齐：orchestrator 依赖 kernel/firecracker/envd
# 版本等字段定位构建产物，必须全量映射，不能只传 template/build/team
CREATE_REQ=$(build_create_req "${SBX_ID}" "admin-svc-test" "49983")
out=$(gadmin "E2BSandboxService" "Create" "${CREATE_REQ}") || true
HOST_IP=$(echo "${out}" | jq -r '.hostIp // empty' 2>/dev/null || true)
CLIENT_ID=$(echo "${out}" | jq -r '.clientId // empty' 2>/dev/null || true)
if [ -n "${HOST_IP}" ] && [ -n "${CLIENT_ID}" ]; then
    log_pass "Create 成功: host_ip=${HOST_IP} client_id=${CLIENT_ID}"
else
    log_fail "Create 失败: ${out}"
    print_summary
    exit 1
fi

log_step "2.3 Create 幂等：同 sandbox_id 重复调用"
out=$(gadmin "E2BSandboxService" "Create" "${CREATE_REQ}") || true
HOST_IP2=$(echo "${out}" | jq -r '.hostIp // empty' 2>/dev/null || true)
if [ "${HOST_IP2}" = "${HOST_IP}" ]; then
    log_pass "幂等 Create 返回相同 host_ip=${HOST_IP2}"
else
    log_fail "幂等 Create 异常: ${out}"
fi

#==================== 3. 可见性与运行事实 ====================#
log_step "3.1 List（orchestrator 视图）包含新沙箱"
out=$(gadmin "E2BSandboxService" "List") || true
if echo "${out}" | jq -e --arg sid "${SBX_ID}" '.sandboxes[]? | select(.config.sandboxId == $sid)' >/dev/null 2>&1; then
    log_pass "List 包含 ${SBX_ID}"
else
    log_fail "List 不包含 ${SBX_ID}: ${out}"
fi

log_step "3.2 CRI ListPodSandbox 可见（与 CRI 路径产物同构）"
out=$(grpc_call "runtime.v1.RuntimeService/ListPodSandbox") || true
if echo "${out}" | jq -e --arg sid "${SBX_ID}" '.items[]? | select(.id == $sid)' >/dev/null 2>&1; then
    log_pass "ListPodSandbox 包含 ${SBX_ID}"
else
    log_fail "ListPodSandbox 不包含 ${SBX_ID}"
fi

log_step "3.3 GetSandboxRuntime 运行事实（Running / CNI PodIP / HostPort）"
out=$(gadmin "E2BSandboxAdminService" "GetSandboxRuntime" "{\"e2b_sandbox_id\": \"${SBX_ID}\"}") || true
RT_STATE=$(echo "${out}" | jq -r '.runtimeState // empty' 2>/dev/null || true)
RT_PODIP=$(echo "${out}" | jq -r '.cni.podIp // empty' 2>/dev/null || true)
HOST_PORT=$(echo "${out}" | jq -r --argjson p "${ENVD_PORT}" '.hostports[]? | select(.sandboxPort == $p) | .hostPort' 2>/dev/null | head -1)
if [ "${RT_STATE}" = "Running" ]; then
    log_pass "runtimeState=Running"
else
    log_fail "runtimeState=${RT_STATE}，期望 Running: ${out}"
fi
if [ -n "${RT_PODIP}" ] && [ "${RT_PODIP}" = "${HOST_IP}" ]; then
    log_pass "CNI PodIP=${RT_PODIP} 与 Create 返回的 host_ip 一致"
else
    log_fail "CNI PodIP=${RT_PODIP} 与 host_ip=${HOST_IP} 不一致"
fi
if [ -n "${HOST_PORT}" ]; then
    # 从 iptables 规则注释提取 cri-multiplex 实际使用的 nodeIP（cri-multiplex:hostport:<nodeIP>:<hostPort>:...）
    if [ -z "${NODE_IP}" ]; then
        NODE_IP=$(iptables -w 5 -t nat -L OUTPUT -n 2>/dev/null | grep -oP "cri-multiplex:hostport:\K[0-9.]+(?=:${HOST_PORT}:)" | head -1 || true)
    fi
    log_pass "HostPort 映射存在: ${NODE_IP:-?}:${HOST_PORT} -> ${HOST_IP}:${ENVD_PORT}"
else
    log_fail "未找到 sandboxPort=${ENVD_PORT} 的 HostPort 映射: ${out}"
fi

#==================== 4. 网络可达性 ====================#
log_step "4.1 通过 CNI PodIP 访问 envd health"
HTTP_CODE=$(curl -sS -o /tmp/e2b-admin-health.out -w "%{http_code}" --max-time 5 \
    "http://${HOST_IP}:${ENVD_PORT}/health" 2>/tmp/e2b-admin-health.err || true)
if health_ok "${HTTP_CODE}"; then
    log_pass "PodIP:${ENVD_PORT}/health 可访问，HTTP ${HTTP_CODE}"
else
    log_fail "PodIP:${ENVD_PORT}/health 访问失败，HTTP ${HTTP_CODE}"
    cat /tmp/e2b-admin-health.err >&2 || true
fi

log_step "4.2 通过 HostPort 访问 envd health"
if [ -n "${HOST_PORT}" ] && [ -n "${NODE_IP}" ]; then
    HTTP_CODE=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 5 \
        "http://${NODE_IP}:${HOST_PORT}/health" 2>/tmp/e2b-admin-hp-health.err || true)
    if health_ok "${HTTP_CODE}"; then
        log_pass "${NODE_IP}:${HOST_PORT}/health 可访问，HTTP ${HTTP_CODE}"
    else
        log_fail "${NODE_IP}:${HOST_PORT}/health 访问失败，HTTP ${HTTP_CODE}"
        cat /tmp/e2b-admin-hp-health.err >&2 || true
    fi
else
    log_skip "无 HostPort 或 NODE_IP，跳过 HostPort 访问验证"
fi

log_step "4.3 expose-ports 三种写法"

# 4.3.1 写法② P:H 指定宿主端口：映射成功且经 nodeIP:H 可访问
log_step "4.3.1 写法② P:H 指定宿主端口成功"
PIN_PORT=$(probe_free_port 38080)
if [ -z "${PIN_PORT}" ] || [ -z "${NODE_IP}" ]; then
    log_skip "无空闲探测端口或 NODE_IP 未提取，跳过写法②成功用例"
else
    out=$(gadmin "E2BSandboxService" "Create" "$(build_create_req "${SBX_PIN}" "pin-ok" "49983:${PIN_PORT}")") || true
    if echo "${out}" | jq -e '.hostIp' >/dev/null 2>&1; then
        HP_PIN=$(get_hostport "${SBX_PIN}" "${ENVD_PORT}")
        if [ "${HP_PIN}" = "${PIN_PORT}" ]; then
            log_pass "写法② 映射到指定宿主端口 ${PIN_PORT}"
        else
            log_fail "写法② hostPort=${HP_PIN}，期望 ${PIN_PORT}"
        fi
        if wait_health "http://${NODE_IP}:${PIN_PORT}/health"; then
            log_pass "经 ${NODE_IP}:${PIN_PORT}/health 访问成功"
        else
            log_fail "经 ${NODE_IP}:${PIN_PORT}/health 访问失败"
        fi
    else
        log_fail "写法② Create 失败: ${out}"
    fi
fi

# 4.3.2 写法② 宿主端口已被另一沙箱占用：Create 失败（ResourceExhausted）且完整回滚
log_step "4.3.2 写法② 宿主端口被占用时创建失败并回滚"
if [ -z "${PIN_PORT:-}" ]; then
    log_skip "无 PIN_PORT，跳过占用失败用例"
else
    out=$(gadmin "E2BSandboxService" "Create" "$(build_create_req "${SBX_PIN2}" "pin-conflict" "49983:${PIN_PORT}")") || true
    if grep -q "ResourceExhausted" <<< "${out}"; then
        log_pass "宿主端口冲突返回 ResourceExhausted"
    else
        log_fail "宿主端口冲突未返回 ResourceExhausted: ${out}"
    fi
    out=$(gadmin "E2BSandboxAdminService" "GetSandboxRuntime" "{\"e2b_sandbox_id\": \"${SBX_PIN2}\"}") || true
    if grep -q "NotFound" <<< "${out}"; then
        log_pass "冲突沙箱本地状态已回滚（NotFound）"
    else
        log_fail "冲突沙箱状态未回滚: ${out}"
    fi
    NETNS_PIN2="${CNI_NETNS_DIR}/$(netns_name_of "${SBX_PIN2}")"
    if [ -e "${NETNS_PIN2}" ]; then
        log_fail "冲突沙箱 netns 残留: ${NETNS_PIN2}"
    else
        log_pass "冲突沙箱 netns 已回滚"
    fi
fi

# 4.3.3 写法③ P:H1-H2 区间：区间内分配成功且可访问
log_step "4.3.3 写法③ P:H1-H2 区间分配成功"
read -r R1 R2 <<< "$(probe_free_port_pair 38300)"
if [ -z "${R1:-}" ] || [ -z "${R2:-}" ] || [ -z "${NODE_IP}" ]; then
    log_skip "未找到连续空闲端口对或 NODE_IP 未提取，跳过区间成功用例"
else
    out=$(gadmin "E2BSandboxService" "Create" "$(build_create_req "${SBX_RANGE}" "range-ok" "49983:${R1}-${R2}")") || true
    if echo "${out}" | jq -e '.hostIp' >/dev/null 2>&1; then
        HP_RANGE=$(get_hostport "${SBX_RANGE}" "${ENVD_PORT}")
        if [ -n "${HP_RANGE}" ] && [ "${HP_RANGE}" -ge "${R1}" ] && [ "${HP_RANGE}" -le "${R2}" ]; then
            log_pass "写法③ 在区间 [${R1},${R2}] 内分配到 ${HP_RANGE}"
        else
            log_fail "写法③ hostPort=${HP_RANGE} 不在区间 [${R1},${R2}]"
        fi
        if [ -n "${HP_RANGE}" ] && wait_health "http://${NODE_IP}:${HP_RANGE}/health"; then
            log_pass "经 ${NODE_IP}:${HP_RANGE}/health 访问成功"
        else
            log_fail "经区间端口访问失败（HP_RANGE=${HP_RANGE}）"
        fi
    else
        log_fail "写法③ Create 失败: ${out}"
    fi
fi

# 4.3.4 写法③ 区间被外部进程全占用：bindProbe 探测失败 → ResourceExhausted
log_step "4.3.4 写法③ 区间全占用时创建失败"
read -r O1 O2 <<< "$(probe_free_port_pair 38500)"
if [ -z "${O1:-}" ] || [ -z "${O2:-}" ]; then
    log_skip "未找到连续空闲端口对，跳过区间全占用用例"
else
    python3 - "${O1}" "${O2}" <<'PYEOF' &
import socket, sys, time
socks = []
for p in (int(sys.argv[1]), int(sys.argv[2])):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("0.0.0.0", p)); s.listen(1); socks.append(s)
time.sleep(180)
PYEOF
    OCC_PID=$!
    sleep 1
    out=$(gadmin "E2BSandboxService" "Create" "$(build_create_req "${SBX_FULL}" "range-full" "49983:${O1}-${O2}")") || true
    kill "${OCC_PID}" 2>/dev/null || true
    wait "${OCC_PID}" 2>/dev/null || true
    OCC_PID=""
    if grep -q "ResourceExhausted" <<< "${out}"; then
        log_pass "区间 [${O1},${O2}] 全占用返回 ResourceExhausted（bindProbe 生效）"
    else
        log_fail "区间全占用未返回 ResourceExhausted: ${out}"
    fi
fi

# 4.3.5 malformed：保留端口 / 非法格式 fail-fast InvalidArgument
log_step "4.3.5 malformed expose-ports 返回 InvalidArgument"
out=$(gadmin "E2BSandboxService" "Create" "$(build_create_req "${SBX_BAD1}" "bad-port" "49983:80")") || true
if grep -q "InvalidArgument" <<< "${out}"; then
    log_pass "保留端口 49983:80 返回 InvalidArgument"
else
    log_fail "保留端口未返回 InvalidArgument: ${out}"
fi
out=$(gadmin "E2BSandboxService" "Create" "$(build_create_req "${SBX_BAD2}" "bad-fmt" "abc")") || true
if grep -q "InvalidArgument" <<< "${out}"; then
    log_pass "非法格式 abc 返回 InvalidArgument"
else
    log_fail "非法格式未返回 InvalidArgument: ${out}"
fi

#==================== 5. Update / ListCachedBuilds ====================#
log_step "5.1 Update 延长 end_time"
NEW_END=$(date -u -d "+2 hours" +%Y-%m-%dT%H:%M:%SZ)
out=$(gadmin "E2BSandboxService" "Update" \
    "{\"sandbox_id\": \"${SBX_ID}\", \"end_time\": \"${NEW_END}\"}") || true
if grep -q "^{}\|^$" <<< "${out}"; then
    log_pass "Update 成功（end_time -> ${NEW_END}）"
else
    log_fail "Update 异常: ${out}"
fi

log_step "5.2 ListCachedBuilds"
out=$(gadmin "E2BSandboxService" "ListCachedBuilds") || true
if grep -q "builds\|^{}" <<< "${out}"; then
    log_pass "ListCachedBuilds 调用成功"
else
    log_fail "ListCachedBuilds 异常: ${out}"
fi

#==================== 6. Delete 与清理验证 ====================#
log_step "6.1 Delete（Stop+Remove 合一）"
out=$(gadmin "E2BSandboxService" "Delete" "{\"sandbox_id\": \"${SBX_ID}\"}") || true
if grep -q "^{}\|^$" <<< "${out}"; then
    log_pass "Delete 成功"
else
    log_fail "Delete 异常: ${out}"
fi

log_step "6.2 Delete 幂等：重复删除返回 OK"
out=$(gadmin "E2BSandboxService" "Delete" "{\"sandbox_id\": \"${SBX_ID}\"}") || true
if grep -q "^{}\|^$" <<< "${out}"; then
    log_pass "重复 Delete 返回 OK（幂等）"
else
    log_fail "重复 Delete 异常: ${out}"
fi

log_step "6.3 清理验证：List / ListPodSandbox / netns / GetSandboxRuntime"
out=$(gadmin "E2BSandboxService" "List") || true
if echo "${out}" | jq -e --arg sid "${SBX_ID}" '.sandboxes[]? | select(.config.sandboxId == $sid)' >/dev/null 2>&1; then
    log_fail "Delete 后 List 仍包含 ${SBX_ID}"
else
    log_pass "Delete 后 List 不再包含 ${SBX_ID}"
fi

out=$(grpc_call "runtime.v1.RuntimeService/ListPodSandbox") || true
if echo "${out}" | jq -e --arg sid "${SBX_ID}" '.items[]? | select(.id == $sid)' >/dev/null 2>&1; then
    log_fail "Delete 后 ListPodSandbox 仍包含 ${SBX_ID}"
else
    log_pass "Delete 后 ListPodSandbox 不再包含 ${SBX_ID}"
fi

NETNS_NAME=$(netns_name_of "${SBX_ID}")
if [ -e "${CNI_NETNS_DIR}/${NETNS_NAME}" ]; then
    log_fail "netns 残留: ${CNI_NETNS_DIR}/${NETNS_NAME}"
else
    log_pass "netns 已清理: ${NETNS_NAME}"
fi

out=$(gadmin "E2BSandboxAdminService" "GetSandboxRuntime" "{\"e2b_sandbox_id\": \"${SBX_ID}\"}") || true
if grep -q "NotFound" <<< "${out}"; then
    log_pass "GetSandboxRuntime 返回 NotFound（本地状态已清除）"
else
    log_fail "GetSandboxRuntime 未返回 NotFound: ${out}"
fi

#==================== 收尾 ====================#
# 正常退出路径也要清理 4.3 节创建的附加沙箱（trap - EXIT 会摘掉兜底清理，需显式调用）
cleanup_26
trap - EXIT
print_summary
if [ "${FAIL_COUNT}" -eq 0 ]; then
    exit 0
fi
exit 1
