#!/bin/bash
###############################################################################
# 12_bulk_cni_start_100.sh — 批量创建 E2B 沙箱并统计启动耗时
#
# 验证目标：
#   1. 使用类似 11 号用例的 RuntimeClass=e2b Pod YAML 批量创建沙箱。
#   2. 默认创建 100 个沙箱，可通过 COUNT 覆盖。
#   3. 统计 kubectl apply 提交耗时、全部 Pod Ready 耗时、成功/失败数量。
#   4. 统计完成后删除本次创建的全部 Pod，并等待清理。
#
# 用法：
#   bash e2b-verify/12_bulk_cni_start_100.sh
#   COUNT=20 bash e2b-verify/12_bulk_cni_start_100.sh
#   PREFIX=e2b-bulk TIMEOUT_SECONDS=900 POLL_INTERVAL=0.1 REPORT_INTERVAL_MS=1000 bash e2b-verify/12_bulk_cni_start_100.sh
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "12 — 批量创建 E2B 沙箱并统计启动耗时"

#==================== 配置 ====================#
COUNT="${COUNT:-95}"
PREFIX="${PREFIX:-e2b-bulk-cni}"
NAMESPACE="${NAMESPACE:-default}"
POD_YAML="${POD_YAML:-/tmp/e2b-kubelet-pod.yaml}"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"
WORK_DIR="${WORK_DIR:-/tmp/e2b-bulk-cni}"
BULK_YAML="${WORK_DIR}/pods.yaml"
RESULT_CSV="${WORK_DIR}/result.csv"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-30}"
CLEANUP_WAIT_SECONDS="${CLEANUP_WAIT_SECONDS:-30}"
POLL_INTERVAL="${POLL_INTERVAL:-0.1}"
REPORT_INTERVAL_MS="${REPORT_INTERVAL_MS:-1000}"

START_TS=0
APPLY_DONE_TS=0
READY_DONE_TS=0
START_MS=0
APPLY_DONE_MS=0
READY_DONE_MS=0

cleanup() {
    if [ -f "${BULK_YAML}" ]; then
        log_info "清理本次批量 Pod: ${PREFIX}-0..$((COUNT-1))"
        kubectl delete -n "${NAMESPACE}" -f "${BULK_YAML}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

require_positive_int() {
    local name="$1"
    local value="$2"
    if ! [[ "${value}" =~ ^[1-9][0-9]*$ ]]; then
        log_fail "${name} 必须是正整数，当前值: ${value}"
        exit 1
    fi
}

require_non_negative_number() {
    local name="$1"
    local value="$2"
    if ! [[ "${value}" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
        log_fail "${name} 必须是非负数字，当前值: ${value}"
        exit 1
    fi
}

now_ms() {
    date +%s%3N
}

fmt_ms() {
    local ms="$1"
    printf '%d.%03ds' "$((ms / 1000))" "$((ms % 1000))"
}

jsonpath_ready() {
    local pod="$1"
    kubectl get pod "${pod}" -n "${NAMESPACE}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true
}

jsonpath_phase() {
    local pod="$1"
    kubectl get pod "${pod}" -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || true
}

#==================== 前置检查 ====================#
log_step "1.1 前置检查"

require_positive_int COUNT "${COUNT}"
require_positive_int TIMEOUT_SECONDS "${TIMEOUT_SECONDS}"
require_positive_int CLEANUP_WAIT_SECONDS "${CLEANUP_WAIT_SECONDS}"
require_non_negative_number POLL_INTERVAL "${POLL_INTERVAL}"
require_positive_int REPORT_INTERVAL_MS "${REPORT_INTERVAL_MS}"
log_pass "参数合法: COUNT=${COUNT}, TIMEOUT_SECONDS=${TIMEOUT_SECONDS}, POLL_INTERVAL=${POLL_INTERVAL}, REPORT_INTERVAL_MS=${REPORT_INTERVAL_MS}"

require_cri_multiplex_cni_enabled || exit 1

if ! kubectl get runtimeclass e2b > /dev/null 2>&1; then
    log_fail "RuntimeClass e2b 不存在"
    exit 1
fi
log_pass "RuntimeClass e2b 存在"

mkdir -p "${WORK_DIR}"

#==================== 准备基础 Pod YAML ====================#
log_step "2.1 准备基础 Pod YAML"

if ! refresh_or_reuse_e2b_yaml "${REFRESH_SCRIPT}" "${PREFIX}-template" "${POD_YAML}"; then
    exit 1
fi

BUILD_ID=$(grep -oP 'e2b\.dev/build-id:\s*"\K[^"]+' "${POD_YAML}" | head -1 || true)
IMAGE_REF=$(grep -oP 'image:\s*\K\S+' "${POD_YAML}" | head -1 || true)
if [ -z "${BUILD_ID}" ]; then
    log_fail "无法从 ${POD_YAML} 提取 build_id"
    exit 1
fi
log_pass "基础 YAML 可用: build_id=${BUILD_ID}, image=${IMAGE_REF}"

#==================== 清理旧批量 Pod ====================#
log_step "2.2 清理同前缀旧 Pod"

OLD_PODS=$(kubectl get pods -n "${NAMESPACE}" --no-headers 2>/dev/null | awk -v p="${PREFIX}-" '$1 ~ "^"p {print $1}' || true)
if [ -n "${OLD_PODS}" ]; then
    echo "${OLD_PODS}" | xargs -r kubectl delete pod -n "${NAMESPACE}" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
    sleep 5
    log_pass "旧 Pod 已提交删除"
else
    log_skip "无同前缀旧 Pod"
fi

#==================== 生成批量 YAML ====================#
log_step "3.1 生成 ${COUNT} 个 Pod YAML"

: > "${BULK_YAML}"
for i in $(seq 0 $((COUNT - 1))); do
    pod_name="${PREFIX}-${i}"
    if command -v uuidgen >/dev/null 2>&1; then
        execution_id="$(uuidgen | tr '[:upper:]' '[:lower:]')"
    else
        execution_id="$(date +%s)-${i}-${RANDOM}"
    fi
    awk -v pod_name="${pod_name}" -v execution_id="${execution_id}" '
        BEGIN { replaced = 0 }
        /^  name: / && replaced == 0 {
            print "  name: " pod_name
            replaced = 1
            next
        }
        /^[[:space:]]+e2b.dev\/execution-id:/ {
            print "    e2b.dev/execution-id: \"" execution_id "\""
            next
        }
        { print }
    ' "${POD_YAML}" >> "${BULK_YAML}"
    if [ "${i}" -lt $((COUNT - 1)) ]; then
        printf '\n---\n' >> "${BULK_YAML}"
    fi
done
log_pass "批量 YAML 已生成: ${BULK_YAML}"

#==================== 批量创建并等待 Ready ====================#
log_step "4.1 批量提交 Pod"

START_TS=$(date +%s)
START_MS=$(now_ms)
kubectl apply -n "${NAMESPACE}" -f "${BULK_YAML}" >&2
APPLY_DONE_TS=$(date +%s)
APPLY_DONE_MS=$(now_ms)
log_pass "kubectl apply 完成，提交耗时 $(fmt_ms "$((APPLY_DONE_MS - START_MS))")"

log_step "4.2 等待全部 Pod Ready"

deadline=$((START_TS + TIMEOUT_SECONDS))
deadline_ms=$((START_MS + TIMEOUT_SECONDS * 1000))
ready_count=0
last_report_ms=0

while true; do
    ready_count=0
    running_count=0
    failed_count=0
    pending_count=0

    for i in $(seq 0 $((COUNT - 1))); do
        pod_name="${PREFIX}-${i}"
        ready=$(jsonpath_ready "${pod_name}")
        phase=$(jsonpath_phase "${pod_name}")
        if [ "${ready}" = "True" ]; then
            ready_count=$((ready_count + 1))
        fi
        case "${phase}" in
            Running) running_count=$((running_count + 1)) ;;
            Failed|Succeeded) failed_count=$((failed_count + 1)) ;;
            *) pending_count=$((pending_count + 1)) ;;
        esac
    done

    now=$(date +%s)
    current_ms=$(now_ms)
    if [ $((current_ms - last_report_ms)) -ge "${REPORT_INTERVAL_MS}" ]; then
        log_info "进度: Ready=${ready_count}/${COUNT}, Running=${running_count}, Pending/Creating=${pending_count}, Terminal=${failed_count}, elapsed=$(fmt_ms "$((current_ms - START_MS))")"
        last_report_ms="${current_ms}"
    fi

    if [ "${ready_count}" -eq "${COUNT}" ]; then
        READY_DONE_TS="${now}"
        READY_DONE_MS="${current_ms}"
        log_pass "全部 Pod Ready，总耗时 $(fmt_ms "$((READY_DONE_MS - START_MS))")，等待 Ready 耗时 $(fmt_ms "$((READY_DONE_MS - APPLY_DONE_MS))")"
        break
    fi

    if [ "${current_ms}" -ge "${deadline_ms}" ]; then
        READY_DONE_TS="${now}"
        READY_DONE_MS="${current_ms}"
        log_fail "等待超时: Ready=${ready_count}/${COUNT}, 总耗时 $(fmt_ms "$((READY_DONE_MS - START_MS))")"
        break
    fi

    sleep "${POLL_INTERVAL}"
done

#==================== 输出明细 ====================#
log_step "5.1 输出启动统计明细"

{
    echo "pod,phase,ready,pod_ip,uid"
    for i in $(seq 0 $((COUNT - 1))); do
        pod_name="${PREFIX}-${i}"
        phase=$(jsonpath_phase "${pod_name}")
        ready=$(jsonpath_ready "${pod_name}")
        pod_ip=$(kubectl get pod "${pod_name}" -n "${NAMESPACE}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
        uid=$(kubectl get pod "${pod_name}" -n "${NAMESPACE}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
        echo "${pod_name},${phase},${ready},${pod_ip},${uid}"
    done
} > "${RESULT_CSV}"

log_info "结果明细: ${RESULT_CSV}"
kubectl get pods -n "${NAMESPACE}" -o wide | grep "${PREFIX}-" >&2 || true

#==================== 删除并等待清理 ====================#
log_step "6.1 删除本次创建的全部 Pod"

cleanup
trap - EXIT

cleanup_deadline=$(( $(date +%s) + CLEANUP_WAIT_SECONDS ))
while true; do
    remaining=$(kubectl get pods -n "${NAMESPACE}" --no-headers 2>/dev/null | awk -v p="${PREFIX}-" '$1 ~ "^"p {count++} END {print count+0}')
    if [ "${remaining}" -eq 0 ]; then
        log_pass "全部批量 Pod 已从 API 中删除"
        break
    fi
    now=$(date +%s)
    if [ "${now}" -ge "${cleanup_deadline}" ]; then
        log_fail "等待删除超时，剩余 Pod 数: ${remaining}"
        break
    fi
    log_info "等待删除中，剩余 Pod 数: ${remaining}"
    sleep 5
done

#==================== 汇总 ====================#
log_step "7.1 汇总"

apply_cost=$((APPLY_DONE_TS - START_TS))
total_cost=$((READY_DONE_TS - START_TS))
ready_wait_cost=$((READY_DONE_TS - APPLY_DONE_TS))
apply_cost_ms=$((APPLY_DONE_MS - START_MS))
total_cost_ms=$((READY_DONE_MS - START_MS))
ready_wait_cost_ms=$((READY_DONE_MS - APPLY_DONE_MS))

log_info "COUNT=${COUNT}"
log_info "apply_cost_seconds=${apply_cost}"
log_info "ready_wait_seconds=${ready_wait_cost}"
log_info "total_ready_seconds=${total_cost}"
log_info "apply_cost_ms=${apply_cost_ms}"
log_info "ready_wait_ms=${ready_wait_cost_ms}"
log_info "total_ready_ms=${total_cost_ms}"
log_info "apply_cost=$(fmt_ms "${apply_cost_ms}")"
log_info "ready_wait=$(fmt_ms "${ready_wait_cost_ms}")"
log_info "total_ready=$(fmt_ms "${total_cost_ms}")"
log_info "ready_count=${ready_count}/${COUNT}"

print_summary

if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "批量启动验证通过"
    exit 0
else
    exit 1
fi
