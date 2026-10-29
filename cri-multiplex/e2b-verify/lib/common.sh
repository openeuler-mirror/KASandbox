#!/bin/bash
###############################################################################
# common.sh — 公共函数库（被所有验证脚本 source）
#
# 提供颜色输出、日志、grpcurl 封装、snapshot 错误处理等通用能力
###############################################################################
set -euo pipefail

#==================== 配置 ====================#
SCRIPT_DIR_COMMON="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="${PATH}:/root/go/bin:/usr/local/go/bin"

# 测试节点专用于验证：kubelet 的 runtime endpoint 就是验证实例要用的 socket。
# 脚本只替换 cri-multiplex 二进制（在同一个 socket 上重启），不改 kubelet 配置。
# 注意：socket 固定跟随 kubelet，不允许用环境变量改成别的路径——否则会出现
# 两个 multiplex 实例同时读写 CNI netns / orchestrator 的危险状态。
KUBELET_FLAGS="${E2B_VERIFY_KUBELET_FLAGS:-/var/lib/kubelet/kubeadm-flags.env}"

kubelet_runtime_endpoint() {
    grep -oP -- '--container-runtime-endpoint=\K\S+' "${KUBELET_FLAGS}" 2>/dev/null | head -1 || true
}

default_socket_path() {
    local endpoint
    endpoint="$(kubelet_runtime_endpoint)"
    endpoint="${endpoint#unix://}"
    echo "${endpoint:-/run/cri-multiplex.sock}"
}

# 所有验证脚本用到的临时文件/目录统一挂在 ${TMP_ROOT} 下（两层：
# /tmp/cri-multiplex/e2b-verify），不再直接散落到 /tmp 根目录：
#   - 一眼能区分“验证产物”和宿主机上其它临时文件；
#   - 一轮跑完只清理这一个目录即可；
#   - 旧版本散落的 /tmp/e2b-*.yaml 等不会再和本轮产物混在一起。
# 可用 E2B_VERIFY_TMP_ROOT 覆盖（并行调试时各用各的根目录）。
TMP_ROOT="${E2B_VERIFY_TMP_ROOT:-/tmp/cri-multiplex/e2b-verify}"
mkdir -p "${TMP_ROOT}" 2>/dev/null || true

SOCKET="$(default_socket_path)"
CRICTL="crictl --runtime-endpoint unix://${SOCKET}"
PROTO_DIR="${PROTO_DIR:-${TMP_ROOT}/cri-proto}"
PROTO_FILE="${PROTO_DIR}/api.proto"
GRPCURL="${GRPCURL:-grpcurl -plaintext -proto ${PROTO_FILE} -import-path ${PROTO_DIR}}"
POD_JSON="${POD_JSON:-${TMP_ROOT}/e2b-pod.json}"
TEST_PY="${TEST_PY:-${SCRIPT_DIR_COMMON}/test.py}"
BUILD_PROD_PY="${BUILD_PROD_PY:-${SCRIPT_DIR_COMMON}/build_prod.py}"
BUILD_IMAGE_NAME="${BUILD_IMAGE_NAME:-ubuntu:22.04-custom}"
MULTIPLEX_DIR="${MULTIPLEX_DIR:-$(cd "${SCRIPT_DIR_COMMON}/.." && pwd)}"
MULTIPLEX_BIN="${MULTIPLEX_BIN:-${MULTIPLEX_DIR}/cri-multiplex}"
CONTAINERD_SOCKET="${CONTAINERD_SOCKET:-/run/containerd/containerd.sock}"
ORCHESTRATOR_ADDRESS="${ORCHESTRATOR_ADDRESS:-localhost:5008}"
ORCHESTRATOR_PROXY_ADDRESS="${ORCHESTRATOR_PROXY_ADDRESS:-localhost:5007}"
# e2b CNI 配置目录必须与 kubelet 部署一致；不能使用 /etc/cni/net.d（Calico）。
CNI_CONF_DIR="${CNI_CONF_DIR:-/etc/cni/net-e2b.d}"
CNI_BIN_DIR="${CNI_BIN_DIR:-/opt/cni/bin}"
CNI_IFNAME="${CNI_IFNAME:-eth0}"
CNI_NETNS_DIR="${CNI_NETNS_DIR:-/var/run/netns}"
E2B_API_NS="${E2B_API_NS:-e2b}"
MULTIPLEX_LOG="${MULTIPLEX_LOG:-${TMP_ROOT}/cri-multiplex.log}"
ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR:-$(for d in /home/fjq/cf17 /opt/android-cuttlefish /opt/cf /root/cf17; do if [ -x "$d/bin/launch_cvd" ]; then echo "$d"; break; fi; done; true)}"

# 直连（非 kubelet）sandbox 的隐藏标签，取值必须与 cri-multiplex 的
# -hide-sandbox-label 一致（01_start_multiplex.sh 默认从 systemd unit 继承）。
#
# 直连 sandbox 是用 crictl/grpc 直接 RunPodSandbox 造出来的，没有对应的
# Kubernetes Pod 对象。它对 kubelet 的 ListPodSandbox 可见时，会被 kubelet 的
# 孤儿清理（HandlePodCleanups / runtime GC）当成垃圾回收：sandbox 中途消失、
# mux 的 podRoutes/containerRoutes 被删，之后任何按 containerID 的调用都会
# fallback 到 containerd 并返回 NotFound。给直连 fixture 打上该标签后，kubelet 空 selector 的
# List 就看不到它，从而不会被回收。
DIRECT_SANDBOX_LABEL_KEY="${DIRECT_SANDBOX_LABEL_KEY:-flux-sandbox.io/direct}"
DIRECT_SANDBOX_LABEL_VALUE="${DIRECT_SANDBOX_LABEL_VALUE:-true}"
DIRECT_SANDBOX_LABEL="${DIRECT_SANDBOX_LABEL_KEY}=${DIRECT_SANDBOX_LABEL_VALUE}"

# 给 Pod JSON 加直连 sandbox 标签（幂等）。23/24/25 由 lib/bulk_direct.sh 调用，
# 02/04/05/06 由 prepare_direct_pod_json 自动调用。
add_direct_sandbox_labels() {
    local pod_json="$1"
    command -v python3 >/dev/null 2>&1 || {
        log_fail "python3 不可用，无法写入直连 sandbox 标签: ${pod_json}"
        return 1
    }
    python3 - "${pod_json}" "${DIRECT_SANDBOX_LABEL_KEY}" "${DIRECT_SANDBOX_LABEL_VALUE}" <<'PY'
import json
import sys

path, key, value = sys.argv[1:4]
with open(path, encoding="utf-8") as source:
    pod = json.load(source)
labels = pod.setdefault("labels", {})
labels[key] = value
labels.setdefault("flux-sandbox.io/runtime", "e2b")
with open(path, "w", encoding="utf-8") as target:
    json.dump(pod, target, indent=2, ensure_ascii=True)
    target.write("\n")
PY
}

# 所有用例共用同一个 socket（跟随 kubelet，通常是 /run/cri-multiplex.sock）。
# state-dir 用独立目录，不改动 systemd 实例的持久状态：测试结束后把 systemd
# 实例原样拉起即可，基线状态不会被测试留下的路由/沙箱记录污染。
STATE_DIR="${STATE_DIR:-${TMP_ROOT}/cri-multiplex-state}"
default_state_dir() {
    echo "${TMP_ROOT}/cri-multiplex-state"
}

# 用例私有的 state-dir。
#
# 18/19/20 会 rm -rf 并伪造脏 state（非法 JSON、孤儿记录），这类操作绝不能打到
# 正在运行的实例所用的目录上：否则一边删一边被写，断言会变成竞态，且会把别的
# 用例的 routes/cleanup task 一起抹掉。因此这三个用例各自使用私有目录。
#
# 注意：不能写成 STATE_DIR="${STATE_DIR:-...}" —— 本文件靠前的位置已经给
# STATE_DIR 赋过默认值，`:-` 永远不会生效（历史上的“所有用例统一共用
# /tmp/cri-multiplex-state”就是这个坑造成的）。要用下面的 select_state_dir()。
STATE_DIR_RESTORE="${STATE_DIR_RESTORE:-${TMP_ROOT}/cri-multiplex-state-restore}"
STATE_DIR_MATRIX="${STATE_DIR_MATRIX:-${TMP_ROOT}/cri-multiplex-state-matrix}"
STATE_DIR_CLEANUP="${STATE_DIR_CLEANUP:-${TMP_ROOT}/cri-multiplex-cleanup-state}"

# 把当前用例切到自己的私有 state-dir：仅在使用共享默认值时生效，
# 外部显式传入的 STATE_DIR 仍然优先（便于并行调试）。
select_state_dir() {
    local private_dir="$1"
    if [ -z "${private_dir}" ]; then
        return 0
    fi
    if [ "${STATE_DIR}" = "$(default_state_dir)" ]; then
        STATE_DIR="${private_dir}"
        export STATE_DIR
    fi
}

export SOCKET TMP_ROOT STATE_DIR STATE_DIR_RESTORE STATE_DIR_MATRIX STATE_DIR_CLEANUP \
    MULTIPLEX_LOG KUBELET_FLAGS \
    DIRECT_SANDBOX_LABEL_KEY DIRECT_SANDBOX_LABEL_VALUE DIRECT_SANDBOX_LABEL

# 测试用常量（会被各脚本引用和覆盖）
export POD_UID="${POD_UID:-irlkuj9aask5hmw37uc51}"
export CONTAINER_ID="${CONTAINER_ID:-${POD_UID}-c}"
export IMAGE_E2B="${IMAGE_E2B:-e2b.dev/base:3c9a7001-5c15-4ac1-99aa-0c8219b104aa}"

# 颜色
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
NC='\033[0m'

# 计数器（如果尚未设置则初始化）
PASS_COUNT="${PASS_COUNT:-0}"
FAIL_COUNT="${FAIL_COUNT:-0}"
SKIP_COUNT="${SKIP_COUNT:-0}"
# 环境/数据面不具备该能力（例如 bridge 数据面不支持 NetworkPolicy）。
# 与 SKIP 的区别：SKIP 是「本用例不适用」，UNSUPPORTED 是「本该验证但当前环境做不到」，
# 必须单独统计，绝不能计入 PASS。
UNSUPPORTED_COUNT="${UNSUPPORTED_COUNT:-0}"
# 用例因 UNSUPPORTED 而无法给出结论时的退出码（由 run_all 识别并单独汇总）。
UNSUPPORTED_EXIT_CODE=77

#==================== 日志函数（输出到 stderr，避免被 $(...) 捕获） ====================#
log_info()  { echo -e "${CYAN}[INFO]${NC}  $*" >&2; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $*" >&2; }
log_pass()  { echo -e "${GREEN}[PASS]${NC}  $*" >&2; PASS_COUNT=$((PASS_COUNT+1)); export PASS_COUNT; }
log_fail()  { echo -e "${RED}[FAIL]${NC}  $*" >&2; FAIL_COUNT=$((FAIL_COUNT+1)); export FAIL_COUNT; }
log_skip()  { echo -e "${YELLOW}[SKIP]${NC}  $*" >&2; SKIP_COUNT=$((SKIP_COUNT+1)); export SKIP_COUNT; }
log_unsupported() { echo -e "${YELLOW}[UNSUPPORTED]${NC}  $*" >&2; UNSUPPORTED_COUNT=$((UNSUPPORTED_COUNT+1)); export UNSUPPORTED_COUNT; }
log_step()  { echo -e "\n${CYAN}========================================${NC}" >&2; echo -e "${CYAN} $* ${NC}" >&2; echo -e "${CYAN}========================================${NC}" >&2; }
log_section() { echo -e "\n${CYAN}╔══════════════════════════════════════════════════╗${NC}" >&2; echo -e "${CYAN}║  $*${NC}" >&2; echo -e "${CYAN}╚══════════════════════════════════════════════════╝${NC}\n" >&2; }

# E2B 环境/模板元数据与 systemd 接管辅助函数
# shellcheck disable=SC1091
. "${SCRIPT_DIR_COMMON}/lib/e2b_env.sh"
# shellcheck disable=SC1091
. "${SCRIPT_DIR_COMMON}/lib/systemd_mux.sh"

find_e2b_api_pod() {
    if [ -n "${E2B_API_POD:-}" ]; then
        echo "${E2B_API_POD}"
        return 0
    fi

    local pod=""
    local selector
    for selector in "app=api" "app.kubernetes.io/name=api" "component=api"; do
        pod=$(kubectl get pods -n "${E2B_API_NS}" -l "${selector}" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        if [ -n "${pod}" ]; then
            echo "${pod}"
            return 0
        fi
    done

    pod=$(kubectl get pods -n "${E2B_API_NS}" --field-selector=status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | awk '/^api(-|$)/ { print; exit }' || true)
    if [ -n "${pod}" ]; then
        echo "${pod}"
        return 0
    fi

    return 1
}

cri_multiplex_ready() {
    [ -S "${SOCKET}" ] && crictl --runtime-endpoint "unix://${SOCKET}" info >/dev/null 2>&1
}

cri_multiplex_pids() {
    pgrep -af "[c]ri-multiplex" 2>/dev/null \
        | awk -v socket="${SOCKET}" '
            $0 ~ " -socket " socket && $0 !~ /codex|kubelet|grep/ { print $1 }
        ' || true
}

stop_cri_multiplex() {
    local timeout_seconds="${1:-10}"
    local pids deadline main_pid

    pids=$(cri_multiplex_pids)
    if [ -z "${pids}" ]; then
        rm -f "${SOCKET}" 2>/dev/null || true
        return 0
    fi

    # systemd 管理的实例不能只 kill：Restart=always 会把它拉起来，且
    # 后续 kill -9/stop 语义会与用例预期不一致。
    main_pid=$(systemd_mux_main_pid 2>/dev/null || true)
    if [ -n "${main_pid}" ] && [ "${main_pid}" != "0" ] && echo " ${pids} " | grep -q " ${main_pid} "; then
        log_info "停止 systemd 管理的 cri-multiplex (MainPID=${main_pid})"
        : > "${SYSTEMD_CLAIM_MARKER}"
        systemctl stop "${SYSTEMD_UNIT}" 2>/dev/null || true
        deadline=$(( $(date +%s) + timeout_seconds ))
        while [ "$(date +%s)" -lt "${deadline}" ]; do
            systemd_mux_active || break
            sleep 1
        done
        pids=$(cri_multiplex_pids)
        [ -z "${pids}" ] && {
            rm -f "${SOCKET}" 2>/dev/null || true
            return 0
        }
    fi

    log_info "停止旧 cri-multiplex 进程: ${pids}"
    kill ${pids} 2>/dev/null || true
    deadline=$(( $(date +%s) + timeout_seconds ))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        [ -z "$(cri_multiplex_pids)" ] && break
        sleep 1
    done

    pids=$(cri_multiplex_pids)
    if [ -n "${pids}" ]; then
        log_info "cri-multiplex 未在 ${timeout_seconds}s 内退出，发送 SIGKILL: ${pids}"
        kill -9 ${pids} 2>/dev/null || true
        deadline=$(( $(date +%s) + timeout_seconds ))
        while [ "$(date +%s)" -lt "${deadline}" ]; do
            [ -z "$(cri_multiplex_pids)" ] && break
            sleep 1
        done
    fi

    pids=$(cri_multiplex_pids)
    if [ -n "${pids}" ]; then
        log_fail "旧 cri-multiplex 进程未能停止: ${pids}"
        return 1
    fi

    rm -f "${SOCKET}" 2>/dev/null || true
}

cri_multiplex_cmdline() {
    local pid
    for pid in $(cri_multiplex_pids); do
        tr '\0' ' ' < "/proc/${pid}/cmdline" 2>/dev/null || true
        echo
    done
}

# 正在运行的实例实际使用的 -state-dir（兼容 "-state-dir /p" 与 "-state-dir=/p"）。
# 取不到时输出空串：调用方不应据此判定失败。
cri_multiplex_state_dir() {
    local pid
    for pid in $(cri_multiplex_pids); do
        tr '\0' '\n' < "/proc/${pid}/cmdline" 2>/dev/null | awk '
            /^-state-dir=/ { sub(/^-state-dir=/, ""); print; exit }
            want == 1 { print; exit }
            /^-state-dir$/ { want = 1 }
        '
        return 0
    done
}

cri_multiplex_cni_enabled() {
    cri_multiplex_cmdline | grep -q -- "-cni-enabled"
}

# 若当前运行的实例正在使用 $1 作为 state-dir，先停掉它。
# 用于 rm -rf state-dir 之前：目录被活进程写入时，删除只会成为竞态，
# 而且下一次保存会把刚写进去的内容（例如故意写坏的 JSON）覆盖掉。
stop_multiplex_if_using_state_dir() {
    local dir="$1"
    [ -n "${dir}" ] || return 0
    if [ "$(cri_multiplex_state_dir 2>/dev/null || true)" = "${dir}" ]; then
        log_info "cri-multiplex 正在使用 ${dir}，先停止再清理该目录"
        if ! stop_cri_multiplex 10; then
            log_fail "无法停止正在使用 ${dir} 的 cri-multiplex 实例，不能安全清理该目录"
            return 1
        fi
    fi
    return 0
}

cri_multiplex_android_cni_enabled() {
    cri_multiplex_cni_enabled && cri_multiplex_cmdline | grep -q -- "-android-enabled"
}

require_cri_multiplex_ready() {
    if ! cri_multiplex_ready; then
        log_fail "cri-multiplex 未运行或 socket 不可连通: ${SOCKET}"
        return 1
    fi
    log_pass "cri-multiplex 已运行且 socket 可连通"
}

require_cri_multiplex_ready_quiet() {
    if ! cri_multiplex_ready; then
        log_info "cri-multiplex 未运行或 socket 不可连通: ${SOCKET}"
        return 1
    fi
    log_info "cri-multiplex 已运行且 socket 可连通"
}

require_cri_multiplex_cni_enabled() {
    require_cri_multiplex_ready || return 1
    if ! cri_multiplex_cni_enabled; then
        log_fail "cri-multiplex 未启用 -cni-enabled，无法验证 CNI 链路"
        return 1
    fi
    log_pass "cri-multiplex 已启用 CNI 模式"
}

require_cri_multiplex_cni_enabled_quiet() {
    require_cri_multiplex_ready_quiet || return 1
    if ! cri_multiplex_cni_enabled; then
        log_info "cri-multiplex 未启用 -cni-enabled，无法验证 CNI 链路"
        return 1
    fi
    log_info "cri-multiplex 已启用 CNI 模式"
}

require_cri_multiplex_android_cni_enabled() {
    require_cri_multiplex_ready || return 1
    if ! cri_multiplex_android_cni_enabled; then
        log_fail "cri-multiplex 未启用 -cni-enabled 或 -android-enabled，无法验证 Android CNI 链路"
        return 1
    fi
    log_pass "cri-multiplex 已启用 Android CNI 模式"
}

require_cri_multiplex_android_cni_enabled_quiet() {
    require_cri_multiplex_ready_quiet || return 1
    if ! cri_multiplex_android_cni_enabled; then
        log_info "cri-multiplex 未启用 -cni-enabled 或 -android-enabled，无法验证 Android CNI 链路"
        return 1
    fi
    log_info "cri-multiplex 已启用 Android CNI 模式"
}

start_cni_android_multiplex() {
    local desc="${1:-启动 cri-multiplex CNI+Android runtime 模式}"
    local adb_port_start="${ANDROID_ADB_PORT_START:-${ANDROID_ADB_PORT:-6520}}"
    local base_instance_start="${ANDROID_BASE_INSTANCE_NUM_START:-${ANDROID_BASE_INSTANCE_NUM:-1}}"
    local count_startup="${START_CNI_ANDROID_COUNT:-1}"
    local android_enabled=1
    local force_restart="${E2B_FORCE_RESTART:-0}"

    # Android 用例依赖 cuttlefish 运行时。当前主机没有 artifacts 时不要强行
    # 传 -android-enabled（否则所有后续 CNI/kubelet 用例都会被启动失败拖垮），
    # 而是降级为 CNI-only，Android 用例自身会输出 SKIP。
    if ! android_available; then
        android_enabled=0
        android_gap_notice "未找到 Android cuttlefish artifacts（launch_cvd），本次以 CNI-only 模式启动"
    fi

    # 已经是目标模式就直接复用，不再重启：避免每次重启都让 kubelet 看到 socket
    # 消失（触发 pod 清理、干扰用例），也避免同一环境缺口被反复统计。
    # 重启本身就是被测行为的用例，自行导出 E2B_FORCE_RESTART=1。
    if [ "${force_restart}" != "1" ] && multiplex_mode_matches 1 "${android_enabled}"; then
        log_info "${desc}：cri-multiplex 已处于目标模式（CNI$([ "${android_enabled}" = "1" ] && echo "+Android" || echo "-only")），跳过重启"
    else
        log_info "${desc} ..."
        if ! STATE_DIR="${STATE_DIR:-$(default_state_dir)}" \
            ANDROID_ENABLED="${android_enabled}" \
            CNI_ENABLED=1 \
            ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR:-}" \
            ANDROID_ADB_PORT_START="${adb_port_start}" \
            ANDROID_BASE_INSTANCE_NUM_START="${base_instance_start}" \
            E2B_FORCE_RESTART=1 \
            E2B_NON_VALIDATION_STARTUP="$([ "${count_startup}" = "0" ] && echo 1 || echo 0)" \
            "${SCRIPT_DIR_COMMON}/01_start_multiplex.sh" >&2; then
            if [ "${count_startup}" = "0" ]; then
                log_info "${desc} 失败"
            else
                log_fail "${desc} 失败"
            fi
            return 1
        fi
    fi

    # 无论复用还是新启动，都做同一套模式断言，保证结论不依赖“刚好已经启动”。
    if [ "${count_startup}" = "0" ]; then
        require_cri_multiplex_cni_enabled_quiet || return 1
    else
        require_cri_multiplex_cni_enabled || return 1
    fi
    if [ "${android_enabled}" = "1" ]; then
        if [ "${count_startup}" = "0" ]; then
            require_cri_multiplex_android_cni_enabled_quiet || return 1
        else
            require_cri_multiplex_android_cni_enabled || return 1
        fi
        if ! cri_multiplex_cmdline | grep -q -- "-android-enabled"; then
            if [ "${count_startup}" = "0" ]; then
                log_info "cri-multiplex 未启用 -android-enabled"
            else
                log_fail "cri-multiplex 未启用 -android-enabled"
            fi
            return 1
        fi
        if [ "${count_startup}" = "0" ]; then
            log_info "cri-multiplex 已启用 CNI+Android runtime 模式"
        else
            log_pass "cri-multiplex 已启用 CNI+Android runtime 模式"
        fi
    else
        if [ "${count_startup}" = "0" ]; then
            log_info "cri-multiplex 已启用 CNI-only runtime 模式"
        else
            log_pass "cri-multiplex 已启用 CNI-only runtime 模式"
        fi
    fi
}

# Android 能力由 run_all 统一探测一次，再通过环境变量下发给各用例：
#   E2B_VERIFY_ANDROID_AVAILABLE=1/0  — 探测结果
#   E2B_VERIFY_ANDROID_CHECKED=1      — 表示 run_all 已经统一判断过（缺口已汇报）
android_available() {
    if [ -n "${E2B_VERIFY_ANDROID_AVAILABLE:-}" ]; then
        if [ "${E2B_VERIFY_ANDROID_AVAILABLE}" = "1" ]; then
            return 0
        fi
        return 1
    fi
    [ -n "${ANDROID_ARTIFACTS_DIR:-}" ] && [ -x "${ANDROID_ARTIFACTS_DIR}/bin/launch_cvd" ]
}

# 环境级能力缺口的统一提示：
#   - run_all 已统一探测过（E2B_VERIFY_ANDROID_CHECKED=1）→ 只提示，不重复计 SKIP；
#   - 单独执行脚本 → 仍然记 SKIP，单跑时能直接看到原因。
android_gap_notice() {
    local message="$1"
    if [ "${E2B_VERIFY_ANDROID_CHECKED:-0}" = "1" ]; then
        log_info "${message}"
    else
        log_skip "${message}"
    fi
}

# 判断运行中的实例是否已经处于期望模式（CNI / Android / state-dir 三者都匹配）。
#
# 用途：不必要的重启会让 kubelet 看到 socket 消失，既拖慢全量验证，也会触发
# kubelet 的 pod 清理，干扰正在跑的用例；因此只有模式真的不匹配时才重启。
# 需要“必须重启”的用例（重启本身就是被测行为，例如 18/19 的状态恢复、08 的
# SIGTERM 后重启）自己导出 E2B_FORCE_RESTART=1 即可。
multiplex_mode_matches() {
    local want_cni="$1"
    local want_android="$2"
    local cur_cni=0
    local cur_android=0
    local cur_state=""

    cri_multiplex_ready || return 1
    # systemd 管理的实例日志在 journald、启停不受脚本控制，必须换成受控实例。
    running_multiplex_is_systemd && return 1

    if cri_multiplex_cni_enabled; then
        cur_cni=1
    fi
    if cri_multiplex_cmdline 2>/dev/null | grep -q -- "-android-enabled"; then
        cur_android=1
    fi

    [ "${cur_cni}" = "${want_cni}" ] || return 1
    [ "${cur_android}" = "${want_android}" ] || return 1

    cur_state="$(cri_multiplex_state_dir 2>/dev/null || true)"
    if [ -n "${cur_state}" ] && [ -n "${STATE_DIR:-}" ] && [ "${cur_state}" != "${STATE_DIR}" ]; then
        return 1
    fi
    return 0
}

# kubelet 用例的统一入口：确保 /run socket 由验证脚本直接控制，
# 这样进程日志固定写入 /tmp/cri-multiplex/e2b-verify/cri-multiplex.log，且启停/state-dir 可控。
ensure_controlled_cni_multiplex() {
    local desc="${1:-准备 cri-multiplex CNI 验证实例}"
    # systemd 实例虽然可连通，但日志在 journald、启停不受脚本控制，
    # 而用例断言依赖 /tmp/cri-multiplex/e2b-verify/cri-multiplex.log，必须先换成受控实例。
    if running_multiplex_is_systemd; then
        start_cni_android_multiplex "${desc}" || return 1
        return 0
    fi
    if ! cri_multiplex_ready; then
        start_cni_android_multiplex "${desc}" || return 1
        return 0
    fi
    if ! cri_multiplex_cni_enabled; then
        start_cni_android_multiplex "${desc}" || return 1
        return 0
    fi
    if android_available && ! cri_multiplex_android_cni_enabled; then
        start_cni_android_multiplex "${desc}" || return 1
        return 0
    fi
    # state-dir 也要匹配：上一个用例可能用私有 state-dir 启动了实例（18/19/20），
    # 不重启的话本用例会静默跑在别人的目录上，断言读到的就不是自己期望的 state。
    local running_state_dir
    running_state_dir="$(cri_multiplex_state_dir 2>/dev/null || true)"
    if [ -n "${running_state_dir}" ] && [ "${running_state_dir}" != "${STATE_DIR}" ]; then
        log_info "实例当前 state-dir=${running_state_dir}，与本用例期望的 ${STATE_DIR} 不一致，重启 cri-multiplex"
        start_cni_android_multiplex "${desc}" || return 1
        return 0
    fi
    return 0
}

# Android-only 用例的统一 SKIP 入口：缺少 artifacts 时输出 1 个 SKIP 并以 0 退出，
# 避免把“环境不具备”误报成脚本失败。
require_android_or_skip() {
    if android_available; then
        return 0
    fi
    log_skip "Android artifacts 不可用（未找到 ${ANDROID_ARTIFACTS_DIR:-<unset>}/bin/launch_cvd，E2B_VERIFY_ANDROID_AVAILABLE=${E2B_VERIFY_ANDROID_AVAILABLE:-unset}），跳过本用例"
    print_summary
    exit 0
}


start_non_cni_multiplex() {
    local desc="${1:-启动 cri-multiplex 非 CNI runtime 模式}"
    local force_restart="${E2B_FORCE_RESTART:-0}"

    # 已是非 CNI 模式就复用，不重启（02/04/05/06 连续执行时只需在 01 号切换一次）。
    if [ "${force_restart}" != "1" ] && multiplex_mode_matches 0 0; then
        log_info "${desc}：cri-multiplex 已处于非 CNI 模式，跳过重启"
    else
        log_info "${desc} ..."
        if ! STATE_DIR="${STATE_DIR:-$(default_state_dir)}" \
            ANDROID_ENABLED=0 \
            CNI_ENABLED=0 \
            E2B_FORCE_RESTART=1 \
            "${SCRIPT_DIR_COMMON}/01_start_multiplex.sh" >&2; then
            log_fail "${desc} 失败"
            return 1
        fi
    fi

    require_cri_multiplex_ready_quiet || return 1
    if cri_multiplex_cni_enabled; then
        log_fail "cri-multiplex 仍处于 CNI 模式"
        return 1
    fi
    log_pass "cri-multiplex 已启用非 CNI 模式"
}

require_refresh_script() {
    local refresh_script="$1"
    if [ ! -f "${refresh_script}" ]; then
        log_fail "刷新脚本不存在: ${refresh_script}"
        return 1
    fi
    log_pass "刷新脚本存在: ${refresh_script}"
}

require_refresh_script_quiet() {
    local refresh_script="$1"
    if [ ! -f "${refresh_script}" ]; then
        log_info "刷新脚本不存在: ${refresh_script}"
        return 1
    fi
    log_info "刷新脚本存在: ${refresh_script}"
}

validate_reusable_e2b_yaml() {
    local yaml="$1"

    if [ ! -f "${yaml}" ]; then
        log_fail "可复用 Pod YAML 不存在: ${yaml}"
        return 1
    fi
    if ! grep -q 'e2b.dev/build-id:' "${yaml}" ||
       ! grep -q 'e2b.dev/execution-id:' "${yaml}" ||
       ! grep -q 'e2b.dev/envd-access-token:' "${yaml}"; then
        log_fail "${yaml} 缺少 build-id/execution-id/envd-access-token，不能复用"
        return 1
    fi
}

validate_reusable_e2b_yaml_quiet() {
    local yaml="$1"

    if [ ! -f "${yaml}" ]; then
        log_info "可复用 Pod YAML 不存在: ${yaml}"
        return 1
    fi
    if ! grep -q 'e2b.dev/build-id:' "${yaml}" ||
       ! grep -q 'e2b.dev/execution-id:' "${yaml}" ||
       ! grep -q 'e2b.dev/envd-access-token:' "${yaml}"; then
        log_info "${yaml} 缺少 build-id/execution-id/envd-access-token，不能复用"
        return 1
    fi
}

reset_e2b_yaml_metadata() {
    local pod_name="$1"
    local yaml="$2"
    local tmp="${yaml}.tmp"

    awk -v name="${pod_name}" '
        /^metadata:/ {
            in_metadata=1
            print
            next
        }
        in_metadata == 1 && /^  name:/ {
            print "  name: " name
            next
        }
        in_metadata == 1 && /^  labels:/ {
            skipping_labels=1
            next
        }
        skipping_labels == 1 && /^  [A-Za-z0-9_.-]+:/ {
            skipping_labels=0
        }
        skipping_labels == 1 {
            next
        }
        in_metadata == 1 && /^spec:/ {
            in_metadata=0
        }
        { print }
    ' "${yaml}" > "${tmp}"
    mv "${tmp}" "${yaml}"
}

patch_yaml_metadata_labels() {
    local yaml="$1"
    local labels="${2:-}"
    local tmp="${yaml}.labels-tmp"

    awk -v labels="${labels}" '
        BEGIN {
            n = split(labels, pairs, ",")
            for (i = 1; i <= n; i++) {
                if (pairs[i] == "") {
                    continue
                }
                split(pairs[i], kv, "=")
                if (kv[1] != "" && kv[2] != "") {
                    label_keys[++label_count] = kv[1]
                    label_values[label_count] = kv[2]
                }
            }
        }
        /^  labels:/ {
            skipping_labels=1
            next
        }
        skipping_labels == 1 && /^  [A-Za-z0-9_.-]+:/ {
            skipping_labels=0
        }
        skipping_labels == 1 {
            next
        }
        /^  annotations:/ && inserted != 1 {
            if (label_count > 0) {
                print "  labels:"
                for (i = 1; i <= label_count; i++) {
                    print "    " label_keys[i] ": \"" label_values[i] "\""
                }
            }
            inserted=1
        }
        { print }
    ' "${yaml}" > "${tmp}"
    mv "${tmp}" "${yaml}"
}

patch_yaml_annotations() {
    local yaml="$1"
    local annotations="${2:-}"
    local tmp="${yaml}.annotations-tmp"

    [ -n "${annotations}" ] || return 0

    awk -v annotations="${annotations}" '
        BEGIN {
            n = split(annotations, pairs, ",")
            for (i = 1; i <= n; i++) {
                if (pairs[i] == "") {
                    continue
                }
                pos = index(pairs[i], "=")
                if (pos > 0) {
                    annotation_keys[++annotation_count] = substr(pairs[i], 1, pos - 1)
                    annotation_values[annotation_count] = substr(pairs[i], pos + 1)
                }
            }
        }
        /^  annotations:/ {
            print
            for (i = 1; i <= annotation_count; i++) {
                print "    " annotation_keys[i] ": \"" annotation_values[i] "\""
            }
            inserted=1
            next
        }
        { print }
        END {
            if (inserted != 1 && annotation_count > 0) {
                exit 1
            }
        }
    ' "${yaml}" > "${tmp}"
    mv "${tmp}" "${yaml}"
}

prepare_e2b_pod_yaml() {
    local pod_name="$1"
    local out_yaml="$2"
    local labels="${3:-}"
    local annotations="${4:-}"
    local refresh_script="${REFRESH_SCRIPT:-${SCRIPT_DIR_COMMON}/lib/refresh_build_id.sh}"
    local base_yaml="${E2B_BASE_POD_YAML:-${TMP_ROOT}/e2b-kubelet-pod.yaml}"

    if [ "${E2B_SKIP_BUILD:-0}" = "1" ] && [ "${out_yaml}" != "${base_yaml}" ] && [ -f "${base_yaml}" ]; then
        cp "${base_yaml}" "${out_yaml}"
    fi

    POD_YAML="${out_yaml}" refresh_or_reuse_e2b_yaml "${refresh_script}" "${pod_name}" "${out_yaml}" || return 1
    patch_yaml_metadata_labels "${out_yaml}" "${labels}"
    patch_yaml_annotations "${out_yaml}" "${annotations}"
}

prepare_default_busybox_pod_yaml() {
    local pod_name="$1"
    local out_yaml="$2"
    local labels="${3:-}"
    local client_image="${CLIENT_IMAGE:-docker.io/library/busybox:latest}"
    local attach="${4:-0}"

    {
        printf 'apiVersion: v1\nkind: Pod\nmetadata:\n  name: %s\n' "${pod_name}"
        if [ -n "${labels}" ]; then
            printf '  labels:\n'
            local item key value
            IFS=',' read -r -a label_items <<< "${labels}"
            for item in "${label_items[@]}"; do
                [ -n "${item}" ] || continue
                key="${item%%=*}"
                value="${item#*=}"
                printf '    %s: "%s"\n' "${key}" "${value}"
            done
        fi
        printf 'spec:\n  restartPolicy: Never\n  containers:\n    - name: app\n      image: %s\n      imagePullPolicy: IfNotPresent\n' "${client_image}"
        if [ "${attach}" = "1" ]; then
            printf '      stdin: true\n      tty: true\n      command: ["sh"]\n'
        else
            printf '      command: ["sleep", "3600"]\n'
        fi
    } > "${out_yaml}"
}

refresh_or_reuse_e2b_yaml() {
    local refresh_script="$1"
    local pod_name="$2"
    local yaml="$3"
    local count_yaml="${E2B_YAML_COUNT:-1}"

    if [ "${E2B_SKIP_BUILD:-0}" = "1" ]; then
        log_info "E2B_SKIP_BUILD=1，跳过 build_id 刷新并复用已有 Pod YAML"
        if [ "${count_yaml}" = "0" ]; then
            validate_reusable_e2b_yaml_quiet "${yaml}" || return 1
        else
            validate_reusable_e2b_yaml "${yaml}" || return 1
        fi
        reset_e2b_yaml_metadata "${pod_name}" "${yaml}"
        if [ "${count_yaml}" = "0" ]; then
            log_info "复用已有 Pod YAML: ${yaml}"
        else
            log_pass "复用已有 Pod YAML: ${yaml}"
        fi
        return 0
    fi

    log_info "执行: bash ${refresh_script} ${pod_name}"
    if bash "${refresh_script}" "${pod_name}" >&2; then
        if [ "${count_yaml}" = "0" ]; then
            log_info "build_id 刷新成功"
        else
            log_pass "build_id 刷新成功"
        fi
        return 0
    fi

    log_info "刷新 build_id 失败，尝试复用已有 ${yaml}"
    if [ "${count_yaml}" = "0" ]; then
        validate_reusable_e2b_yaml_quiet "${yaml}" || return 1
    else
        validate_reusable_e2b_yaml "${yaml}" || return 1
    fi
    reset_e2b_yaml_metadata "${pod_name}" "${yaml}"
    if [ "${count_yaml}" = "0" ]; then
        log_info "复用已有 Pod YAML: ${yaml}"
    else
        log_pass "复用已有 Pod YAML: ${yaml}"
    fi
}

wait_tcp_connect() {
    local host="$1"
    local port="$2"
    local timeout_seconds="${3:-120}"
    local desc="${4:-TCP ${host}:${port}}"

    for _ in $(seq 1 "${timeout_seconds}"); do
        if timeout 2 bash -c "cat < /dev/null > /dev/tcp/${host}/${port}" 2>/dev/null; then
            log_pass "${desc} 可连接"
            return 0
        fi
        sleep 1
    done

    log_fail "${desc} 在 ${timeout_seconds}s 内不可连接"
    return 1
}

kubectl_exec_output_with_retry() {
    local pod_name="$1"
    local timeout_seconds="${2:-45}"
    shift 2

    local deadline output
    deadline=$(( $(date +%s) + timeout_seconds ))
    while true; do
        output=$(kubectl exec "${pod_name}" -- "$@" 2>&1) && {
            printf '%s\n' "${output}"
            return 0
        }
        if ! grep -qiE 'dial unix .*cri-multiplex.*no such file|unable to upgrade connection|connection error|transport: Error while dialing|Unavailable' <<< "${output}"; then
            printf '%s\n' "${output}"
            return 1
        fi
        if [ "$(date +%s)" -ge "${deadline}" ]; then
            printf '%s\n' "${output}"
            return 1
        fi
        sleep 1
    done
}

normalize_timeout_seconds() {
    local timeout="${1:-120}"
    timeout="${timeout%s}"
    echo "${timeout}"
}

wait_pod_ready() {
    local pod_name="$1"
    local timeout_seconds
    timeout_seconds=$(normalize_timeout_seconds "${2:-120}")

    if kubectl wait --for=condition=Ready "pod/${pod_name}" --timeout="${timeout_seconds}s" >&2; then
        log_pass "Pod 已 Ready: ${pod_name}"
        return 0
    fi

    log_fail "Pod 未在 ${timeout_seconds}s 内 Ready: ${pod_name}"
    kubectl describe pod "${pod_name}" >&2 || true
    return 1
}

wait_pod_deleted() {
    local pod_name="$1"
    local timeout_seconds
    timeout_seconds=$(normalize_timeout_seconds "${2:-60}")

    for _ in $(seq 1 "${timeout_seconds}"); do
        if ! kubectl get pod "${pod_name}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

delete_pod_and_wait_gone() {
    local pod_name="$1"
    local timeout_seconds
    timeout_seconds=$(normalize_timeout_seconds "${2:-60}")

    kubectl delete pod "${pod_name}" --force --grace-period=0 --ignore-not-found >&2 || true
    wait_pod_deleted "${pod_name}" "${timeout_seconds}"
}

wait_cri_pod_absent() {
    local pod_id="$1"
    local timeout_seconds
    timeout_seconds=$(normalize_timeout_seconds "${2:-30}")

    [ -n "${pod_id}" ] || return 0
    for _ in $(seq 1 "${timeout_seconds}"); do
        if ! ${CRICTL} inspectp "${pod_id}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

wait_cri_container_absent() {
    local container_id="$1"
    local timeout_seconds
    timeout_seconds=$(normalize_timeout_seconds "${2:-30}")

    [ -n "${container_id}" ] || return 0
    for _ in $(seq 1 "${timeout_seconds}"); do
        if ! grpc_call "runtime.v1.RuntimeService/ContainerStatus" "{\"container_id\": \"${container_id}\"}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

wait_cri_pod_ready() {
    local pod_id="$1"
    local timeout_seconds
    timeout_seconds=$(normalize_timeout_seconds "${2:-30}")

    local output
    [ -n "${pod_id}" ] || return 1
    for _ in $(seq 1 "${timeout_seconds}"); do
        output=$(${CRICTL} inspectp "${pod_id}" 2>&1 || true)
        if echo "${output}" | grep -q "SANDBOX_READY"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

pod_uid() {
    kubectl get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null || true
}

pod_container_id() {
    local pod_name="$1"
    local uid="${2:-}"
    local cid
    cid=$(kubectl get pod "${pod_name}" -o jsonpath='{.status.containerStatuses[0].containerID}' 2>/dev/null | sed -E 's#^[^:]+://##' || true)
    if [ -n "${cid}" ]; then
        echo "${cid}"
        return 0
    fi
    if [ -n "${uid}" ]; then
        echo "${uid}-c"
    fi
}

android_pgid_from_state() {
    local sandbox_id="$1"
    local state_file="${2:-${STATE_DIR:-/var/lib/cri-multiplex/state}/state.json}"

    python3 - "${state_file}" "${sandbox_id}" <<'PY' 2>/dev/null || true
import json
import sys

path, sandbox_id = sys.argv[1:3]
try:
    with open(path, encoding="utf-8") as source:
        state = json.load(source)
except (FileNotFoundError, json.JSONDecodeError):
    raise SystemExit(0)

for pod in (state.get("android") or {}).get("pods") or []:
    if pod.get("sandbox_id") == sandbox_id:
        pgid = int(pod.get("launch_pgid") or 0)
        if pgid > 0:
            print(pgid)
        break
PY
}

# 根据 sandbox id 找到 CNI netns 路径。
# cri-multiplex 的 netns 名不是 ID 前缀，而是随机 12 位后缀（如 e2b-13aacc84f296），
# 因此不能按 UID 截断猜测；优先从 multiplex 日志/状态文件读取实际路径。
cni_netns_path_for_sandbox() {
    local sandbox_id="$1"
    local log_file="${MULTIPLEX_LOG:-${TMP_ROOT}/cri-multiplex.log}"
    local state_file="${STATE_DIR:-$(default_state_dir)}/state.json"
    local path=""

    if [ -f "${log_file}" ]; then
        path=$(grep -aoP "CNI ADD: sandbox=${sandbox_id}\\b.*?netns=\\K[^ ]+" "${log_file}" 2>/dev/null | tail -1 || true)
    fi
    if [ -z "${path}" ] && [ -f "${state_file}" ]; then
        path=$(python3 - "${state_file}" "${sandbox_id}" <<'PYSTATE' 2>/dev/null || true
import json, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
sid = sys.argv[2]

def netns_of(record):
    if not isinstance(record, dict):
        return ""
    # 实际落盘字段是 NetNSPath/NetNSName（Go 结构体无 json tag），
    # 兼容历史字段名以防格式演进。
    for key in ("NetNSPath", "netns_path", "netns", "cni_netns"):
        value = record.get(key)
        if isinstance(value, str) and value:
            return value
    name = record.get("NetNSName") or ""
    if isinstance(name, str) and name:
        return "/var/run/netns/" + name
    return ""

for section in ("e2b", "android"):
    for pod in ((state.get(section) or {}).get("pods") or []):
        if sid not in (pod.get("sandbox_id"), pod.get("pod_uid")):
            continue
        found = netns_of(pod.get("cni_record"))
        if found:
            print(found)
            raise SystemExit(0)
PYSTATE
)
    fi
    echo "${path}"
}

wait_android_pod_cleanup() {
    local pod_name="$1"
    local sandbox_id="$2"
    local netns_path="$3"
    local adb_host="$4"
    local adb_port="$5"
    local pgid="${6:-}"
    local timeout_seconds="${7:-120}"

    local deadline pod_gone cri_gone netns_gone port_closed pgid_gone
    deadline=$(( $(date +%s) + timeout_seconds ))
    while true; do
        pod_gone=0
        cri_gone=0
        netns_gone=0
        port_closed=0
        pgid_gone=0

        kubectl get pod "${pod_name}" >/dev/null 2>&1 || pod_gone=1
        ${CRICTL} inspectp "${sandbox_id}" >/dev/null 2>&1 || cri_gone=1
        [ -z "${netns_path}" ] || [ ! -e "${netns_path}" ] && netns_gone=1
        if [ -z "${adb_host}" ] || [ -z "${adb_port}" ] ||
           ! timeout 2 bash -c "cat < /dev/null > /dev/tcp/${adb_host}/${adb_port}" 2>/dev/null; then
            port_closed=1
        fi
        if [ -z "${pgid}" ] || ! kill -0 -- "-${pgid}" 2>/dev/null; then
            pgid_gone=1
        fi

        if [ "${pod_gone}" = "1" ] && [ "${cri_gone}" = "1" ] &&
           [ "${netns_gone}" = "1" ] && [ "${port_closed}" = "1" ] &&
           [ "${pgid_gone}" = "1" ]; then
            log_pass "Android Pod 资源已清理: ${pod_name}"
            return 0
        fi

        if [ "$(date +%s)" -ge "${deadline}" ]; then
            log_fail "Android Pod 资源未在 ${timeout_seconds}s 内清理: pod=${pod_name} sandbox=${sandbox_id} netns=${netns_path} adb=${adb_host}:${adb_port} pgid=${pgid}"
            return 1
        fi
        sleep 1
    done
}

sync_e2b_pod_json_from_kubelet_yaml() {
    local yaml="${1:-${TMP_ROOT}/e2b-kubelet-pod.yaml}"
    local json="${2:-${POD_JSON}}"

    if [ ! -f "${yaml}" ] || [ ! -f "${json}" ]; then
        return 1
    fi

    local image_ref
    image_ref=$(python3 - "${yaml}" "${json}" <<'PY'
import json
import re
import sys

yaml_path, json_path = sys.argv[1:]
annotations = {}
in_annotations = False
annotations_indent = -1

with open(yaml_path, encoding="utf-8") as source:
    for raw_line in source:
        line = raw_line.rstrip("\r\n")
        stripped = line.lstrip()
        indent = len(line) - len(stripped)
        if not in_annotations:
            if stripped == "annotations:":
                in_annotations = True
                annotations_indent = indent
            continue
        if stripped and indent <= annotations_indent:
            break
        if not stripped or stripped.startswith("#"):
            continue
        match = re.match(r"\s*(e2b\.dev/[A-Za-z0-9_.-]+):\s*(.*?)\s*$", line)
        if not match:
            continue
        key, encoded = match.groups()
        if encoded.startswith('"'):
            try:
                value = json.loads(encoded)
            except json.JSONDecodeError as exc:
                raise SystemExit(f"invalid quoted annotation {key}: {exc}")
        elif len(encoded) >= 2 and encoded[0] == encoded[-1] == "'":
            value = encoded[1:-1].replace("''", "'")
        else:
            value = encoded.split(" #", 1)[0].strip()
        annotations[key] = str(value)

required = (
    "e2b.dev/template-id",
    "e2b.dev/build-id",
    "e2b.dev/team-id",
    "e2b.dev/execution-id",
    "e2b.dev/envd-access-token",
)
missing = [key for key in required if not annotations.get(key)]
if missing:
    raise SystemExit("missing E2B annotations: " + ", ".join(missing))

with open(json_path, encoding="utf-8") as source:
    pod = json.load(source)
pod_annotations = pod.setdefault("annotations", {})
# The kubelet fixture owns credentials and runtime parameters, while the base
# CRI fixture may carry scenario-specific annotations such as expose-ports.
pod_annotations.update(annotations)

tmp_path = json_path + ".sync-tmp"
with open(tmp_path, "w", encoding="utf-8") as target:
    json.dump(pod, target, indent=2, ensure_ascii=True)
    target.write("\n")
import os
os.replace(tmp_path, json_path)
print("e2b.dev/%s:%s" % (
    annotations["e2b.dev/template-id"], annotations["e2b.dev/build-id"]
))
PY
    ) || return 1

    [ -n "${image_ref}" ] || return 1
    export IMAGE_E2B="${image_ref}"
    return 0
}

prepare_direct_pod_json() {
    local prefix="${1:-direct}"
    local base_json="${2:-${POD_JSON}}"

    if [ ! -f "${base_json}" ]; then
        log_fail "基础 Pod JSON 不存在: ${base_json}"
        return 1
    fi

    local uid
    uid="e2b${prefix}$(date +%s)$RANDOM"
    local tmp_json="${TMP_ROOT}/e2b-pod-${prefix}-${uid}.json"
    cp "${base_json}" "${tmp_json}"
    sed -i \
        -e "s|\"uid\":\\s*\"[^\"]*\"|\"uid\": \"${uid}\"|" \
        -e "0,/\"name\":\\s*\"[^\"]*\"/s//\"name\": \"test-e2b-${prefix}-${uid}\"/" \
        "${tmp_json}"

    # 打上直连 sandbox 标签：kubelet 的空 selector List 看不到它，孤儿清理（runtime
    # GC / HandlePodCleanups）就不会在用例执行中途把 sandbox 删掉；同时保持
    # “带该 label 的定向查询仍可见”（23 号用例覆盖该行为）。
    if ! add_direct_sandbox_labels "${tmp_json}"; then
        rm -f "${tmp_json}" 2>/dev/null || true
        log_fail "写入直连 sandbox 标签失败: ${tmp_json}"
        return 1
    fi

    export POD_UID="${uid}"
    export CONTAINER_ID="${uid}-c"
    export POD_JSON="${tmp_json}"
    log_pass "已生成独立 Pod JSON（含 ${DIRECT_SANDBOX_LABEL}）: ${POD_JSON}"
}

#==================== grpcurl 统一封装 ====================#
# 用法: grpc_call <service/method> [json_data]
grpc_call() {
    local service_method="$1"
    local data="${2:-}"
    if [ -n "$data" ]; then
        ${GRPCURL} -d "${data}" "unix://${SOCKET}" "${service_method}" 2>&1
    else
        ${GRPCURL} "unix://${SOCKET}" "${service_method}" 2>&1
    fi
}

#==================== Snapshot 错误处理 ====================#
# 当 RunPodSandbox 遇到 "snapshot/load EOF" 错误时：
# 1. 重新从 E2B API 解析当前模板 build-id（首选，快且无副作用）
# 2. 若显式开启 E2B_REBUILD_ON_SNAPSHOT=1，再调用 build_prod.py 重建模板
# 3. 更新 e2b-pod.json 中的模板元数据
# 返回 0 表示已修复可重试，1 表示非 snapshot 错误或修复失败
handle_snapshot_error() {
    local output="$1"

    if ! echo "${output}" | grep -q "snapshot/load"; then
        return 1
    fi

    log_info "检测到 snapshot/load EOF 错误，刷新 E2B 模板元数据..."

    if [ "${E2B_REBUILD_ON_SNAPSHOT:-0}" = "1" ] && [ -f "${BUILD_PROD_PY}" ]; then
        log_info "E2B_REBUILD_ON_SNAPSHOT=1，执行 python3 ${BUILD_PROD_PY} ${BUILD_IMAGE_NAME}..."
        python3 "${BUILD_PROD_PY}" "${BUILD_IMAGE_NAME}" >&2 || true
        sleep 2
    fi

    if ! resolve_e2b_template --force; then
        log_fail "刷新 E2B 模板元数据失败"
        return 1
    fi
    if ! ensure_default_pod_json; then
        log_fail "更新 ${POD_JSON} 失败"
        return 1
    fi

    local updated_build_id
    updated_build_id=$(jq -r '.annotations["e2b.dev/build-id"] // empty' "${POD_JSON}" 2>/dev/null || true)
    log_info "${POD_JSON} build-id 已更新为: ${updated_build_id}"
    [ -n "${updated_build_id}" ]
}

#==================== RunPodSandbox（带 snapshot 重试） ====================#
# 返回 Pod ID 到 stdout
run_pod_sandbox() {
    local output
    local max_retries=5
    local attempt=1
    local snapshot_fixed=0

    ensure_default_pod_json || {
        log_fail "无法准备有效的 E2B Pod JSON: ${POD_JSON}"
        return 1
    }

    # kubelet fixture 如果比直连 JSON 新，说明调用方刚刷新过模板，采用其注解。
    if [ -f ${TMP_ROOT}/e2b-kubelet-pod.yaml ] && [ -f "${POD_JSON}" ] &&
       [ ${TMP_ROOT}/e2b-kubelet-pod.yaml -nt "${POD_JSON}" ]; then
        sync_e2b_pod_json_from_kubelet_yaml ${TMP_ROOT}/e2b-kubelet-pod.yaml "${POD_JSON}" || true
    fi

    while [ $attempt -le $max_retries ]; do
        log_info "RunPodSandbox 尝试 ${attempt}/${max_retries}..."
        # 高并发创建时 CNI ADD 近乎串行（200 并发 p50≈200s），把取消时限放宽到 900s
        output=$(${CRICTL} runp -T 900s -r e2b "${POD_JSON}" 2>&1) || true

        # 检查是否成功（纯 ID 字符串）
        if echo "${output}" | grep -qE "^[a-z0-9-]+$" && ! echo "${output}" | grep -qi "error\|FATA"; then
            echo "${output}" | head -1 | tr -d '[:space:]'
            return 0
        fi

        # snapshot 错误则修复后重试（只修复一次）
        if echo "${output}" | grep -q "snapshot/load"; then
            if [ $snapshot_fixed -eq 0 ]; then
                if handle_snapshot_error "${output}"; then
                    snapshot_fixed=1
                    attempt=$((attempt+1))
                    continue
                fi
            else
                # 已修复过但仍然失败，等待后重试
                log_info "snapshot 已修复过，等待重试..."
                sleep 5
                attempt=$((attempt+1))
                continue
            fi
        fi

        # 其他错误
        log_info "RunPodSandbox 输出: ${output}"
        return 1
    done

    log_fail "RunPodSandbox 重试 ${max_retries} 次后仍失败"
    return 1
}

#==================== 创建并启动 Container ====================#
# 参数: $1 = pod_sandbox_id
# 输出 container_id 到 stdout
create_and_start_container() {
    local pod_id="$1"
    local data
    data=$(cat <<EOF
{"pod_sandbox_id": "${pod_id}", "config": {"metadata": {"name": "sandbox"}, "image": {"image": "${IMAGE_E2B}"}}, "sandbox_config": {"metadata": {"name": "test-e2b-pod", "uid": "${pod_id}"}}}
EOF
)
    local output
    output=$(grpc_call "runtime.v1.RuntimeService/CreateContainer" "${data}") || true

    if ! echo "${output}" | grep -q "containerId"; then
        log_fail "CreateContainer 失败: ${output}"
        return 1
    fi

    local cid
    cid=$(echo "${output}" | grep -oP '"containerId":\s*"\K[^"]+')

    wait_cri_pod_ready "${pod_id}" 20 || true

    local attempt
    for attempt in 1 2 3 4 5; do
        output=$(grpc_call "runtime.v1.RuntimeService/StartContainer" "{\"container_id\": \"${cid}\"}") || true
        if echo "${output}" | grep -q "^{}" || echo "${output}" | grep -q "^$"; then
            echo "${cid}"
            return 0
        fi
        if ! echo "${output}" | grep -qiE "FailedPrecondition|not running|Unavailable|DeadlineExceeded"; then
            log_fail "StartContainer 失败: ${output}"
            return 1
        fi
        log_info "StartContainer 暂未就绪，第 ${attempt}/5 次: ${output}"
        sleep 2
    done

    log_fail "StartContainer 重试后仍失败: ${output}"
    return 1
}

#==================== 清理资源 ====================#
cleanup_container() {
    local cid="${1:-${CONTAINER_ID}}"
    if [ -n "${cid}" ]; then
        grpc_call "runtime.v1.RuntimeService/RemoveContainer" "{\"container_id\": \"${cid}\"}" > /dev/null 2>&1 || true
        wait_cri_container_absent "${cid}" 10 || true
    fi
}

cleanup_pod() {
    local pid="${1:-${POD_UID}}"
    if [ -n "${pid}" ]; then
        grpc_call "runtime.v1.RuntimeService/RemovePodSandbox" "{\"pod_sandbox_id\": \"${pid}\"}" > /dev/null 2>&1 || true
        wait_cri_pod_absent "${pid}" 15 || true
    fi
}

#==================== 输出汇总 ====================#
# 统一退出码约定（输出到 stdout，调用方用 exit "$(summary_exit_code)"）：
#   0  = 全部断言通过
#   1  = 有 FAIL
#   77 = 无 FAIL，但有 UNSUPPORTED（当前环境无法验证该能力）
summary_exit_code() {
    if [ "${FAIL_COUNT}" -gt 0 ]; then
        echo 1
        return 0
    fi
    if [ "${UNSUPPORTED_COUNT}" -gt 0 ]; then
        echo "${UNSUPPORTED_EXIT_CODE}"
        return 0
    fi
    echo 0
}

print_summary() {
    echo -e "\n${CYAN}════════════════════════════════════════════${NC}"
    echo -e "${GREEN}  PASS: ${PASS_COUNT}${NC}"
    echo -e "${RED}  FAIL: ${FAIL_COUNT}${NC}"
    echo -e "${YELLOW}  SKIP: ${SKIP_COUNT}${NC}"
    if [ "${UNSUPPORTED_COUNT}" -gt 0 ]; then
        echo -e "${YELLOW}  UNSUPPORTED: ${UNSUPPORTED_COUNT}${NC}"
    fi
    echo -e "${CYAN}  TOTAL: $((PASS_COUNT+FAIL_COUNT+SKIP_COUNT+UNSUPPORTED_COUNT))${NC}"
    echo -e "${CYAN}════════════════════════════════════════════${NC}\n"
}
