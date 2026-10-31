#!/bin/bash
###############################################################################
# 26_adminv2_e2b_lifecycle.sh — admin v2 × E2B 创建/暂停/恢复/检查点验证
#
# 验证目标（admin v2 协议层，带 proto 直连 admin.sock，不用 reflection）。
# 下表按 § 分组：每条用例测什么功能场景、落在哪个接口。
#
# 接口缩写：
#   S.Create  = crimultiplex.admin.v2.SandboxService/Create
#   S.List    = crimultiplex.admin.v2.SandboxService/List
#   S.Delete  = crimultiplex.admin.v2.SandboxService/Delete
#   S.Update  = crimultiplex.admin.v2.SandboxService/Update
#   S.Cached  = crimultiplex.admin.v2.SandboxService/ListCachedBuilds
#   SA.Get    = crimultiplex.admin.v2.SandboxAdminService/GetSandboxRuntime
#   SA.Pause  = crimultiplex.admin.v2.SandboxAdminService/PauseSandbox
#   SA.Ckpt   = crimultiplex.admin.v2.SandboxAdminService/CheckpointSandbox
#   API.Pause = E2B 控制面：暂停快照预分配（前置，不计用例）
#   API.Ckpt  = E2B 控制面：检查点模板预分配（前置，不计用例）
#   API.Auth  = E2B 控制面：快照状态查询，确认鉴权生效
#   —         = 不经过 admin v2（本地工具 / 进程 / socket / 数据面 / CRI 检查）
#
# §0 预检（环境与依赖，不驱动业务）
# | 用例 | 测试的功能场景                               | 验证接口 |
# |------|----------------------------------------------|----------|
# | P1   | 脚本依赖的工具链齐全                         | —        |
# | P2   | admin proto 定义齐全，可直连查询             | —        |
# | P3   | 受控 cri-multiplex 实例运行中就绪            | —        |
# | P4   | admin unix socket 可连接                     | —        |
# | P5   | admin v2 服务已在实例上注册可用              | SA.Get   |
# | P6   | E2B 模板元数据完整（模板、规格、内核、envd） | —        |
# | P7   | E2B 控制面鉴权生效、数据库迁移满足前置       | API.Auth |
#
# §1 Create（首次创建 → 幂等重试 → 不重复创建）
# | 用例 | 测试的功能场景                            | 验证接口 |
# |------|-------------------------------------------|----------|
# | C1   | 首次创建 E2B 沙箱，返回沙箱标识与接入信息 | S.Create |
# | C2   | 重复的创建请求不会新建沙箱（幂等）        | S.Create |
# | C3   | 幂等重试后列表中该沙箱只有一条            | S.List   |
#
# §2 读一致性与数据面
# | 用例 | 测试的功能场景                                 | 验证接口 |
# |------|------------------------------------------------|----------|
# | V1   | 查询沙箱运行事实（状态、模板、pod 归属、网络） | SA.Get   |
# | V2   | 列表视图与 orchestrator 侧真实状态一致         | S.List   |
# | V3   | 沙箱数据面端口可从节点访问                     | SA.Get   |
# | V4   | 沙箱网络命名空间已建立                         | SA.Get   |
#
# §2.1 Update 与构建缓存
# | 用例 | 测试的功能场景                                             | 验证接口          |
# |------|------------------------------------------------------------|-------------------|
# | U1   | 更新沙箱存活时长（TTL），列表反映新到期时间且状态/模板不变 | S.Update + S.List |
# | L1   | 查询本节点已缓存的构建，返回构建 ID 与到期时间             | S.Cached          |
#
# §3 Pause（预分配 → 暂停 → 状态 → 幂等 → 删源沙箱）
# | 用例 | 测试的功能场景                     | 验证接口          |
# |------|------------------------------------|-------------------|
# | PZ1  | 暂停前预分配暂停快照产物           | API.Pause         |
# | PZ2  | 暂停沙箱并持久化快照               | SA.Pause          |
# | PZ3  | 暂停后沙箱状态为 Paused            | SA.Get            |
# | PZ4  | 同一操作重试不重复暂停（幂等）     | SA.Pause          |
# | PZ5  | 删除已暂停的源沙箱，删除后查询不到 | S.Delete + SA.Get |
#
# §4 Resume（基于暂停快照恢复）
# | 用例 | 测试的功能场景                             | 验证接口 |
# |------|--------------------------------------------|----------|
# | R1   | 基于暂停快照恢复同一沙箱                   | S.Create |
# | R2   | 恢复后运行事实指向快照模板且状态为 Running | SA.Get   |
# | R3   | 恢复后数据面重新可访问                     | SA.Get   |
#
# §5 Checkpoint（预分配 → 检查点 → 源沙箱不变 → 幂等）
# | 用例 | 测试的功能场景                     | 验证接口 |
# |------|------------------------------------|----------|
# | K1   | 检查点前预分配快照模板             | API.Ckpt |
# | K2   | 对运行中沙箱打检查点并持久化快照   | SA.Ckpt  |
# | K3   | 检查点不重建源沙箱（网络身份不变） | SA.Get   |
# | K4   | 同一操作重试不重复检查点（幂等）   | SA.Ckpt  |
#
# §6 从检查点创建新沙箱
# | 用例 | 测试的功能场景               | 验证接口          |
# |------|------------------------------|-------------------|
# | KC1  | 从检查点创建新沙箱           | S.Create          |
# | KC2  | 新沙箱运行事实来自检查点模板 | SA.Get            |
# | KC3  | 新沙箱数据面可访问           | SA.Get            |
# | KC4  | 列表视图反映新沙箱的模板来源 | S.List            |
# | KC5  | 删除新沙箱后查询不到         | S.Delete + SA.Get |
#
# §7 负向与能力分发
# | 用例 | 测试的功能场景                               | 验证接口          |
# |------|----------------------------------------------|-------------------|
# | N1   | 创建请求缺少沙箱标识时被拒绝，且不产生半创建 | S.Create + S.List |
# | N2   | 请求未实现 v2 生命周期的后端时返回未实现     | S.Create          |
# | N3   | 创建请求缺少团队标识时被拒绝，且不产生半创建 | S.Create + S.List |
# | N4   | 暂停请求缺少操作标识时被拒绝，且沙箱状态不变 | SA.Pause + SA.Get |
# | N5   | 创建请求指向未注册的后端时返回不存在         | S.Create          |
# | N6   | 暂停请求的团队标识与沙箱归属不符时被拒绝     | SA.Pause + SA.Get |
#
# §8 Delete 与回收
# | 用例 | 测试的功能场景                             | 验证接口                |
# |------|--------------------------------------------|-------------------------|
# | D1   | 删除运行中的沙箱                           | S.Delete                |
# | D2   | 重复删除仍成功（幂等，后端名大小写不敏感） | S.Delete                |
# | D3   | 删除后查询返回不存在                       | SA.Get                  |
# | D4   | 删除后列表视图不再出现                     | S.List                  |
# | D5   | 删除后 CRI 视图同步回收                    | —（CRI ListPodSandbox） |
# | D6   | 删除请求缺少沙箱标识时被拒绝               | S.Delete                |
#
# §9 收尾
# | 用例 | 测试的功能场景     | 验证接口 |
# |------|--------------------|----------|
# | Z1   | 用例结束无残留沙箱 | S.List   |
# | Z2   | 汇总统计与退出码   | —        |
#
# 说明：§0.1 残留清理与各处 E2B API 收尾属于步骤，不计入 47 条用例；
#       本集覆盖 v2 全部 8 个方法：SandboxService 的 Create/Update/List/Delete/
#       ListCachedBuilds，SandboxAdminService 的 PauseSandbox/CheckpointSandbox/
#       GetSandboxRuntime。
#
# 与 22 号（v1 黄金参考）的关系：
#   - 22 号用 kubectl 造 Pod（webhook 打桩），验证 v1 管理面；
#   - 26 号用 admin v2 真正驱动 E2B 创建，管理面全部走 v2，
#     Pause/Checkpoint 的 E2B API 预分配/收尾序列照搬 22 号；
#   - E2B API 调用只作前置/收尾：除 pause-snapshot / snapshot-templates
#     （硬前置，失败即 die）外，成功不计数、失败只 log_warn。
#
# 注意：admin 沙箱没有 K8s Pod 对象，也不能用 crictl inspectp
# （v2 创建不写 mux 路由，会回退 containerd 并 NotFound）。
#
# 用例集固定 47 条（每段内连号）：
#   P1 P2 P3 P4 P5 P6 P7 | C1 C2 C3 | V1 V2 V3 V4 | U1 L1 |
#   PZ1 PZ2 PZ3 PZ4 PZ5 | R1 R2 R3 | K1 K2 K3 K4 |
#   KC1 KC2 KC3 KC4 KC5 | N1 N2 N3 N4 N5 N6 | D1 D2 D3 D4 D5 D6 | Z1 Z2
#
# 用法:
#   ./26_adminv2_e2b_lifecycle.sh
#   HOLD_SECONDS=15 ./26_adminv2_e2b_lifecycle.sh   # 排障：创建后先观察再继续
#   KEEP_SANDBOX=1  ./26_adminv2_e2b_lifecycle.sh   # 失败时保留沙箱
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib/common.sh disable=SC1091
source "${SCRIPT_DIR}/lib/common.sh"

log_section "26 — admin v2 E2B 创建/暂停/恢复/检查点验证"

#==================== 环境变量 ====================#
API="${API:-http://127.0.0.1:3000}"
DSN="${DSN:-postgres://postgres:local@10.233.90.40:5432/mydatabase?sslmode=disable}"
E2B_VERIFY_NO_RESTART="${E2B_VERIFY_NO_RESTART:-0}"
CREATE_TIMEOUT="${CREATE_TIMEOUT:-600}"
OP_TIMEOUT="${OP_TIMEOUT:-60}"
PAUSE_TIMEOUT="${PAUSE_TIMEOUT:-300}"          # v2 上限 300s（0→60s，>300→300s）
PAUSE_RPC_TIMEOUT="${PAUSE_RPC_TIMEOUT:-360}"  # 必须大于服务端 300s
HOLD_SECONDS="${HOLD_SECONDS:-0}"              # 仅排障用，默认不做稳定性窗口
TCP_TIMEOUT="${TCP_TIMEOUT:-60}"
CLEAN_TIMEOUT="${CLEAN_TIMEOUT:-60}"
GOOSE_MIN_VERSION="${GOOSE_MIN_VERSION:-20260219120000}"
KEEP_SANDBOX="${KEEP_SANDBOX:-0}"

ADMIN_PROTO_DIR="${SCRIPT_DIR}/../proto"
# common.sh 的 PROTO_DIR 指向 CRI api.proto，admin proto 必须用独立变量。

#==================== 全局状态 ====================#
CREATED_IDS=()        # 本脚本创建过的所有 ID（含已删除，供 Z1 核对）
ALIVE_IDS=()          # 当前仍然存在的 ID（trap 逐个 Delete）
FINALIZED_BUILDS=()   # 已写终态的 build
PAUSE_TMPL=""
PAUSE_BUILD=""
CKPT_TMPL=""
CKPT_BUILD=""
CKPT_SNAPSHOT=""
PAUSE_SNAPSHOT_CLEANED=0  # §8 已删除 pause snapshot 引用
CKPT_SNAPSHOT_CLEANED=0   # §8 已删除 checkpoint 快照
OP1=""
OP2=""
STARTED_AT=""
STARTED_AT2=""
NODE_NAME=""
ADMIN_TOKEN=""
SBX_ID=""
SBX_ID2=""
CNI_MODE=0
SCRIPT_START_MS="$(date +%s%3N)"
DETAIL_LOG="${TMP_ROOT}/e2b26-detail.log"
: > "${DETAIL_LOG}"

#==================== 基础工具 ====================#

now_ms() { date +%s%3N; }

fmt_ms() {
    local ms="${1:-0}"
    printf '%d.%03ds' $((ms / 1000)) $((ms % 1000))
}

# JSON 单行化（非法 JSON 原样返回），用于日志
compact_json() {
    printf '%s' "${1:-}" | jq -c . 2>/dev/null || printf '%s' "${1:-}"
}

# 打印脱敏：绝不输出 envd_access_token 明文
redact_json() {
    sed -E 's/("envd_access_token"[[:space:]]*:[[:space:]]*")[^"]*/\1<redacted>/g'
}

# 安全取 JSON 字段：false 不被 // empty 吞掉
jqr() { printf '%s' "${1:-}" | jq -r "($2) | if . == null then empty else . end" 2>/dev/null || true; }

#==================== 用例断言收集 ====================#
# 每条用例最终只产生 1 个 PASS 或 1 个 FAIL（保证 47 条用例 ↔ 47 个计数）。
CASE_ERRS=()

ck() {
    local label="$1" got="$2" want="$3"
    [ "${got}" = "${want}" ] || CASE_ERRS+=("${label} 期望[${want}] 实际[${got}]")
}

ck_nonempty() {
    local label="$1" val="$2"
    [ -n "${val}" ] || CASE_ERRS+=("${label} 为空")
}

ck_true() {
    local label="$1" val="$2"
    [ "${val}" = "true" ] || CASE_ERRS+=("${label} 应为 true，实际[${val:-<empty>}]")
}

ck_contains() {
    local label="$1" hay="$2" needle="$3"
    case "${hay}" in
        *"${needle}"*) ;;
        *) CASE_ERRS+=("${label} 缺少[${needle}]") ;;
    esac
}

ck_ge() {
    local label="$1" got="$2" want="$3"
    if [ -n "${got}" ] && [ "${got}" -ge "${want}" ] 2>/dev/null; then
        return 0
    fi
    CASE_ERRS+=("${label} 期望>=${want} 实际[${got:-<empty>}]")
}

finish_case() {
    local id="$1" desc="$2"
    if [ "${#CASE_ERRS[@]}" -eq 0 ]; then
        log_pass "${id} ${desc}"
    else
        local joined=""
        local e
        for e in "${CASE_ERRS[@]}"; do
            joined+="${e}; "
        done
        log_fail "${id} ${desc} — ${joined}"
    fi
    CASE_ERRS=()
}

#==================== 诊断与退出 ====================#

show_diagnostics() {
    {
        echo "----- 现场快照 -----"
        echo "SBX_ID(A)=${SBX_ID:-<unset>}  SBX_ID2(B)=${SBX_ID2:-<unset>}"
        echo "ALIVE_IDS=${ALIVE_IDS[*]:-<none>}"
        echo "PAUSE_TMPL=${PAUSE_TMPL:-<unset>} PAUSE_BUILD=${PAUSE_BUILD:-<unset>} OP1=${OP1:-<unset>}"
        echo "CKPT_TMPL=${CKPT_TMPL:-<unset>} CKPT_BUILD=${CKPT_BUILD:-<unset>} CKPT_SNAPSHOT=${CKPT_SNAPSHOT:-<unset>} OP2=${OP2:-<unset>}"
        echo "ADMIN_SOCK=${ADMIN_SOCK:-<unset>} API=${API} CNI=${CNI_MODE}"
        echo "----- ${MULTIPLEX_LOG} 尾部 20 行 -----"
        tail -20 "${MULTIPLEX_LOG}" 2>/dev/null || echo "（无 ${MULTIPLEX_LOG}）"
    } >&2
}

die() {
    printf '\n[FATAL] %s\n' "$*" >&2
    show_diagnostics
    exit 1
}

# fatal_case：本条用例失败并且无法继续后续用例（Create/Pause 等关键路径）
fatal_case() {
    local id="$1" desc="$2" reason="$3"
    log_fail "${id} ${desc} — ${reason}"
    die "${id} 失败，无法继续后续用例"
}

#==================== RPC 封装 ====================#

# v2_rpc <Method> <json> [max-time]
#   Method 形如 "SandboxService/Create"；结果写入全局：
#   V2_RC / V2_OUT / V2_ERR / V2_CODE / V2_MSG
#   说明：不向 stdout 输出，调用方一律读全局变量——用 $(...) 捕获会 fork 子 shell，
#   导致 V2_RC/V2_CODE 等全局丢失。
V2_RC=0
V2_OUT=""
V2_ERR=""
V2_CODE=""
V2_MSG=""
V2_ERR_FILE="${TMP_ROOT}/e2b26-v2-rpc.err"

v2_rpc() {
    local method="$1" json="$2" t="${3:-${OP_TIMEOUT}}"
    local started_ms rc=0 dur
    started_ms="$(now_ms)"
    printf '[v2] --> crimultiplex.admin.v2.%s\n' "${method}" >&2
    printf '[v2]   request: %s\n' "$(compact_json "${json}" | redact_json)" >&2
    V2_OUT="$(grpcurl -plaintext -max-time "${t}" \
        -import-path "${ADMIN_PROTO_DIR}" -proto admin_v2.proto \
        -d "${json}" "unix://${ADMIN_SOCK}" \
        "crimultiplex.admin.v2.${method}" 2>"${V2_ERR_FILE}")" || rc=$?
    dur=$(( $(now_ms) - started_ms ))
    V2_RC=${rc}
    V2_ERR="$(cat "${V2_ERR_FILE}" 2>/dev/null || true)"
    V2_CODE="$(printf '%s' "${V2_ERR}" | sed -n 's/^[[:space:]]*Code:[[:space:]]*\([A-Za-z]*\).*/\1/p' | head -1)"
    V2_MSG="$(printf '%s' "${V2_ERR}" | sed -n 's/^[[:space:]]*Message:[[:space:]]*\(.*\)$/\1/p' | head -1)"
    if [ "${rc}" -eq 0 ]; then
        printf '[v2] <-- OK duration=%s response: %s\n' "$(fmt_ms "${dur}")" \
            "$(compact_json "${V2_OUT}" | redact_json)" >&2
    else
        printf '[v2] <-- ERROR duration=%s code=%s msg=%s\n' "$(fmt_ms "${dur}")" \
            "${V2_CODE:-rc=${rc}}" "${V2_MSG:-<none>}" >&2
    fi
}

# E2B API（照搬 22 号）：stdout 只输出 "<http_code>\t<body>"，请求/响应打印走 stderr
api_call() {
    local method="$1" path="$2" body="${3:-}"
    printf '[API] --> %s %s\n' "${method}" "${path}" >&2
    if [ -n "${body}" ]; then
        printf '[API]   request: %s\n' "$(compact_json "${body}")" >&2
    fi
    local args=(-sS -X "${method}" "${API}${path}" -H "X-Admin-Token: ${ADMIN_TOKEN}")
    if [ -n "${body}" ]; then
        args+=(-H "Content-Type: application/json" -d "${body}")
    fi
    local out code payload
    out="$(curl "${args[@]}" -w $'\n%{http_code}' 2>&1)" || true
    code="${out##*$'\n'}"
    payload="${out%$'\n'*}"
    [ -n "${code}" ] || code="000"
    if [ -n "${payload}" ]; then
        printf '[API] <-- http=%s response: %s\n' "${code}" "$(compact_json "${payload}")" >&2
    else
        printf '[API] <-- http=%s (empty body)\n' "${code}" >&2
    fi
    printf '%s\t%s' "${code}" "${payload}"
}

# RuntimeSnapshotMetadata 请求体：op / pod_uid 可变，其余取全局（照搬 22 号）
# $3 = sandbox_started_at（可选，缺省用 STARTED_AT）
snapshot_meta_body() {
    local op="$1" pod_uid="$2" started="${3:-${STARTED_AT}}"
    cat <<EOF
{
  "operation_id": "${op}",
  "team_id": "${TEAM_ID}",
  "base_template_id": "${BASE_ENV}",
  "source_pod_uid": "${pod_uid}",
  "source_node_name": "${NODE_NAME}",
  "sandbox_started_at": "${started}",
  "vcpu": ${VCPU}, "ram_mb": ${RAM_MB}, "total_disk_size_mb": ${DISK_MB},
  "kernel_version": "${KERNEL}",
  "firecracker_version": "${FC_VER}",
  "envd_version": "${ENVD_VER}",
  "secure": false,
  "allow_internet_access": true,
  "metadata": {"source": "admin-v2-26"}
}
EOF
}

# 构造 Create 请求；$1=id $2=template $3=build $4=snapshot(true|false)
# $5=backend（"-" 表示省略该字段）$6=token $7=execution_id
create_payload() {
    local id="$1" tmpl="$2" build="$3" snapshot="$4" backend="$5" token="$6" exec_id="$7"
    jq -n \
        --arg backend "${backend}" \
        --arg id "${id}" --arg tmpl "${tmpl}" --arg build "${build}" \
        --arg alias "v2-${id}" \
        --argjson vcpu "${VCPU}" --argjson ram "${RAM_MB}" --argjson disk "${DISK_MB}" \
        --arg team "${TEAM_ID}" --arg base "${BASE_ENV}" \
        --arg kernel "${KERNEL}" --arg fc "${FC_VER}" --arg envd "${ENVD_VER}" \
        --arg token "${token}" --arg exec_id "${exec_id}" --arg snapshot "${snapshot}" \
        '{
            sandbox: {
              sandbox_id: $id,
              template_id: $tmpl,
              build_id: $build,
              alias: $alias,
              vcpu: $vcpu,
              ram_mb: $ram,
              disk_mb: $disk,
              max_sandbox_length_hours: 1,
              allow_internet: true,
              env_vars: {"E2B_VERIFY_CASE": "26"},
              metadata: {"e2b.dev/expose-ports": "49983"},
              extra: {
                team_id: $team,
                base_template_id: $base,
                kernel_version: $kernel,
                firecracker_version: $fc,
                envd_version: $envd,
                envd_access_token: $token,
                execution_id: $exec_id,
                huge_pages: "true",
                auto_pause: "false",
                snapshot: $snapshot,
                auto_resume_policy: "off"
              }
            }
         }
         + (if $backend == "-" then {} else {backend: $backend} end)'
}

#==================== 轮询辅助 ====================#

# wait_state <id> <want-state> <seconds>：轮询 v2 GetSandboxRuntime
wait_state() {
    local id="$1" want="$2" secs="$3"
    local deadline=$(( $(date +%s) + secs ))
    local state=""
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${id}\"}" 20 >/dev/null
        if [ "${V2_RC}" -eq 0 ]; then
            state="$(jqr "${V2_OUT}" '.runtime.runtimeState')"
            [ "${state}" = "${want}" ] && { echo "${state}"; return 0; }
        fi
        sleep 2
    done
    echo "${state}"
    return 1
}

# wait_tcp <host> <port> <seconds>
wait_tcp() {
    local host="$1" port="$2" secs="$3"
    local deadline=$(( $(date +%s) + secs ))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        # 子 shell 内打开即可，退出时 fd 自动关闭
        if (exec 3<>"/dev/tcp/${host}/${port}") 2>/dev/null; then
            return 0
        fi
        sleep 2
    done
    return 1
}

# list_ids：v2 List 中所有 sandboxId（每行一个）
list_ids() {
    v2_rpc "SandboxService/List" '{"backend":"e2b"}' 30 >/dev/null
    [ "${V2_RC}" -eq 0 ] || return 1
    jqr "${V2_OUT}" '.sandboxes[]?.spec.sandboxId' 2>/dev/null || true
}

# list_count <id>：v2 List 中该 ID 出现次数
list_count() {
    local id="$1" ids
    ids="$(list_ids)" || return 1
    printf '%s\n' "${ids}" | grep -c -x -- "${id}" || true
}

# wait_list_absent <seconds> <id...>
wait_list_absent() {
    local secs="$1"; shift
    local deadline=$(( $(date +%s) + secs ))
    local ids id found
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        ids="$(list_ids)" || { sleep 2; continue; }
        found=0
        for id in "$@"; do
            [ -n "${id}" ] || continue
            if printf '%s\n' "${ids}" | grep -q -x -- "${id}"; then
                found=1
            fi
        done
        [ "${found}" -eq 0 ] && return 0
        sleep 2
    done
    return 1
}

# cri_pods_contains <id>：CRI ListPodSandbox 是否仍含该 ID
cri_pods_contains() {
    local id="$1" all
    all="$(${CRICTL} pods -q 2>/dev/null || true)"
    printf '%s\n' "${all}" | grep -q -- "${id}"
}

#==================== 沙箱清理 ====================#

# alive_push <id>
alive_push() {
    local id="$1"
    CREATED_IDS+=("${id}")
    ALIVE_IDS+=("${id}")
}

# alive_pop <id>
alive_pop() {
    local id="$1" out=() i
    for i in "${ALIVE_IDS[@]:-}"; do
        [ -n "${i}" ] || continue
        [ "${i}" = "${id}" ] || out+=("${i}")
    done
    ALIVE_IDS=()
    if [ "${#out[@]}" -gt 0 ]; then
        ALIVE_IDS=("${out[@]}")
    fi
}

# v2_delete <id> [backend]：删除并维护 ALIVE_IDS
v2_delete() {
    local id="$1" backend="${2:-e2b}"
    v2_rpc "SandboxService/Delete" "{\"backend\":\"${backend}\",\"sandbox_id\":\"${id}\"}" "${OP_TIMEOUT}" >/dev/null
    if [ "${V2_RC}" -eq 0 ]; then
        alive_pop "${id}"
        return 0
    fi
    return 1
}

# shellcheck disable=SC2317  # 只由 cleanup 调用
print_manual_delete_cmd() {
    local id="$1"
    printf '  grpcurl -plaintext -import-path cri-multiplex/proto -proto admin_v2.proto \\\n' >&2
    printf "    -d '{\"backend\":\"e2b\",\"sandbox_id\":\"%s\"}' unix://%s \\\\\n" "${id}" "${ADMIN_SOCK}" >&2
    printf '    crimultiplex.admin.v2.SandboxService/Delete\n' >&2
}

# shellcheck disable=SC2317  # 只由 trap 调用，静态不可见
cleanup() {
    local rc=$?
    trap - EXIT
    set +e

    echo "" >&2
    echo "==================== 清理（trap EXIT，rc=${rc}） ====================" >&2

    if [ "${KEEP_SANDBOX}" = "1" ] && [ "${#ALIVE_IDS[@]}" -gt 0 ]; then
        log_warn "KEEP_SANDBOX=1，保留沙箱用于排障（删除命令见下）"
        local id
        for id in "${ALIVE_IDS[@]}"; do
            log_info "sandbox_id=${id}"
            print_manual_delete_cmd "${id}"
        done
    else
        local id attempt
        for id in "${ALIVE_IDS[@]:-}"; do
            [ -n "${id}" ] || continue
            for attempt in 1 2; do
                log_info "v2 Delete ${id}（第 ${attempt} 次）"
                if v2_delete "${id}"; then
                    log_info "已删除 ${id}"
                    break
                fi
                if [ "${attempt}" -eq 2 ]; then
                    log_warn "v2 Delete ${id} 失败，可手工执行:"
                    print_manual_delete_cmd "${id}"
                fi
            done
        done
    fi

    # 未收尾的 build 尽力写 failed，避免 API 侧卡在 building
    local build
    for build in "${PAUSE_BUILD}" "${CKPT_BUILD}"; do
        [ -n "${build}" ] || continue
        case " ${FINALIZED_BUILDS[*]:-} " in
            *" ${build} "*) continue ;;
        esac
        local op="${OP1}"
        [ "${build}" = "${CKPT_BUILD}" ] && op="${OP2}"
        local resp code
        resp="$(api_call POST "/internal/builds/${build}/status" "{\"operation_id\":\"${op}\",\"status\":\"failed\"}")"
        code="${resp%%$'\t'*}"
        if [ "${code}" = "200" ]; then
            log_info "build ${build} 已尽力写 failed"
        else
            log_warn "build ${build} 写 failed 返回 http=${code}（可能已 success 或不存在）"
        fi
    done

    # 已预分配但未收尾的快照引用：避免 API 侧残留 paused snapshot / checkpoint 模板
    # （正常路径由 §8 收尾，收尾成功会把 *_CLEANED 置 1）
    local resp code
    if [ -n "${PAUSE_TMPL}" ] && [ "${PAUSE_SNAPSHOT_CLEANED}" = "0" ]; then
        resp="$(api_call DELETE "/internal/sandboxes/${SBX_ID}/snapshot")"
        code="${resp%%$'\t'*}"
        if [ "${code}" = "204" ]; then
            log_info "（清理）已删除 pause snapshot 引用"
        else
            log_warn "（清理）DELETE sandboxes/${SBX_ID}/snapshot 返回 http=${code}"
        fi
    fi
    if [ -n "${CKPT_SNAPSHOT}" ] && [ "${CKPT_SNAPSHOT_CLEANED}" = "0" ]; then
        resp="$(api_call DELETE "/internal/snapshot-templates/${CKPT_SNAPSHOT}?team_id=${TEAM_ID}")"
        code="${resp%%$'\t'*}"
        if [ "${code}" = "204" ]; then
            log_info "（清理）已删除 checkpoint 快照 ${CKPT_SNAPSHOT}"
        else
            log_warn "（清理）DELETE snapshot-templates/${CKPT_SNAPSHOT} 返回 http=${code}"
        fi
    fi

    echo "" >&2
    echo "==================== 结果汇总 ====================" >&2
    echo "sandbox_id(A)=${SBX_ID:-<unset>}  source_pod_uid=${SBX_ID:-<unset>}" >&2
    echo "sandbox_id(B)=${SBX_ID2:-<unset>}" >&2
    echo "client_id=${CLIENT_ID:-<unset>}  host_ip=${HOST_IP:-<unset>}  pod_ip=${POD_IP:-<unset>}" >&2
    echo "netns=${NETNS_PATH:-<unset>}  host_port=${HOST_PORT:-<unset>}" >&2
    echo "pause_snapshot=${PAUSE_TMPL:-?}/${PAUSE_BUILD:-?}  ckpt_snapshot=${CKPT_SNAPSHOT:-?}" >&2
    echo "总耗时=$(fmt_ms $(( $(now_ms) - SCRIPT_START_MS )))" >&2
    print_summary

    exit "${rc}"
}
trap cleanup EXIT
# Ctrl-C / SIGTERM 也必须走清理（只 trap EXIT 的话，信号默认终止不会触发 cleanup，
# 会留下已创建的沙箱与未收尾的 build）。与 run_all.sh 的约定一致。
trap 'exit 130' INT
trap 'exit 143' TERM

#==================== §0 预检 ====================#

log_step "0. 预检（P1-P7）"

# --- P1 工具 ---
for tool in grpcurl jq openssl curl; do
    command -v "${tool}" >/dev/null 2>&1 || CASE_ERRS+=("${tool} 不可用")
done
ck_nonempty "grpcurl 路径" "$(command -v grpcurl || true)"
finish_case "P1" "工具可用（grpcurl/jq/openssl/curl）"
command -v jq >/dev/null 2>&1 || die "jq 不可用，无法继续"
command -v grpcurl >/dev/null 2>&1 || die "grpcurl 不可用，无法继续"

# --- P2 proto 文件 ---
[ -f "${ADMIN_PROTO_DIR}/admin_v2.proto" ] || CASE_ERRS+=("admin_v2.proto 不存在")
[ -f "${ADMIN_PROTO_DIR}/admin.proto" ] || CASE_ERRS+=("admin.proto 不存在")
finish_case "P2" "proto 文件存在（admin_v2.proto / admin.proto）"
[ -f "${ADMIN_PROTO_DIR}/admin_v2.proto" ] || die "缺少 proto/admin_v2.proto"

# 启动/接管 mux 是前置动作，不是被测用例：START_CNI_ANDROID_COUNT=0 让它走
# log_info 而不是 log_pass，保证 PASS 计数严格对应 47 条用例。
export START_CNI_ANDROID_COUNT=0

# --- P3 受控实例 ---
if [ "${E2B_VERIFY_NO_RESTART}" = "1" ]; then
    log_info "E2B_VERIFY_NO_RESTART=1：跳过重启，仅检查当前实例"
    cri_multiplex_ready || CASE_ERRS+=("当前实例不可用（E2B_VERIFY_NO_RESTART=1）")
else
    if ! ensure_controlled_cni_multiplex "26 — 准备 cri-multiplex CNI 受控实例"; then
        CASE_ERRS+=("ensure_controlled_cni_multiplex 失败")
    fi
    cri_multiplex_ready || CASE_ERRS+=("cri_multiplex_ready 为假")
fi
finish_case "P3" "受控 cri-multiplex 实例就绪"

if cri_multiplex_cni_enabled 2>/dev/null; then CNI_MODE=1; fi

# --- P4 admin socket ---
resolve_admin_socket() {
    local cmdline val=""
    cmdline="$(cri_multiplex_cmdline 2>/dev/null || true)"
    if [ -n "${cmdline}" ]; then
        val="$(printf '%s' "${cmdline}" | sed -n 's/.*-admin-socket[= ]\([^ ]*\).*/\1/p' | head -1)"
    fi
    printf '%s' "${val:-/run/cri-multiplex/admin.sock}"
}
ADMIN_SOCK="${ADMIN_SOCK:-$(resolve_admin_socket)}"
[ -S "${ADMIN_SOCK}" ] || CASE_ERRS+=("${ADMIN_SOCK} 不存在或不是 socket")
finish_case "P4" "admin socket 存在（${ADMIN_SOCK}）"
[ -S "${ADMIN_SOCK}" ] || die "admin socket ${ADMIN_SOCK} 不可用"

# --- P5 v2 已注册：调用 + 期望 InvalidArgument（server 未开 reflection，不能用 list） ---
v2_rpc "SandboxAdminService/GetSandboxRuntime" '{"backend":"e2b"}' 30
case "${V2_CODE}" in
    InvalidArgument)
        ;;
    Unimplemented)
        CASE_ERRS+=("v2 服务未注册（unknown service），运行实例是旧二进制：请用 E2B_FORCE_RESTART=1 ./01_start_multiplex.sh 启动本 checkout 构建")
        ;;
    "")
        CASE_ERRS+=("调用失败且无 gRPC Code（rc=${V2_RC}），检查 admin.sock 与运行实例")
        ;;
    *)
        CASE_ERRS+=("期望 InvalidArgument，实际 ${V2_CODE}: ${V2_MSG}")
        ;;
esac
finish_case "P5" "admin v2 已注册（GetSandboxRuntime 返回 InvalidArgument）"
if [ "${V2_CODE}" = "Unimplemented" ] || [ -z "${V2_CODE}" ]; then
    die "admin v2 未就绪：请先执行 cd cri-multiplex/e2b-verify && E2B_FORCE_RESTART=1 ./01_start_multiplex.sh"
fi

# --- P6 模板元数据 ---
if ! resolve_e2b_template; then
    CASE_ERRS+=("resolve_e2b_template 失败")
fi
TEAM_ID="${E2B_TEAM_ID}"
BASE_ENV="${E2B_TEMPLATE_ID}"
BASE_BUILD="${E2B_BUILD_ID}"
KERNEL="${E2B_KERNEL_VERSION}"
FC_VER="${E2B_FIRECRACKER_VERSION}"
ENVD_VER="${E2B_ENVD_VERSION}"
VCPU="${E2B_VCPU}"
RAM_MB="${E2B_RAM_MB}"
DISK_MB="${E2B_DISK_MB}"
ck_nonempty "team_id" "${TEAM_ID}"
ck_nonempty "template_id" "${BASE_ENV}"
ck_nonempty "build_id" "${BASE_BUILD}"
ck_nonempty "kernel_version" "${KERNEL}"
ck_nonempty "firecracker_version" "${FC_VER}"
ck_nonempty "envd_version" "${ENVD_VER}"
ck_ge "vcpu" "${VCPU}" 1
ck_ge "ram_mb" "${RAM_MB}" 1
ck_ge "disk_mb" "${DISK_MB}" 1
finish_case "P6" "模板元数据完整（team/template/build/kernel/fc/envd/规格）"
[ "${#CASE_ERRS[@]}" -eq 0 ] || die "模板元数据不完整，无法继续"

# --- P7 API 前置（Pause/Checkpoint 必需） ---
API_PID="$(pgrep -f '^/api --port' | head -1 || true)"
if [ -n "${API_PID}" ]; then
    ADMIN_TOKEN="$(tr '\0' '\n' < "/proc/${API_PID}/environ" 2>/dev/null | sed -n 's/^ADMIN_TOKEN=//p' | head -1 || true)"
fi
ck_nonempty "ADMIN_TOKEN" "${ADMIN_TOKEN}"
if [ -n "${ADMIN_TOKEN}" ]; then
    nohdr_code="$(curl -sS -o /dev/null -w '%{http_code}' \
        "${API}/internal/sandboxes/zzzzzzzzzzzzzzzzzzzz/snapshot-state" 2>/dev/null || true)"
    ck "无 header snapshot-state" "${nohdr_code}" "401"
    resp="$(api_call GET "/internal/sandboxes/zzzzzzzzzzzzzzzzzzzz/snapshot-state")"
    hdr_code="${resp%%$'\t'*}"
    if [ "${hdr_code}" = "401" ] || [ "${hdr_code}" = "000" ]; then
        CASE_ERRS+=("带 token snapshot-state 返回 ${hdr_code}（token 无效或 API 不可达）")
    fi
fi
cur_ver="$(kubectl -n e2b exec deploy/postgres -- psql -U postgres -d mydatabase -tA \
    -c 'select max(version_id) from "_migrations"' 2>/dev/null | tr -d '[:space:]' || true)"
if [ -z "${cur_ver}" ]; then
    CASE_ERRS+=("无法查询 _migrations")
elif [ "${cur_ver}" -lt "${GOOSE_MIN_VERSION}" ]; then
    log_info "迁移版本 ${cur_ver} < ${GOOSE_MIN_VERSION}，执行 goose up"
    if (cd "${SCRIPT_DIR}/../../packages/db" && \
        GOOSE_DBSTRING="${DSN}" go tool goose -table _migrations -dir migrations postgres up) >>"${DETAIL_LOG}" 2>&1; then
        cur_ver="$(kubectl -n e2b exec deploy/postgres -- psql -U postgres -d mydatabase -tA \
            -c 'select max(version_id) from "_migrations"' 2>/dev/null | tr -d '[:space:]' || true)"
    else
        CASE_ERRS+=("goose up 失败，见 ${DETAIL_LOG}")
    fi
    [ -n "${cur_ver}" ] && [ "${cur_ver}" -ge "${GOOSE_MIN_VERSION}" ] || CASE_ERRS+=("迁移后版本仍不足: ${cur_ver:-<empty>}")
fi
finish_case "P7" "API 前置就绪（ADMIN_TOKEN/鉴权/迁移 ${cur_ver:-?}）"
[ "${#CASE_ERRS[@]}" -eq 0 ] || die "API 前置不满足，Pause/Checkpoint 会在预分配阶段失败"

#==================== §0.1 残留清理（步骤，不计用例） ====================#

log_step "0.1 清理残留（v2e2b 前缀）与环境画像"

residue_ids="$(list_ids || true)"
to_delete=()
for id in ${residue_ids}; do
    case "${id}" in
        v2e2b*) to_delete+=("${id}") ;;
    esac
done
if [ "${#to_delete[@]}" -gt 0 ]; then
    log_warn "发现 ${#to_delete[@]} 个残留沙箱，逐个删除（最多 3 个）"
    n=0
    for id in "${to_delete[@]}"; do
        n=$((n + 1))
        [ "${n}" -le 3 ] || break
        v2_delete "${id}" || die "残留沙箱 ${id} 删除失败，请手工清理后重试"
        log_info "已删除残留 ${id}"
    done
else
    log_info "无 v2e2b 前缀残留沙箱"
fi

NODE_NAME="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
[ -n "${NODE_NAME}" ] || NODE_NAME="$(hostname)"

KUBELET_ENDPOINT="$(kubelet_runtime_endpoint 2>/dev/null || true)"
HIDE_LABEL="$(printf '%s' "$(cri_multiplex_cmdline 2>/dev/null || true)" | sed -n 's/.*-hide-sandbox-label[= ]\([^ ]*\).*/\1/p' | head -1)"
log_info "环境画像: cni=${CNI_MODE} hide_label=${HIDE_LABEL:-<none>} kubelet_endpoint=${KUBELET_ENDPOINT:-<unknown>} socket=${SOCKET} admin_sock=${ADMIN_SOCK} api=${API} orchestrator=${ORCHESTRATOR_ADDRESS} node=${NODE_NAME}"

#==================== §1 Create（C1-C3） ====================#

log_step "1. Create（C1 首次 / C2 幂等 / C3 不重复创建）"

SBX_ID="v2e2b$(head -c 256 /dev/urandom | tr -dc 'a-z0-9' | cut -c1-15)"
ALIAS="v2-${SBX_ID}"
ENVD_TOKEN="$(_new_envd_token)"
EXEC_ID="$(_new_execution_id)"
log_info "SBX_ID=${SBX_ID} ALIAS=${ALIAS} EXEC_ID=${EXEC_ID}"

# --- C1 首次创建（完整创建请求） ---
CREATE_JSON="$(create_payload "${SBX_ID}" "${BASE_ENV}" "${BASE_BUILD}" "false" "e2b" "${ENVD_TOKEN}" "${EXEC_ID}")"
t0="$(now_ms)"
v2_rpc "SandboxService/Create" "${CREATE_JSON}" "${CREATE_TIMEOUT}"
CREATE_MS=$(( $(now_ms) - t0 ))
if [ "${V2_RC}" -ne 0 ]; then
    CASE_ERRS+=("Create 失败 code=${V2_CODE} msg=${V2_MSG}")
    fatal_case "C1" "首次创建（backend=e2b）" "code=${V2_CODE} msg=${V2_MSG}"
fi
CREATE_RESP="${V2_OUT}"
CLIENT_ID="$(jqr "${CREATE_RESP}" '.clientId')"
HOST_IP="$(jqr "${CREATE_RESP}" '.hostIp')"
ck ".backend" "$(jqr "${CREATE_RESP}" '.backend')" "e2b"
ck ".sandboxId" "$(jqr "${CREATE_RESP}" '.sandboxId')" "${SBX_ID}"
ck_nonempty ".clientId" "${CLIENT_ID}"
ck_nonempty ".hostIp" "${HOST_IP}"
finish_case "C1" "首次创建成功（backend=e2b，耗时 $(fmt_ms "${CREATE_MS}")，clientId=${CLIENT_ID} hostIp=${HOST_IP}）"
[ "${#CASE_ERRS[@]}" -eq 0 ] || die "首次 Create 断言失败，无法继续"
alive_push "${SBX_ID}"

# --- C2 幂等重试（省略 backend，同时覆盖“空 = e2b”） ---
RETRY_JSON="$(create_payload "${SBX_ID}" "${BASE_ENV}" "${BASE_BUILD}" "false" "-" "${ENVD_TOKEN}" "${EXEC_ID}")"
v2_rpc "SandboxService/Create" "${RETRY_JSON}" "${OP_TIMEOUT}"
RETRY_RESP="${V2_OUT}"
RETRY_CLIENT="$(jqr "${RETRY_RESP}" '.clientId')"
ck "退出码" "${V2_RC}" "0"
ck ".sandboxId" "$(jqr "${RETRY_RESP}" '.sandboxId')" "${SBX_ID}"
ck ".hostIp 与首次相同" "$(jqr "${RETRY_RESP}" '.hostIp')" "${HOST_IP}"
if [ -n "${RETRY_CLIENT}" ] && [ "${RETRY_CLIENT}" != "${CLIENT_ID}" ]; then
    CASE_ERRS+=(".clientId 应留空或等于首次[${CLIENT_ID}]，实际[${RETRY_CLIENT}]")
fi
finish_case "C2" "幂等重试成功（省略 backend，hostIp 不变，clientId=${RETRY_CLIENT:-<空>}）"

# --- C3 不重复创建 ---
count="$(list_count "${SBX_ID}" || echo "-1")"
ck "List 中该 ID 条数" "${count}" "1"
finish_case "C3" "v2 List 中该 ID 恰好 1 条"

#==================== §2 读一致性 + 数据面（V1-V4） ====================#

log_step "2. 读一致性与数据面（V1-V4）"

if [ "${HOLD_SECONDS}" -gt 0 ] 2>/dev/null; then
    log_info "HOLD_SECONDS=${HOLD_SECONDS}：等待后再复查（仅排障用，不计用例）"
    sleep "${HOLD_SECONDS}"
fi

# --- V1 v2 Get（target=sandbox_id） ---
v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID}\"}" "${OP_TIMEOUT}"
RT="${V2_OUT}"
POD_IP="$(jqr "${RT}" '.runtime.podIp')"
NETNS_PATH="$(jqr "${RT}" '.runtime.netnsPath')"
NODE_NAME_RT="$(jqr "${RT}" '.runtime.nodeName')"
ck ".runtime.backend" "$(jqr "${RT}" '.runtime.backend')" "e2b"
ck ".runtime.criSandboxId" "$(jqr "${RT}" '.runtime.criSandboxId')" "${SBX_ID}"
ck ".runtime.sandboxId" "$(jqr "${RT}" '.runtime.sandboxId')" "${SBX_ID}"
ck ".runtime.runtimeState" "$(jqr "${RT}" '.runtime.runtimeState')" "Running"
ck ".runtime.templateId" "$(jqr "${RT}" '.runtime.templateId')" "${BASE_ENV}"
ck ".runtime.buildId" "$(jqr "${RT}" '.runtime.buildId')" "${BASE_BUILD}"
ck ".runtime.podName" "$(jqr "${RT}" '.runtime.podName')" "${ALIAS}"
ck ".runtime.podNamespace" "$(jqr "${RT}" '.runtime.podNamespace')" "e2b-admin"
ck ".runtime.podUid" "$(jqr "${RT}" '.runtime.podUid')" "${SBX_ID}"
ck_nonempty ".runtime.hostIp" "$(jqr "${RT}" '.runtime.hostIp')"
ck "activeOperations 条数" "$(jqr "${RT}" '(.runtime.activeOperations // []) | length')" "0"
if [ -n "${NODE_NAME_RT}" ]; then
    ck ".runtime.nodeName（非空时比对）" "${NODE_NAME_RT}" "${NODE_NAME}"
fi
finish_case "V1" "v2 Get 运行事实正确（Running / e2b-admin / podUid=ID）"

# --- V2 v2 List ---
v2_rpc "SandboxService/List" '{"backend":"e2b"}' "${OP_TIMEOUT}"
LIST="${V2_OUT}"
LIST_ONE="$(jqr "${LIST}" "[.sandboxes[]? | select(.spec.sandboxId == \"${SBX_ID}\")] | first")"
ck "List 中条数" "$(jqr "${LIST}" "[.sandboxes[]? | select(.spec.sandboxId == \"${SBX_ID}\")] | length")" "1"
ck ".spec.templateId" "$(jqr "${LIST_ONE}" '.spec.templateId')" "${BASE_ENV}"
ck ".spec.buildId" "$(jqr "${LIST_ONE}" '.spec.buildId')" "${BASE_BUILD}"
ck ".runtimeState" "$(jqr "${LIST_ONE}" '.runtimeState')" "Running"
LIST_POD_IP="$(jqr "${LIST_ONE}" '.podIp')"
if [ "${CNI_MODE}" = "1" ]; then
    ck_nonempty "List .podIp（CNI）" "${LIST_POD_IP}"
else
    log_warn "CNI 未开启，跳过 List .podIp 断言"
fi
finish_case "V2" "v2 List 反映 orchestrator 视图（template/build/Running/podIp）"

# --- V3 数据面（hostport 硬断言；podIP 软断言） ---
# 用 runtime.hostIp（节点 IP），不能用 Create.hostIp（orchestrator slot IP）
RUNTIME_HOST_IP="$(jqr "${RT}" '.runtime.hostIp')"
ck_nonempty ".runtime.hostIp" "${RUNTIME_HOST_IP}"
HOST_PORT="$(jqr "${RT}" '[.runtime.hostports[]? | select(.sandboxPort == 49983) | .hostPort] | first')"
ck_nonempty "hostports 含 sandboxPort=49983" "${HOST_PORT}"
if [ -n "${HOST_PORT}" ]; then
    ck_ge "hostPort" "${HOST_PORT}" 1
    if wait_tcp "${RUNTIME_HOST_IP}" "${HOST_PORT}" "${TCP_TIMEOUT}"; then
        :
    else
        CASE_ERRS+=("TCP ${RUNTIME_HOST_IP}:${HOST_PORT} 在 ${TCP_TIMEOUT}s 内不可连")
    fi
fi
if [ "${CNI_MODE}" = "1" ] && [ -n "${POD_IP}" ]; then
    if wait_tcp "${POD_IP}" 49983 "${TCP_TIMEOUT}"; then
        :
    else
        log_warn "CNI podIP ${POD_IP}:49983 不可连（软断言，不阻塞）"
    fi
else
    log_warn "CNI 未开启或无 podIp，跳过 podIP:49983（软断言）"
fi
finish_case "V3" "数据面可达（hostPort=${HOST_PORT:-?}）"

# --- V4 netns ---
if [ "${CNI_MODE}" = "1" ]; then
    ck_nonempty ".runtime.netnsPath" "${NETNS_PATH}"
    if [ -n "${NETNS_PATH}" ] && [ ! -e "${NETNS_PATH}" ]; then
        CASE_ERRS+=("netns ${NETNS_PATH} 不存在")
    fi
    finish_case "V4" "netns 存在（${NETNS_PATH}）"
else
    log_skip "V4 netns：CNI 未开启，跳过"
fi

# STARTED_AT 供 Pause 的 API meta 使用（v2 List.startTime）
STARTED_AT="$(jqr "${LIST_ONE}" '.startTime')"
if [ -z "${STARTED_AT}" ]; then
    STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    log_warn "List.startTime 为空，sandbox_started_at 用当前时间兜底: ${STARTED_AT}"
else
    log_info "STARTED_AT=${STARTED_AT}"
fi

#==================== §2.1 Update 与构建缓存（U1 / L1） ====================#

log_step "2.1 Update 与构建缓存（U1 / L1）"

# --- U1 Update 改 TTL ---
# v2 没有 endTime 的读接口，只能经 List 核对；且 Update 只带 sandbox_id 会把
# end_time 置零（orchestrator 直接 SetEndAt），所以必须显式带 end_time。
# 注意：TTL 只出现在 List 响应顶层 .endTime —— SandboxSpec 里没有 start/end_time
# 字段（sandboxSpecToProto 也不写），不要断言 .spec.endTime。
U1_END_S=$(( $(date +%s) + 7200 ))
U1_END="$(date -u -d "@${U1_END_S}" +%Y-%m-%dT%H:%M:%SZ)"
U1_BEFORE="$(jqr "${LIST_ONE}" '.endTime')"
v2_rpc "SandboxService/Update" \
    "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID}\",\"end_time\":\"${U1_END}\"}" "${OP_TIMEOUT}"
ck "退出码" "${V2_RC}" "0"
v2_rpc "SandboxService/List" '{"backend":"e2b"}' "${OP_TIMEOUT}"
U1_ONE="$(jqr "${V2_OUT}" "[.sandboxes[]? | select(.spec.sandboxId == \"${SBX_ID}\")] | first")"
U1_GOT="$(jqr "${U1_ONE}" '.endTime')"
U1_GOT_S=""
[ -n "${U1_GOT}" ] && U1_GOT_S="$(date -u -d "${U1_GOT}" +%s 2>/dev/null || true)"
ck "List .endTime 已更新" "${U1_GOT_S}" "${U1_END_S}"
if [ -n "${U1_BEFORE}" ]; then
    # Update 前是 Create 算出的 end_time（max_sandbox_length_hours=1），必须真的变了。
    ck ".endTime 相对 Update 前已变化" "$([ "${U1_GOT}" = "${U1_BEFORE}" ] && echo 未变 || echo 已变)" "已变"
fi
ck ".runtimeState 仍为 Running" "$(jqr "${U1_ONE}" '.runtimeState')" "Running"
ck ".spec.templateId 未被改动" "$(jqr "${U1_ONE}" '.spec.templateId')" "${BASE_ENV}"
ck ".spec.buildId 未被改动" "$(jqr "${U1_ONE}" '.spec.buildId')" "${BASE_BUILD}"
finish_case "U1" "Update 改 TTL 生效（List endTime=${U1_GOT:-?}，状态/模板未变）"

# --- L1 ListCachedBuilds ---
# 本节点刚用 BASE_BUILD 创建过沙箱，模板缓存（ttl 25h，key=build_id）里应有它。
v2_rpc "SandboxService/ListCachedBuilds" '{"backend":"e2b"}' "${OP_TIMEOUT}"
LCB="${V2_OUT}"
LCB_N="$(jqr "${LCB}" '(.builds // []) | length')"
ck "退出码" "${V2_RC}" "0"
ck_ge "缓存 build 条数" "${LCB_N}" 1
ck "buildId 非空条数" "$(jqr "${LCB}" '[.builds[]? | select((.buildId // "") != "")] | length')" "${LCB_N}"
ck "expirationTime 齐全" "$(jqr "${LCB}" '[.builds[]? | select(.expirationTime == null)] | length')" "0"
ck_contains "缓存包含 BASE_BUILD" "$(jqr "${LCB}" '[.builds[]?.buildId] | join(",")')" "${BASE_BUILD}"
finish_case "L1" "ListCachedBuilds 返回本节点缓存（${LCB_N:-?} 条，含 BASE_BUILD）"

#==================== §3 Pause（PZ1-PZ5） ====================#

log_step "3. Pause（API 预分配 → v2 PauseSandbox → Paused → 幂等 → 删源沙箱）"

# --- PZ1 API 预分配（硬前置，失败即 die） ---
OP1="v2-pause-$(date +%s)"
log_info "POST /internal/sandboxes/${SBX_ID}/pause-snapshot op=${OP1}"
resp="$(api_call POST "/internal/sandboxes/${SBX_ID}/pause-snapshot" "$(snapshot_meta_body "${OP1}" "${SBX_ID}")")"
code="${resp%%$'\t'*}"
body="${resp#*$'\t'}"
if [ "${code}" != "200" ]; then
    CASE_ERRS+=("pause-snapshot 返回 http=${code}: ${body}")
    fatal_case "PZ1" "API 预分配 pause-snapshot" "http=${code}"
fi
PAUSE_TMPL="$(jqr "${body}" '.template_id')"
PAUSE_BUILD="$(jqr "${body}" '.build_id')"
ck_nonempty "PAUSE_TMPL" "${PAUSE_TMPL}"
ck_nonempty "PAUSE_BUILD" "${PAUSE_BUILD}"
finish_case "PZ1" "pause-snapshot 预分配成功（template=${PAUSE_TMPL} build=${PAUSE_BUILD}）"
[ "${#CASE_ERRS[@]}" -eq 0 ] || die "PAUSE_TMPL/PAUSE_BUILD 缺失，无法继续"

# --- PZ2 v2 PauseSandbox（backend 省略，target=sandbox_id） ---
PAUSE_JSON="$(jq -n \
    --arg op "${OP1}" --arg id "${SBX_ID}" --argjson t "${PAUSE_TIMEOUT}" \
    --arg team "${TEAM_ID}" --arg tmpl "${PAUSE_TMPL}" --arg build "${PAUSE_BUILD}" \
    '{operation_id: $op, timeout_seconds: $t, sandbox_id: $id,
      metadata: {team_id: $team, template_id: $tmpl, build_id: $build}}')"
v2_rpc "SandboxAdminService/PauseSandbox" "${PAUSE_JSON}" "${PAUSE_RPC_TIMEOUT}"
PAUSE_RESP="${V2_OUT}"
PZ2_STARTED="$(jqr "${PAUSE_RESP}" '.startedAtUnix')"
PZ2_FINISHED="$(jqr "${PAUSE_RESP}" '.finishedAtUnix')"
ck "退出码" "${V2_RC}" "0"
ck_true "snapshotPersisted" "$(jqr "${PAUSE_RESP}" '.snapshotPersisted')"
ck_ge "startedAtUnix" "${PZ2_STARTED}" 1
ck_ge "finishedAtUnix>=startedAtUnix" "${PZ2_FINISHED}" "${PZ2_STARTED:-0}"
finish_case "PZ2" "v2 PauseSandbox 成功（target=sandbox_id，snapshotPersisted=true）"
if [ "${V2_RC}" -ne 0 ]; then
    die "Pause 失败 code=${V2_CODE} msg=${V2_MSG}（现场已保留，PAUSE_TMPL/BUILD 见汇总）"
fi

# --- PZ3 暂停状态（只能看 v2 Get） ---
# Pause 时 orchestrator 会把沙箱从 live 集合摘除（acquireSandboxForSnapshot → Remove），
# 所以暂停后 v2 List 里根本查不到它，List 也无法表达 Paused；状态只能由 Get 观察。
PZ3_STATE="$(wait_state "${SBX_ID}" "Paused" 30 || true)"
ck ".runtime.runtimeState" "${PZ3_STATE}" "Paused"
finish_case "PZ3" "v2 Get 状态为 Paused"

# --- PZ4 幂等重试（同 op1） ---
v2_rpc "SandboxAdminService/PauseSandbox" "${PAUSE_JSON}" "${PAUSE_RPC_TIMEOUT}"
PZ4_RESP="${V2_OUT}"
ck "退出码" "${V2_RC}" "0"
ck_true "snapshotPersisted" "$(jqr "${PZ4_RESP}" '.snapshotPersisted')"
ck "startedAtUnix 沿用首次" "$(jqr "${PZ4_RESP}" '.startedAtUnix')" "${PZ2_STARTED}"
finish_case "PZ4" "同 operation_id 幂等重试成功（时间沿用首次）"

# --- PZ5 删除源沙箱 + Get NotFound ---
t0="$(now_ms)"
if v2_delete "${SBX_ID}"; then
    PZ5_MS=$(( $(now_ms) - t0 ))
    v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID}\"}" "${OP_TIMEOUT}"
    ck "Get 已删 A 的 Code" "${V2_CODE}" "NotFound"
    finish_case "PZ5" "v2 Delete 源沙箱成功（$(fmt_ms "${PZ5_MS}")），Get → NotFound"
else
    CASE_ERRS+=("Delete 失败 code=${V2_CODE} msg=${V2_MSG}")
    finish_case "PZ5" "v2 Delete 源沙箱 + Get NotFound"
    die "删除源沙箱失败，无法继续 Resume"
fi

# --- API 收尾：build success + last-snapshot（不计数，仅 log） ---
resp="$(api_call GET "/internal/sandboxes/${SBX_ID}/snapshot-state")"
code="${resp%%$'\t'*}"
body="${resp#*$'\t'}"
if [ "${code}" = "200" ] && [ "$(jqr "${body}" '.paused')" = "true" ] && [ "$(jqr "${body}" '.build_id')" = "${PAUSE_BUILD}" ]; then
    log_info "（收尾）snapshot-state paused=true 且 build 匹配"
else
    log_warn "（收尾）snapshot-state http=${code} body=$(compact_json "${body}")"
fi

resp="$(api_call POST "/internal/builds/${PAUSE_BUILD}/status" "{\"operation_id\":\"${OP1}\",\"status\":\"success\"}")"
code="${resp%%$'\t'*}"
if [ "${code}" = "200" ]; then
    FINALIZED_BUILDS+=("${PAUSE_BUILD}")
    log_info "（收尾）builds/status ${PAUSE_BUILD} → success"
else
    log_warn "（收尾）builds/status ${PAUSE_BUILD} 返回 http=${code}"
fi

resp="$(api_call GET "/internal/sandboxes/${SBX_ID}/last-snapshot")"
code="${resp%%$'\t'*}"
body="${resp#*$'\t'}"
if [ "${code}" = "200" ] && [ "$(jqr "${body}" '.build_id')" = "${PAUSE_BUILD}" ]; then
    log_info "（收尾）last-snapshot build 匹配，origin_node=$(jqr "${body}" '.origin_node')"
else
    log_warn "（收尾）last-snapshot http=${code} body=$(compact_json "${body}")"
fi

#==================== §4 Resume（R1-R3） ====================#

log_step "4. Resume（v2 Create snapshot=true，同一 sandbox_id）"

RESUME_EXEC="$(_new_execution_id)"
RESUME_JSON="$(create_payload "${SBX_ID}" "${PAUSE_TMPL}" "${PAUSE_BUILD}" "true" "e2b" "${ENVD_TOKEN}" "${RESUME_EXEC}")"
v2_rpc "SandboxService/Create" "${RESUME_JSON}" "${CREATE_TIMEOUT}"
RESUME_RESP="${V2_OUT}"
RESUME_HOST_IP="$(jqr "${RESUME_RESP}" '.hostIp')"
ck "退出码" "${V2_RC}" "0"
ck ".sandboxId" "$(jqr "${RESUME_RESP}" '.sandboxId')" "${SBX_ID}"
ck_nonempty ".clientId" "$(jqr "${RESUME_RESP}" '.clientId')"
ck_nonempty ".hostIp" "${RESUME_HOST_IP}"
finish_case "R1" "v2 Resume 创建成功（同一 sandbox_id，PAUSE 模板/build，snapshot=true）"
if [ "${V2_RC}" -ne 0 ]; then
    die "Resume 失败 code=${V2_CODE} msg=${V2_MSG}"
fi
alive_push "${SBX_ID}"

v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID}\"}" "${OP_TIMEOUT}"
RT2="${V2_OUT}"
RESUME_NETNS="$(jqr "${RT2}" '.runtime.netnsPath')"
RESUME_POD_IP="$(jqr "${RT2}" '.runtime.podIp')"
ck ".runtime.runtimeState" "$(jqr "${RT2}" '.runtime.runtimeState')" "Running"
ck ".runtime.templateId" "$(jqr "${RT2}" '.runtime.templateId')" "${PAUSE_TMPL}"
ck ".runtime.buildId" "$(jqr "${RT2}" '.runtime.buildId')" "${PAUSE_BUILD}"
ck ".runtime.podUid" "$(jqr "${RT2}" '.runtime.podUid')" "${SBX_ID}"
ck_nonempty ".runtime.netnsPath" "${RESUME_NETNS}"
if [ -n "${RESUME_NETNS}" ] && [ ! -e "${RESUME_NETNS}" ]; then
    CASE_ERRS+=("netns ${RESUME_NETNS} 不存在")
fi
finish_case "R2" "Resume 后运行事实正确（Running / PAUSE template+build / netns 存在）"

RESUME_HOST_PORT="$(jqr "${RT2}" '[.runtime.hostports[]? | select(.sandboxPort == 49983) | .hostPort] | first')"
RESUME_HOST_IP_ACTUAL="$(jqr "${RT2}" '.runtime.hostIp')"
ck_nonempty "hostports 含 sandboxPort=49983" "${RESUME_HOST_PORT}"
if [ -n "${RESUME_HOST_PORT}" ]; then
    if wait_tcp "${RESUME_HOST_IP_ACTUAL}" "${RESUME_HOST_PORT}" "${TCP_TIMEOUT}"; then
        :
    else
        CASE_ERRS+=("TCP ${RESUME_HOST_IP_ACTUAL}:${RESUME_HOST_PORT} 在 ${TCP_TIMEOUT}s 内不可连")
    fi
fi
if [ "${CNI_MODE}" = "1" ] && [ -n "${RESUME_POD_IP}" ]; then
    if wait_tcp "${RESUME_POD_IP}" 49983 "${TCP_TIMEOUT}"; then
        :
    else
        log_warn "CNI podIP ${RESUME_POD_IP}:49983 不可连（软断言）"
    fi
fi
finish_case "R3" "Resume 后数据面可达（hostPort=${RESUME_HOST_PORT:-?}）"

#==================== §5 Checkpoint（K1-K4） ====================#

log_step "5. Checkpoint（API 预分配 → v2 CheckpointSandbox → 源沙箱不重建 → 幂等）"

# 刷新 STARTED_AT2：Resume 之后 orchestrator 的 startTime 已更新
v2_rpc "SandboxService/List" '{"backend":"e2b"}' "${OP_TIMEOUT}"
STARTED_AT2="$(jqr "${V2_OUT}" "[.sandboxes[]? | select(.spec.sandboxId == \"${SBX_ID}\")] | first | .startTime")"
if [ -z "${STARTED_AT2}" ]; then
    STARTED_AT2="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    log_warn "Resume 后 List.startTime 为空，sandbox_started_at 用当前时间兜底: ${STARTED_AT2}"
else
    log_info "STARTED_AT2=${STARTED_AT2}"
fi

# --- K1 API snapshot-templates（硬前置） ---
OP2="v2-ckpt-$(date +%s)"
log_info "POST /internal/sandboxes/${SBX_ID}/snapshot-templates op=${OP2}"
resp="$(api_call POST "/internal/sandboxes/${SBX_ID}/snapshot-templates" "$(snapshot_meta_body "${OP2}" "${SBX_ID}" "${STARTED_AT2}")")"
code="${resp%%$'\t'*}"
body="${resp#*$'\t'}"
if [ "${code}" != "200" ]; then
    CASE_ERRS+=("snapshot-templates 返回 http=${code}: ${body}")
    fatal_case "K1" "API 预分配 snapshot-templates" "http=${code}"
fi
CKPT_TMPL="$(jqr "${body}" '.template_id')"
CKPT_BUILD="$(jqr "${body}" '.build_id')"
CKPT_SNAPSHOT="$(jqr "${body}" '.snapshot_id')"
ck_nonempty "CKPT_TMPL" "${CKPT_TMPL}"
ck_nonempty "CKPT_BUILD" "${CKPT_BUILD}"
ck_nonempty "CKPT_SNAPSHOT" "${CKPT_SNAPSHOT}"
case "${CKPT_SNAPSHOT}" in
    *:default) ;;
    *) CASE_ERRS+=("CKPT_SNAPSHOT 应以 :default 结尾，实际[${CKPT_SNAPSHOT}]") ;;
esac
finish_case "K1" "snapshot-templates 预分配成功（snapshot=${CKPT_SNAPSHOT}）"
[ "${#CASE_ERRS[@]}" -eq 0 ] || die "CKPT_TMPL/BUILD/SNAPSHOT 缺失，无法继续"

# checkpoint 前快照：netns / podIp 用于 K3 对比
v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID}\"}" "${OP_TIMEOUT}"
K3_BEFORE_NETNS="$(jqr "${V2_OUT}" '.runtime.netnsPath')"
K3_BEFORE_POD_IP="$(jqr "${V2_OUT}" '.runtime.podIp')"
log_info "checkpoint 前: netns=${K3_BEFORE_NETNS} podIp=${K3_BEFORE_POD_IP}"

# --- K2 v2 CheckpointSandbox（target=cri_sandbox_id） ---
CKPT_JSON="$(jq -n \
    --arg backend "e2b" --arg op "${OP2}" --arg id "${SBX_ID}" --argjson t "${PAUSE_TIMEOUT}" \
    --arg team "${TEAM_ID}" --arg tmpl "${CKPT_TMPL}" --arg build "${CKPT_BUILD}" \
    '{backend: $backend, operation_id: $op, timeout_seconds: $t, cri_sandbox_id: $id,
      metadata: {team_id: $team, template_id: $tmpl, build_id: $build}}')"
v2_rpc "SandboxAdminService/CheckpointSandbox" "${CKPT_JSON}" "${PAUSE_RPC_TIMEOUT}"
CKPT_RESP="${V2_OUT}"
K2_STARTED="$(jqr "${CKPT_RESP}" '.startedAtUnix')"
K2_FINISHED="$(jqr "${CKPT_RESP}" '.finishedAtUnix')"
ck "退出码" "${V2_RC}" "0"
ck_true "snapshotPersisted" "$(jqr "${CKPT_RESP}" '.snapshotPersisted')"
ck_true "vmResumed" "$(jqr "${CKPT_RESP}" '.vmResumed')"
ck_ge "startedAtUnix" "${K2_STARTED}" 1
ck_ge "finishedAtUnix>=startedAtUnix" "${K2_FINISHED}" "${K2_STARTED:-0}"
finish_case "K2" "v2 CheckpointSandbox 成功（target=cri_sandbox_id，persisted+resumed）"
if [ "${V2_RC}" -ne 0 ]; then
    die "Checkpoint 失败 code=${V2_CODE} msg=${V2_MSG}"
fi

# --- K3 源沙箱不重建（netns / podIp 不变） ---
v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID}\"}" "${OP_TIMEOUT}"
K3_AFTER="${V2_OUT}"
ck "checkpoint 后状态" "$(jqr "${K3_AFTER}" '.runtime.runtimeState')" "Running"
ck "netns 未变" "$(jqr "${K3_AFTER}" '.runtime.netnsPath')" "${K3_BEFORE_NETNS}"
ck "podIp 未变" "$(jqr "${K3_AFTER}" '.runtime.podIp')" "${K3_BEFORE_POD_IP}"
finish_case "K3" "源沙箱未重建（Running，netns/podIp 不变）"

# --- K4 幂等重试（同 op2） ---
v2_rpc "SandboxAdminService/CheckpointSandbox" "${CKPT_JSON}" "${PAUSE_RPC_TIMEOUT}"
ck "退出码" "${V2_RC}" "0"
ck_true "snapshotPersisted" "$(jqr "${V2_OUT}" '.snapshotPersisted')"
ck_true "vmResumed" "$(jqr "${V2_OUT}" '.vmResumed')"
finish_case "K4" "同 operation_id 幂等重试成功"

# --- API 收尾：build success + resolve（不计数） ---
resp="$(api_call POST "/internal/builds/${CKPT_BUILD}/status" "{\"operation_id\":\"${OP2}\",\"status\":\"success\"}")"
code="${resp%%$'\t'*}"
if [ "${code}" = "200" ]; then
    FINALIZED_BUILDS+=("${CKPT_BUILD}")
    log_info "（收尾）builds/status ${CKPT_BUILD} → success"
else
    log_warn "（收尾）builds/status ${CKPT_BUILD} 返回 http=${code}"
fi

resp="$(api_call GET "/internal/snapshots/${CKPT_SNAPSHOT}/resolve?team_id=${TEAM_ID}")"
code="${resp%%$'\t'*}"
body="${resp#*$'\t'}"
if [ "${code}" = "200" ] && [ "$(jqr "${body}" '.template_id')" = "${CKPT_TMPL}" ] && [ "$(jqr "${body}" '.build_id')" = "${CKPT_BUILD}" ]; then
    log_info "（收尾）resolve 200，template/build 匹配"
else
    log_warn "（收尾）resolve http=${code} body=$(compact_json "${body}")"
fi

#==================== §6 从 Checkpoint 创建（KC1-KC5） ====================#

log_step "6. 从 Checkpoint 创建新沙箱（KC1-KC5）"

SBX_ID2="v2e2b$(head -c 256 /dev/urandom | tr -dc 'a-z0-9' | cut -c1-15)"
CKPT_CREATE_EXEC="$(_new_execution_id)"
log_info "SBX_ID2=${SBX_ID2}"

CKPT_CREATE_JSON="$(create_payload "${SBX_ID2}" "${CKPT_TMPL}" "${CKPT_BUILD}" "true" "e2b" "${ENVD_TOKEN}" "${CKPT_CREATE_EXEC}")"
v2_rpc "SandboxService/Create" "${CKPT_CREATE_JSON}" "${CREATE_TIMEOUT}"
KC1_RESP="${V2_OUT}"
ck "退出码" "${V2_RC}" "0"
ck ".sandboxId" "$(jqr "${KC1_RESP}" '.sandboxId')" "${SBX_ID2}"
ck_nonempty ".clientId" "$(jqr "${KC1_RESP}" '.clientId')"
ck_nonempty ".hostIp" "$(jqr "${KC1_RESP}" '.hostIp')"
finish_case "KC1" "从 checkpoint 创建成功（新 ID，CKPT 模板/build，snapshot=true）"
if [ "${V2_RC}" -ne 0 ]; then
    die "从 checkpoint 创建失败 code=${V2_CODE} msg=${V2_MSG}"
fi
alive_push "${SBX_ID2}"

v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID2}\"}" "${OP_TIMEOUT}"
KC2_RT="${V2_OUT}"
NETNS_PATH2="$(jqr "${KC2_RT}" '.runtime.netnsPath')"
KC2_HOST_IP="$(jqr "${KC2_RT}" '.runtime.hostIp')"
KC2_POD_IP="$(jqr "${KC2_RT}" '.runtime.podIp')"
ck ".runtime.runtimeState" "$(jqr "${KC2_RT}" '.runtime.runtimeState')" "Running"
ck ".runtime.templateId" "$(jqr "${KC2_RT}" '.runtime.templateId')" "${CKPT_TMPL}"
ck ".runtime.buildId" "$(jqr "${KC2_RT}" '.runtime.buildId')" "${CKPT_BUILD}"
ck_nonempty ".runtime.netnsPath" "${NETNS_PATH2}"
finish_case "KC2" "B 的运行事实正确（Running / CKPT template+build / netns）"

KC3_PORT="$(jqr "${KC2_RT}" '[.runtime.hostports[]? | select(.sandboxPort == 49983) | .hostPort] | first')"
ck_nonempty "hostports 含 sandboxPort=49983" "${KC3_PORT}"
if [ -n "${KC3_PORT}" ]; then
    if wait_tcp "${KC2_HOST_IP}" "${KC3_PORT}" "${TCP_TIMEOUT}"; then
        :
    else
        CASE_ERRS+=("TCP ${KC2_HOST_IP}:${KC3_PORT} 在 ${TCP_TIMEOUT}s 内不可连")
    fi
fi
if [ "${CNI_MODE}" = "1" ] && [ -n "${KC2_POD_IP}" ]; then
    if wait_tcp "${KC2_POD_IP}" 49983 "${TCP_TIMEOUT}"; then
        :
    else
        log_warn "CNI podIP ${KC2_POD_IP}:49983 不可连（软断言）"
    fi
fi
finish_case "KC3" "B 的数据面可达（hostPort=${KC3_PORT:-?}）"

v2_rpc "SandboxService/List" '{"backend":"e2b"}' "${OP_TIMEOUT}"
KC4_ONE="$(jqr "${V2_OUT}" "[.sandboxes[]? | select(.spec.sandboxId == \"${SBX_ID2}\")] | first")"
ck "List 中条数" "$(jqr "${V2_OUT}" "[.sandboxes[]? | select(.spec.sandboxId == \"${SBX_ID2}\")] | length")" "1"
ck ".spec.templateId" "$(jqr "${KC4_ONE}" '.spec.templateId')" "${CKPT_TMPL}"
ck ".spec.buildId" "$(jqr "${KC4_ONE}" '.spec.buildId')" "${CKPT_BUILD}"
finish_case "KC4" "v2 List 中 B 的 template/build 匹配"

if v2_delete "${SBX_ID2}"; then
    v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID2}\"}" "${OP_TIMEOUT}"
    ck "Get 已删 B 的 Code" "${V2_CODE}" "NotFound"
    finish_case "KC5" "v2 Delete B 成功，Get → NotFound"
else
    CASE_ERRS+=("Delete 失败 code=${V2_CODE} msg=${V2_MSG}")
    finish_case "KC5" "v2 Delete B + Get NotFound"
fi

#==================== §7 负向与能力分发（N1-N6） ====================#

log_step "7. 负向与能力分发（N1-N6）"

# 负向调用前后比对 v2e2b 前缀 ID 集合，抓“半创建”
prefix_id_set() {
    list_ids 2>/dev/null | grep '^v2e2b' | sort | tr '\n' ' ' || true
}

# --- N1 Create 缺 sandbox_id ---
set_before="$(prefix_id_set)"
N1_JSON="$(printf '%s' "${CREATE_JSON}" | jq -c 'del(.sandbox.sandbox_id)')"
v2_rpc "SandboxService/Create" "${N1_JSON}" "${OP_TIMEOUT}"
ck "Code" "${V2_CODE}" "InvalidArgument"
ck_contains "message" "${V2_ERR}" "sandbox_id"
set_after="$(prefix_id_set)"
ck "List 中 v2e2b 集合不变" "${set_after}" "${set_before}"
finish_case "N1" "Create 缺 sandbox_id → InvalidArgument（无半创建）"

# --- N2 Create 无能力 backend（container 注册了但不实现 v2 生命周期） ---
N2_JSON="$(printf '%s' "${CREATE_JSON}" | jq -c '.backend = "container"')"
v2_rpc "SandboxService/Create" "${N2_JSON}" "${OP_TIMEOUT}"
case "${V2_CODE}" in
    Unimplemented)
        ck_contains "message" "${V2_ERR}" "does not implement admin lifecycle"
        ;;
    NotFound)
        CASE_ERRS+=("backend=container 返回 NotFound：container 后端未注册，检查 main.go 装配")
        ;;
    *)
        CASE_ERRS+=("期望 Unimplemented，实际 ${V2_CODE}: ${V2_MSG}")
        ;;
esac
finish_case "N2" "Create backend=container → Unimplemented（能力分发）"

# --- N3 Create 缺 team_id（extra 与 metadata 都不含） ---
set_before="$(prefix_id_set)"
N3_JSON="$(printf '%s' "${CREATE_JSON}" | jq -c 'del(.sandbox.extra.team_id) | del(.sandbox.metadata.team_id)')"
v2_rpc "SandboxService/Create" "${N3_JSON}" "${OP_TIMEOUT}"
ck "Code" "${V2_CODE}" "InvalidArgument"
ck_contains "message" "${V2_ERR}" "team_id"
set_after="$(prefix_id_set)"
ck "List 中 v2e2b 集合不变" "${set_after}" "${set_before}"
finish_case "N3" "Create 缺 team_id → InvalidArgument（无半创建）"

# --- N4 Pause 空 operation_id（A 必须仍在 Running） ---
N4_JSON="$(printf '%s' "${PAUSE_JSON}" | jq -c '.operation_id = ""')"
v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID}\"}" "${OP_TIMEOUT}"
n4_state_before="$(jqr "${V2_OUT}" '.runtime.runtimeState')"
v2_rpc "SandboxAdminService/PauseSandbox" "${N4_JSON}" "${PAUSE_RPC_TIMEOUT}"
ck "Code" "${V2_CODE}" "InvalidArgument"
ck_contains "message" "${V2_ERR}" "operation_id"
v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID}\"}" "${OP_TIMEOUT}"
ck "A 状态未变" "$(jqr "${V2_OUT}" '.runtime.runtimeState')" "${n4_state_before:-Running}"
finish_case "N4" "Pause 缺 operation_id → InvalidArgument（A 状态不变）"

# --- N5 Create 未注册 backend（注册表解析失败，不进能力分发） ---
# 与 N2 的分工：N2 是"后端已注册但不实现 v2 生命周期"（Unimplemented），
# N5 是"后端名根本不存在"（NotFound），走 moduleFor 的注册表查询。
set_before="$(prefix_id_set)"
N5_JSON="$(printf '%s' "${CREATE_JSON}" | jq -c '.backend = "bogus"')"
v2_rpc "SandboxService/Create" "${N5_JSON}" "${OP_TIMEOUT}"
ck "Code" "${V2_CODE}" "NotFound"
ck_contains "message" "${V2_ERR}" "bogus"
set_after="$(prefix_id_set)"
ck "List 中 v2e2b 集合不变" "${set_after}" "${set_before}"
finish_case "N5" "Create backend=bogus → NotFound（未注册后端，无半创建）"

# --- N6 Pause 的团队归属不符 ---
# 说明：target 在 proto 里是 oneof，两个 arm 不能同时给（protojson 直接报
# "oneof ... is already set"），所以 adapter 里"双边 ID 一致性"那个分支在 v2
# 线路上不可达；这里改测 validateAdminOpTarget 的 team 归属校验——metadata.team_id
# 会覆盖 pod 上的 team（adminOperationFrom），不匹配时在拍快照之前就被拒绝。
N6_OP="v2-team-$(date +%s)"
N6_JSON="$(jq -n --arg op "${N6_OP}" --arg id "${SBX_ID}" --argjson t "${PAUSE_TIMEOUT}" \
    '{backend: "e2b", operation_id: $op, timeout_seconds: $t, sandbox_id: $id,
      metadata: {team_id: "v2-team-not-owner"}}')"
v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID}\"}" "${OP_TIMEOUT}"
n6_state_before="$(jqr "${V2_OUT}" '.runtime.runtimeState')"
v2_rpc "SandboxAdminService/PauseSandbox" "${N6_JSON}" "${PAUSE_RPC_TIMEOUT}"
ck "Code" "${V2_CODE}" "FailedPrecondition"
ck_contains "message" "${V2_ERR}" "team mismatch"
v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID}\"}" "${OP_TIMEOUT}"
ck "A 状态未变" "$(jqr "${V2_OUT}" '.runtime.runtimeState')" "${n6_state_before:-Running}"
finish_case "N6" "Pause 的 team 归属不符 → FailedPrecondition（A 状态不变）"

#==================== §8 Delete 与回收（D1-D6） ====================#

log_step "8. Delete 与回收（D1-D6）"

# --- D1 删除 A（checkpoint 后仍 Running 的源沙箱） ---
t0="$(now_ms)"
if v2_delete "${SBX_ID}"; then
    D1_MS=$(( $(now_ms) - t0 ))
    finish_case "D1" "v2 Delete A 成功（$(fmt_ms "${D1_MS}")）"
else
    CASE_ERRS+=("Delete 失败 code=${V2_CODE} msg=${V2_MSG}")
    finish_case "D1" "v2 Delete A"
fi

# --- D2 幂等删除（大写 backend，验证大小写解析） ---
v2_rpc "SandboxService/Delete" "{\"backend\":\"E2B\",\"sandbox_id\":\"${SBX_ID}\"}" "${OP_TIMEOUT}"
ck "退出码" "${V2_RC}" "0"
finish_case "D2" "重复 Delete（backend=E2B 大写）幂等成功"

# --- D3 Get 已删 ---
v2_rpc "SandboxAdminService/GetSandboxRuntime" "{\"backend\":\"e2b\",\"sandbox_id\":\"${SBX_ID}\"}" "${OP_TIMEOUT}"
ck "Code" "${V2_CODE}" "NotFound"
finish_case "D3" "Get 已删 A → NotFound（tracker/state 已清理）"

# --- D4 v2 List 回收 ---
if wait_list_absent "${CLEAN_TIMEOUT}" "${SBX_ID}" "${SBX_ID2}"; then
    finish_case "D4" "v2 List 中 A/B 已消失（≤${CLEAN_TIMEOUT}s）"
else
    CASE_ERRS+=("v2 List 仍在 ${CLEAN_TIMEOUT}s 内可见 A/B")
    finish_case "D4" "v2 List 中 A/B 已消失"
fi

# --- D5 CRI 列表回收 ---
d5_ok=0
deadline=$(( $(date +%s) + CLEAN_TIMEOUT ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
    if cri_pods_contains "${SBX_ID}" || cri_pods_contains "${SBX_ID2}"; then
        sleep 2
        continue
    fi
    d5_ok=1
    break
done
ck "CRI ListPodSandbox 不再含 A/B" "${d5_ok}" "1"
finish_case "D5" "CRI 列表已回收 A/B（≤${CLEAN_TIMEOUT}s）"

# --- D6 Delete 缺 sandbox_id ---
# 只在 v2 协议层做参数校验，不触碰后端；放在 A/B 都回收之后，纯负向调用。
v2_rpc "SandboxService/Delete" '{"backend":"e2b"}' "${OP_TIMEOUT}"
ck "Code" "${V2_CODE}" "InvalidArgument"
ck_contains "message" "${V2_ERR}" "sandbox_id"
finish_case "D6" "Delete 缺 sandbox_id → InvalidArgument"

# --- API 收尾：删 checkpoint 快照与 pause snapshot（不计数） ---
resp="$(api_call DELETE "/internal/snapshot-templates/${CKPT_SNAPSHOT}?team_id=${TEAM_ID}")"
code="${resp%%$'\t'*}"
if [ "${code}" = "204" ]; then
    CKPT_SNAPSHOT_CLEANED=1
    log_info "（收尾）DELETE snapshot-templates → 204"
else
    log_warn "（收尾）DELETE snapshot-templates 返回 http=${code}"
fi

resp="$(api_call DELETE "/internal/sandboxes/${SBX_ID}/snapshot")"
code="${resp%%$'\t'*}"
if [ "${code}" = "204" ]; then
    PAUSE_SNAPSHOT_CLEANED=1
    log_info "（收尾）DELETE sandboxes/${SBX_ID}/snapshot → 204"
else
    log_warn "（收尾）DELETE sandboxes/${SBX_ID}/snapshot 返回 http=${code}"
fi

#==================== §9 收尾（Z1-Z2） ====================#

log_step "9. 收尾检查（Z1 / Z2）"

# --- Z1 无残留 ---
final_ids="$(list_ids || true)"
leftover="$(printf '%s\n' "${final_ids}" | grep '^v2e2b' || true)"
ck "v2 List 无 v2e2b 前缀残留" "${leftover:-<none>}" "<none>"
ck "ALIVE_IDS 已清空" "${ALIVE_IDS[*]:-<none>}" "<none>"
finish_case "Z1" "无残留（v2 List 干净，CREATED_IDS=$((${#CREATED_IDS[@]})) 个均已删）"

# --- Z2 汇总（最终退出码由 summary_exit_code 决定） ---
finish_case "Z2" "汇总（PASS/FAIL/SKIP 见 print_summary）"

exit "$(summary_exit_code)"
