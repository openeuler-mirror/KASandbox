#!/bin/bash
###############################################################################
# 05_hot.sh — 并发热启动：create + pause + resume（默认 100）
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

log_section "05 — Cube 并发热启动"

N="${CUBE_E2E_N:-100}"
if ! cube_multiplex_ready; then
  bash "${SCRIPT_DIR}/01_start_multiplex.sh" || exit 1
fi

run_cube_client hot "${N}" || { log_fail "hot 失败"; exit 1; }
log_pass "hot n=${N}"
