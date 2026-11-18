#!/bin/bash
###############################################################################
# 06_density.sh — 同时存活 N 个沙箱，估 per-VM 内存（默认 20）
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

log_section "06 — Cube 密度"

N="${CUBE_E2E_N:-20}"
if ! cube_multiplex_ready; then
  bash "${SCRIPT_DIR}/01_start_multiplex.sh" || exit 1
fi

run_cube_client density "${N}" || { log_fail "density 失败"; exit 1; }
log_pass "density n=${N}"
