#!/bin/bash
###############################################################################
# 01_start_multiplex.sh — 启动 cri-multiplex
#
# 职责：
#   1. 构建 cri-multiplex（如需要）
#   2. 启动 cri-multiplex（如未运行）
#   3. 验证 socket 就绪
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

# 恢复 systemd 管理的生产实例（手动执行，例如 ./01_start_multiplex.sh --restore）。
if [ "${1:-}" = "--restore" ] || [ "${E2B_VERIFY_RESTORE:-0}" = "1" ]; then
    restore_systemd_multiplex
    exit $?
fi

log_section "01 — 启动 cri-multiplex"

CNI_ENABLED="${CNI_ENABLED:-1}"
E2B_FORCE_RESTART="${E2B_FORCE_RESTART:-0}"
E2B_FORCE_BUILD="${E2B_FORCE_BUILD:-0}"
E2B_SKIP_BUILD="${E2B_SKIP_BUILD:-0}"
HIDE_SANDBOX_LABEL="${HIDE_SANDBOX_LABEL:-}"
STATE_DIR="${STATE_DIR:-$(default_state_dir)}"
NODE_IP="${NODE_IP:-}"
ANDROID_ENABLED="${ANDROID_ENABLED:-0}"
ANDROID_NODE_IP="${ANDROID_NODE_IP:-}"
ANDROID_ADB_PORT_START="${ANDROID_ADB_PORT_START:-6520}"
ANDROID_BASE_INSTANCE_NUM_START="${ANDROID_BASE_INSTANCE_NUM_START:-1}"
ANDROID_LAUNCH_TIMEOUT="${ANDROID_LAUNCH_TIMEOUT:-30s}"
ANDROID_STATE_DIR="${ANDROID_STATE_DIR:-/var/lib/cri-multiplex/android}"
ANDROID_CNI_NETNS_PREFIX="${ANDROID_CNI_NETNS_PREFIX:-android-}"
ORPHAN_RECONCILE_ENABLED="${ORPHAN_RECONCILE_ENABLED:-1}"
ORPHAN_RECONCILE_INTERVAL="${ORPHAN_RECONCILE_INTERVAL:-60s}"
ORPHAN_GRACE_PERIOD="${ORPHAN_GRACE_PERIOD:-120s}"
CLEANUP_MAX_RETRIES="${CLEANUP_MAX_RETRIES:-10}"
CNI_POOL_SIZE="${CNI_POOL_SIZE:-0}"
CNI_POOL_ENABLED="${CNI_POOL_ENABLED:-0}"
E2B_NON_VALIDATION_STARTUP="${E2B_NON_VALIDATION_STARTUP:-0}"

# SOCKET 命中 systemd 管理的 socket 时采用“接管”模式：停止 systemd 实例，
# 用当前仓编译的二进制在同一个 socket 上启动，kubelet 配置保持不动。
ADOPT_SYSTEMD=0
if [ "${SOCKET}" = "${SYSTEMD_SOCKET}" ]; then
    ADOPT_SYSTEMD=1
fi

startup_pass() {
    if [ "${E2B_NON_VALIDATION_STARTUP}" = "1" ]; then
        log_info "$*"
    else
        log_pass "$*"
    fi
}

startup_fail() {
    if [ "${E2B_NON_VALIDATION_STARTUP}" = "1" ]; then
        log_info "$*"
    else
        log_fail "$*"
    fi
}

# 本脚本接管 systemd 实例后，如果启动失败就把 systemd 拉回来，
# 避免节点上没有可用的 CRI 实现。
CLAIMED_BY_THIS_RUN=0
restore_on_failure() {
    local rc=$?
    set +e
    if [ "${rc}" -ne 0 ] && [ "${CLAIMED_BY_THIS_RUN}" = "1" ]; then
        log_info "cri-multiplex 启动失败，恢复 systemd ${SYSTEMD_UNIT}"
        restore_systemd_multiplex 1 || true
    fi
    exit "${rc}"
}
trap restore_on_failure EXIT

# 从 systemd unit 的 ExecStart 里取某个 flag 的值（例如 -hide-sandbox-label）。
# 取不到时返回空串，调用方保留自己的默认值。
unit_flag_value() {
    local flag="$1"
    [ "${ADOPT_SYSTEMD}" = "1" ] || return 0
    systemctl show -p ExecStart --value "${SYSTEMD_UNIT}" 2>/dev/null \
        | grep -oP -- "${flag}\s+\K[^; ]+" | head -1 || true
}

# 构建/定位 cri-multiplex 二进制。adopt 模式会在停机之前先调用一次：
# 构建失败时旧实例还在，节点不会失去 CRI。
ensure_multiplex_binary() {
    if [ "${E2B_SKIP_BUILD}" = "1" ]; then
        log_step "检查 cri-multiplex 二进制"
        if [ ! -x "${MULTIPLEX_BIN}" ]; then
            if [ -x /opt/e2b-infra/bin/cri-multiplex ]; then
                MULTIPLEX_BIN=/opt/e2b-infra/bin/cri-multiplex
                log_info "使用已部署二进制（不是当前仓构建）: ${MULTIPLEX_BIN}"
            else
                startup_fail "E2B_SKIP_BUILD=1，但可执行文件不存在: ${MULTIPLEX_BIN}"
                return 1
            fi
        fi
        log_info "E2B_SKIP_BUILD=1，跳过构建"
        return 0
    fi

    log_step "构建 cri-multiplex"
    local need_build=0
    if [ ! -x "${MULTIPLEX_BIN}" ] || [ "${E2B_FORCE_BUILD}" = "1" ]; then
        need_build=1
    elif find "${MULTIPLEX_DIR}/cmd" "${MULTIPLEX_DIR}/pkg" -name '*.go' -newer "${MULTIPLEX_BIN}" -print -quit 2>/dev/null | grep -q .; then
        need_build=1
        log_info "检测到源码比 cri-multiplex 二进制更新，重新构建"
    fi
    if [ "${need_build}" = "1" ]; then
        log_info "执行 go build -> ${MULTIPLEX_BIN} ..."
        (cd "${MULTIPLEX_DIR}" && \
            GOWORK=off \
            GOCACHE="${GOCACHE:-/tmp/go-build-cache}" \
            GOMODCACHE="${GOMODCACHE:-/tmp/go-mod-cache}" \
            go build -o "${MULTIPLEX_BIN}" ./cmd/cri-multiplex) || {
            startup_fail "构建 cri-multiplex 失败"
            return 1
        }
    fi
    log_info "cri-multiplex 二进制已就绪: ${MULTIPLEX_BIN}"
}

if [ "${CNI_ENABLED}" != "0" ] && [ "${CNI_ENABLED}" != "1" ]; then
    startup_fail "CNI_ENABLED 必须是 0 或 1，当前值: ${CNI_ENABLED}"
    exit 1
fi
if [ "${E2B_FORCE_RESTART}" != "0" ] && [ "${E2B_FORCE_RESTART}" != "1" ]; then
	startup_fail "E2B_FORCE_RESTART 必须是 0 或 1，当前值: ${E2B_FORCE_RESTART}"
	exit 1
fi
if [ "${ANDROID_ENABLED}" != "0" ] && [ "${ANDROID_ENABLED}" != "1" ]; then
    startup_fail "ANDROID_ENABLED 必须是 0 或 1，当前值: ${ANDROID_ENABLED}"
    exit 1
fi
MODE_DESC="非 CNI"
if [ "${CNI_ENABLED}" = "1" ]; then
	MODE_DESC="CNI"
fi
if [ "${ANDROID_ENABLED}" = "1" ]; then
    if ! android_available; then
        startup_fail "ANDROID_ENABLED=1 但未找到可执行的 ${ANDROID_ARTIFACTS_DIR}/bin/launch_cvd"
        exit 1
    fi
    MODE_DESC="${MODE_DESC}+Android"
fi

# adopt 模式：先用当前仓构建/确认二进制，再停 systemd 实例。
# 顺序反过来的话，构建失败会让节点上没有可用的 CRI 实现。
if [ "${ADOPT_SYSTEMD}" = "1" ]; then
    ensure_multiplex_binary || exit 1
fi

#==================== 1. 检查是否已在运行（含连通性验证） ====================#
log_step "检查 cri-multiplex 运行状态（目标模式: ${MODE_DESC}）"
force_restart="${E2B_FORCE_RESTART}"
if [ "${force_restart}" != "1" ] && cri_multiplex_ready; then
    if running_multiplex_is_systemd; then
        # systemd 实例的日志在 journald、启停不受脚本控制，验证期间一律换成受控实例。
        log_info "socket ${SOCKET} 上的实例由 systemd ${SYSTEMD_UNIT} 管理，切换为受控实例"
        force_restart=1
    else
        # 验证 socket 实际可连通（不只是文件存在）
        current_cni=0
        if cri_multiplex_cni_enabled; then
            current_cni=1
        fi
        if [ "${current_cni}" = "${CNI_ENABLED}" ]; then
            current_android=0
            if cri_multiplex_cmdline | grep -q -- "-android-enabled"; then
                current_android=1
            fi
            if [ "${current_android}" = "${ANDROID_ENABLED}" ]; then
                log_info "cri-multiplex 已在运行且模式匹配，socket: ${SOCKET}, mode=${MODE_DESC}"
                exit 0
            fi
            log_info "cri-multiplex 已运行但 Android 模式不匹配，准备重启为 ${MODE_DESC} 模式"
        else
            log_info "cri-multiplex 已运行但模式不匹配，准备重启为 ${MODE_DESC} 模式"
        fi
    fi
elif [ "${force_restart}" = "1" ]; then
    log_info "E2B_FORCE_RESTART=1，准备强制重启 cri-multiplex"
fi

# kubelet 使用 systemd 的 socket。停止 systemd 实例后由脚本在同一个 socket 上启动
# 受控实例，以便统一控制启停/日志/state-dir；kubelet 配置保持不变。
if [ "${force_restart}" = "1" ]; then
    claim_systemd_socket || exit 1
    CLAIMED_BY_THIS_RUN=1
fi

old_pids=$(cri_multiplex_pids)
if [ -n "${old_pids}" ]; then
    stop_cri_multiplex 10 || exit 1
fi
rm -f "${SOCKET}"

#==================== 2. 准备二进制 ====================#
# adopt 模式已在停机前构建完成，这里再确认一次（幂等）。
ensure_multiplex_binary || exit 1

#==================== 3. 启动 ====================#
log_step "启动 cri-multiplex（${MODE_DESC} 模式）"
rm -f "${SOCKET}"
mkdir -p "$(dirname "${SOCKET}")" 2>/dev/null || true

cd "${MULTIPLEX_DIR}"

# adopt 模式：从 systemd unit 继承会影响验证正确性的生产参数。
# -hide-sandbox-label 尤其重要：缺了它，kubelet 的 orphan GC 会把 agent-direct
# sandbox 当成孤儿删掉。
if [ -z "${HIDE_SANDBOX_LABEL}" ]; then
    HIDE_SANDBOX_LABEL="$(unit_flag_value -hide-sandbox-label)"
    if [ -n "${HIDE_SANDBOX_LABEL}" ]; then
        log_info "继承 ${SYSTEMD_UNIT} 的 -hide-sandbox-label ${HIDE_SANDBOX_LABEL}"
    fi
fi
if [ -z "${NODE_IP}" ]; then
    NODE_IP="$(unit_flag_value -node-ip)"
    if [ -n "${NODE_IP}" ]; then
        log_info "继承 ${SYSTEMD_UNIT} 的 -node-ip ${NODE_IP}"
    fi
fi

args=(
    -socket "${SOCKET}"
    -containerd-socket "${CONTAINERD_SOCKET}"
    -orchestrator-address "${ORCHESTRATOR_ADDRESS}"
    -orchestrator-proxy-address "${ORCHESTRATOR_PROXY_ADDRESS}"
    -state-dir "${STATE_DIR}"
	-orphan-reconcile-enabled="${ORPHAN_RECONCILE_ENABLED}"
	-orphan-reconcile-interval "${ORPHAN_RECONCILE_INTERVAL}"
	-orphan-grace-period "${ORPHAN_GRACE_PERIOD}"
	-cleanup-max-retries "${CLEANUP_MAX_RETRIES}"
)
if [ -n "${NODE_IP}" ]; then
    args+=(-node-ip "${NODE_IP}")
fi
if [ -n "${HIDE_SANDBOX_LABEL}" ]; then
    args+=(-hide-sandbox-label "${HIDE_SANDBOX_LABEL}")
fi
if [ "${CNI_ENABLED}" = "1" ]; then
    args+=(
        -cni-enabled
        -cni-conf-dir "${CNI_CONF_DIR}"
        -cni-bin-dir "${CNI_BIN_DIR}"
        -cni-ifname "${CNI_IFNAME}"
        -cni-netns-dir "${CNI_NETNS_DIR}"
    )
    if [ "${CNI_POOL_SIZE}" != "0" ]; then
        args+=(-cni-pool-size "${CNI_POOL_SIZE}")
        if [ "${CNI_POOL_ENABLED}" = "1" ]; then
            args+=(-cni-pool-enabled)
        fi
    fi
fi
if [ "${ANDROID_ENABLED}" = "1" ]; then
    args+=(
        -android-enabled
        -android-artifacts-dir "${ANDROID_ARTIFACTS_DIR}"
        -android-adb-port-start "${ANDROID_ADB_PORT_START}"
        -android-base-instance-num-start "${ANDROID_BASE_INSTANCE_NUM_START}"
        -android-launch-timeout "${ANDROID_LAUNCH_TIMEOUT}"
        -android-state-dir "${ANDROID_STATE_DIR}"
    )
    if [ "${CNI_ENABLED}" = "1" ]; then
        args+=(
            -android-cni-netns-prefix "${ANDROID_CNI_NETNS_PREFIX}"
        )
    fi
    if [ -n "${ANDROID_NODE_IP}" ]; then
        args+=(-android-node-ip "${ANDROID_NODE_IP}")
    fi
fi

# 使用 setsid 完全脱离会话，防止脚本退出时进程被杀
: > "${MULTIPLEX_LOG}"
setsid "${MULTIPLEX_BIN}" "${args[@]}" > "${MULTIPLEX_LOG}" 2>&1 < /dev/null &
start_pid=$!
echo "${start_pid}" > /tmp/cri-multiplex.pid

# 等待 socket 就绪
retries=15
while [ $retries -gt 0 ]; do
    if cri_multiplex_ready; then
        startup_pass "cri-multiplex 已启动，socket: ${SOCKET}, mode=${MODE_DESC}"
        log_info "日志: ${MULTIPLEX_LOG}"
        exit 0
    fi
    if ! kill -0 "${start_pid}" 2>/dev/null; then
        startup_fail "cri-multiplex 进程已退出"
        log_info "日志内容:"
        cat "${MULTIPLEX_LOG}" 2>/dev/null || true
        exit 1
    fi
    sleep 1
    retries=$((retries-1))
done

startup_fail "cri-multiplex 启动超时"
log_info "日志内容:"
cat "${MULTIPLEX_LOG}" 2>/dev/null || true
exit 1
