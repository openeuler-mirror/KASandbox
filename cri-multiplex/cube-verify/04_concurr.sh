#!/bin/bash
###############################################################################
# 04_concurr.sh — 并发冷启动（默认小规模：c=10 n=20）
#
# 旧默认 n=100（且 c 默认同 n）会在清理前堆积大量沙箱，p95 被长尾抬高。
# 可用环境变量覆盖：CUBE_E2E_C / CUBE_E2E_N
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

log_section "04 — Cube 并发冷启动"

C="${CUBE_E2E_C:-10}"
N="${CUBE_E2E_N:-20}"
if ! cube_multiplex_ready; then
  bash "${SCRIPT_DIR}/01_start_multiplex.sh" || exit 1
fi

run_cube_client concurr "${N}" "${C}" || { log_fail "concurr 失败"; exit 1; }
log_pass "concurr c=${C} n=${N}"
