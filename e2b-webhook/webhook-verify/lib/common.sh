#!/bin/bash
###############################################################################
# common.sh — webhook 验证套件公共库
#
# 提供：路径与默认配置、日志、用例计数、state.env、耗时、退出码汇总。
# 所有类别脚本与 run_all.sh 都 source 本文件；日志一律写 stderr，
# 汇总计数写 stdout（run_all 从日志里解析 "PASS: n"）。
###############################################################################
set -euo pipefail

#==================== 路径 ====================#
WV_VERIFY_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WV_REPO_ROOT="$(cd "${WV_VERIFY_ROOT}/../.." && pwd)"

# 可选：把本机配置放到 webhook-verify/.env（参考 .env.example），避免每次导出变量。
if [ -f "${WV_ENV_FILE:-${WV_VERIFY_ROOT}/.env}" ]; then
    set -a
    # shellcheck disable=SC1090
    . "${WV_ENV_FILE:-${WV_VERIFY_ROOT}/.env}"
    set +a
fi

#==================== 默认配置（均可用同名环境变量覆盖） ====================#
export KUBECONFIG="${KUBECONFIG:-${HOME}/.kube/config}"

export WV_NAMESPACE="${WV_NAMESPACE:-e2b}"
export WV_DEPLOYMENT="${WV_DEPLOYMENT:-e2b-webhook}"
export WV_CONTAINER="${WV_CONTAINER:-e2b-webhook}"
export WV_SERVICE="${WV_SERVICE:-e2b-webhook}"
export WV_MWC="${WV_MWC:-e2b-webhook}"
export WV_TLS_SECRET="${WV_TLS_SECRET:-e2b-webhook-tls}"
export WV_APIKEY_SECRET="${WV_APIKEY_SECRET:-e2b-api-key}"

export WV_TEST_NS="${WV_TEST_NS:-webhook-verify}"
export WV_POD_LABEL="${WV_POD_LABEL:-batch-sandbox.sandbox.opensandbox.io/pod-index}"

export WV_MOCK_PORT="${WV_MOCK_PORT:-18080}"
# webhook 在集群内，mock 跑在本机；默认用节点 InternalIP，保证 Pod 网络可达。
export WV_MOCK_HOST="${WV_MOCK_HOST:-}"

export WV_PORT="${WV_PORT:-18443}"
export WV_E2E="${WV_E2E:-1}"
export WV_E2E_TIMEOUT="${WV_E2E_TIMEOUT:-300}"
export WV_E2E_TEMPLATE="${WV_E2E_TEMPLATE:-ubuntu-22-04-custom-1}"
export WV_E2E_IMAGE="${WV_E2E_IMAGE:-}"
export WV_E2E_POD="${WV_E2E_POD:-webhook-e2e-sandbox}"

export WV_SDK="${WV_SDK:-1}"
export WV_SDK_TIMEOUT="${WV_SDK_TIMEOUT:-300}"
export WV_SDK_API_URL="${WV_SDK_API_URL:-}"
export WV_E2B_CONFIG="${WV_E2B_CONFIG:-/root/.e2b/config.json}"

export WV_RESTORE_ON_EXIT="${WV_RESTORE_ON_EXIT:-1}"
export WV_KEEP="${WV_KEEP:-0}"
export WV_STRICT="${WV_STRICT:-0}"
export WV_UNIT="${WV_UNIT:-1}"
export WV_E2E_BATCHSANDBOX="${WV_E2E_BATCHSANDBOX:-0}"
export WV_E2E_ADVANCED="${WV_E2E_ADVANCED:-0}"

export WV_CRI_SOCKET="${WV_CRI_SOCKET:-/run/cri-multiplex.sock}"
export WV_ORCHESTRATOR="${WV_ORCHESTRATOR:-localhost:5008}"

# 单模块超时（秒）：run_all 用 timeout 包一层，避免卡死的用例拖垮整轮。
export WV_MODULE_TIMEOUT="${WV_MODULE_TIMEOUT:-3600}"
# 结束是否删除“本轮新部署”的 webhook 资源（--cleanup-fresh）
export WV_CLEANUP_FRESH="${WV_CLEANUP_FRESH:-0}"

export UNSUPPORTED_EXIT_CODE="${UNSUPPORTED_EXIT_CODE:-77}"

export REPO_ROOT="${REPO_ROOT:-${WV_REPO_ROOT}}"
export WV_IMAGE_REPO="${WV_IMAGE_REPO:-}"
export WV_IMAGE_TAG="${WV_IMAGE_TAG:-verify-$(date +%Y%m%d-%H%M%S)-$(git -C "${WV_REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || echo nogit)}"
IMAGE_REF=""

export WV_TMP="${WV_TMP:-/tmp/e2b-webhook-verify}"
export LOG_DIR="${LOG_DIR:-${WV_TMP}/logs}"
export ARTIFACT_DIR="${ARTIFACT_DIR:-${WV_TMP}/artifacts}"
export BASELINE_DIR="${BASELINE_DIR:-${WV_TMP}/baseline}"
export STATE_FILE="${STATE_FILE:-${WV_TMP}/state.env}"
export RESULT_FILE="${RESULT_FILE:-${WV_TMP}/results.tsv}"
export FIXTURE_DIR="${WV_VERIFY_ROOT}/fixtures"

mkdir -p "${WV_TMP}" "${LOG_DIR}" "${ARTIFACT_DIR}" "${BASELINE_DIR}" 2>/dev/null || true

#==================== 颜色 ====================#
if [ -t 2 ]; then
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
    CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; CYAN=''; BOLD=''; NC=''
fi

#==================== 计数 ====================#
PASS_COUNT="${PASS_COUNT:-0}"
FAIL_COUNT="${FAIL_COUNT:-0}"
SKIP_COUNT="${SKIP_COUNT:-0}"
UNSUPPORTED_COUNT="${UNSUPPORTED_COUNT:-0}"

#==================== 结构化报告事件流 ====================#
# 未设置 E2B_VERIFY_REPORT_DIR 时 report_emit 全部 no-op；CI 由 run-suite.sh 注入。
export E2B_VERIFY_SUITE="${E2B_VERIFY_SUITE:-webhook-verify}"
# shellcheck source=lib/report.sh
. "${WV_VERIFY_ROOT}/lib/report.sh"

# webhook 套件的 pass/fail/skip/unsupported 即一条用例，事件里直接形成
# case_start + assert + case_end；case 字段取 P1/A1/D1…，JUnit 每条一个 testcase。
_report_case_result() {
    local status="${1:-}" id="${2:-}"; shift 2 2>/dev/null || true
    report_enabled || return 0
    local exit_code=0 p=0 f=0 s=0 u=0
    case "${status}" in
        pass) p=1 ;;
        fail) f=1; exit_code=1 ;;
        skip) s=1 ;;
        unsupported) u=1; exit_code=77 ;;
    esac
    local counts="{\"pass\":${p},\"fail\":${f},\"skip\":${s},\"unsupported\":${u}}"
    local file_id="${E2B_VERIFY_CASE_FILE:-${E2B_VERIFY_CASE_NUM:-standalone}}"
    report_begin_case "${id}" "$*" "${file_id}"
    report_emit assert "${status}" "$*"
    report_end_case "${id}" "${exit_code}" 0 "${counts}"
}

_wv_record() {
    # $1=状态 $2=用例编号 $3=描述
    printf '%s\t%s\t%s\n' "$1" "$2" "$3" >> "${RESULT_FILE}" 2>/dev/null || true
}

log_info()  { printf '%b\n' "${CYAN}[INFO]${NC}  $*" >&2; }
log_warn()  { printf '%b\n' "${YELLOW}[WARN]${NC}  $*" >&2; }
log_error() { printf '%b\n' "${RED}[ERROR]${NC} $*" >&2; }
log_debug() { [ "${WV_DEBUG:-0}" = "1" ] && printf '%b\n' "${CYAN}[DEBUG]${NC} $*" >&2 || true; }

log_step() {
    report_emit step_start "" "$*"
    printf '\n%b\n' "${CYAN}────────────────────────────────────────────────────${NC}" >&2
    printf '%b\n' "${CYAN} $* ${NC}" >&2
    printf '%b\n' "${CYAN}────────────────────────────────────────────────────${NC}" >&2
}

log_section() {
    printf '\n%b\n' "${CYAN}╔══════════════════════════════════════════════════╗${NC}" >&2
    printf '%b\n' "${CYAN}║  $*${NC}" >&2
    printf '%b\n' "${CYAN}╚══════════════════════════════════════════════════╝${NC}" >&2
}

#==================== 用例记录 ====================#
# 约定：每个用例都有稳定编号（P1/A1/D1/K1/E1/Z1），编号同时用于日志与汇总，
# 便于把一次失败直接对回用例编号。
pass() {
    local id="$1"; shift
    _report_case_result pass "${id}" "$*"
    PASS_COUNT=$((PASS_COUNT + 1)); export PASS_COUNT
    printf '%b\n' "${GREEN}[PASS]${NC} ${BOLD}${id}${NC}  $*" >&2
    _wv_record PASS "${id}" "$*"
}

fail() {
    local id="$1"; shift
    _report_case_result fail "${id}" "$*"
    FAIL_COUNT=$((FAIL_COUNT + 1)); export FAIL_COUNT
    printf '%b\n' "${RED}[FAIL]${NC} ${BOLD}${id}${NC}  $*" >&2
    _wv_record FAIL "${id}" "$*"
}

skip() {
    local id="$1"; shift
    _report_case_result skip "${id}" "$*"
    SKIP_COUNT=$((SKIP_COUNT + 1)); export SKIP_COUNT
    printf '%b\n' "${YELLOW}[SKIP]${NC} ${BOLD}${id}${NC}  $*" >&2
    _wv_record SKIP "${id}" "$*"
}

unsupported() {
    local id="$1"; shift
    _report_case_result unsupported "${id}" "$*"
    UNSUPPORTED_COUNT=$((UNSUPPORTED_COUNT + 1)); export UNSUPPORTED_COUNT
    printf '%b\n' "${YELLOW}[UNSUPPORTED]${NC} ${BOLD}${id}${NC}  $*" >&2
    _wv_record UNSUPPORTED "${id}" "$*"
}

# 实现与理想行为的已知差异：默认 SKIP，--strict 时按 FAIL 计。
known_diff() {
    local id="$1"; shift
    if [ "${WV_STRICT}" = "1" ]; then
        fail "${id}" "$*（已知差异，--strict）"
    else
        skip "${id}" "$*（已知差异，默认跳过；--strict 视为失败）"
    fi
}

#==================== 断言辅助 ====================#
# 断言失败时只返回非 0，由调用方决定记 pass/fail（便于一次收集多条证据）。
assert_eq() { [ "$1" = "$2" ] || { log_debug "assert_eq: '$1' != '$2'"; return 1; }; }
assert_ne() { [ "$1" != "$2" ]; }
assert_nonempty() { [ -n "${1:-}" ]; }
assert_contains() { case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac; }
assert_file() { [ -s "$1" ]; }

# 把失败证据落到 artifacts 目录，返回文件路径（调用方打进 fail 消息）。
artifact_save() {
    local name="$1" content="$2"
    local path="${ARTIFACT_DIR}/${name}"
    printf '%s\n' "${content}" > "${path}" 2>/dev/null || true
    printf '%s' "${path}"
}

#==================== state.env ====================#
# 跨脚本共享的运行时事实（镜像 tag、基线、模板 ID 等）。值只允许单行。
state_set() {
    local key="$1" value="${2-}"
    value="${value//$'\n'/ }"
    mkdir -p "$(dirname "${STATE_FILE}")"
    touch "${STATE_FILE}"
    { grep -v "^${key}=" "${STATE_FILE}" 2>/dev/null || true; printf '%s=%s\n' "${key}" "${value}"; } \
        > "${STATE_FILE}.tmp"
    mv "${STATE_FILE}.tmp" "${STATE_FILE}"
}

state_get() {
    [ -f "${STATE_FILE}" ] || return 0
    local line
    line=$(grep -m1 "^$1=" "${STATE_FILE}" 2>/dev/null || true)
    printf '%s' "${line#*=}"
}

state_has() { [ -f "${STATE_FILE}" ] && grep -q "^$1=" "${STATE_FILE}" 2>/dev/null; }

state_dump() {
    log_info "state.env（${STATE_FILE}）"
    [ -f "${STATE_FILE}" ] && sed 's/^/    /' "${STATE_FILE}" >&2 || true
}

#==================== 时间 ====================#
now_ms() { date +%s%3N; }

# 每个类别脚本 source 本文件时记下起点，finish 里输出本模块耗时。
WV_MODULE_START_MS="${WV_MODULE_START_MS:-$(now_ms)}"

format_ms() {
    local ms="${1:-0}"
    [ -z "${ms}" ] && ms=0
    local s=$((ms / 1000))
    local d=$(((ms % 1000) / 100))
    if [ "${s}" -ge 60 ]; then
        printf '%dm%02ds' $((s / 60)) $((s % 60))
    else
        printf '%d.%ds' "${s}" "${d}"
    fi
}

#==================== 工具 ====================#
need_cmd() {
    command -v "$1" >/dev/null 2>&1
}

require_cmd() {
    local missing=()
    local c
    for c in "$@"; do
        need_cmd "${c}" || missing+=("${c}")
    done
    if [ ${#missing[@]} -gt 0 ]; then
        log_error "缺少命令: ${missing[*]}"
        return 1
    fi
}

# 在超时限制内执行命令（timeout 命令不存在时直接执行）。
run_with_timeout() {
    local seconds="$1"; shift
    if need_cmd timeout; then
        timeout "${seconds}" "$@"
    else
        "$@"
    fi
}

# 本机端口是否仍在 LISTEN（优先 ss，其次 lsof，最后 /dev/tcp）。
# 清理类用例用它验证“进程真的停了”，而不是只看 pid 文件是否被删掉。
port_listening() {
    local port="$1"
    if need_cmd ss; then
        ss -ltn 2>/dev/null | awk 'NR>1 {print $4}' | grep -qE "[:.]${port}$"
    elif need_cmd lsof; then
        lsof -nP -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1
    else
        (exec 3<>"/dev/tcp/127.0.0.1/${port}") >/dev/null 2>&1
    fi
}

# 轮询等待条件成立：wait_until <超时秒> <描述> <命令...>
wait_until() {
    local seconds="$1" desc="$2"; shift 2
    local deadline=$(( $(date +%s) + seconds ))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        if "$@" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    log_warn "等待超时（${seconds}s）: ${desc}"
    return 1
}

is_true() {
    case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
        1|true|yes|on) return 0 ;;
        *) return 1 ;;
    esac
}

#==================== 退出码 / 汇总 ====================#
summary_exit_code() {
    if [ "${FAIL_COUNT}" -gt 0 ]; then
        printf '1'
    elif [ "${UNSUPPORTED_COUNT}" -gt 0 ]; then
        printf '%s' "${UNSUPPORTED_EXIT_CODE}"
    else
        printf '0'
    fi
}

print_summary() {
    local title="${1:-验证汇总}"
    printf '\n%b\n' "${CYAN}════════════════════════════════════════════${NC}"
    printf '  %s\n' "${title}"
    printf '%b\n' "${CYAN}════════════════════════════════════════════${NC}"
    printf '%b\n' "${GREEN}  PASS: ${PASS_COUNT}${NC}"
    printf '%b\n' "${RED}  FAIL: ${FAIL_COUNT}${NC}"
    printf '%b\n' "${YELLOW}  SKIP: ${SKIP_COUNT}${NC}"
    printf '%b\n' "${YELLOW}  UNSUPPORTED: ${UNSUPPORTED_COUNT}${NC}"
    printf '%b\n' "${CYAN}  TOTAL: $((PASS_COUNT + FAIL_COUNT + SKIP_COUNT + UNSUPPORTED_COUNT))${NC}"
    local module_start elapsed_ms
    module_start="${WV_MODULE_START_MS:-$(now_ms)}"
    elapsed_ms=$(( $(now_ms) - module_start ))
    printf '%b\n' "${CYAN}  ⏱  耗时: $(format_ms "${elapsed_ms}")（${elapsed_ms} ms）${NC}"
    printf '%b\n' "${CYAN}════════════════════════════════════════════${NC}"

    if [ -f "${RESULT_FILE}" ]; then
        local failed
        failed=$(grep -P '^FAIL\t' "${RESULT_FILE}" 2>/dev/null || true)
        if [ -n "${failed}" ]; then
            printf '\n%b\n' "${RED}  失败用例：${NC}" >&2
            printf '%s\n' "${failed}" | while IFS=$'\t' read -r _ id desc; do
                printf '    ✗ %s  %s\n' "${id}" "${desc}" >&2
            done
        fi
    fi
    printf '\n' >&2
    report_emit summary "" "PASS=${PASS_COUNT} FAIL=${FAIL_COUNT} SKIP=${SKIP_COUNT} UNSUPPORTED=${UNSUPPORTED_COUNT}"
    log_info "日志: ${LOG_DIR}   证据: ${ARTIFACT_DIR}"
    if [ -f "${STATE_FILE}" ]; then
        log_info "状态: ${STATE_FILE}"
    fi
}

finish() {
    local title="${1:-验证汇总}"
    print_summary "${title}"
    exit "$(summary_exit_code)"
}
