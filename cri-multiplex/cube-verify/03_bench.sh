#!/bin/bash
###############################################################################
# 03_bench.sh — 多轮 create/exec/pause/resume 压测（默认 20）
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

log_section "03 — Cube bench"

N="${CUBE_E2E_N:-20}"
if ! cube_multiplex_ready; then
  bash "${SCRIPT_DIR}/01_start_multiplex.sh" || exit 1
fi

run_cube_client bench "${N}" || { log_fail "bench 失败"; exit 1; }
log_pass "bench n=${N}"
