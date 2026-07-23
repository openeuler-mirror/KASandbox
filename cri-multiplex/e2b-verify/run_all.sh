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
    "11|Calico CNI PodIP 访问 E2B 沙箱验证|${SCRIPT_DIR}/11_cni_podip_access.sh"
    "12|Android RuntimeClass kubelet 沙箱创建验证|${SCRIPT_DIR}/12_android_kubelet_sandbox.sh"
    "13|Android 多实例 kubelet 沙箱创建验证|${SCRIPT_DIR}/13_android_multi_sandbox.sh"
    "14|E2B CNI Service/DNS 行为验证|${SCRIPT_DIR}/14_cni_service_dns.sh"
    "15|E2B CNI NetworkPolicy ingress 验证|${SCRIPT_DIR}/15_cni_networkpolicy_ingress.sh"
    "16|E2B CNI NetworkPolicy egress 验证|${SCRIPT_DIR}/16_cni_networkpolicy_egress.sh"
    "17|Android CNI PodIP/Netns 访问验证|${SCRIPT_DIR}/17_android_cni_podip.sh"
    "18|cri-multiplex 重启恢复验证|${SCRIPT_DIR}/18_state_restore.sh"
    "19|状态持久化完整场景矩阵验证|${SCRIPT_DIR}/19_state_persistence_matrix.sh"
)

#==================== 执行 ====================#
TOTAL_PASS=0
TOTAL_FAIL=0
TOTAL_SKIP=0
MATCHED=0
RESULTS=()

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
                set +e
                switch_output=$(E2B_CNI_ENABLED=0 E2B_FORCE_RESTART=1 "${SCRIPT_DIR}/01_start_multiplex.sh" 2>&1)
                switch_code=$?
                set -e
                echo "${switch_output}"
                if [ ${switch_code} -ne 0 ]; then
                    RESULTS+=("01-non-cni|切换 cri-multiplex 到非 CNI 模式|FAIL(0/0/0)")
                    TOTAL_FAIL=$((TOTAL_FAIL+1))
                    continue
                fi
                ;;
            07|08|09|10|11|14|15|16)
                log_info "切换 cri-multiplex 到 CNI 模式，用于 kubelet/CNI 用例 ..."
                set +e
                switch_output=$(E2B_CNI_ENABLED=1 E2B_FORCE_RESTART=1 "${SCRIPT_DIR}/01_start_multiplex.sh" 2>&1)
                switch_code=$?
                set -e
                echo "${switch_output}"
                if [ ${switch_code} -ne 0 ]; then
                    RESULTS+=("01-cni|切换 cri-multiplex 到 CNI 模式|FAIL(0/0/0)")
                    TOTAL_FAIL=$((TOTAL_FAIL+1))
                    continue
                fi
                ;;
            12|13|17)
                log_info "Android 用例会自行切换 cri-multiplex 到 CNI+Android runtime 模式 ..."
                ;;
        esac
    else
        if [ "${num}" = "01" ]; then
            env_args=(E2B_CNI_ENABLED=0 E2B_FORCE_RESTART=1)
        elif [ "${num}" = "07" ]; then
            log_info "切换 cri-multiplex 到 CNI 模式，用于 kubelet/CNI 用例 ..."
            set +e
            switch_output=$(E2B_CNI_ENABLED=1 "${SCRIPT_DIR}/01_start_multiplex.sh" 2>&1)
            switch_code=$?
            set -e
            echo "${switch_output}"
            if [ ${switch_code} -ne 0 ]; then
                RESULTS+=("01-cni|切换 cri-multiplex 到 CNI 模式|FAIL(0/0/0)")
                TOTAL_FAIL=$((TOTAL_FAIL+1))
                continue
            fi
        elif [ "${num}" = "12" ] || [ "${num}" = "13" ] || [ "${num}" = "17" ]; then
            log_info "Android 用例会自行切换 cri-multiplex 到 CNI+Android runtime 模式 ..."
        fi
    fi

    # 执行子脚本，捕获退出码
    set +e
    if [ ${#env_args[@]} -gt 0 ]; then
        script_output=$(env "${env_args[@]}" "${path}" 2>&1)
    else
        script_output=$("${path}" 2>&1)
    fi
    exit_code=$?
    set -e

    # 输出子脚本内容
    echo "${script_output}"

    # 解析子脚本的 PASS/FAIL/SKIP 计数
    sub_pass=$(echo "${script_output}" | grep -oP 'PASS:\s*\K[0-9]+' | tail -1 || echo "0")
    sub_fail=$(echo "${script_output}" | grep -oP 'FAIL:\s*\K[0-9]+' | tail -1 || echo "0")
    sub_skip=$(echo "${script_output}" | grep -oP 'SKIP:\s*\K[0-9]+' | tail -1 || echo "0")

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
