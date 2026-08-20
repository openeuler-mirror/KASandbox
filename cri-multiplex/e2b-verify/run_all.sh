#!/bin/bash
###############################################################################
# run_all.sh — 全量顺序执行调度脚本
#
# 按顺序执行所有验证脚本，汇总最终结果
#
# 用法:
#   ./run_all.sh              # 执行全部
#   ./run_all.sh --skip-setup # 跳过环境准备（已安装过）
#   ./run_all.sh --only 02    # 只执行 02_lifecycle.sh
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "cri-multiplex grpc_e2b 全接口自动化验证"

#==================== 参数解析 ====================#
SKIP_SETUP=0
ONLY=""
LOG_DIR="${E2B_VERIFY_LOG_DIR:-/tmp}"
SHARED_E2B_FIXTURE_READY=0

while [[ $# -gt 0 ]]; do
    case $1 in
        --skip-setup)
            SKIP_SETUP=1
            shift
            ;;
        --only)
            ONLY="$2"
            shift 2
            ;;
        --help|-h)
            echo "用法: $0 [--skip-setup] [--only <脚本编号>]"
            echo "  --skip-setup   跳过环境准备"
            echo "  --only 02      只执行指定编号的脚本"
            exit 0
            ;;
        *)
            echo "未知参数: $1"
            exit 1
            ;;
    esac
done

#==================== 脚本列表 ====================#
# 格式: "编号|描述|脚本路径"
SCRIPTS=(
    "00|验证工具安装与环境准备|${SCRIPT_DIR}/00_setup.sh"
    "01|启动 cri-multiplex|${SCRIPT_DIR}/01_start_multiplex.sh"
    "02|生命周期管理验证|${SCRIPT_DIR}/02_lifecycle.sh"
    "03|镜像管理验证|${SCRIPT_DIR}/03_image.sh"
    "04|Exec 能力验证|${SCRIPT_DIR}/04_exec.sh"
    "05|ExecSync 能力验证|${SCRIPT_DIR}/05_execsync.sh"
    "06|Attach 能力验证|${SCRIPT_DIR}/06_attach.sh"
    "07|kubelet 对接 Pod 保持 Running 验证|${SCRIPT_DIR}/07_kubelet_pod_running.sh"
    "08|Exec 能力 kubelet 验证|${SCRIPT_DIR}/08_exec_kubelet.sh"
    "09|ExecSync 能力 kubelet 验证|${SCRIPT_DIR}/09_execsync_kubelet.sh"
    "10|Attach 能力 kubelet 验证|${SCRIPT_DIR}/10_attach_kubelet.sh"
    "11|CNI PodIP 访问 E2B 沙箱验证（Calico/bridge 自适应）|${SCRIPT_DIR}/11_cni_podip_access.sh"
    "12|Android RuntimeClass kubelet 沙箱创建验证|${SCRIPT_DIR}/12_android_kubelet_sandbox.sh"
    "13|Android 多实例 kubelet 沙箱创建验证|${SCRIPT_DIR}/13_android_multi_sandbox.sh"
    "14|E2B CNI Service/DNS 行为验证|${SCRIPT_DIR}/14_cni_service_dns.sh"
    "15|E2B CNI NetworkPolicy ingress 验证|${SCRIPT_DIR}/15_cni_networkpolicy_ingress.sh"
    "16|E2B CNI NetworkPolicy egress 验证|${SCRIPT_DIR}/16_cni_networkpolicy_egress.sh"
    "17|Android CNI PodIP/Netns 访问验证|${SCRIPT_DIR}/17_android_cni_podip.sh"
	"18|cri-multiplex 重启恢复验证|${SCRIPT_DIR}/18_state_restore.sh"
    "19|状态持久化完整场景矩阵验证|${SCRIPT_DIR}/19_state_persistence_matrix.sh"
	"20|清理与孤儿资源回收验证|${SCRIPT_DIR}/20_cleanup_orphan_recovery.sh"
    "21|cri-multiplex 多 Runtime 路由验证|${SCRIPT_DIR}/21_mux_multi_runtime_routing.sh"
    "22|Pause Checkpoint Resume 全流程端到端验证|${SCRIPT_DIR}/22_pause_checkpoint_resume.sh"
    "23|重启 cri-multiplex 并隐藏 direct sandbox|${SCRIPT_DIR}/23_restart_multiplex_hide_direct.sh"
)

#==================== 执行 ====================#
TOTAL_PASS=0
TOTAL_FAIL=0
TOTAL_SKIP=0
MATCHED=0
RESULTS=()
mkdir -p "${LOG_DIR}"

parse_count_from_log() {
    local log_file="$1"
    local key="$2"
    # 同时兼容 "PASS: 12"（lib/common.sh print_summary）与 "PASS=12"（22 号自包含汇总）两种格式
    local v
    v=$(grep -aoP "${key}[:=]\\s*\\K[0-9]+" "${log_file}" 2>/dev/null | tail -1)
    echo "${v:-0}"
}

run_streamed() {
    local log_file="$1"
    shift
    local restore_errexit=0
    if [[ $- == *e* ]]; then
        restore_errexit=1
    fi

    : > "${log_file}"
    set +e
    "$@" 2>&1 | tee "${log_file}"
    local exit_code=${PIPESTATUS[0]}
    if [ "${restore_errexit}" = "1" ]; then
        set -e
    else
        set +e
    fi
    return "${exit_code}"
}

prepare_shared_e2b_fixture_once() {
    if [ "${SHARED_E2B_FIXTURE_READY}" = "1" ]; then
        return 0
    fi

    log_info "准备 07 及之后用例共享 E2B fixture: /tmp/e2b-kubelet-pod.yaml"
    E2B_SKIP_BUILD=0 E2B_YAML_COUNT=0 refresh_or_reuse_e2b_yaml \
        "${SCRIPT_DIR}/lib/refresh_build_id.sh" \
        "e2b-kubelet-test" \
        "/tmp/e2b-kubelet-pod.yaml" || return 1
    export E2B_SKIP_BUILD=1
    export E2B_BASE_POD_YAML=/tmp/e2b-kubelet-pod.yaml
    SHARED_E2B_FIXTURE_READY=1
}

for entry in "${SCRIPTS[@]}"; do
    IFS='|' read -r num desc path <<< "${entry}"

    # --only 过滤
    if [ -n "${ONLY}" ] && [ "${num}" != "${ONLY}" ]; then
        continue
    fi
    MATCHED=$((MATCHED+1))

    # --skip-setup 过滤
    if [ "${SKIP_SETUP}" = "1" ] && [ "${num}" = "00" ]; then
        log_skip "${desc}（--skip-setup）"
        RESULTS+=("${num}|${desc}|SKIP")
        TOTAL_SKIP=$((TOTAL_SKIP+1))
        continue
    fi

    echo ""
    log_info "执行 [${num}] ${desc} ..."

    env_args=()
    if [ -n "${ONLY}" ]; then
        case "${num}" in
            02|04|05|06)
                log_info "切换 cri-multiplex 到非 CNI 模式，用于 crictl 直连用例 ..."
                switch_log="${LOG_DIR}/e2b-verify-switch-non-cni.log"
                if ! run_streamed "${switch_log}" env CNI_ENABLED=0 E2B_FORCE_RESTART=1 "${SCRIPT_DIR}/01_start_multiplex.sh"; then
                    RESULTS+=("01-non-cni|切换 cri-multiplex 到非 CNI 模式|FAIL(0/0/0)")
                    TOTAL_FAIL=$((TOTAL_FAIL+1))
                    continue
                fi
                ;;
            07|08|09|10|11|12|13|14|15|16|17|21|22)
                log_info "切换 cri-multiplex 到 CNI+Android runtime 模式，用于 07 及之后用例 ..."
                switch_log="${LOG_DIR}/e2b-verify-switch-cni-android.log"
                if ! run_streamed "${switch_log}" start_cni_android_multiplex "切换 cri-multiplex 到 CNI+Android runtime 模式"; then
                    RESULTS+=("01-cni-android|切换 cri-multiplex 到 CNI+Android runtime 模式|FAIL(0/0/0)")
                    TOTAL_FAIL=$((TOTAL_FAIL+1))
                    continue
                fi
                ;;
			18|19|20)
                log_info "[${num}] 用例内部会以 CNI+Android runtime 模式启动 cri-multiplex，并使用独立 state-dir ..."
                ;;
        esac
    else
        if [ "${num}" = "01" ]; then
            env_args=(CNI_ENABLED=0 E2B_FORCE_RESTART=1)
        elif [ "${num}" = "07" ]; then
            log_info "切换 cri-multiplex 到 CNI+Android runtime 模式，用于 07 及之后用例 ..."
            switch_log="${LOG_DIR}/e2b-verify-switch-cni-android.log"
            if ! run_streamed "${switch_log}" start_cni_android_multiplex "切换 cri-multiplex 到 CNI+Android runtime 模式"; then
                RESULTS+=("01-cni-android|切换 cri-multiplex 到 CNI+Android runtime 模式|FAIL(0/0/0)")
                TOTAL_FAIL=$((TOTAL_FAIL+1))
                continue
            fi
            if ! prepare_shared_e2b_fixture_once; then
                RESULTS+=("fixture-e2b|准备 07 及之后共享 E2B fixture|FAIL(0/0/0)")
                TOTAL_FAIL=$((TOTAL_FAIL+1))
                continue
            fi
        fi
    fi

    # 执行子脚本，实时输出到控制台，同时保留完整日志便于定位卡住/失败。
    log_file="${LOG_DIR}/e2b-verify-${num}.log"
    set +e
    if [ ${#env_args[@]} -gt 0 ]; then
        run_streamed "${log_file}" env "${env_args[@]}" "${path}"
    else
        run_streamed "${log_file}" "${path}"
    fi
    exit_code=$?
    set -e

    # 解析子脚本的 PASS/FAIL/SKIP 计数
    sub_pass=$(parse_count_from_log "${log_file}" "PASS")
    sub_fail=$(parse_count_from_log "${log_file}" "FAIL")
    sub_skip=$(parse_count_from_log "${log_file}" "SKIP")

    TOTAL_PASS=$((TOTAL_PASS + sub_pass))
    TOTAL_FAIL=$((TOTAL_FAIL + sub_fail))
    TOTAL_SKIP=$((TOTAL_SKIP + sub_skip))

    if [ ${exit_code} -eq 0 ]; then
        RESULTS+=("${num}|${desc}|PASS(${sub_pass}/${sub_fail}/${sub_skip})")
    else
        if [ "${sub_fail}" = "0" ]; then
            TOTAL_FAIL=$((TOTAL_FAIL + 1))
            sub_fail=1
        fi
        RESULTS+=("${num}|${desc}|FAIL(${sub_pass}/${sub_fail}/${sub_skip})")
    fi
done

if [ -n "${ONLY}" ] && [ "${MATCHED}" -eq 0 ]; then
    log_fail "未找到编号为 ${ONLY} 的用例"
    echo "可用编号:"
    for entry in "${SCRIPTS[@]}"; do
        IFS='|' read -r num desc path <<< "${entry}"
        echo "  ${num}  ${desc}"
    done
    exit 1
fi

#==================== 最终汇总 ====================#
echo ""
echo -e "${CYAN}╔══════════════════════════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║                    全量验证最终汇总报告                          ║${NC}"
echo -e "${CYAN}╠══════════════════════════════════════════════════════════════════╣${NC}"
echo -e "${CYAN}║                                                                  ║${NC}"

for r in "${RESULTS[@]}"; do
    IFS='|' read -r num desc status <<< "${r}"
    # 计算填充空格
    padding_num=$((3 - ${#num}))
    padding_desc=$((30 - ${#desc}))
    pad_num=""
    pad_desc=""
    for ((i=0; i<padding_num; i++)); do pad_num+=" "; done
    for ((i=0; i<padding_desc; i++)); do pad_desc+=" "; done

    if echo "${status}" | grep -q "^PASS"; then
        printf "${CYAN}║${NC} [${num}]${pad_num} ${desc}${pad_desc} ${GREEN}%-20s${NC} ${CYAN}║${NC}\n" "${status}"
    elif echo "${status}" | grep -q "^SKIP"; then
        printf "${CYAN}║${NC} [${num}]${pad_num} ${desc}${pad_desc} ${YELLOW}%-20s${NC} ${CYAN}║${NC}\n" "${status}"
    else
        printf "${CYAN}║${NC} [${num}]${pad_num} ${desc}${pad_desc} ${RED}%-20s${NC} ${CYAN}║${NC}\n" "${status}"
    fi
done

echo -e "${CYAN}╠══════════════════════════════════════════════════════════════════╣${NC}"
printf "${CYAN}║${NC}  ${GREEN}PASS: %-5d${NC}  ${RED}FAIL: %-5d${NC}  ${YELLOW}SKIP: %-5d${NC}  TOTAL: %-5d   ${CYAN}║${NC}\n" \
    "${TOTAL_PASS}" "${TOTAL_FAIL}" "${TOTAL_SKIP}" "$((TOTAL_PASS+TOTAL_FAIL+TOTAL_SKIP))"
echo -e "${CYAN}╚══════════════════════════════════════════════════════════════════╝${NC}"
echo ""

if [ ${TOTAL_FAIL} -eq 0 ]; then
    echo -e "${GREEN}✓ 全部验证通过！${NC}"
    exit 0
else
    echo -e "${RED}✗ 有 ${TOTAL_FAIL} 个测试失败${NC}"
    exit 1
fi
