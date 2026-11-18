#!/bin/bash
###############################################################################
# common.sh — cube-verify 公共配置与启停（风格对齐 e2b-verify/lib/common.sh）
###############################################################################
set -euo pipefail

SCRIPT_DIR_COMMON="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MULTIPLEX_DIR="${MULTIPLEX_DIR:-$(cd "${SCRIPT_DIR_COMMON}/.." && pwd)}"

# 默认走 /tmp，与 e2b-verify 一致；均可用环境变量覆盖
E2E_DIR="${CUBE_E2E_DIR:-/tmp/cube-verify}"
SOCKET="${SOCKET:-${E2E_DIR}/cri.sock}"
ADMIN_SOCKET="${ADMIN_SOCKET:-${E2E_DIR}/admin.sock}"
CONTAINERD_SOCKET="${CONTAINERD_SOCKET:-/run/containerd/containerd.sock}"
CUBELET_ADDR="${CUBELET_ADDR:-127.0.0.1:9999}"
CUBE_API_URL="${CUBE_API_URL:-http://127.0.0.1:3000}"
CUBE_TEMPLATE="${CUBE_E2E_TEMPLATE:-sandbox-code}"
CUBE_CLIENT_DIR="${CUBE_CLIENT_DIR:-${MULTIPLEX_DIR}/scripts/cube-e2e}"
LOG_DIR="${CUBE_VERIFY_LOG_DIR:-${E2E_DIR}/logs}"
PID_FILE="${E2E_DIR}/multiplex.pid"
MULTIPLEX_BIN="${MULTIPLEX_DIR}/cri-multiplex"

export GOCACHE="${GOCACHE:-/tmp/cri-multiplex-gocache}"
export GOPATH="${GOPATH:-/tmp/cri-multiplex-gopath}"
export GOWORK=off
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOSUMDB="${GOSUMDB:-off}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
NC='\033[0m'

PASS_COUNT="${PASS_COUNT:-0}"
FAIL_COUNT="${FAIL_COUNT:-0}"
SKIP_COUNT="${SKIP_COUNT:-0}"

log_info()  { echo -e "${CYAN}[INFO]${NC}  $*" >&2; }
log_pass()  { echo -e "${GREEN}[PASS]${NC}  $*" >&2; PASS_COUNT=$((PASS_COUNT+1)); export PASS_COUNT; }
log_fail()  { echo -e "${RED}[FAIL]${NC}  $*" >&2; FAIL_COUNT=$((FAIL_COUNT+1)); export FAIL_COUNT; }
log_skip()  { echo -e "${YELLOW}[SKIP]${NC}  $*" >&2; SKIP_COUNT=$((SKIP_COUNT+1)); export SKIP_COUNT; }
log_step()  { echo -e "\n${CYAN}========================================${NC}" >&2; echo -e "${CYAN} $* ${NC}" >&2; echo -e "${CYAN}========================================${NC}" >&2; }
log_section() { echo -e "\n${CYAN}╔══════════════════════════════════════════════════╗${NC}" >&2; echo -e "${CYAN}║  $*${NC}" >&2; echo -e "${CYAN}╚══════════════════════════════════════════════════╝${NC}\n" >&2; }

cube_multiplex_ready() {
  [[ -S "${SOCKET}" ]]
}

stop_cube_multiplex() {
  if [[ -f "${PID_FILE}" ]]; then
    local pid
    pid="$(cat "${PID_FILE}" 2>/dev/null || true)"
    if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
      kill "${pid}" 2>/dev/null || true
      wait "${pid}" 2>/dev/null || true
    fi
    rm -f "${PID_FILE}"
  fi
  pkill -f "${MULTIPLEX_BIN} .*${SOCKET}" 2>/dev/null || true
  rm -f "${SOCKET}" "${ADMIN_SOCKET}"
}

build_multiplex() {
  log_step "构建 cri-multiplex"
  (cd "${MULTIPLEX_DIR}" && ./build.sh) || {
    log_fail "build.sh 失败"
    return 1
  }
  log_pass "二进制就绪: ${MULTIPLEX_BIN}"
}

start_cube_multiplex() {
  mkdir -p "${E2E_DIR}/state" "${LOG_DIR}"
  stop_cube_multiplex
  log_step "启动 cri-multiplex（cube-enabled）"
  log_info "socket=${SOCKET} cubelet=${CUBELET_ADDR} template=${CUBE_TEMPLATE}"
  nohup "${MULTIPLEX_BIN}" \
    -socket "${SOCKET}" \
    -admin-socket "${ADMIN_SOCKET}" \
    -containerd-socket "${CONTAINERD_SOCKET}" \
    -state-dir "${E2E_DIR}/state" \
    -node-ip 127.0.0.1 \
    -node-name "$(hostname)" \
    -cube-enabled=true \
    -cubelet-address "${CUBELET_ADDR}" \
    -orphan-reconcile-enabled=false \
    -android-enabled=false \
    -cni-enabled=false \
    > "${LOG_DIR}/multiplex.log" 2>&1 &
  echo $! > "${PID_FILE}"
  local i=0
  while ! cube_multiplex_ready; do
    i=$((i + 1))
    if [[ $i -gt 50 ]]; then
      log_fail "cri socket 未就绪；日志:"
      tail -50 "${LOG_DIR}/multiplex.log" >&2 || true
      return 1
    fi
    sleep 0.1
  done
  log_pass "cri socket ready: ${SOCKET}"
}

# 跑 scripts/cube-e2e Go 客户端；mode=e2e|bench|density|concurr|hot
# 用法: run_cube_client <mode> [n] [c]
# concurr 默认建议小规模（n≈c），避免创建堆积把 p95 拉高。
run_cube_client() {
  local mode="$1"
  local n="${2:-1}"
  local c="${3:-}"
  local out="${LOG_DIR}/${mode}.out"
  mkdir -p "${LOG_DIR}"
  log_info "cube client mode=${mode} n=${n} c=${c:-auto} template=${CUBE_TEMPLATE} api=${CUBE_API_URL}"
  (
    cd "${CUBE_CLIENT_DIR}"
    args=(
      -mode "${mode}"
      -n "${n}"
      -cri-socket "unix://${SOCKET}"
      -admin-socket "unix://${ADMIN_SOCKET}"
      -cube-api "${CUBE_API_URL}"
      -template "${CUBE_TEMPLATE}"
    )
    if [[ -n "${c}" ]]; then
      args+=(-c "${c}")
    fi
    go run . "${args[@]}"
  ) | tee "${out}"
}
