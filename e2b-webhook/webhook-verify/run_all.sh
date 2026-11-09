#!/bin/bash
###############################################################################
# run_all.sh — webhook 验证套件入口
#
#   bash ./run_all.sh                 # 全量：00 → 01 → 02 → 03 → 04 → 05
#   bash ./run_all.sh --check-only    # 只探测，不改集群
#   bash ./run_all.sh --deploy-only   # 只做 00 的部署/替换
#   bash ./run_all.sh --skip-deploy   # 已部署，只验证
#   bash ./run_all.sh --skip-e2e      # 不跑 04
#   bash ./run_all.sh --no-sdk        # 04 里跳过 E8
#   bash ./run_all.sh --only contract # 只跑 01
#   bash ./run_all.sh --only 01,02    # 按编号
#
# 模块与用例（共 66 条）：
#   00  P1–P9    环境检查 / 构建 / 部署或替换 / 基线 / 单元测试
#   01  A1–A23   准入契约（A20 已移除、A22 已移入 02，共 21 条）：入口、Pod 注入、选路、选择器、边界
#   02  D1–D10 + A22  降级与安全：API 500/超时/拒连/非法 JSON/超大响应、Key、A22 合并超限、TLS、非法请求、证书 SAN/有效期、MWC 契约
#   03  K1–K7    K8S 集成：dry-run、真实创建、BatchSandbox、并发、删 Pod、滚动
#   04  E1–E14   e2b 沙箱端到端（E9 已移除，E8 含 pause/resume，共 12 条）
#   05  Z1–Z6    回滚与清理
#
# 退出码：0 全通过 / 1 有失败 / 77 无失败但存在 UNSUPPORTED
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"
. "${SCRIPT_DIR}/lib/k8s.sh"
. "${SCRIPT_DIR}/lib/webhook.sh"
. "${SCRIPT_DIR}/lib/provider.sh"

#==================== 参数 ====================#
CHECK_ONLY=0
DEPLOY_ONLY=0
SKIP_DEPLOY=0
SKIP_E2E_FLAG=0
NO_SDK=0
MOCK_ONLY=0
SKIP_UNIT=0
ONLY=""

usage() {
    cat <<'EOF'
用法: bash ./run_all.sh [选项]

  --check-only            只做 P1/P2/P3/P8（不改集群）
  --deploy-only           只做 00 的构建/部署/替换
  --skip-deploy           假定已部署，只验证
  --skip-e2e              不跑 04（e2b 沙箱端到端）
  --no-sdk                04 里跳过 E8（SDK）
  --only <类/编号>        只跑指定类别，可用逗号分隔：
                          prepare contract degraded k8s e2b（或 e2e） cleanup 或 00 01 02 03 04 05
  --e2e-batchsandbox      启用 E12（BatchSandbox 路径）
  --e2e-advanced          兼容旧参数（E13 已并入 E8，无实际作用）
  --mock                  用 mock API 顶替真实 API（隐含 --skip-e2e）
  --strict                已知差异 SKIP → FAIL（当前无此类用例，保留开关）
  --keep                  保留资源与镜像
  --cleanup-fresh         结束删除本轮新部署的 webhook 资源
  --skip-unit             00 里跳过 go vet/build/test
  --help                  显示本帮助

环境变量见 .env.example（WV_NAMESPACE / WV_E2E_TEMPLATE / WV_MOCK_HOST ...）
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --check-only) CHECK_ONLY=1; SKIP_DEPLOY=1 ;;
        --deploy-only) DEPLOY_ONLY=1 ;;
        --skip-deploy) SKIP_DEPLOY=1 ;;
        --skip-e2e) SKIP_E2E_FLAG=1 ;;
        --no-sdk) NO_SDK=1 ;;
        --only) ONLY="$2"; shift ;;
        --e2e-batchsandbox) WV_E2E_BATCHSANDBOX=1 ;;
        --e2e-advanced) WV_E2E_ADVANCED=1 ;;
        --mock) MOCK_ONLY=1; SKIP_E2E_FLAG=1; NO_SDK=1 ;;
        --strict) WV_STRICT=1 ;;
        --keep) WV_KEEP=1 ;;
        --cleanup-fresh) WV_CLEANUP_FRESH=1 ;;
        --skip-unit) SKIP_UNIT=1 ;;
        --help|-h) usage; exit 0 ;;
        *) log_error "未知参数: $1"; usage; exit 1 ;;
    esac
    shift
done

export WV_E2E_BATCHSANDBOX WV_E2E_ADVANCED WV_STRICT WV_KEEP \
    WV_CLEANUP_FRESH WV_UNIT WV_RESTORE_ON_EXIT
if [ "${NO_SDK}" = "1" ]; then
    WV_SDK=0
fi
if [ "${SKIP_UNIT}" = "1" ]; then
    WV_UNIT=0
fi
export WV_SDK

#==================== 模块表 ====================#
# 类别|编号|描述|脚本
MODULES=(
    "prepare|00|环境检查 / 部署 / 基线|00_prepare.sh"
    "contract|01|准入契约（/mutate + 注入 + 选路）|01_admission_contract.sh"
    "degraded|02|降级与安全（API 异常 / TLS / 非法请求）|02_degraded_security.sh"
    "k8s|03|K8S 集成 / 并发 / 可用性|03_k8s_integration.sh"
    "e2b|04|e2b 沙箱端到端|04_e2b_e2e.sh"
    "cleanup|05|回滚与清理|05_rollback_cleanup.sh"
)

selected() {
    local category="$1" id="$2"
    [ -z "${ONLY}" ] && return 0
    local item
    for item in ${ONLY//,/ }; do
        case "${item}" in
            "${category}"|"${id}") return 0 ;;
            e2e) [ "${category}" = "e2b" ] && return 0 ;;
        esac
    done
    return 1
}

# 用户是否显式指定了某个模块（类别名或编号都算；--only 时不做隐式跳过）
explicitly_selected() {
    local category="$1" id="$2"
    [ -z "${ONLY}" ] && return 1
    local item
    for item in ${ONLY//,/ }; do
        [ "${item}" = "${category}" ] && return 0
        [ "${item}" = "${id}" ] && return 0
        [ "${item}" = "e2e" ] && [ "${category}" = "e2b" ] && return 0
    done
    return 1
}

only_has_prepare() {
    local item
    for item in ${ONLY//,/ }; do
        case "${item}" in 00|prepare) return 0 ;; esac
    done
    return 1
}

#==================== 初始化 ====================#
log_section "webhook 准入控制器验证套件"

NOW_AT=$(date '+%F %T')
RUN_START_MS=$(now_ms)
: > "${RESULT_FILE}"

if [ -z "${ONLY}" ] || only_has_prepare; then
    # 只有本轮会跑 00 时才清空状态文件（--only cleanup 需要读上一轮的基线）。
    rm -f "${STATE_FILE}"
fi

log_info "仓库: ${REPO_ROOT}"
log_info "集群: $(k8s_context 2>/dev/null || echo '不可达')   namespace: ${WV_NAMESPACE}"
log_info "镜像: $(webhook_image_repo 2>/dev/null || echo '<未解析>'):${WV_IMAGE_TAG}"
log_info "日志: ${LOG_DIR}"
log_info "开始: ${NOW_AT}"

CLEANUP_DONE=0
RUN_RESULTS=()
TOTAL_PASS=0
TOTAL_FAIL=0
TOTAL_SKIP=0
TOTAL_UNSUPPORTED=0
ABORTED=0

emergency_cleanup() {
    local rc=$?
    set +e
    if [ "${CLEANUP_DONE}" != "1" ] && [ "${WV_RESTORE_ON_EXIT}" = "1" ] \
        && { [ "${ABORTED}" = "1" ] || [ "${rc}" -ne 0 ]; }; then
        log_warn "异常退出（rc=${rc}），执行应急恢复（05 --emergency）"
        "${SCRIPT_DIR}/05_rollback_cleanup.sh" --emergency >"${LOG_DIR}/run-05-emergency.log" 2>&1 || true
    fi
    admission_stop_port_forward 2>/dev/null
    mock_api_stop 2>/dev/null
    return "${rc}"
}
trap emergency_cleanup EXIT
trap 'ABORTED=1; exit 130' INT
trap 'ABORTED=1; exit 143' TERM

#==================== 计数解析 ====================#
parse_count() {
    local log_file="$1" key="$2"
    local value
    value=$(grep -aoP "(^|[^A-Z])${key}[:=]\\s*\\K[0-9]+" "${log_file}" 2>/dev/null | tail -1 || true)
    printf '%s' "${value:-0}"
}

format_status() {
    local pass=$1 fail=$2 skip=$3 unsup=$4
    printf 'PASS(%s/%s/%s)' "${pass}" "${fail}" "${skip}"
    if [ "${unsup}" != "0" ]; then
        printf ' UNSUP(%s)' "${unsup}"
    fi
    return 0
}

run_module() {
    local category="$1" id="$2" desc="$3" script="$4"; shift 4
    local log_file="${LOG_DIR}/run-${id}.log"
    local start_ms end_ms rc errexit_was_on=0

    # set -e 是 shell 全局状态：如果这里无条件 set -e 收尾，会把调用方的
    # “允许模块失败”重新打开——模块返回 77/1 时整个 run_all 会当场退出，
    # 后面的模块根本不会跑。所以必须先记住入口状态再原样恢复。
    if [ -o errexit ]; then
        errexit_was_on=1
    fi

    log_step "[${id}] ${desc}"
    start_ms=$(now_ms)
    set +e
    ( cd "${SCRIPT_DIR}" && run_with_timeout "${WV_MODULE_TIMEOUT}" "./${script}" "$@" ) 2>&1 | tee "${log_file}"
    rc=${PIPESTATUS[0]}
    if [ "${errexit_was_on}" = "1" ]; then
        set -e
    fi
    end_ms=$(now_ms)

    local p f s u
    p=$(parse_count "${log_file}" "PASS")
    f=$(parse_count "${log_file}" "FAIL")
    s=$(parse_count "${log_file}" "SKIP")
    u=$(parse_count "${log_file}" "UNSUPPORTED")

    if [ "${rc}" = "124" ]; then
        log_error "[${id}] 超时（${WV_MODULE_TIMEOUT}s）"
        f=$((f + 1))
    elif [ "${rc}" -ne 0 ] && [ "$((p + f + s + u))" -eq 0 ]; then
        # 脚本自身崩溃且没有产出任何用例记录
        log_error "[${id}] 异常退出（rc=${rc}），未产生用例记录"
        f=$((f + 1))
    fi

    TOTAL_PASS=$((TOTAL_PASS + p))
    TOTAL_FAIL=$((TOTAL_FAIL + f))
    TOTAL_SKIP=$((TOTAL_SKIP + s))
    TOTAL_UNSUPPORTED=$((TOTAL_UNSUPPORTED + u))

    local duration=$((end_ms - start_ms))
    RUN_RESULTS+=("${id}|${desc}|${p}|${f}|${s}|${u}|${duration}|${rc}")
    log_info "[${id}] $(format_status "${p}" "${f}" "${s}" "${u}")  耗时 $(format_ms "${duration}")"
    return "${rc}"
}

#==================== 执行 ====================#
for entry in "${MODULES[@]}"; do
    IFS='|' read -r category id desc script <<< "${entry}"
    selected "${category}" "${id}" || continue

    # 隐式跳过：--check-only / --deploy-only 只跑 00；--skip-e2e 跳过 04；
    # --mock 时 04 无意义。显式 --only 的模块不被隐式跳过。
    if ! explicitly_selected "${category}" "${id}"; then
        if [ "${CHECK_ONLY}" = "1" ] && [ "${id}" != "00" ]; then
            continue
        fi
        if [ "${DEPLOY_ONLY}" = "1" ] && [ "${id}" != "00" ]; then
            continue
        fi
        if [ "${id}" = "04" ] && { [ "${SKIP_E2E_FLAG}" = "1" ] || [ "${WV_E2E}" != "1" ]; }; then
            log_warn "[04] 跳过 e2b 沙箱端到端（--skip-e2e / --mock / WV_E2E=0）"
            RUN_RESULTS+=("04|${desc}|0|0|1|0|0|0")
            TOTAL_SKIP=$((TOTAL_SKIP + 1))
            continue
        fi
    fi

    module_args=()
    case "${id}" in
        00)
            [ "${SKIP_DEPLOY}" = "1" ] && module_args+=(--skip-deploy)
            [ "${CHECK_ONLY}" = "1" ] && module_args+=(--check-only)
            [ "${DEPLOY_ONLY}" = "1" ] && module_args+=(--deploy-only)
            [ "${WV_UNIT}" != "1" ] && module_args+=(--skip-unit)
            ;;
        04)
            [ "${WV_SDK}" != "1" ] && module_args+=(--no-sdk)
            [ "${WV_E2E_BATCHSANDBOX}" = "1" ] && module_args+=(--e2e-batchsandbox)
            [ "${WV_E2E_ADVANCED}" = "1" ] && module_args+=(--e2e-advanced)
            ;;
    esac

    set +e
    run_module "${category}" "${id}" "${desc}" "${script}" "${module_args[@]}"
    module_rc=$?
    set -e

    if [ "${id}" = "05" ]; then
        CLEANUP_DONE=1
    fi

    # 00 失败则终止后续（除非用户显式只要求某个模块）
    if [ "${id}" = "00" ] && [ "${module_rc}" -ne 0 ] && [ -z "${ONLY}" ] && [ "${CHECK_ONLY}" != "1" ]; then
        log_error "00 失败，终止后续模块（可用 --skip-deploy 只做验证）"
        ABORTED=1
        break
    fi
done

if [ ${#RUN_RESULTS[@]} -eq 0 ]; then
    log_error "没有匹配的模块（--only ${ONLY}）"
    usage
    exit 1
fi

# 全量跑完（或跳过 05）时确保清理
if [ "${CLEANUP_DONE}" != "1" ]; then
    if [ -z "${ONLY}" ] || explicitly_selected cleanup 05; then
        set +e
        run_module cleanup 05 "回滚与清理（补充执行）" "05_rollback_cleanup.sh"
        set -e
        CLEANUP_DONE=1
    fi
fi

#==================== 汇总 ====================#
RUN_TOTAL_MS=$(( $(now_ms) - RUN_START_MS ))
TOTAL=$((TOTAL_PASS + TOTAL_FAIL + TOTAL_SKIP + TOTAL_UNSUPPORTED))

printf '\n'
printf '%b\n' "${CYAN}════════════════════════════════════════════════════════════════${NC}"
printf '%b\n' "${BOLD}               webhook 验证汇总（${TOTAL} 条用例）${NC}"
printf '%b\n' "${CYAN}════════════════════════════════════════════════════════════════${NC}"
for row in "${RUN_RESULTS[@]}"; do
    IFS='|' read -r id desc p f s u duration rc <<< "${row}"
    status=$(format_status "${p}" "${f}" "${s}" "${u}")
    if [ "${f:-0}" -gt 0 ] || { [ "${rc:-0}" -ne 0 ] && [ "${rc:-0}" -ne 77 ]; }; then
        color="${RED}"
    elif [ "${u:-0}" -gt 0 ] || [ "${s:-0}" -gt 0 ]; then
        color="${YELLOW}"
    else
        color="${GREEN}"
    fi
    printf '%b [%s] %-44s %b%-18s%b %6s\n' "${CYAN}" "${id}" "${desc}" "${color}" "${status}" "${NC}" "$(format_ms "${duration}")"
done
printf '%b\n' "${CYAN}────────────────────────────────────────────────────────────────${NC}"
printf '%b\n' "${GREEN} PASS: ${TOTAL_PASS}${NC}   ${RED}FAIL: ${TOTAL_FAIL}${NC}   ${YELLOW}SKIP: ${TOTAL_SKIP}${NC}   ${YELLOW}UNSUP: ${TOTAL_UNSUPPORTED}${NC}   TOTAL: ${TOTAL}"
printf '%b\n' "${CYAN}════════════════════════════════════════════════════════════════${NC}"

if [ -f "${RESULT_FILE}" ]; then
    failed_rows=$(grep -P '^FAIL\t' "${RESULT_FILE}" 2>/dev/null || true)
    if [ -n "${failed_rows}" ]; then
        printf '\n%b\n' "${RED}失败用例：${NC}"
        printf '%s\n' "${failed_rows}" | while IFS=$'\t' read -r _ id desc; do
            printf '  ✗ %s  %s\n' "${id}" "${desc}"
        done
    fi
fi

if [ -f "${STATE_FILE}" ]; then
    log_info "状态文件: ${STATE_FILE}"
fi
log_info "完整日志: ${LOG_DIR}/run-*.log"
log_info "失败证据: ${ARTIFACT_DIR}/"

#==================== 耗时统计 ====================#
# 明细（按执行顺序）→ 最慢 3 项 → 总耗时（含毫秒）。
printf '\n'
printf '%b\n' "${CYAN}──────────────────── 用例耗时明细（按执行顺序）────────────────────${NC}"
for row in "${RUN_RESULTS[@]}"; do
    IFS='|' read -r id desc p f s u duration rc <<< "${row}"
    printf '  [%s] %8s  %s\n' "${id}" "$(format_ms "${duration:-0}")" "${desc}"
done

# 最慢 3 项：毫秒补零后排序，避免 head 触发 SIGPIPE 与 pipefail 冲突。
slows=$(printf '%s\n' "${RUN_RESULTS[@]}" \
    | awk -F'|' '{printf "%010d|%s|%s\n", (($7=="")?0:$7), $1, $2}' \
    | sort -t'|' -k1,1 -rn)
slow_idx=0
printf '%b\n' "${CYAN}  —— 最慢 3 项 ——${NC}"
while IFS='|' read -r ms id desc; do
    if [ -z "${ms}" ]; then
        continue
    fi
    if [ "${slow_idx}" -ge 3 ]; then
        break
    fi
    printf '  %8s  [%s] %s\n' "$(format_ms "$((10#${ms}))")" "${id}" "${desc}"
    slow_idx=$((slow_idx + 1))
done <<< "${slows}"
printf '%b\n' "${CYAN}──────────────────────────────────────────────────────────────────${NC}"
printf '%b\n' "${CYAN}⏱  总耗时 $(format_ms "${RUN_TOTAL_MS}")（${RUN_TOTAL_MS} ms）  ${NOW_AT} → $(date '+%F %T')${NC}"

if [ "${TOTAL_FAIL}" -gt 0 ]; then
    printf '%b\n' "${RED}✗ 有 ${TOTAL_FAIL} 条用例失败${NC}"
    exit 1
elif [ "${TOTAL_UNSUPPORTED}" -gt 0 ]; then
    printf '%b\n' "${YELLOW}△ 无失败，但有 ${TOTAL_UNSUPPORTED} 条 UNSUPPORTED（环境不具备能力，未计入 PASS）${NC}"
    exit "${UNSUPPORTED_EXIT_CODE}"
fi
printf '%b\n' "${GREEN}✓ 全部用例通过${NC}"
exit 0
