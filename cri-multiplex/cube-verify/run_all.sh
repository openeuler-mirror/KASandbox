#!/bin/bash
###############################################################################
# run_all.sh — cube-verify 顺序调度（对齐 e2b-verify/run_all.sh）
#
# 用法:
#   ./run_all.sh              # 01→02（冒烟）
#   ./run_all.sh --all        # 01→06 全量
#   ./run_all.sh --only 02    # 只跑指定编号
#   ./run_all.sh --keep       # 结束后不杀 multiplex
# 多轮 CRI 与原生 CubeBench 对照不在这里，见同目录 run_stability.sh。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

log_section "cri-multiplex cube-verify"

ONLY=""
KEEP=0
FULL=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --only)
      ONLY="${2:-}"
      shift 2
      ;;
    --all)
      FULL=1
      shift
      ;;
    --keep)
      KEEP=1
      shift
      ;;
    -h|--help)
      sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "未知参数: $1" >&2
      exit 1
      ;;
  esac
done

SCRIPTS=(
  "01|启动 cri-multiplex|${SCRIPT_DIR}/01_start_multiplex.sh"
  "02|生命周期 create/checkpoint/pause/resume|${SCRIPT_DIR}/02_lifecycle.sh"
  "03|多轮 bench|${SCRIPT_DIR}/03_bench.sh"
  "04|并发冷启动|${SCRIPT_DIR}/04_concurr.sh"
  "05|并发热启动|${SCRIPT_DIR}/05_hot.sh"
  "06|密度|${SCRIPT_DIR}/06_density.sh"
)

should_run() {
  local num="$1"
  if [[ -n "${ONLY}" ]]; then
    [[ "${num}" == "${ONLY}" ]]
    return
  fi
  if [[ "${FULL}" = "1" ]]; then
    return 0
  fi
  # 默认冒烟：01 + 02
  [[ "${num}" == "01" || "${num}" == "02" ]]
}

TOTAL_PASS=0
TOTAL_FAIL=0
RESULTS=()

cleanup() {
  if [[ "${KEEP}" = "1" ]]; then
    log_info "保留 multiplex pid=$(cat "${PID_FILE}" 2>/dev/null || echo '?') log=${LOG_DIR}/multiplex.log"
  else
    stop_cube_multiplex || true
  fi
}
trap cleanup EXIT

for entry in "${SCRIPTS[@]}"; do
  IFS='|' read -r num desc path <<<"${entry}"
  if ! should_run "${num}"; then
    continue
  fi
  log_step "${num} ${desc}"
  set +e
  bash "${path}"
  rc=$?
  set -e
  if [[ $rc -eq 0 ]]; then
    TOTAL_PASS=$((TOTAL_PASS + 1))
    RESULTS+=("${num} PASS")
  else
    TOTAL_FAIL=$((TOTAL_FAIL + 1))
    RESULTS+=("${num} FAIL")
    log_fail "${num} 退出码 ${rc}"
    break
  fi
done

echo
log_section "汇总"
for r in "${RESULTS[@]}"; do
  echo "  ${r}"
done
echo "  pass=${TOTAL_PASS} fail=${TOTAL_FAIL}"
[[ "${TOTAL_FAIL}" -eq 0 ]]
