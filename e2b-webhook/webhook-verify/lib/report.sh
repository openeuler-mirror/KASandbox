#!/bin/bash
###############################################################################
# report.sh — webhook-verify 结构化事件流（报告契约 v1）
#
# 设计要点：
#   - 只在日志/断言函数里追加一处 report_emit，不改任何用例脚本；
#   - 未设置 E2B_VERIFY_REPORT_DIR 时全部 no-op（本地直接跑输出与旧版一致）；
#   - 每行一个 JSON 对象，append-only，转换器在 CI 仓 report/ 下实现；
#   - 任何失败都不得影响用例本身（所有写入 best-effort，返回 0）。
#
###############################################################################

E2B_VERIFY_REPORT_CONTRACT="1"

# 纯 bash 处理 JSON 字符串转义：反斜杠、双引号、换行、制表符与控制字符。
_json_escape() {
    local s="${1-}"
    s="${s//\\/\\\\}"
    s="${s//\"/\\\"}"
    s="${s//$'\n'/\\n}"
    s="${s//$'\r'/\\r}"
    s="${s//$'\t'/\\t}"
    # 去掉其余不可打印控制字符（保留可读文本）
    printf '%s' "${s}" | LC_ALL=C tr -d '\000-\010\013\014\016-\037' 2>/dev/null || printf '%s' "${s}"
}

_report_now_ms() {
    date +%s%3N 2>/dev/null || echo 0
}

report_enabled() {
    [ -n "${E2B_VERIFY_REPORT_DIR:-}" ]
}

# report_emit <kind> [status] [msg]
# 额外字段通过环境变量 REPORT_EXTRA 传入（必须以逗号开头，例如 ',"exit":0'）。
report_emit() {
    report_enabled || return 0
    local kind="${1:-}" status="${2:-}" msg="${3:-}"
    shift 3 2>/dev/null || true

    local event_dir="${E2B_VERIFY_REPORT_DIR}/events"
    local file_id="${E2B_VERIFY_CASE_FILE:-${E2B_VERIFY_CASE_NUM:-standalone}}"
    local case_id="${E2B_VERIFY_CASE_ID:-${E2B_VERIFY_CASE_NUM:-standalone}}"
    local case_desc="${E2B_VERIFY_CASE_ID_DESC:-${E2B_VERIFY_CASE_DESC:-}}"
    local suite_id="${E2B_VERIFY_SUITE:-webhook-verify}"

    mkdir -p "${event_dir}" 2>/dev/null || return 0

    local esc_msg esc_desc esc_case esc_file esc_suite
    esc_msg="$(_json_escape "${msg}")"
    esc_desc="$(_json_escape "${case_desc}")"
    esc_case="$(_json_escape "${case_id}")"
    esc_file="$(_json_escape "${file_id}")"
    esc_suite="$(_json_escape "${suite_id}")"

    printf '{"v":1,"ts_ms":%s,"suite":"%s","case":"%s","case_desc":"%s","kind":"%s","status":"%s","msg":"%s"%s,"pid":%s}\n' \
        "$(_report_now_ms)" "${esc_suite}" "${esc_case}" "${esc_desc}" \
        "${kind}" "${status}" "${esc_msg}" "${REPORT_EXTRA:-}" "$$" \
        >>"${event_dir}/${esc_file}.jsonl" 2>/dev/null || true
    return 0
}

# report_begin_case <case_id> <desc> [file_id]
report_begin_case() {
    local id="${1:-standalone}" desc="${2:-}" file_id="${3:-${1:-standalone}}"
    export E2B_VERIFY_CASE_ID="${id}"
    export E2B_VERIFY_CASE_ID_DESC="${desc}"
    export E2B_VERIFY_CASE_FILE="${file_id}"
    report_emit case_start "" "${desc}"
}

# report_end_case <case_id> <exit_code> <dur_ms> [counts_json]
report_end_case() {
    local id="${1:-standalone}" exit_code="${2:-0}" dur_ms="${3:-0}"
    local counts="${4:-}"
    [ -n "${counts}" ] || counts='{"pass":0,"fail":0,"skip":0,"unsupported":0}'
    export E2B_VERIFY_CASE_ID="${id}"
    REPORT_EXTRA=",\"exit\":${exit_code},\"dur_ms\":${dur_ms},\"counts\":${counts}" \
        report_emit case_end "" ""
}

# report_attach <绝对路径> [说明]
report_attach() {
    local file="${1:-}" name="${2:-}"
    report_enabled || return 0
    [ -n "${file}" ] || return 0
    REPORT_EXTRA=",\"file\":\"$(_json_escape "${file}")\",\"name\":\"$(_json_escape "${name}")\"" \
        report_emit attach "" "${name}"
}

# report_meta <key> <value>：整轮的静态事实（版本、参数、阶段等）
report_meta() {
    local key="${1:-}" value="${2:-}"
    report_enabled || return 0
    [ -n "${key}" ] || return 0
    REPORT_EXTRA=",\"key\":\"$(_json_escape "${key}")\"" \
        report_emit meta "" "${value}"
}

# report_run_end <exit_code> <dur_ms> <counts_json>
report_run_end() {
    local exit_code="${1:-0}" dur_ms="${2:-0}"
    local counts="${3:-}"
    [ -n "${counts}" ] || counts='{"pass":0,"fail":0,"skip":0,"unsupported":0}'
    REPORT_EXTRA=",\"exit\":${exit_code},\"dur_ms\":${dur_ms},\"counts\":${counts}" \
        report_emit run_end "" ""
}

return 0