#!/bin/bash
###############################################################################
# 01_start_multiplex.sh — 构建并启动 cube-enabled cri-multiplex
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

log_section "01 — 启动 cri-multiplex (cube)"

CUBE_FORCE_BUILD="${CUBE_FORCE_BUILD:-1}"
CUBE_FORCE_RESTART="${CUBE_FORCE_RESTART:-1}"

if [[ "${CUBE_FORCE_BUILD}" = "1" ]] || [[ ! -x "${MULTIPLEX_BIN}" ]]; then
  build_multiplex || exit 1
else
  log_info "跳过构建（CUBE_FORCE_BUILD=0 且二进制存在）"
fi

if cube_multiplex_ready && [[ "${CUBE_FORCE_RESTART}" != "1" ]]; then
  log_pass "已在运行: ${SOCKET}"
  exit 0
fi

start_cube_multiplex || exit 1
