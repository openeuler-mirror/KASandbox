#!/bin/bash
###############################################################################
# common.sh — 公共函数库（被所有验证脚本 source）
#
# 提供颜色输出、日志、grpcurl 封装、snapshot 错误处理等通用能力
###############################################################################
set -euo pipefail

#==================== 配置 ====================#
SCRIPT_DIR_COMMON="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOCKET="${SOCKET:-/tmp/cri-multiplex.sock}"
CRICTL="crictl --runtime-endpoint unix://${SOCKET}"
PROTO_DIR="${PROTO_DIR:-/tmp/cri-proto}"
PROTO_FILE="${PROTO_DIR}/api.proto"
GRPCURL="grpcurl -plaintext -proto ${PROTO_FILE} -import-path ${PROTO_DIR}"
POD_JSON="/tmp/e2b-pod.json"
TEST_PY="${TEST_PY:-${SCRIPT_DIR_COMMON}/test.py}"
BUILD_PROD_PY="${BUILD_PROD_PY:-${SCRIPT_DIR_COMMON}/build_prod.py}"
BUILD_IMAGE_NAME="${BUILD_IMAGE_NAME:-ubuntu:22.04-custom}"
MULTIPLEX_DIR="${MULTIPLEX_DIR:-$(cd "${SCRIPT_DIR_COMMON}/.." && pwd)}"
CONTAINERD_SOCKET="${CONTAINERD_SOCKET:-/run/containerd/containerd.sock}"
ORCHESTRATOR_ADDRESS="${ORCHESTRATOR_ADDRESS:-localhost:5008}"
ORCHESTRATOR_PROXY_ADDRESS="${ORCHESTRATOR_PROXY_ADDRESS:-localhost:5007}"
CNI_CONF_DIR="${CNI_CONF_DIR:-/etc/cni/net.d}"
CNI_BIN_DIR="${CNI_BIN_DIR:-/opt/cni/bin}"
CNI_IFNAME="${CNI_IFNAME:-eth0}"
CNI_NETNS_DIR="${CNI_NETNS_DIR:-/var/run/netns}"
E2B_API_NS="${E2B_API_NS:-e2b}"

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

#==================== 日志函数（输出到 stderr，避免被 $(...) 捕获） ====================#
log_info()  { echo -e "${CYAN}[INFO]${NC}  $*" >&2; }
log_pass()  { echo -e "${GREEN}[PASS]${NC}  $*" >&2; PASS_COUNT=$((PASS_COUNT+1)); export PASS_COUNT; }
log_fail()  { echo -e "${RED}[FAIL]${NC}  $*" >&2; FAIL_COUNT=$((FAIL_COUNT+1)); export FAIL_COUNT; }
log_skip()  { echo -e "${YELLOW}[SKIP]${NC}  $*" >&2; SKIP_COUNT=$((SKIP_COUNT+1)); export SKIP_COUNT; }
log_step()  { echo -e "\n${CYAN}========================================${NC}" >&2; echo -e "${CYAN} $* ${NC}" >&2; echo -e "${CYAN}========================================${NC}" >&2; }
log_section() { echo -e "\n${CYAN}╔══════════════════════════════════════════════════╗${NC}" >&2; echo -e "${CYAN}║  $*${NC}" >&2; echo -e "${CYAN}╚══════════════════════════════════════════════════╝${NC}\n" >&2; }

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
    local pids deadline

    pids=$(cri_multiplex_pids)
    if [ -z "${pids}" ]; then
        rm -f "${SOCKET}" 2>/dev/null || true
        return 0
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

cri_multiplex_cni_enabled() {
    cri_multiplex_cmdline | grep -q -- "-cni-enabled"
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

    log_info "${desc} ..."
    if ! STATE_DIR="${STATE_DIR:-/var/lib/cri-multiplex/state}" \
        ANDROID_ENABLED=1 \
        CNI_ENABLED=1 \
        ANDROID_ARTIFACTS_DIR="${ANDROID_ARTIFACTS_DIR:-/home/fjq/cf17}" \
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
    if [ "${count_startup}" = "0" ]; then
        require_cri_multiplex_cni_enabled_quiet || return 1
        require_cri_multiplex_android_cni_enabled_quiet || return 1
    else
        require_cri_multiplex_cni_enabled || return 1
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
}

start_non_cni_multiplex() {
    local desc="${1:-启动 cri-multiplex 非 CNI runtime 模式}"

    log_info "${desc} ..."
    if ! STATE_DIR="${STATE_DIR:-/var/lib/cri-multiplex/state}" \
        ANDROID_ENABLED=0 \
        CNI_ENABLED=0 \
        E2B_FORCE_RESTART=1 \
        "${SCRIPT_DIR_COMMON}/01_start_multiplex.sh" >&2; then
        log_fail "${desc} 失败"
        return 1
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
    local base_yaml="${E2B_BASE_POD_YAML:-/tmp/e2b-kubelet-pod.yaml}"

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
    local yaml="${1:-/tmp/e2b-kubelet-pod.yaml}"
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
    local tmp_json="/tmp/e2b-pod-${prefix}-${uid}.json"
    cp "${base_json}" "${tmp_json}"
    sed -i \
        -e "s|\"uid\":\\s*\"[^\"]*\"|\"uid\": \"${uid}\"|" \
        -e "0,/\"name\":\\s*\"[^\"]*\"/s//\"name\": \"test-e2b-${prefix}-${uid}\"/" \
        "${tmp_json}"

    export POD_UID="${uid}"
    export CONTAINER_ID="${uid}-c"
    export POD_JSON="${tmp_json}"
    log_pass "已生成独立 Pod JSON: ${POD_JSON}"
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
# 1. 执行 build_prod.py 重新构建模板
# 2. 执行 test.py 触发新 sandbox 创建
# 3. 从 kubectl logs 获取最新 build_id
# 4. 更新 e2b-pod.json 中的 build-id
# 返回 0 表示已修复可重试，1 表示非 snapshot 错误或修复失败
handle_snapshot_error() {
    local output="$1"

    if ! echo "${output}" | grep -q "snapshot/load"; then
        return 1
    fi

    log_info "检测到 snapshot/load EOF 错误，执行修复流程..."

    # Step 1: 执行 build_prod.py 重新构建模板
    log_info "执行 python3 ${BUILD_PROD_PY} ${BUILD_IMAGE_NAME}..."
    python3 "${BUILD_PROD_PY}" "${BUILD_IMAGE_NAME}" >&2 || true
    sleep 2

    # Step 2: 执行 test.py 触发新 sandbox 创建
    log_info "执行 python3 ${TEST_PY}..."
    python3 "${TEST_PY}" >&2 || true
    sleep 2

    # Step 3: 获取最新日志
    local api_pod
    if ! api_pod=$(find_e2b_api_pod); then
        log_fail "无法找到 e2b namespace 下运行中的 api Pod"
        return 1
    fi

    local log_line
    log_line=$(kubectl logs "${api_pod}" -n e2b 2>/dev/null | grep "base_template_id" | tail -1 || true)

    if [ -z "${log_line}" ]; then
        log_fail "无法从 api Pod ${api_pod} 的 kubectl logs 获取 base_template_id 日志"
        return 1
    fi

    # Step 4: 提取 build_id
    local build_id
    build_id=$(echo "${log_line}" | grep -oP '"build_id":\s*"\K[^"]+' || true)

    if [ -z "${build_id}" ]; then
        log_fail "无法从日志中提取 build_id"
        return 1
    fi

    log_info "获取到最新 build_id: ${build_id}"

    # Step 5: 更新 e2b-pod.json 中的 build-id
    sed -i "s|\"e2b.dev/build-id\":\s*\"[^\"]*\"|\"e2b.dev/build-id\": \"${build_id}\"|" "${POD_JSON}"

    # 验证更新
    local updated_build_id
    updated_build_id=$(grep -oP '"e2b.dev/build-id":\s*"\K[^"]+' "${POD_JSON}" || true)

    if [ "${updated_build_id}" = "${build_id}" ]; then
        log_info "e2b-pod.json build-id 已更新为: ${build_id}"
        return 0
    else
        log_fail "更新 e2b-pod.json 失败"
        return 1
    fi
}

#==================== RunPodSandbox（带 snapshot 重试） ====================#
# 返回 Pod ID 到 stdout
run_pod_sandbox() {
    local output
    local max_retries=5
    local attempt=1
    local snapshot_fixed=0

    sync_e2b_pod_json_from_kubelet_yaml /tmp/e2b-kubelet-pod.yaml "${POD_JSON}" || true

    while [ $attempt -le $max_retries ]; do
        log_info "RunPodSandbox 尝试 ${attempt}/${max_retries}..."
        output=$(${CRICTL} runp -r e2b "${POD_JSON}" 2>&1) || true

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
print_summary() {
    echo -e "\n${CYAN}════════════════════════════════════════════${NC}"
    echo -e "${GREEN}  PASS: ${PASS_COUNT}${NC}"
    echo -e "${RED}  FAIL: ${FAIL_COUNT}${NC}"
    echo -e "${YELLOW}  SKIP: ${SKIP_COUNT}${NC}"
    echo -e "${CYAN}  TOTAL: $((PASS_COUNT+FAIL_COUNT+SKIP_COUNT))${NC}"
    echo -e "${CYAN}════════════════════════════════════════════${NC}\n"
}
