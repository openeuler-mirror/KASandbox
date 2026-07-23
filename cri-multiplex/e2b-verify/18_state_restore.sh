#!/bin/bash
###############################################################################
# 18_state_restore.sh — cri-multiplex 重启恢复验证
#
# 验证目标：
#   1. 创建 E2B Pod/Container 后，cri-multiplex 重启不丢失路由和状态
#   2. 重启后 PodSandboxStatus / ContainerStatus / List 接口仍可用
#   3. 重启后 Stop / Remove 仍可正常执行
#
# 说明：
#   - 这是 MVP 级恢复用例，当前优先验证 E2B 路径
#   - 通过独立重启 cri-multiplex 验证本地状态文件恢复能力
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "18 — cri-multiplex 重启恢复验证"

POD_NAME="${POD_NAME:-e2b-state-restore-test}"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"
RESTART_CNI_ENABLED="${RESTART_CNI_ENABLED:-0}"
STATE_DIR="${STATE_DIR:-/tmp/cri-multiplex-state-restore}"
export STATE_DIR

cleanup_all() {
    cleanup_container "${CONTAINER_ID:-}" >/dev/null 2>&1 || true
    cleanup_pod "${POD_UID:-}" >/dev/null 2>&1 || true
}
trap cleanup_all EXIT

log_step "1.1 前置检查"

if [ ! -f "${REFRESH_SCRIPT}" ]; then
    log_fail "刷新脚本不存在: ${REFRESH_SCRIPT}"
    exit 1
fi
log_pass "刷新脚本存在: ${REFRESH_SCRIPT}"

if cri_multiplex_ready; then
    log_pass "cri-multiplex 已运行"
else
    log_info "cri-multiplex 当前未运行，后续将直接启动非 CNI 模式"
fi

log_step "1.2 切换 cri-multiplex 到非 CNI 模式"
rm -rf "${STATE_DIR}"
mkdir -p "${STATE_DIR}"
if ! E2B_CNI_ENABLED=0 E2B_FORCE_RESTART=1 "${SCRIPT_DIR}/01_start_multiplex.sh" >&2; then
    log_fail "启动 cri-multiplex 非 CNI 模式失败"
    exit 1
fi
require_cri_multiplex_ready || exit 1
log_pass "cri-multiplex 已处于非 CNI 模式"

log_step "2.1 刷新 build_id 并准备独立 Pod JSON"
log_info "执行: bash ${REFRESH_SCRIPT} ${POD_NAME}"
if bash "${REFRESH_SCRIPT}" "${POD_NAME}" >&2; then
    if [ ! -f "/tmp/e2b-kubelet-pod.yaml" ]; then
        log_fail "刷新脚本未生成 /tmp/e2b-kubelet-pod.yaml"
        exit 1
    fi
    prepare_direct_pod_json "restore" "${BASE_POD_JSON:-${POD_JSON}}" || exit 1
    if ! sync_e2b_pod_json_from_kubelet_yaml "/tmp/e2b-kubelet-pod.yaml" "${POD_JSON}"; then
        log_fail "同步 build_id 到 Pod JSON 失败"
        exit 1
    fi
    log_pass "build_id 刷新并同步成功"
else
    log_info "刷新 build_id 失败，尝试复用现有 ${POD_JSON}"
    if [ ! -f "${POD_JSON}" ]; then
        log_fail "现有 Pod JSON 不存在: ${POD_JSON}"
        exit 1
    fi
    if ! grep -q 'e2b.dev/template-id' "${POD_JSON}" ||
       ! grep -q 'e2b.dev/build-id' "${POD_JSON}" ||
       ! grep -q 'e2b.dev/team-id' "${POD_JSON}"; then
        log_fail "现有 Pod JSON 缺少必要的 E2B annotations: ${POD_JSON}"
        exit 1
    fi
    prepare_direct_pod_json "restore" "${POD_JSON}" || exit 1
    log_pass "已复用现有 Pod JSON"
fi
log_pass "已生成待恢复验证的 Pod JSON: ${POD_JSON}"

log_step "2.2 创建 Pod 和 Container"
POD_UID=$(run_pod_sandbox) || {
    log_fail "RunPodSandbox 失败"
    exit 1
}
export POD_UID
log_pass "RunPodSandbox 成功，Pod UID: ${POD_UID}"

CONTAINER_ID=$(create_and_start_container "${POD_UID}") || {
    log_fail "Create/Start Container 失败"
    exit 1
}
export CONTAINER_ID
log_pass "Create/Start Container 成功，Container ID: ${CONTAINER_ID}"

log_step "2.3 重启前状态校验"
before_pod=$(${CRICTL} inspectp "${POD_UID}" 2>&1) || true
if echo "${before_pod}" | grep -q "SANDBOX_READY"; then
    log_pass "重启前 PodSandboxStatus 正常"
else
    log_fail "重启前 PodSandboxStatus 异常: ${before_pod}"
    exit 1
fi

before_ctr=$(grpc_call "runtime.v1.RuntimeService/ContainerStatus" "{\"container_id\": \"${CONTAINER_ID}\"}") || true
if echo "${before_ctr}" | grep -q "CONTAINER_RUNNING"; then
    log_pass "重启前 ContainerStatus 正常"
else
    log_fail "重启前 ContainerStatus 异常: ${before_ctr}"
    exit 1
fi

before_list_pod=$(${CRICTL} pods 2>&1) || true
if echo "${before_list_pod}" | grep -q "${POD_UID:0:12}"; then
    log_pass "重启前 ListPodSandbox 包含目标 Pod"
else
    log_fail "重启前 ListPodSandbox 未找到目标 Pod: ${before_list_pod}"
    exit 1
fi

before_list_ctr=$(${CRICTL} ps 2>&1) || true
if echo "${before_list_ctr}" | grep -q "${CONTAINER_ID:0:12}"; then
    log_pass "重启前 ListContainers 包含目标 Container"
else
    log_fail "重启前 ListContainers 未找到目标 Container: ${before_list_ctr}"
    exit 1
fi

log_step "3.1 重启 cri-multiplex"
if ! E2B_CNI_ENABLED="${RESTART_CNI_ENABLED}" E2B_FORCE_RESTART=1 "${SCRIPT_DIR}/01_start_multiplex.sh" >&2; then
    log_fail "重启 cri-multiplex 失败"
    exit 1
fi
require_cri_multiplex_ready || exit 1
log_pass "cri-multiplex 重启完成"

log_step "3.2 重启后状态恢复校验"
after_pod=$(${CRICTL} inspectp "${POD_UID}" 2>&1) || true
if echo "${after_pod}" | grep -q "SANDBOX_READY" && echo "${after_pod}" | grep -q "${POD_UID}"; then
    log_pass "重启后 PodSandboxStatus 恢复正常"
else
    log_fail "重启后 PodSandboxStatus 恢复失败: ${after_pod}"
    exit 1
fi

after_ctr=$(grpc_call "runtime.v1.RuntimeService/ContainerStatus" "{\"container_id\": \"${CONTAINER_ID}\"}") || true
if echo "${after_ctr}" | grep -q "CONTAINER_RUNNING" && echo "${after_ctr}" | grep -q "${CONTAINER_ID}"; then
    log_pass "重启后 ContainerStatus 恢复正常"
else
    log_fail "重启后 ContainerStatus 恢复失败: ${after_ctr}"
    exit 1
fi

after_list_pod=$(${CRICTL} pods 2>&1) || true
if echo "${after_list_pod}" | grep -q "${POD_UID:0:12}"; then
    log_pass "重启后 ListPodSandbox 仍包含目标 Pod"
else
    log_fail "重启后 ListPodSandbox 未找到目标 Pod: ${after_list_pod}"
    exit 1
fi

after_list_ctr=$(${CRICTL} ps 2>&1) || true
if echo "${after_list_ctr}" | grep -q "${CONTAINER_ID:0:12}"; then
    log_pass "重启后 ListContainers 仍包含目标 Container"
else
    log_fail "重启后 ListContainers 未找到目标 Container: ${after_list_ctr}"
    exit 1
fi

log_step "3.3 重启后 Stop / Remove 校验"
output=$(grpc_call "runtime.v1.RuntimeService/StopContainer" "{\"container_id\": \"${CONTAINER_ID}\", \"timeout\": 30}") || true
if echo "${output}" | grep -q "^{}\|^$"; then
    log_pass "重启后 StopContainer 成功"
else
    log_fail "重启后 StopContainer 异常: ${output}"
    exit 1
fi

output=$(grpc_call "runtime.v1.RuntimeService/StopPodSandbox" "{\"pod_sandbox_id\": \"${POD_UID}\"}") || true
if echo "${output}" | grep -q "^{}\|^$"; then
    log_pass "重启后 StopPodSandbox 成功"
else
    log_fail "重启后 StopPodSandbox 异常: ${output}"
    exit 1
fi

output=$(grpc_call "runtime.v1.RuntimeService/RemoveContainer" "{\"container_id\": \"${CONTAINER_ID}\"}") || true
if echo "${output}" | grep -q "^{}\|^$"; then
    log_pass "重启后 RemoveContainer 成功"
else
    log_fail "重启后 RemoveContainer 异常: ${output}"
    exit 1
fi

output=$(grpc_call "runtime.v1.RuntimeService/RemovePodSandbox" "{\"pod_sandbox_id\": \"${POD_UID}\"}") || true
if echo "${output}" | grep -q "^{}\|^$"; then
    log_pass "重启后 RemovePodSandbox 成功"
else
    log_fail "重启后 RemovePodSandbox 异常: ${output}"
    exit 1
fi

log_step "3.4 删除后列表校验"
final_pods=$(${CRICTL} pods 2>&1) || true
if echo "${final_pods}" | grep -q "${POD_UID}"; then
    log_fail "Pod 仍存在于列表中"
    exit 1
else
    log_pass "Pod 已从列表中删除"
fi

final_ctrs=$(${CRICTL} ps 2>&1) || true
if echo "${final_ctrs}" | grep -q "${CONTAINER_ID}"; then
    log_fail "Container 仍存在于列表中"
    exit 1
else
    log_pass "Container 已从列表中删除"
fi

print_summary

if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_info "验证通过：cri-multiplex 重启后可恢复 E2B 路由、Pod 和 Container 状态"
    exit 0
else
    exit 1
fi
