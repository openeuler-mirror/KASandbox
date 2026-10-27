#!/bin/bash
###############################################################################
# systemd_mux.sh — 让验证脚本安全接管 systemd 管理的 cri-multiplex
#
# 背景：
#   - 测试节点上 cri-multiplex 由 systemd 管理，socket 为 /run/cri-multiplex.sock，
#     kubelet 的 runtime-endpoint 也指向它。
#   - 验证只替换二进制：停止 systemd 实例后，用当前仓构建的二进制在同一个 socket
#     上启动受控实例；kubelet 配置不动（socket 重建后它的 gRPC 连接会自动重连）。
#
# 策略：
#   1. 当 SOCKET 命中 systemd socket 且 unit 正在运行时，验证脚本先
#      `systemctl stop`，并写 marker；
#   2. 脚本用相同 socket 直接启动受控进程（日志固定 /tmp/cri-multiplex/e2b-verify/cri-multiplex.log）；
#   3. 恢复：run_all.sh 的 EXIT trap 自动执行，也可手动 01_start_multiplex.sh --restore。
#
# 这样既保留 kubelet 真实 endpoint，又避免 systemd 的 Restart=always 干扰
# 18/19/20 这类“停止/重启/kill -9”用例。
###############################################################################

# 本文件由 lib/common.sh 引入（TMP_ROOT 已在那边定义并 export）；
# 单独 source 时兜底成同一默认值，避免拼出 /e2b-verify-systemd-claimed 这种根路径。
TMP_ROOT="${TMP_ROOT:-/tmp/cri-multiplex/e2b-verify}"
SYSTEMD_UNIT="${SYSTEMD_UNIT:-cri-multiplex.service}"
SYSTEMD_SOCKET="${SYSTEMD_SOCKET:-/run/cri-multiplex.sock}"
SYSTEMD_CLAIM_MARKER="${SYSTEMD_CLAIM_MARKER:-${TMP_ROOT}/e2b-verify-systemd-claimed}"
MULTIPLEX_LOG="${MULTIPLEX_LOG:-${TMP_ROOT}/cri-multiplex.log}"

systemd_mux_available() {
    command -v systemctl >/dev/null 2>&1 && systemctl cat "${SYSTEMD_UNIT}" >/dev/null 2>&1
}

systemd_mux_active() {
    systemd_mux_available && systemctl is-active --quiet "${SYSTEMD_UNIT}"
}

systemd_mux_main_pid() {
    systemd_mux_available || return 0
    systemctl show -p MainPID --value "${SYSTEMD_UNIT}" 2>/dev/null || true
}

pid_is_systemd_mux() {
    local pid="$1"
    local main
    [ -n "${pid}" ] || return 1
    main=$(systemd_mux_main_pid)
    [ -n "${main}" ] && [ "${main}" != "0" ] && [ "${pid}" = "${main}" ]
}

# socket 上正在监听的实例是否由 systemd 管理（而不是脚本启动的受控实例）。
# 用例断言依赖 /tmp/cri-multiplex/e2b-verify/cri-multiplex.log，而 systemd 实例的日志在 journald，
# 所以即使是“可连通”的 systemd 实例也必须接管。
running_multiplex_is_systemd() {
    systemd_mux_active || return 1
    local pid
    for pid in $(cri_multiplex_pids 2>/dev/null || true); do
        pid_is_systemd_mux "${pid}" && return 0
    done
    return 1
}

systemd_mux_claimed() {
    [ -f "${SYSTEMD_CLAIM_MARKER}" ]
}

# 停止 systemd 实例，把 SOCKET 让给验证脚本。只有 SOCKET 命中 systemd socket 时生效。
claim_systemd_socket() {
    [ "${SOCKET}" = "${SYSTEMD_SOCKET}" ] || return 0
    systemd_mux_available || return 0
    if systemd_mux_active; then
        log_info "检测到 systemd ${SYSTEMD_UNIT} 正在管理 ${SOCKET}，验证期间临时由脚本接管"
        if ! systemctl stop "${SYSTEMD_UNIT}"; then
            log_fail "停止 ${SYSTEMD_UNIT} 失败，无法接管 ${SOCKET}"
            return 1
        fi
    fi
    # 即使 unit 本来未运行，也记录 marker，便于 run_all 结束时恢复。
    : > "${SYSTEMD_CLAIM_MARKER}"
    local deadline=$(( $(date +%s) + 10 ))
    while systemd_mux_active && [ "$(date +%s)" -lt "${deadline}" ]; do
        sleep 1
    done
    if systemd_mux_active; then
        log_fail "${SYSTEMD_UNIT} 未在 10s 内停止"
        return 1
    fi
    return 0
}

# 恢复 systemd 服务（幂等）。会先清理脚本直接启动的 /run 实例。
restore_systemd_multiplex() {
    local quiet="${1:-0}"
    if [ ! -f "${SYSTEMD_CLAIM_MARKER}" ]; then
        return 0
    fi
    systemd_mux_available || {
        rm -f "${SYSTEMD_CLAIM_MARKER}"
        return 0
    }

    [ "${quiet}" = "1" ] || log_info "恢复 systemd ${SYSTEMD_UNIT} ..."
    local pids old_socket
    old_socket="${SOCKET}"
    SOCKET="${SYSTEMD_SOCKET}"
    pids=$(cri_multiplex_pids 2>/dev/null || true)
    if [ -n "${pids}" ]; then
        [ "${quiet}" = "1" ] || log_info "停止验证临时进程: ${pids}"
        kill ${pids} 2>/dev/null || true
        sleep 1
        pids=$(cri_multiplex_pids 2>/dev/null || true)
        [ -n "${pids}" ] && kill -9 ${pids} 2>/dev/null || true
    fi
    rm -f "${SOCKET}" 2>/dev/null || true

    SOCKET="${old_socket}"
    if systemctl start "${SYSTEMD_UNIT}"; then
        local deadline=$(( $(date +%s) + 30 ))
        while [ "$(date +%s)" -lt "${deadline}" ]; do
            if systemd_mux_active && [ -S "${SYSTEMD_SOCKET}" ]; then
                [ "${quiet}" = "1" ] || log_pass "systemd ${SYSTEMD_UNIT} 已恢复"
                rm -f "${SYSTEMD_CLAIM_MARKER}"
                return 0
            fi
            sleep 1
        done
        [ "${quiet}" = "1" ] || log_fail "${SYSTEMD_UNIT} 已启动但 socket 未就绪: ${SYSTEMD_SOCKET}"
        return 1
    fi

    [ "${quiet}" = "1" ] || log_fail "启动 ${SYSTEMD_UNIT} 失败"
    return 1
}
