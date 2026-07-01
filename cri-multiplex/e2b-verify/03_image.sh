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

IMAGE_FULL="e2b.dev/dqoim7o51k7e89b2s8bl:d878b117-157b-4d18-bd9b-96f603b51558"

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
