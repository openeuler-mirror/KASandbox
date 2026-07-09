#!/bin/bash
###############################################################################
# 03_image.sh — 镜像管理验证
#
# 覆盖验证指南：
#   四、ImageService 验证
#     4.1 PullImage
#     4.2 ImageStatus（存在）
#     4.4 ImageStatus（不存在）
#     4.5 RemoveImage
#     4.6 ImageFsInfo
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "03 — 镜像管理验证"

POD_YAML="/tmp/e2b-kubelet-pod.yaml"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"

log_step "0.1 刷新 build_id"
log_info "执行: bash ${REFRESH_SCRIPT} e2b-image-test"
if bash "${REFRESH_SCRIPT}" e2b-image-test >&2; then
    log_pass "build_id 刷新成功"
else
    log_info "刷新 build_id 失败，尝试复用已有 ${POD_YAML}"
    if [ -f "${POD_YAML}" ] && grep -q 'e2b.dev/build-id:' "${POD_YAML}"; then
        log_pass "复用已有 Pod YAML: ${POD_YAML}"
    else
        log_fail "刷新 build_id 失败，且没有可复用的 Pod YAML"
        print_summary
        exit 1
    fi
fi

TEMPLATE_ID=$(grep -oP 'e2b\.dev/template-id:\s*"\K[^"]+' "${POD_YAML}" | head -1 || true)
BUILD_ID=$(grep -oP 'e2b\.dev/build-id:\s*"\K[^"]+' "${POD_YAML}" | head -1 || true)
if [ -z "${TEMPLATE_ID}" ] || [ -z "${BUILD_ID}" ]; then
    log_fail "无法从 ${POD_YAML} 提取 template_id/build_id"
    print_summary
    exit 1
fi

IMAGE_FULL="e2b.dev/${TEMPLATE_ID}:${BUILD_ID}"
log_pass "使用镜像: ${IMAGE_FULL}"
if sync_e2b_pod_json_from_kubelet_yaml "${POD_YAML}" "${POD_JSON}"; then
    log_pass "已同步 ${POD_JSON} 的 build_id"
else
    log_fail "同步 ${POD_JSON} 失败"
fi

#==================== 4.1 PullImage ====================#
log_step "4.1 PullImage"
output=$(${CRICTL} --image-endpoint "unix://${SOCKET}" pull "${IMAGE_FULL}" 2>&1) || true
if echo "${output}" | grep -q "Image is up to date\|Successfully\|already"; then
    log_pass "PullImage 成功"
else
    # 有可能已经拉取过，只要没报错就行
    if ! echo "${output}" | grep -qi "error\|FATA"; then
        log_pass "PullImage 成功（${output}）"
    else
        log_fail "PullImage 异常: ${output}"
    fi
fi

#==================== 4.2 ImageStatus（存在） ====================#
log_step "4.2 ImageStatus（存在）"
output=$(grpc_call "runtime.v1.ImageService/ImageStatus" "{\"image\": {\"image\": \"${IMAGE_FULL}\"}}") || true
if echo "${output}" | grep -q "repoTags\|image"; then
    log_pass "ImageStatus（存在）返回正确"
else
    log_fail "ImageStatus（存在）异常: ${output}"
fi

#==================== 4.4 ImageStatus（不存在） ====================#
log_step "4.4 ImageStatus（不存在）"
output=$(grpc_call "runtime.v1.ImageService/ImageStatus" '{"image": {"image": "e2b.dev/base:notexist"}}') || true
if echo "${output}" | grep -q "^{}\|^$"; then
    log_pass "ImageStatus（不存在）返回空 {}"
else
    log_fail "ImageStatus（不存在）异常: ${output}"
fi

#==================== 4.5 RemoveImage ====================#
log_step "4.5 RemoveImage"
output=$(grpc_call "runtime.v1.ImageService/RemoveImage" "{\"image\": {\"image\": \"${IMAGE_FULL}\"}}") || true
if echo "${output}" | grep -q "^{}\|^$"; then
    log_pass "RemoveImage 成功"
else
    log_fail "RemoveImage 异常: ${output}"
fi

#==================== 4.6 ImageFsInfo ====================#
log_step "4.6 ImageFsInfo"
output=$(grpc_call "runtime.v1.ImageService/ImageFsInfo") || true
if echo "${output}" | grep -q "imageFilesystems\|^{}"; then
    log_pass "ImageFsInfo 返回正确"
else
    log_fail "ImageFsInfo 异常: ${output}"
fi

print_summary
exit 0
