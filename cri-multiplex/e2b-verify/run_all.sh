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
)

#==================== 执行 ====================#
TOTAL_PASS=0
TOTAL_FAIL=0
TOTAL_SKIP=0
RESULTS=()

for entry in "${SCRIPTS[@]}"; do
    IFS='|' read -r num desc path <<< "${entry}"

    # --only 过滤
    if [ -n "${ONLY}" ] && [ "${num}" != "${ONLY}" ]; then
        continue
    fi

    # --skip-setup 过滤
    if [ "${SKIP_SETUP}" = "1" ] && [ "${num}" = "00" ]; then
        log_skip "${desc}（--skip-setup）"
        RESULTS+=("${num}|${desc}|SKIP")
        TOTAL_SKIP=$((TOTAL_SKIP+1))
        continue
    fi

    echo ""
    log_info "执行 [${num}] ${desc} ..."

    # 执行子脚本，捕获退出码
    set +e
    script_output=$("${path}" 2>&1)
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
        RESULTS+=("${num}|${desc}|FAIL(${sub_pass}/${sub_fail}/${sub_skip})")
    fi
done

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
