#!/bin/bash
###############################################################################
# 06_attach.sh — Attach 能力验证
#
# 覆盖验证指南：
#   5.3 Attach
#
# 流程：
#   1. 创建 Pod + Container
#   2. 调用 Attach 获取 URL
#   3. 用 Python WebSocket/PTY 客户端连接 Attach URL，验证能收到输出
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "06 — Attach 能力验证"

STREAM_CLIENT="${SCRIPT_DIR}/test_stream_client.py"
BASE_POD_JSON="${POD_JSON}"
POD_UID=""
CONTAINER_ID=""
POD_JSON=""
export POD_UID CONTAINER_ID POD_JSON

cleanup_06() {
    cleanup_container "${CONTAINER_ID:-}" >/dev/null 2>&1 || true
    cleanup_pod "${POD_UID:-}" >/dev/null 2>&1 || true
    [ -n "${POD_JSON:-}" ] && [ "${POD_JSON}" != "${BASE_POD_JSON}" ] && rm -f "${POD_JSON}" || true
}

prepare_06_pod_json() {
    POD_UID="e2battach$(date +%s)$RANDOM"
    CONTAINER_ID="${POD_UID}-c"
    POD_JSON="/tmp/e2b-pod-06-${POD_UID}.json"
    export POD_UID CONTAINER_ID POD_JSON

    cp "${BASE_POD_JSON}" "${POD_JSON}"
    sed -i \
        -e "s|\"uid\":\\s*\"[^\"]*\"|\"uid\": \"${POD_UID}\"|" \
        -e "0,/\"name\":\\s*\"[^\"]*\"/s//\"name\": \"test-e2b-attach-${POD_UID}\"/" \
        "${POD_JSON}"
    log_pass "已生成独立 Pod JSON: ${POD_JSON}"
}

finish_06() {
    local code=0
    if [ "${FAIL_COUNT}" -ne 0 ]; then
        code=1
    fi
    print_summary
    cleanup_06
    exit "${code}"
}
trap cleanup_06 EXIT

#==================== 前置：确保 cri-multiplex 可用 ====================#
log_step "前置检查：cri-multiplex 非 CNI 模式"
if cri_multiplex_ready && ! cri_multiplex_cni_enabled; then
    log_pass "cri-multiplex 已运行且处于非 CNI 模式"
else
    log_info "cri-multiplex 未就绪或当前不是非 CNI 模式，自动启动非 CNI 模式..."
    if E2B_CNI_ENABLED=0 "${SCRIPT_DIR}/01_start_multiplex.sh"; then
        log_pass "cri-multiplex 非 CNI 模式已就绪"
    else
        log_fail "cri-multiplex 非 CNI 模式启动失败"
        exit 1
    fi
fi

#==================== 前置：创建 Pod + Container ====================#
log_step "前置准备：创建 Pod 和 Container"
if [ ! -f "${BASE_POD_JSON}" ]; then
    log_fail "基础 Pod JSON 不存在: ${BASE_POD_JSON}"
    finish_06
fi

created=0
for attempt in 1 2 3; do
    if [ "${attempt}" -gt 1 ]; then
        log_info "重新创建 Attach 测试 sandbox，第 ${attempt} 次尝试..."
        cleanup_06
    fi
    fail_before_attempt="${FAIL_COUNT}"
    prepare_06_pod_json
    if POD_UID=$(run_pod_sandbox) && CONTAINER_ID=$(create_and_start_container "${POD_UID}"); then
        export POD_UID CONTAINER_ID
        created=1
        break
    fi
    if [ "${attempt}" -lt 3 ]; then
        FAIL_COUNT="${fail_before_attempt}"
        export FAIL_COUNT
    fi
    log_info "Attach 测试 sandbox 创建失败，第 ${attempt} 次尝试结束"
done

if [ "${created}" != "1" ]; then
    log_fail "创建并启动 Container 失败"
    finish_06
fi
export POD_UID CONTAINER_ID
log_info "Pod: ${POD_UID}, Container: ${CONTAINER_ID}"

#==================== Step 1: 调用 Attach ====================#
log_step "5.3.1 调用 Attach"
attach_output=$(grpc_call "runtime.v1.RuntimeService/Attach" \
    "{\"container_id\": \"${CONTAINER_ID}\", \"tty\": true, \"stdin\": true, \"stdout\": true, \"stderr\": true}") || true

if ! grep -q "url" <<< "${attach_output}"; then
    log_fail "Attach 失败: ${attach_output}"
    finish_06
fi

ATTACH_URL=$(echo "${attach_output}" | grep -oP '"url":\s*"\K[^"]+')
log_pass "Attach 返回 URL: ${ATTACH_URL}"

#==================== Step 2: 连接 Attach URL 验证 ====================#
log_step "5.3.2 连接 Attach URL 验证输出"
attach_result=$(printf 'echo attach_test_ok\nexit\n' | python3 "${STREAM_CLIENT}" "${ATTACH_URL}" 8 2>&1) || true

if grep -q "attach_test_ok" <<< "${attach_result}"; then
    log_pass "Attach 连接成功，收到输出"
else
    log_fail "Attach 连接未收到 attach_test_ok 回显: ${attach_result}"
fi

#==================== 清理 ====================#
log_step "清理资源"
cleanup_06
trap - EXIT
log_info "清理完成"

finish_06
