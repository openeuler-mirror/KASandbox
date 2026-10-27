#!/bin/bash
###############################################################################
# 00_setup.sh — 验证工具安装与环境准备
#
# 职责：
#   1. 安装 grpcurl（如未安装）
#   2. 下载 CRI API proto 文件
#   3. 创建默认 e2b-pod.json
#   4. 检查 crictl 可用性
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "00 — 验证工具安装与环境准备"

#==================== 1. 检查 crictl ====================#
log_step "1.1 检查 crictl"
if command -v crictl &> /dev/null; then
    log_info "crictl 已安装: $(crictl --version 2>&1 | head -1)"
else
    log_info "crictl 未安装"
    exit 1
fi

#==================== 2. 检查/安装 grpcurl ====================#
log_step "1.2 检查/安装 grpcurl"
if command -v grpcurl &> /dev/null; then
    log_info "grpcurl 已安装: $(grpcurl -version 2>&1 | head -1)"
else
    log_info "grpcurl 未安装，开始安装..."
    export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
    go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest
    export PATH="$PATH:$(go env GOPATH)/bin"
    if command -v grpcurl &> /dev/null; then
        log_info "grpcurl 安装成功"
    else
        log_info "grpcurl 安装失败，请手动安装"
        exit 1
    fi
fi

#==================== 3. 下载 CRI API proto ====================#
log_step "1.3 准备 CRI API proto 文件"
mkdir -p "${PROTO_DIR}"
if [ -f "${PROTO_FILE}" ]; then
    log_info "proto 文件已存在: ${PROTO_FILE}"
else
    log_info "下载 CRI API proto..."
    curl -sL https://raw.githubusercontent.com/kubernetes/cri-api/master/pkg/apis/runtime/v1/api.proto \
        -o "${PROTO_FILE}"
    if [ -f "${PROTO_FILE}" ]; then
        log_info "proto 文件下载成功"
    else
        log_info "proto 文件下载失败"
        exit 1
    fi
fi

#==================== 4. 解析 E2B 模板并创建默认 e2b-pod.json ====================#
log_step "1.4 解析 E2B 模板并准备 e2b-pod.json"
if ! resolve_e2b_template; then
    log_info "无法从 E2B API 解析模板元数据（API=${E2B_API_URL}），请检查 API/凭证"
    log_info "  .env: ${SCRIPT_DIR}/.env"
    log_info "  config: /root/.e2b/config.json"
    exit 1
fi
log_info "模板别名: ${E2B_TEMPLATE_ALIAS} (template=${E2B_TEMPLATE_ID}, build=${E2B_BUILD_ID})"
log_info "团队: ${E2B_TEAM_ID}, 规格: ${E2B_VCPU}vCPU/${E2B_RAM_MB}MB/${E2B_DISK_MB}MB"

# 测试 fixture 应始终反映当前 live 模板；旧的 build-id 会造成 snapshot 失败。
if [ "${E2B_KEEP_POD_JSON:-0}" != "1" ] || [ ! -f "${POD_JSON}" ]; then
    if write_default_pod_json "${POD_UID}" "test-default-pod" "${POD_JSON}"; then
        log_info "e2b-pod.json 已生成: ${POD_JSON}"
    else
        log_info "e2b-pod.json 生成失败"
        exit 1
    fi
else
    ensure_default_pod_json || exit 1
    log_info "沿用已有 e2b-pod.json: ${POD_JSON}"
fi
if ! jq -e '.annotations["e2b.dev/build-id"] and .annotations["e2b.dev/execution-id"] and .annotations["e2b.dev/envd-access-token"]' "${POD_JSON}" >/dev/null 2>&1; then
    log_info "e2b-pod.json 缺少必要注解"
    exit 1
fi
log_info "e2b-pod.json 注解校验通过"

#==================== 5. 检查 kubectl ====================#
log_step "1.5 检查 kubectl"
if command -v kubectl &> /dev/null; then
    log_info "kubectl 已安装: $(kubectl version --client --short 2>/dev/null || kubectl version --client 2>&1 | head -1)"
else
    log_info "kubectl 未安装（kubelet 用例需要）"
fi

#==================== 6. 检查 test.py 和 build_prod.py ====================#
log_step "1.6 检查 test.py / build_prod.py（可选回退）"
if [ -f "${TEST_PY}" ]; then
    log_info "test.py 已存在: ${TEST_PY}（新流程不再依赖）"
else
    log_info "test.py 不存在（新流程不依赖）"
fi
if [ -f "${BUILD_PROD_PY}" ]; then
    log_info "build_prod.py 已存在: ${BUILD_PROD_PY}（仅重建模板时使用）"
else
    log_info "build_prod.py 不存在（仅重建模板时使用）"
fi

#==================== 7. 检查 .env ====================#
log_step "1.7 检查 .env"
if [ -f "${SCRIPT_DIR}/.env" ]; then
    log_info ".env 已存在: ${SCRIPT_DIR}/.env"
    if [ -f /root/.e2b/config.json ]; then
        log_info "发现 /root/.e2b/config.json，凭证以该文件中的 teamApiKey 为准"
    fi
else
    log_info ".env 不存在，将回退使用 /root/.e2b/config.json"
fi

#==================== 8. 环境能力检查 ====================#
log_step "1.8 CNI / Android / systemd 能力检查"
if [ -d "${CNI_CONF_DIR}" ] && ls "${CNI_CONF_DIR}"/*.conflist >/dev/null 2>&1; then
    log_info "CNI 配置目录可用: ${CNI_CONF_DIR}"
else
    log_info "CNI 配置目录不可用: ${CNI_CONF_DIR}"
fi
if android_available; then
    log_info "Android artifacts 可用: ${ANDROID_ARTIFACTS_DIR}"
else
    log_info "Android artifacts 不可用（12/13/17/20/21 的 Android 部分将 SKIP）"
fi
if systemd_mux_active; then
    log_info "systemd ${SYSTEMD_UNIT} 正在运行，验证期间由脚本接管 ${SYSTEMD_SOCKET}（结束后自动恢复）"
elif systemd_mux_available; then
    log_info "systemd ${SYSTEMD_UNIT} 存在但未运行"
else
    log_info "未检测到 systemd ${SYSTEMD_UNIT}"
fi
log_info "验证 socket: ${SOCKET}（跟随 kubelet endpoint），state-dir: ${STATE_DIR}"

echo ""
log_info "环境准备完成"
exit 0
