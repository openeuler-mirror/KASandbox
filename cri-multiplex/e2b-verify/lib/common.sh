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
MULTIPLEX_DIR="${MULTIPLEX_DIR:-/home/zrj/cri-multiplex}"
CONTAINERD_SOCKET="${CONTAINERD_SOCKET:-/run/containerd/containerd.sock}"

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
    local log_line
    log_line=$(kubectl logs api-8cfbf9cfd-vbfw6 -ne2b 2>/dev/null | grep "base_template_id" | tail -1 || true)

    if [ -z "${log_line}" ]; then
        log_fail "无法从 kubectl logs 获取 base_template_id 日志"
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

    while [ $attempt -le $max_retries ]; do
        log_info "RunPodSandbox 尝试 ${attempt}/${max_retries}..."
        output=$(${CRICTL} runp -r e2b "${POD_JSON}" 2>&1) || true

        # 检查是否成功（纯 ID 字符串）
        if echo "${output}" | grep -qE "^[a-z0-9]+$" && ! echo "${output}" | grep -qi "error\|FATA"; then
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

    # StartContainer
    output=$(grpc_call "runtime.v1.RuntimeService/StartContainer" "{\"container_id\": \"${cid}\"}") || true
    if ! echo "${output}" | grep -q "^{}" && ! echo "${output}" | grep -q "^$"; then
        log_fail "StartContainer 失败: ${output}"
        return 1
    fi

    echo "${cid}"
    return 0
}

#==================== 清理资源 ====================#
cleanup_container() {
    local cid="${1:-${CONTAINER_ID}}"
    [ -n "${cid}" ] && grpc_call "runtime.v1.RuntimeService/RemoveContainer" "{\"container_id\": \"${cid}\"}" > /dev/null 2>&1 || true
}

cleanup_pod() {
    local pid="${1:-${POD_UID}}"
    [ -n "${pid}" ] && grpc_call "runtime.v1.RuntimeService/RemovePodSandbox" "{\"pod_sandbox_id\": \"${pid}\"}" > /dev/null 2>&1 || true
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
