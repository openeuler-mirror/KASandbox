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

log_section "01 — 启动 cri-multiplex"

E2B_CNI_ENABLED="${E2B_CNI_ENABLED:-1}"
E2B_FORCE_RESTART="${E2B_FORCE_RESTART:-0}"
STATE_DIR="${STATE_DIR:-/var/lib/cri-multiplex/state}"
ANDROID_ENABLED="${ANDROID_ENABLED:-0}"
ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR:-/home/fjq/cf17}"
ANDROID_NODE_IP="${ANDROID_NODE_IP:-}"
ANDROID_ADB_PORT_START="${ANDROID_ADB_PORT_START:-6520}"
ANDROID_BASE_INSTANCE_NUM_START="${ANDROID_BASE_INSTANCE_NUM_START:-1}"
ANDROID_LAUNCH_TIMEOUT="${ANDROID_LAUNCH_TIMEOUT:-30s}"
ANDROID_STATE_DIR="${ANDROID_STATE_DIR:-/var/lib/cri-multiplex/android}"
ANDROID_CVD_GROUP="${ANDROID_CVD_GROUP:-cvdnetwork}"
ANDROID_CNI_ENABLED="${ANDROID_CNI_ENABLED:-1}"
ANDROID_CNI_CONF_DIR="${ANDROID_CNI_CONF_DIR:-/etc/cni/net.d}"
ANDROID_CNI_BIN_DIR="${ANDROID_CNI_BIN_DIR:-/opt/cni/bin}"
ANDROID_CNI_IFNAME="${ANDROID_CNI_IFNAME:-eth0}"
ANDROID_CNI_NETNS_DIR="${ANDROID_CNI_NETNS_DIR:-/var/run/netns}"
ANDROID_CNI_NETNS_PREFIX="${ANDROID_CNI_NETNS_PREFIX:-android-}"
if [ "${E2B_CNI_ENABLED}" != "0" ] && [ "${E2B_CNI_ENABLED}" != "1" ]; then
    log_fail "E2B_CNI_ENABLED 必须是 0 或 1，当前值: ${E2B_CNI_ENABLED}"
    exit 1
fi
if [ "${E2B_FORCE_RESTART}" != "0" ] && [ "${E2B_FORCE_RESTART}" != "1" ]; then
	log_fail "E2B_FORCE_RESTART 必须是 0 或 1，当前值: ${E2B_FORCE_RESTART}"
	exit 1
fi
if [ "${ANDROID_ENABLED}" != "0" ] && [ "${ANDROID_ENABLED}" != "1" ]; then
    log_fail "ANDROID_ENABLED 必须是 0 或 1，当前值: ${ANDROID_ENABLED}"
    exit 1
fi
if [ "${ANDROID_CNI_ENABLED}" != "0" ] && [ "${ANDROID_CNI_ENABLED}" != "1" ]; then
    log_fail "ANDROID_CNI_ENABLED 必须是 0 或 1，当前值: ${ANDROID_CNI_ENABLED}"
    exit 1
fi
MODE_DESC="非 CNI"
if [ "${E2B_CNI_ENABLED}" = "1" ]; then
	MODE_DESC="CNI"
fi
if [ "${ANDROID_ENABLED}" = "1" ]; then
	MODE_DESC="${MODE_DESC}+Android"
fi

#==================== 1. 检查是否已在运行（含连通性验证） ====================#
log_step "检查 cri-multiplex 运行状态（目标模式: ${MODE_DESC}）"
if cri_multiplex_ready && [ "${E2B_FORCE_RESTART}" != "1" ]; then
    # 验证 socket 实际可连通（不只是文件存在）
    current_cni=0
    if cri_multiplex_cni_enabled; then
        current_cni=1
    fi
    if [ "${current_cni}" = "${E2B_CNI_ENABLED}" ]; then
        current_android=0
        if cri_multiplex_cmdline | grep -q -- "-android-enabled"; then
            current_android=1
        fi
        if [ "${current_android}" = "${ANDROID_ENABLED}" ]; then
            log_pass "cri-multiplex 已在运行且模式匹配，socket: ${SOCKET}, mode=${MODE_DESC}"
            exit 0
        fi
        log_info "cri-multiplex 已运行但 Android 模式不匹配，准备重启为 ${MODE_DESC} 模式"
    else
        log_info "cri-multiplex 已运行但模式不匹配，准备重启为 ${MODE_DESC} 模式"
    fi
elif [ "${E2B_FORCE_RESTART}" = "1" ]; then
    log_info "E2B_FORCE_RESTART=1，准备强制重启 cri-multiplex"
fi

old_pids=$(cri_multiplex_pids)
if [ -n "${old_pids}" ]; then
    log_info "停止旧 cri-multiplex 进程: ${old_pids}"
    kill ${old_pids} 2>/dev/null || true
    sleep 1
    old_pids=$(cri_multiplex_pids)
    if [ -n "${old_pids}" ]; then
        kill -9 ${old_pids} 2>/dev/null || true
    fi
fi
rm -f "${SOCKET}"

#==================== 2. 构建 ====================#
log_step "构建 cri-multiplex"
if [ ! -f "${MULTIPLEX_DIR}/cri-multiplex" ]; then
    log_info "执行 go build..."
    (cd "${MULTIPLEX_DIR}" && go build ./cmd/cri-multiplex) || {
        log_fail "构建 cri-multiplex 失败"
        exit 1
    }
fi
log_pass "cri-multiplex 二进制已就绪"

#==================== 3. 启动 ====================#
log_step "启动 cri-multiplex（${MODE_DESC} 模式）"
rm -f "${SOCKET}"

cd "${MULTIPLEX_DIR}"
args=(
    -socket "${SOCKET}"
    -containerd-socket "${CONTAINERD_SOCKET}"
    -e2b-backend grpc
    -orchestrator-address "${ORCHESTRATOR_ADDRESS}"
    -orchestrator-proxy-address "${ORCHESTRATOR_PROXY_ADDRESS}"
    -state-dir "${STATE_DIR}"
)
if [ "${E2B_CNI_ENABLED}" = "1" ]; then
    args+=(
        -e2b-cni-enabled
        -cni-conf-dir "${CNI_CONF_DIR}"
        -cni-bin-dir "${CNI_BIN_DIR}"
        -cni-ifname "${CNI_IFNAME}"
        -cni-netns-dir "${CNI_NETNS_DIR}"
    )
fi
if [ "${ANDROID_ENABLED}" = "1" ]; then
    args+=(
        -android-enabled
        -android-artifacts-dir "${ANDROID_ARTIFACTS_DIR}"
        -android-adb-port-start "${ANDROID_ADB_PORT_START}"
        -android-base-instance-num-start "${ANDROID_BASE_INSTANCE_NUM_START}"
        -android-launch-timeout "${ANDROID_LAUNCH_TIMEOUT}"
        -android-state-dir "${ANDROID_STATE_DIR}"
        -android-cvd-group "${ANDROID_CVD_GROUP}"
    )
    if [ "${ANDROID_CNI_ENABLED}" = "1" ]; then
        args+=(
            -android-cni-enabled
            -android-cni-conf-dir "${ANDROID_CNI_CONF_DIR}"
            -android-cni-bin-dir "${ANDROID_CNI_BIN_DIR}"
            -android-cni-ifname "${ANDROID_CNI_IFNAME}"
            -android-cni-netns-dir "${ANDROID_CNI_NETNS_DIR}"
            -android-cni-netns-prefix "${ANDROID_CNI_NETNS_PREFIX}"
        )
    fi
    if [ -n "${ANDROID_NODE_IP}" ]; then
        args+=(-android-node-ip "${ANDROID_NODE_IP}")
    fi
fi

# 使用 setsid 完全脱离会话，防止脚本退出时进程被杀
setsid ./cri-multiplex "${args[@]}" > /tmp/cri-multiplex.log 2>&1 < /dev/null &

# 等待 socket 就绪
retries=10
while [ $retries -gt 0 ]; do
    if cri_multiplex_ready; then
        log_pass "cri-multiplex 已启动，socket: ${SOCKET}, mode=${MODE_DESC}"
        log_info "日志: /tmp/cri-multiplex.log"
        exit 0
    fi
    sleep 1
    retries=$((retries-1))
done

log_fail "cri-multiplex 启动超时"
log_info "日志内容:"
cat /tmp/cri-multiplex.log 2>/dev/null || true
exit 1
