#!/bin/bash
###############################################################################
# 22_pause_checkpoint_resume.sh — Pause / Checkpoint / Resume 全流程端到端自动化
#
# 自动化落地《手动端到端验证 Pause Checkpoint Resume 操作手册.md》§1-§6：
#   §1 创建沙箱（手写 annotations 代替 webhook）→ GetSandboxRuntime 核对
#   §2 Pause（API 预分配 → Admin PauseSandbox → 幂等重试 → 删 Pod → 回写 build 终态）
#   §3 Resume（last-snapshot → 新 Pod snapshot=true，同一稳定 sandbox-id）
#   §4 Checkpoint（API snapshot-templates → Admin CheckpointSandbox → resolve 校验）
#   §5 从 Checkpoint 创建新沙箱
#   §6 删除与 409/404/幂等断言
#
# 自包含：不依赖 lib/common.sh。断言失败即 fail 退出（trap 负责清理现场）。
#
# 输出说明：
#   - 每个新增 API 接口 / Admin gRPC 的调用参数与返回值实时打印（[API]/[gRPC] 前缀）
#   - 各阶段用 kubectl 打印 Pod 状态及 base_template_id/template_id/build_id/
#     sandbox_id/snapshot 注解（[Pod] 前缀）：初始创建、Pause 后、删除后、
#     Resume 后、Checkpoint 后、从 Checkpoint 创建
#
#==================== webhook 打桩说明 ====================#
# 真实架构中，本脚本扮演的角色是 webhook（当前未实现，由本脚本打桩）。
# 下列参数/动作在真实链路中由 webhook 生成或组装，脚本中以
#   # [webhook] ...
# 注释标出：
#   1) SBX_ID / SBX_ID2        稳定 sandbox-id，由 webhook 从 E2B API 获取组装
#                              （API 在 POST /sandboxes / POST /sandboxes/transform 中
#                              用 id.Generate() 生成；webhook 只透传写入 Pod annotation；
#                              Resume 路径沿用原 ID 不变）。本脚本无 webhook/API 创建环节，
#                              由 gen_sbx_id 打桩代替 API 的生成动作
#   2) EXEC_ID / EXEC_ID2/3    execution-id，每次（重）创建一个 Pod 由 webhook 新生成
#   3) OP_ID / OP_ID2          operation_id，每次 Pause/Checkpoint 操作由 webhook 生成，
#                              用于 API 预分配与 Admin 调用的幂等关联
#   4) Pod YAML 全部 e2b.dev/* annotations，由 webhook 从 E2B API 模板/沙箱元数据
#                              获取组装后写入 Pod（render_pod_yaml）
#   5) RuntimeSnapshotMetadata 请求体（snapshot_meta_body），由 webhook 在调用
#                              pause-snapshot / snapshot-templates 前获取组装：
#                              operation_id 由 webhook 生成；team_id/base_template_id/
#                              vcpu/ram/disk/内核与版本号从 E2B API 模板元数据获取组装；
#                              source_pod_uid/source_node_name/sandbox_started_at
#                              从源 Pod 对象获取组装
# 非 webhook 组装的参数来源：TEAM_ID/BASE_ENV/BASE_BUILD/内核与版本号 = 从 E2B API
# 模板元数据获取；NODE_NAME = 从 Pod 调度结果（.spec.nodeName）获取；
# POD_UID/STARTED_AT = 从 Pod 对象获取（kubectl 读取）。
###############################################################################
set -euo pipefail

#==================== 基础工具 ====================#

PASS_COUNT=0
FAIL_COUNT=0
START_TS=$(date +%s)
RESULTS_FILE=$(mktemp /tmp/e2b22-results.XXXXXX)
DETAIL_LOG=$(mktemp /tmp/e2b22-detail.XXXXXX)

log_step()  { echo ""; echo "[$(date +%H:%M:%S)] >>> $*"; }
log_info()  { echo "    $*"; }
log_pass()  { PASS_COUNT=$((PASS_COUNT+1)); echo "PASS  $*" | tee -a "${RESULTS_FILE}"; }
log_fail()  { FAIL_COUNT=$((FAIL_COUNT+1)); echo "FAIL  $*" | tee -a "${RESULTS_FILE}"; }
die()       { echo ""; echo "[FATAL] $*" >&2; echo "[FATAL] 详细日志: ${DETAIL_LOG}" >&2; exit 1; }

# curl 封装：返回 "<http_code>\t<body>"
# 调用参数/返回打印走 stderr（stdout 是数据通道，被 $(...) 捕获）
# JSON 美化打印；非法 JSON 原样输出
pretty_json() { echo "$1" | jq . 2>/dev/null || echo "$1"; }
api_call() {
    local method="$1" path="$2" body="${3:-}"
    echo "  [API] --> ${method} ${path}" >&2
    if [ -n "${body}" ]; then
        echo "  [API]   request:" >&2
        pretty_json "${body}" | sed 's/^/  [API]     /' >&2
    fi
    local args=(-sS -X "${method}" "${API}${path}" -H "X-Admin-Token: ${ADMIN_TOKEN}")
    [ -n "${body}" ] && args+=(-H "Content-Type: application/json" -d "${body}")
    local out
    out=$(curl "${args[@]}" -w $'\n%{http_code}' 2>&1) || die "curl 调用失败: ${method} ${path}: ${out}"
    local code="${out##*$'\n'}"
    local payload="${out%$'\n'*}"
    if [ -n "${payload}" ]; then
        echo "  [API] <-- http=${code} response:" >&2
        pretty_json "${payload}" | sed 's/^/  [API]     /' >&2
    else
        echo "  [API] <-- http=${code} (empty body)" >&2
    fi
    printf '%s\t%s' "${code}" "${payload}"
}

# grpcurl 调 admin.sock（本机 grpcurl -unix flag 有 bug，必须用 unix:/// scheme + -plaintext）
# 调用参数/返回打印走 stderr（stdout 是数据通道，被 $(...) 捕获）
gadmin() {
    echo "  [gRPC] --> E2BSandboxAdminService/$2 request:" >&2
    pretty_json "$1" | sed 's/^/  [gRPC]     /' >&2
    local out
    out=$(/root/go/bin/grpcurl -plaintext \
        -import-path /home/zrj/KASandbox/cri-multiplex/proto -proto admin.proto \
        -d "$1" unix:///run/cri-multiplex/admin.sock \
        "crimultiplex.admin.v1.E2BSandboxAdminService/$2" 2>&1)
    local rc=$?
    echo "  [gRPC] <-- response:" >&2
    pretty_json "${out}" | sed 's/^/  [gRPC]     /' >&2
    [ ${rc} -eq 0 ] && printf '%s' "${out}" || return ${rc}
}

# 注意：jq 的 `// empty` 会把 false 也吞掉，布尔字段必须显式判 null
jq_get() { echo "$2" | jq -r "($1) | if . == null then empty else . end"; }

# 打印某阶段 Pod 的完整信息：摘要（状态 + 关键 annotations）+ 完整 kubectl describe 输出
# 用法: dump_pod <pod_name> <阶段标签>
dump_pod() {
    local pod="$1" label="$2"
    echo ""
    echo "  ┌─[Pod] ${label}: pod/${pod} ──────────────────────────────────"
    if ! kubectl get pod "${pod}" >/dev/null 2>&1; then
        echo "  │   已删除（不存在）"
        echo "  └──────────────────────────────────────────────────────────────"
        return 0
    fi
    local phase uid node ready
    phase=$(kubectl get pod "${pod}" -o jsonpath='{.status.phase}' 2>/dev/null)
    uid=$(kubectl get pod "${pod}" -o jsonpath='{.metadata.uid}' 2>/dev/null)
    node=$(kubectl get pod "${pod}" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
    ready=$(kubectl get pod "${pod}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
    echo "  │   phase=${phase}  ready=${ready:-False}  node=${node}"
    echo "  │   uid=${uid}"
    local a
    a=$(kubectl get pod "${pod}" -o jsonpath='{.metadata.annotations}' 2>/dev/null)
    echo "  │   sandbox_id       = $(jq_get '."e2b.dev/sandbox-id"' "${a}")"
    echo "  │   base_template_id = $(jq_get '."e2b.dev/base-template-id"' "${a}")"
    echo "  │   template_id      = $(jq_get '."e2b.dev/template-id"' "${a}")"
    echo "  │   build_id         = $(jq_get '."e2b.dev/build-id"' "${a}")"
    echo "  │   snapshot         = $(jq_get '."e2b.dev/snapshot"' "${a}")"
    echo "  │   execution_id     = $(jq_get '."e2b.dev/execution-id"' "${a}")"
    echo "  ├─ kubectl describe pod ${pod} ────────────────────────────────"
    kubectl describe pod "${pod}" 2>&1 | sed 's/^/  │ /'
    echo "  └──────────────────────────────────────────────────────────────"
}

#==================== 环境与常量（已核实的 live 事实） ====================#

API="http://127.0.0.1:3000"
DSN='postgres://postgres:local@127.0.0.1:5432/mydatabase?sslmode=disable'
TEAM_ID="db8250ab-9929-48a2-8fb6-c4e992d59288"        # webhook 从 E2B API team 元数据获取组装
BASE_ENV="dqoim7o51k7e89b2s8bl"                      # webhook 从 E2B API 模板元数据获取组装（base template_id）
BASE_BUILD="d7138a4f-7c26-4bc9-8f00-ff92ccbec71c"    # webhook 从 E2B API 模板元数据获取组装（base build_id）
NODE_NAME="hostname-6il0x"                           # webhook 从 Pod 调度结果（.spec.nodeName）获取组装
KERNEL="vmlinux-6.1.158"                             # webhook 从 E2B API 模板元数据获取组装
FC_VER="v1.13.1"                                     # webhook 从 E2B API 模板元数据获取组装
ENVD_VER="0.5.3"                                     # webhook 从 E2B API 模板元数据获取组装
GOOSE_MIN_VERSION=20260219120000

POD1="manual-sbx-1"
POD1R="manual-sbx-1-resumed"
POD2="manual-sbx-2"
CREATED_PODS=()
TMP_YAMLS=()

#==================== trap 清理 ====================#

cleanup() {
    echo ""
    echo "==================== 清理（trap EXIT） ===================="
    for p in "${CREATED_PODS[@]:-}"; do
        [ -n "${p}" ] || continue
        log_info "kubectl delete pod ${p} --ignore-not-found --wait=false"
        kubectl delete pod "${p}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done
    for y in "${TMP_YAMLS[@]:-}"; do
        [ -n "${y}" ] && rm -f "${y}" || true
    done
    echo ""
    echo "==================== 结果汇总 ===================="
    cat "${RESULTS_FILE}"
    echo "---------------------------------------------------"
    echo "PASS=${PASS_COUNT}  FAIL=${FAIL_COUNT}  耗时=$(($(date +%s)-START_TS))s"
    echo "详细日志: ${DETAIL_LOG}"
    rm -f "${RESULTS_FILE}"
}
trap cleanup EXIT

# 轮询等待 Pod Ready：每 3s 一次，最多 60 次（180s），失败打印 describe + cri-multiplex 日志尾部
wait_pod_ready() {
    local pod="$1" i
    for i in $(seq 1 60); do
        local ready
        ready=$(kubectl get pod "${pod}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
        if [ "${ready}" = "True" ]; then
            log_info "pod/${pod} Ready（第 ${i} 次轮询）"
            return 0
        fi
        sleep 3
    done
    echo "----- kubectl describe pod ${pod} -----" | tee -a "${DETAIL_LOG}"
    kubectl describe pod "${pod}" 2>&1 | tail -40 | tee -a "${DETAIL_LOG}"
    echo "----- /tmp/cri-multiplex.log 尾部 30 行 -----" | tee -a "${DETAIL_LOG}"
    tail -30 /tmp/cri-multiplex.log 2>&1 | tee -a "${DETAIL_LOG}"
    return 1
}

# [webhook] 渲染 Pod YAML（§1 / §3.2 / §5 共用模板）
# 真实链路中由 webhook 将全部 e2b.dev/* annotations 写入 Pod；本脚本手写 YAML 打桩。
# annotations 取值来源：
#   sandbox-id               = webhook 从 E2B API 获取组装的稳定 ID（初始创建/从 Checkpoint
#                              创建时由 API 新生成，Resume 沿用原 ID）
#   execution-id / snapshot  = [webhook] 按场景生成/设置
#   team-id / 规格（vcpu/ram/disk/max-sandbox-length）/ 版本号（kernel/firecracker/envd）
#                            = webhook 从 E2B API 模板元数据获取组装
#   template-id / build-id   = webhook 从 E2B API 获取组装（初始创建用 base 模板；
#                              Resume 用 last-snapshot 返回；从 Checkpoint 创建用 resolve 返回）
render_pod_yaml() {
    local file="$1" name="$2" sbx="$3" snapshot="$4" tmpl="$5" build="$6" exec_id="$7"
    cat > "${file}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: default
  annotations:
    e2b.dev/sandbox-id: "${sbx}"
    e2b.dev/base-template-id: "${BASE_ENV}"
    e2b.dev/template-id: "${tmpl}"
    e2b.dev/build-id: "${build}"
    e2b.dev/team-id: "${TEAM_ID}"
    e2b.dev/vcpu: "1"
    e2b.dev/ram-mb: "1024"
    e2b.dev/total-disk-size-mb: "942"
    e2b.dev/max-sandbox-length: "10000"
    e2b.dev/huge-pages: "true"
    e2b.dev/auto-pause: "false"
    e2b.dev/snapshot: "${snapshot}"
    e2b.dev/allow-internet: "true"
    e2b.dev/envd-version: "${ENVD_VER}"
    e2b.dev/kernel-version: "${KERNEL}"
    e2b.dev/firecracker-version: "${FC_VER}"
    e2b.dev/execution-id: "${exec_id}"
    e2b.dev/envd-access-token: ""
    e2b.dev/env-vars: "{}"
    e2b.dev/network: "{\"egress\":{},\"ingress\":{}}"
    e2b.dev/volume-mounts: "[]"
    e2b.dev/auto-resume: "{\"policy\":\"off\"}"
spec:
  runtimeClassName: e2b
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: ${NODE_NAME}
  containers:
  - name: app
    image: e2b.dev/${BASE_ENV}:${BASE_BUILD}
    imagePullPolicy: IfNotPresent
    command: ["sleep", "3600"]
EOF
}

# [webhook] RuntimeSnapshotMetadata 请求体（§2.1 / §4.1 共用）
# 真实链路中由 webhook 在调 pause-snapshot / snapshot-templates 前获取组装：
#   operation_id                                  = [webhook] 本次操作生成（幂等键）
#   team_id / base_template_id                    = webhook 从 E2B API 元数据获取组装
#   vcpu/ram/disk/kernel/firecracker/envd/secure/allow_internet
#                                                 = webhook 从 E2B API 模板+沙箱元数据获取组装
#   source_pod_uid/source_node_name/sandbox_started_at
#                                                 = webhook 从源 Pod 对象获取组装
snapshot_meta_body() {
    local op="$1" pod_uid="$2"
    cat <<EOF
{
  "operation_id": "${op}",
  "team_id": "${TEAM_ID}",
  "base_template_id": "${BASE_ENV}",
  "source_pod_uid": "${pod_uid}",
  "source_node_name": "${NODE_NAME}",
  "sandbox_started_at": "${STARTED_AT}",
  "vcpu": 1, "ram_mb": 1024, "total_disk_size_mb": 942,
  "kernel_version": "${KERNEL}",
  "firecracker_version": "${FC_VER}",
  "envd_version": "${ENVD_VER}",
  "secure": false,
  "allow_internet_access": true,
  "metadata": {"source": "manual-e2e"}
}
EOF
}

#==================== §0 preflight ====================#

log_step "0.1 取 ADMIN_TOKEN（API 进程环境）"
API_PID=$(pgrep -f '/api --port' | head -1) || die "未找到 API 进程"
ADMIN_TOKEN=$(tr '\0' '\n' < "/proc/${API_PID}/environ" | sed -n 's/^ADMIN_TOKEN=//p')
[ -n "${ADMIN_TOKEN}" ] || die "ADMIN_TOKEN 为空"
log_info "api_pid=${API_PID} token_len=${#ADMIN_TOKEN}"
log_pass "ADMIN_TOKEN 获取成功"

log_step "0.2 API 鉴权自检（无 header 应 401，带 token 不应 401）"
code=$(curl -sS -o /dev/null -w '%{http_code}' "${API}/internal/sandboxes/zzzzzzzzzzzzzzzzzzzz/snapshot-state")
[ "${code}" = "401" ] || die "无 header 未返回 401: ${code}"
resp=$(api_call GET "/internal/sandboxes/zzzzzzzzzzzzzzzzzzzz/snapshot-state")
code="${resp%%$'\t'*}"
# 迁移未跑时这里是 500，迁移后是 200(paused=false)；只要不是 401 就说明 token 有效
[ "${code}" != "401" ] || die "带 token 仍 401，token 无效"
log_pass "token 有效（带 token 返回 http=${code}，非 401）"

log_step "0.3 goose 迁移检查（需 >= ${GOOSE_MIN_VERSION}，不足则 up）"
cur_ver=$(docker exec postgres psql -U postgres -d mydatabase -tA -c 'select max(version_id) from "_migrations"') \
    || die "无法查询 _migrations"
log_info "当前迁移版本: ${cur_ver}"
if [ "${cur_ver}" -lt "${GOOSE_MIN_VERSION}" ]; then
    log_info "版本不足，执行 goose up"
    (cd /home/zrj/KASandbox/packages/db && \
        GOOSE_DBSTRING="${DSN}" go tool goose -table _migrations -dir migrations postgres up) \
        >>"${DETAIL_LOG}" 2>&1 || die "goose up 失败，见 ${DETAIL_LOG}"
    cur_ver=$(docker exec postgres psql -U postgres -d mydatabase -tA -c 'select max(version_id) from "_migrations"')
fi
[ "${cur_ver}" -ge "${GOOSE_MIN_VERSION}" ] || die "迁移后版本仍不足: ${cur_ver}"
log_pass "迁移版本满足: ${cur_ver}"

log_step "0.4 环境清理确认（无残留 manual-sbx pod）"
if kubectl get pods --no-headers 2>/dev/null | grep -q 'manual-sbx'; then
    kubectl get pods | grep 'manual-sbx' >&2
    die "存在残留 manual-sbx pod，请先清理"
fi
log_pass "无残留"

#==================== §1 创建沙箱 ====================#

gen_sbx_id() {
    # [webhook] 稳定 sandbox-id 真实链路中由 E2B API 生成（id.Generate()），
    # webhook 从创建/transform 响应获取组装进 Pod annotation；此处脚本打桩代替 API 生成
    # 注意：tr </dev/urandom | head 会让 tr 吃 SIGPIPE，在 pipefail 下直接挂；先限量再截断
    local id
    id=$(head -c 256 /dev/urandom | tr -dc a-z0-9)
    echo "${id:0:20}"
}

log_step "1.1 生成 ID 并创建 Pod ${POD1}"
SBX_ID=$(gen_sbx_id)                            # [webhook] 稳定 sandbox-id（全链路不变）；真实链路由 E2B API 生成，webhook 获取组装
EXEC_ID=$(cat /proc/sys/kernel/random/uuid)     # [webhook] execution-id（每个 Pod 一个新值）
OP_ID="manual-op-$(date +%s)"                   # [webhook] 本次 Pause 操作的 operation_id（幂等键）
log_info "SBX_ID=${SBX_ID} EXEC_ID=${EXEC_ID} OP_ID=${OP_ID}"
Y1=$(mktemp /tmp/sbx-pod-1.XXXXXX.yaml); TMP_YAMLS+=("${Y1}")
render_pod_yaml "${Y1}" "${POD1}" "${SBX_ID}" "false" "${BASE_ENV}" "${BASE_BUILD}" "${EXEC_ID}"
kubectl apply -f "${Y1}" >>"${DETAIL_LOG}" 2>&1 || die "kubectl apply ${POD1} 失败"
CREATED_PODS+=("${POD1}")
wait_pod_ready "${POD1}" || die "pod/${POD1} 未在 180s 内 Ready"
POD_UID=$(kubectl get pod "${POD1}" -o jsonpath='{.metadata.uid}')        # webhook 从 Pod 对象获取组装（→ RuntimeSnapshotMetadata.source_pod_uid）
STARTED_AT=$(kubectl get pod "${POD1}" -o jsonpath='{.status.startTime}') # webhook 从 Pod 对象获取组装（→ sandbox_started_at）
log_info "POD_UID=${POD_UID} STARTED_AT=${STARTED_AT}"
log_pass "Pod ${POD1} Ready"
dump_pod "${POD1}" "初始创建"

log_step "1.2 Admin GetSandboxRuntime（按 cri_sandbox_id）"
out=$(gadmin "{\"cri_sandbox_id\":\"${POD_UID}\"}" GetSandboxRuntime) || die "GetSandboxRuntime(cri) 调用失败: ${out}"
echo "${out}" >> "${DETAIL_LOG}"
got_sbx=$(jq_get '.e2bSandboxId' "${out}")
got_state=$(jq_get '.runtimeState' "${out}")
log_info "e2b_sandbox_id=${got_sbx} runtime_state=${got_state}"
[ "${got_sbx}" = "${SBX_ID}" ] || die "e2b_sandbox_id 不匹配: got=${got_sbx} want=${SBX_ID}"
[ "${got_state}" = "Running" ] || die "runtime_state 非 Running: ${got_state}"
log_pass "GetSandboxRuntime(cri) 正确: sbx=${got_sbx} state=${got_state}"

log_step "1.3 Admin GetSandboxRuntime（按 e2b_sandbox_id）"
out=$(gadmin "{\"e2b_sandbox_id\":\"${SBX_ID}\"}" GetSandboxRuntime) || die "GetSandboxRuntime(e2b) 调用失败: ${out}"
echo "${out}" >> "${DETAIL_LOG}"
got_cri=$(jq_get '.criSandboxId' "${out}")
got_state=$(jq_get '.runtimeState' "${out}")
[ "${got_cri}" = "${POD_UID}" ] || die "cri_sandbox_id 反查不匹配: got=${got_cri} want=${POD_UID}"
[ "${got_state}" = "Running" ] || die "runtime_state 非 Running: ${got_state}"
log_pass "GetSandboxRuntime(e2b) 双向索引正确: cri=${got_cri} state=${got_state}"

#==================== §2 Pause ====================#

log_step "2.1 API POST pause-snapshot（预分配快照元数据）"
log_info "POST /internal/sandboxes/${SBX_ID}/pause-snapshot op=${OP_ID}"
resp=$(api_call POST "/internal/sandboxes/${SBX_ID}/pause-snapshot" "$(snapshot_meta_body "${OP_ID}" "${POD_UID}")")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
echo "${body}" >> "${DETAIL_LOG}"
[ "${code}" = "200" ] || die "pause-snapshot 返回 ${code}: ${body}"
PAUSE_TMPL=$(jq_get '.template_id' "${body}")
PAUSE_BUILD=$(jq_get '.build_id' "${body}")
[ -n "${PAUSE_TMPL}" ] && [ -n "${PAUSE_BUILD}" ] || die "pause-snapshot 响应缺字段: ${body}"
log_info "PAUSE_TMPL=${PAUSE_TMPL} PAUSE_BUILD=${PAUSE_BUILD}"
log_pass "pause-snapshot 200，template/build 已分配"

log_step "2.2 Admin PauseSandbox（等待快照上传持久化）"
pause_req="{\"operation_id\":\"${OP_ID}\",\"cri_sandbox_id\":\"${POD_UID}\",\"e2b_sandbox_id\":\"${SBX_ID}\",\"team_id\":\"${TEAM_ID}\",\"template_id\":\"${PAUSE_TMPL}\",\"build_id\":\"${PAUSE_BUILD}\",\"timeout_seconds\":600}"
out=$(gadmin "${pause_req}" PauseSandbox) || die "PauseSandbox 调用失败: ${out}"
echo "${out}" >> "${DETAIL_LOG}"
persisted=$(jq_get '.snapshotPersisted' "${out}")
log_info "snapshot_persisted=${persisted}"
[ "${persisted}" = "true" ] || die "PauseSandbox snapshot_persisted!=true: ${out}"
log_pass "PauseSandbox snapshot_persisted=true"
dump_pod "${POD1}" "Pause 后（删除前）"
log_info "Pause 快照: template_id=${PAUSE_TMPL} build_id=${PAUSE_BUILD}（pause 无 snapshot_id 概念）"

log_step "2.3 Admin PauseSandbox 幂等重试（同 operation_id）"
out=$(gadmin "${pause_req}" PauseSandbox) || die "PauseSandbox 重试调用失败: ${out}"
echo "${out}" >> "${DETAIL_LOG}"
persisted=$(jq_get '.snapshotPersisted' "${out}")
[ "${persisted}" = "true" ] || die "PauseSandbox 重试 snapshot_persisted!=true: ${out}"
log_pass "PauseSandbox 幂等重试仍 snapshot_persisted=true"

log_step "2.4 API GET snapshot-state（paused=true 且 build 匹配）"
resp=$(api_call GET "/internal/sandboxes/${SBX_ID}/snapshot-state")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
echo "${body}" >> "${DETAIL_LOG}"
[ "${code}" = "200" ] || die "snapshot-state 返回 ${code}: ${body}"
paused=$(jq_get '.paused' "${body}")
st_build=$(jq_get '.build_id' "${body}")
log_info "paused=${paused} build_id=${st_build}"
[ "${paused}" = "true" ] || die "snapshot-state paused!=true: ${body}"
[ "${st_build}" = "${PAUSE_BUILD}" ] || die "snapshot-state build 不匹配: got=${st_build} want=${PAUSE_BUILD}"
log_pass "snapshot-state paused=true 且 build 匹配"

log_step "2.5 删除源 Pod ${POD1}（--wait）"
kubectl delete pod "${POD1}" --wait >>"${DETAIL_LOG}" 2>&1 || die "删除 ${POD1} 失败"
log_pass "Pod ${POD1} 已删除"
dump_pod "${POD1}" "Pause 删除后"

log_step "2.6 API 回写 build 终态：success（重复 success 幂等 200，改 failed 应 409）"
resp=$(api_call POST "/internal/builds/${PAUSE_BUILD}/status" "{\"operation_id\":\"${OP_ID}\",\"status\":\"success\"}")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
[ "${code}" = "200" ] || die "builds/status success 返回 ${code}: ${body}"
log_pass "builds/status success → 200"
resp=$(api_call POST "/internal/builds/${PAUSE_BUILD}/status" "{\"operation_id\":\"${OP_ID}\",\"status\":\"success\"}")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
[ "${code}" = "200" ] || die "重复 success 返回 ${code}: ${body}"
log_pass "重复 success 幂等 → 200"
resp=$(api_call POST "/internal/builds/${PAUSE_BUILD}/status" "{\"operation_id\":\"${OP_ID}\",\"status\":\"failed\"}")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
log_info "failed 终态冲突返回 http=${code} body=${body}"
if [ "${code}" = "409" ]; then
    log_pass "success 后改 failed → 409（终态保护生效）"
else
    log_fail "success 后改 failed 应 409，实际 ${code}: ${body}"
fi

#==================== §3 Resume ====================#

log_step "3.1 API GET last-snapshot（恢复元数据）"
resp=$(api_call GET "/internal/sandboxes/${SBX_ID}/last-snapshot")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
echo "${body}" >> "${DETAIL_LOG}"
[ "${code}" = "200" ] || die "last-snapshot 返回 ${code}: ${body}"
ls_build=$(jq_get '.build_id' "${body}")
ls_node=$(jq_get '.origin_node' "${body}")
log_info "build_id=${ls_build} origin_node=${ls_node}"
[ "${ls_build}" = "${PAUSE_BUILD}" ] || die "last-snapshot build 不匹配: got=${ls_build} want=${PAUSE_BUILD}"
[ "${ls_node}" = "${NODE_NAME}" ] || die "last-snapshot origin_node 不匹配: got=${ls_node} want=${NODE_NAME}"
log_pass "last-snapshot 200，build/origin_node 匹配"

log_step "3.2 创建 Resume Pod ${POD1R}（snapshot=true，sandbox-id 不变）"
EXEC_ID2=$(cat /proc/sys/kernel/random/uuid)    # [webhook] 新 Pod 的 execution-id；sandbox-id 沿用 SBX_ID
Y2=$(mktemp /tmp/sbx-pod-1r.XXXXXX.yaml); TMP_YAMLS+=("${Y2}")
render_pod_yaml "${Y2}" "${POD1R}" "${SBX_ID}" "true" "${PAUSE_TMPL}" "${PAUSE_BUILD}" "${EXEC_ID2}"
kubectl apply -f "${Y2}" >>"${DETAIL_LOG}" 2>&1 || die "kubectl apply ${POD1R} 失败"
CREATED_PODS+=("${POD1R}")
wait_pod_ready "${POD1R}" || die "pod/${POD1R} 未在 180s 内 Ready"
POD_UID2=$(kubectl get pod "${POD1R}" -o jsonpath='{.metadata.uid}')
log_info "POD_UID2=${POD_UID2} EXEC_ID2=${EXEC_ID2}"
log_pass "Pod ${POD1R} Ready（从快照恢复）"
dump_pod "${POD1R}" "Resume 后"

log_step "3.3 Admin GetSandboxRuntime（新 POD_UID2 反查同一 SBX_ID）"
out=$(gadmin "{\"cri_sandbox_id\":\"${POD_UID2}\"}" GetSandboxRuntime) || die "GetSandboxRuntime 调用失败: ${out}"
echo "${out}" >> "${DETAIL_LOG}"
got_sbx=$(jq_get '.e2bSandboxId' "${out}")
got_state=$(jq_get '.runtimeState' "${out}")
[ "${got_sbx}" = "${SBX_ID}" ] || die "新 Pod 未映射到同一稳定 ID: got=${got_sbx} want=${SBX_ID}"
[ "${got_state}" = "Running" ] || die "runtime_state 非 Running: ${got_state}"
log_pass "Resume 后双向索引正确: 新 cri=${POD_UID2} → sbx=${got_sbx}"

#==================== §4 Checkpoint ====================#

log_step "4.1 API POST snapshot-templates（checkpoint 元数据）"
OP_ID2="manual-ckpt-$(date +%s)"                # [webhook] 本次 Checkpoint 操作的 operation_id（幂等键）
log_info "POST /internal/sandboxes/${SBX_ID}/snapshot-templates op=${OP_ID2} source_pod_uid=${POD_UID2}"
resp=$(api_call POST "/internal/sandboxes/${SBX_ID}/snapshot-templates" "$(snapshot_meta_body "${OP_ID2}" "${POD_UID2}")")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
echo "${body}" >> "${DETAIL_LOG}"
[ "${code}" = "200" ] || die "snapshot-templates 返回 ${code}: ${body}"
CKPT_TMPL=$(jq_get '.template_id' "${body}")
CKPT_BUILD=$(jq_get '.build_id' "${body}")
CKPT_SNAPSHOT=$(jq_get '.snapshot_id' "${body}")
[ -n "${CKPT_TMPL}" ] && [ -n "${CKPT_BUILD}" ] && [ -n "${CKPT_SNAPSHOT}" ] || die "snapshot-templates 响应缺字段: ${body}"
log_info "CKPT_TMPL=${CKPT_TMPL} CKPT_BUILD=${CKPT_BUILD} CKPT_SNAPSHOT=${CKPT_SNAPSHOT}"
if [ "${CKPT_SNAPSHOT}" = "${CKPT_TMPL}:default" ]; then
    log_pass "snapshot_id 形如 <template_id>:default"
else
    log_fail "snapshot_id 不符合 <template_id>:default: ${CKPT_SNAPSHOT}"
fi

log_step "4.2 Admin CheckpointSandbox（无 template_id，原地快照）"
out=$(gadmin "{\"operation_id\":\"${OP_ID2}\",\"cri_sandbox_id\":\"${POD_UID2}\",\"e2b_sandbox_id\":\"${SBX_ID}\",\"team_id\":\"${TEAM_ID}\",\"build_id\":\"${CKPT_BUILD}\",\"timeout_seconds\":600}" CheckpointSandbox) \
    || die "CheckpointSandbox 调用失败: ${out}"
echo "${out}" >> "${DETAIL_LOG}"
persisted=$(jq_get '.snapshotPersisted' "${out}")
resumed=$(jq_get '.vmResumed' "${out}")
log_info "snapshot_persisted=${persisted} vm_resumed=${resumed}"
[ "${persisted}" = "true" ] || die "CheckpointSandbox snapshot_persisted!=true: ${out}"
[ "${resumed}" = "true" ] || die "CheckpointSandbox vm_resumed!=true: ${out}"
log_pass "CheckpointSandbox persisted=true 且 vm_resumed=true"
dump_pod "${POD1R}" "Checkpoint 后"
log_info "Checkpoint 快照: snapshot_id=${CKPT_SNAPSHOT} template_id=${CKPT_TMPL} build_id=${CKPT_BUILD}"

log_step "4.3 API 回写 checkpoint build 终态 success"
resp=$(api_call POST "/internal/builds/${CKPT_BUILD}/status" "{\"operation_id\":\"${OP_ID2}\",\"status\":\"success\"}")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
[ "${code}" = "200" ] || die "checkpoint builds/status 返回 ${code}: ${body}"
log_pass "checkpoint build success → 200"

log_step "4.4 API GET resolve/${CKPT_SNAPSHOT}"
resp=$(api_call GET "/internal/snapshots/${CKPT_SNAPSHOT}/resolve?team_id=${TEAM_ID}")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
echo "${body}" >> "${DETAIL_LOG}"
[ "${code}" = "200" ] || die "resolve 返回 ${code}: ${body}"
rs_tmpl=$(jq_get '.template_id' "${body}")
rs_build=$(jq_get '.build_id' "${body}")
[ "${rs_tmpl}" = "${CKPT_TMPL}" ] || die "resolve template 不匹配: got=${rs_tmpl} want=${CKPT_TMPL}"
[ "${rs_build}" = "${CKPT_BUILD}" ] || die "resolve build 不匹配: got=${rs_build} want=${CKPT_BUILD}"
log_pass "resolve 200，template/build 匹配"

log_step "4.5 确认源 Pod 全程 Running（未重建）"
phase=$(kubectl get pod "${POD1R}" -o jsonpath='{.status.phase}')
uid_now=$(kubectl get pod "${POD1R}" -o jsonpath='{.metadata.uid}')
[ "${phase}" = "Running" ] || die "checkpoint 后 Pod 非 Running: ${phase}"
[ "${uid_now}" = "${POD_UID2}" ] || die "checkpoint 后 Pod UID 变化（被重建）: ${uid_now} != ${POD_UID2}"
log_pass "Pod ${POD1R} 仍 Running 且 UID 未变"

#==================== §5 从 Checkpoint 创建新沙箱 ====================#

log_step "5.1 创建 Pod ${POD2}（新 sandbox-id，checkpoint template/build）"
SBX_ID2=$(gen_sbx_id)                           # [webhook] 新沙箱的稳定 sandbox-id；真实链路由 E2B API transform 生成，webhook 获取组装
EXEC_ID3=$(cat /proc/sys/kernel/random/uuid)    # [webhook] 新 Pod 的 execution-id
Y3=$(mktemp /tmp/sbx-pod-2.XXXXXX.yaml); TMP_YAMLS+=("${Y3}")
render_pod_yaml "${Y3}" "${POD2}" "${SBX_ID2}" "true" "${CKPT_TMPL}" "${CKPT_BUILD}" "${EXEC_ID3}"
kubectl apply -f "${Y3}" >>"${DETAIL_LOG}" 2>&1 || die "kubectl apply ${POD2} 失败"
CREATED_PODS+=("${POD2}")
wait_pod_ready "${POD2}" || die "pod/${POD2} 未在 180s 内 Ready"
log_info "SBX_ID2=${SBX_ID2}"
log_pass "Pod ${POD2} Ready（从 checkpoint 快照创建）"
dump_pod "${POD2}" "从 Checkpoint 创建"

log_step "5.2 Admin GetSandboxRuntime 核对 ${POD2}"
POD_UID3=$(kubectl get pod "${POD2}" -o jsonpath='{.metadata.uid}')
out=$(gadmin "{\"cri_sandbox_id\":\"${POD_UID3}\"}" GetSandboxRuntime) || die "GetSandboxRuntime 调用失败: ${out}"
echo "${out}" >> "${DETAIL_LOG}"
got_sbx=$(jq_get '.e2bSandboxId' "${out}")
got_state=$(jq_get '.runtimeState' "${out}")
[ "${got_sbx}" = "${SBX_ID2}" ] || die "sbx 不匹配: got=${got_sbx} want=${SBX_ID2}"
[ "${got_state}" = "Running" ] || die "runtime_state 非 Running: ${got_state}"
log_pass "Pod ${POD2} runtime 正确: sbx=${got_sbx} state=${got_state}"

#==================== §6 删除与清理断言 ====================#

log_step "6.1 删除测试 Pod（${POD2}、${POD1R}，--wait）"
kubectl delete pod "${POD2}" --wait >>"${DETAIL_LOG}" 2>&1 || die "删除 ${POD2} 失败"
kubectl delete pod "${POD1R}" --wait >>"${DETAIL_LOG}" 2>&1 || die "删除 ${POD1R} 失败"
CREATED_PODS=()
log_pass "测试 Pod 已删除"

log_step "6.2 API DELETE snapshot-templates/${CKPT_SNAPSHOT}（204，重复 204 幂等）"
resp=$(api_call DELETE "/internal/snapshot-templates/${CKPT_SNAPSHOT}?team_id=${TEAM_ID}")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
[ "${code}" = "204" ] || die "DELETE snapshot-templates 返回 ${code}: ${body}"
log_pass "DELETE snapshot-templates → 204"
resp=$(api_call DELETE "/internal/snapshot-templates/${CKPT_SNAPSHOT}?team_id=${TEAM_ID}")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
[ "${code}" = "204" ] || die "重复 DELETE snapshot-templates 返回 ${code}: ${body}"
log_pass "重复 DELETE snapshot-templates → 204（幂等）"

log_step "6.3 resolve 已删 checkpoint（应 404）"
resp=$(api_call GET "/internal/snapshots/${CKPT_SNAPSHOT}/resolve?team_id=${TEAM_ID}")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
if [ "${code}" = "404" ]; then
    log_pass "resolve 已删 snapshot → 404"
else
    log_fail "resolve 已删 snapshot 应 404，实际 ${code}: ${body}"
fi

# 注意顺序：必须先测 base 模板删除保护（409），此时 paused snapshot 仍引用 BASE_ENV；
# 若先删 paused snapshot，引用被清，409 不再成立。
log_step "6.4 API DELETE templates/${BASE_ENV}（有 snapshot 引用，应 409 保护）"
resp=$(api_call DELETE "/internal/templates/${BASE_ENV}?team_id=${TEAM_ID}")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
log_info "http=${code} body=${body}"
if [ "${code}" = "409" ]; then
    log_pass "DELETE base 模板 → 409（引用保护生效，未真删）"
else
    log_fail "DELETE base 模板应 409，实际 ${code}: ${body}（注意：绝不能真删 base env）"
fi

log_step "6.5 API DELETE sandboxes/${SBX_ID}/snapshot（204；重复删当前实现返回 404）"
resp=$(api_call DELETE "/internal/sandboxes/${SBX_ID}/snapshot")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
[ "${code}" = "204" ] || die "DELETE sandbox snapshot 返回 ${code}: ${body}"
log_pass "DELETE sandbox snapshot → 204"
resp=$(api_call DELETE "/internal/sandboxes/${SBX_ID}/snapshot")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
log_info "重复删除返回 http=${code}"
# 手册 §6.1：当前实现重复删除返回 404（非幂等 204），按此断言
if [ "${code}" = "404" ]; then
    log_pass "重复 DELETE sandbox snapshot → 404（与手册记录的非幂等行为一致）"
else
    log_fail "重复 DELETE sandbox snapshot 应 404（手册记录），实际 ${code}: ${body}"
fi

log_step "6.6 last-snapshot 已删（应 404）"
resp=$(api_call GET "/internal/sandboxes/${SBX_ID}/last-snapshot")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
if [ "${code}" = "404" ]; then
    log_pass "last-snapshot → 404"
else
    log_fail "last-snapshot 应 404，实际 ${code}: ${body}"
fi

log_step "6.7 snapshot-state（paused=false）"
resp=$(api_call GET "/internal/sandboxes/${SBX_ID}/snapshot-state")
code="${resp%%$'\t'*}"; body="${resp#*$'\t'}"
[ "${code}" = "200" ] || die "snapshot-state 返回 ${code}: ${body}"
paused=$(jq_get '.paused' "${body}")
if [ "${paused}" = "false" ]; then
    log_pass "snapshot-state paused=false"
else
    log_fail "snapshot-state 应 paused=false，实际: ${body}"
fi

#==================== 结束 ====================#

log_step "验证完毕"
if [ "${FAIL_COUNT}" -ne 0 ]; then
    echo "存在 ${FAIL_COUNT} 项失败断言" >&2
    exit 1
fi
exit 0
