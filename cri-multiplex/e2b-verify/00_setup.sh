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
    log_pass "crictl 已安装: $(crictl --version 2>&1 | head -1)"
else
    log_fail "crictl 未安装"
    exit 1
fi

#==================== 2. 检查/安装 grpcurl ====================#
log_step "1.2 检查/安装 grpcurl"
if command -v grpcurl &> /dev/null; then
    log_pass "grpcurl 已安装: $(grpcurl -version 2>&1 | head -1)"
else
    log_info "grpcurl 未安装，开始安装..."
    export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
    go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest
    export PATH="$PATH:$(go env GOPATH)/bin"
    if command -v grpcurl &> /dev/null; then
        log_pass "grpcurl 安装成功"
    else
        log_fail "grpcurl 安装失败，请手动安装"
        exit 1
    fi
fi

#==================== 3. 下载 CRI API proto ====================#
log_step "1.3 准备 CRI API proto 文件"
mkdir -p "${PROTO_DIR}"
if [ -f "${PROTO_FILE}" ]; then
    log_pass "proto 文件已存在: ${PROTO_FILE}"
else
    log_info "下载 CRI API proto..."
    curl -sL https://raw.githubusercontent.com/kubernetes/cri-api/master/pkg/apis/runtime/v1/api.proto \
        -o "${PROTO_FILE}"
    if [ -f "${PROTO_FILE}" ]; then
        log_pass "proto 文件下载成功"
    else
        log_fail "proto 文件下载失败"
        exit 1
    fi
fi

#==================== 4. 创建默认 e2b-pod.json ====================#
log_step "1.4 准备 e2b-pod.json"
if [ -f "${POD_JSON}" ]; then
    log_pass "e2b-pod.json 已存在: ${POD_JSON}"
else
    log_info "创建默认 e2b-pod.json..."
    cat > "${POD_JSON}" <<'EOF'
{
  "metadata": {
    "name": "test-default-pod",
    "namespace": "default",
    "uid": "irlkuj9aask5hmw37uc51"
  },
  "annotations": {
    "e2b.dev/base_template_id": "dqoim7o51k7e89b2s8bl",
    "e2b.dev/template-id": "dqoim7o51k7e89b2s8bl",
    "e2b.dev/build-id": "d878b117-157b-4d18-bd9b-96f603b51558",
    "e2b.dev/team-id": "db8250ab-9929-48a2-8fb6-c4e992d59288",
    "e2b.dev/vcpu": "1",
    "e2b.dev/ram-mb": "1024",
    "e2b.dev/total-disk-size-mb": "942",
    "e2b.dev/max-sandbox-length": "10000",
    "e2b.dev/huge-pages": "true",
    "e2b.dev/auto-pause": "false",
    "e2b.dev/snapshot": "false",
    "e2b.dev/allow-internet": "true",
    "e2b.dev/envd-version": "0.5.3",
    "e2b.dev/kernel-version": "vmlinux-6.1.158",
    "e2b.dev/firecracker-version": "v1.13.1",
    "e2b.dev/execution-id": "bd0b32b0-7a29-4961-9d65-3d71de28fc5c",
    "e2b.dev/envd-access-token": "88dbe4bd9c41b6a184d237e1021c867c0352607330c673aa819d339c38d227f7",
    "e2b.dev/env-vars": "{}",
    "e2b.dev/network": "{\"egress\":{},\"ingress\":{}}",
    "e2b.dev/volume-mounts": "[]",
    "e2b.dev/auto-resume": "{\"policy\":\"off\"}"
  },
  "labels": {
    "app": "test"
  },
  "log_directory": "/tmp",
  "linux": {
    "security_context": {
      "namespace_options": {
        "network": 2
      }
    }
  }
}
EOF
    log_pass "e2b-pod.json 创建成功"
fi

#==================== 5. 检查 kubectl ====================#
log_step "1.5 检查 kubectl"
if command -v kubectl &> /dev/null; then
    log_pass "kubectl 已安装: $(kubectl version --client --short 2>/dev/null || kubectl version --client 2>&1 | head -1)"
else
    log_info "kubectl 未安装（snapshot 错误修复需要 kubectl，可后续安装）"
fi

#==================== 6. 检查 test.py 和 build_prod.py ====================#
log_step "1.6 检查 test.py 和 build_prod.py"
if [ -f "${TEST_PY}" ]; then
    log_pass "test.py 已存在: ${TEST_PY}"
else
    log_fail "test.py 不存在（snapshot 错误修复需要该脚本）"
fi
if [ -f "${BUILD_PROD_PY}" ]; then
    log_pass "build_prod.py 已存在: ${BUILD_PROD_PY}"
else
    log_fail "build_prod.py 不存在（snapshot 错误修复需要该脚本）"
fi

#==================== 7. 检查 .env ====================#
log_step "1.7 检查 .env"
if [ -f "${SCRIPT_DIR}/.env" ]; then
    log_pass ".env 已存在: ${SCRIPT_DIR}/.env"
else
    log_fail ".env 不存在（build_prod.py 和 test.py 需要）"
fi

echo ""
log_info "环境准备完成"
exit 0
