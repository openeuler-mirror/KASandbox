#!/bin/bash
###############################################################################
# 02_lifecycle.sh — Create / Checkpoint / Pause / Resume / Remove
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

log_section "02 — Cube 生命周期"

if ! cube_multiplex_ready; then
  log_info "multiplex 未就绪，先启动"
  bash "${SCRIPT_DIR}/01_start_multiplex.sh" || exit 1
fi

if run_cube_client e2e 1; then
  log_pass "lifecycle 完成"
else
  log_fail "lifecycle 失败（详见 ${LOG_DIR}/e2e.out）"
  exit 1
fi
