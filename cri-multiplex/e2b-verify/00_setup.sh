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

#==================== 4. 创建默认 e2b-pod.json ====================#
log_step "1.4 准备 e2b-pod.json"
if [ -f "${POD_JSON}" ]; then
    log_info "e2b-pod.json 已存在: ${POD_JSON}"
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
    log_info "e2b-pod.json 创建成功"
fi

# 自愈：base pod json 若被历史手工测试写入 direct 隐藏标签（mux 带
# -hide-sandbox-label 时会导致 02 等用例 ListPodSandbox 看不到自建 Pod），剥除之
if [ -f "${POD_JSON}" ] && grep -q '"flux-sandbox.io/direct"' "${POD_JSON}"; then
    python3 - "${POD_JSON}" <<'PY'
import json, sys
path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    pod = json.load(f)
labels = pod.get("labels") or {}
if labels.pop("flux-sandbox.io/direct", None) is not None:
    with open(path, "w", encoding="utf-8") as f:
        json.dump(pod, f, indent=2, ensure_ascii=False)
    print("stripped")
PY
    log_info "已从 ${POD_JSON} 剥除 flux-sandbox.io/direct 隐藏标签"
fi

#==================== 5. 检查 kubectl ====================#
log_step "1.5 检查 kubectl"
if command -v kubectl &> /dev/null; then
    log_info "kubectl 已安装: $(kubectl version --client --short 2>/dev/null || kubectl version --client 2>&1 | head -1)"
else
    log_info "kubectl 未安装（snapshot 错误修复需要 kubectl，可后续安装）"
fi

#==================== 6. 检查 test.py 和 build_prod.py ====================#
log_step "1.6 检查 test.py 和 build_prod.py"
if [ -f "${TEST_PY}" ]; then
    log_info "test.py 已存在: ${TEST_PY}"
else
    log_info "test.py 不存在（snapshot 错误修复需要该脚本）"
fi
if [ -f "${BUILD_PROD_PY}" ]; then
    log_info "build_prod.py 已存在: ${BUILD_PROD_PY}"
else
    log_info "build_prod.py 不存在（snapshot 错误修复需要该脚本）"
fi

#==================== 7. 检查 .env ====================#
log_step "1.7 检查 .env"
if [ -f "${SCRIPT_DIR}/.env" ]; then
    log_info ".env 已存在: ${SCRIPT_DIR}/.env"
else
    log_info ".env 不存在（build_prod.py 和 test.py 需要）"
fi

#==================== 8. 看护 busybox 本地镜像 ====================#
# 14/15/16/19/21 号用例的 client/target Pod 使用 docker.io/library/busybox:latest
# （yaml 均为 imagePullPolicy: IfNotPresent）。镜像一旦被 containerd GC 回收，
# 在 docker.io 不可达的节点上会 ErrImagePull 且 kubelet Events 极具迷惑性，
# 这里显式看护：缺失时尝试拉回，拉不回则 fail-fast 给出指引。
log_step "1.8 看护 busybox 本地镜像"
BUSYBOX_IMAGE="${BUSYBOX_IMAGE:-docker.io/library/busybox:latest}"
BUSYBOX_IMAGE_MIRROR="${BUSYBOX_IMAGE_MIRROR:-}"
if crictl inspecti "${BUSYBOX_IMAGE}" >/dev/null 2>&1; then
    log_info "busybox 镜像已在本地: ${BUSYBOX_IMAGE}"
else
    log_info "busybox 镜像缺失，尝试拉取: ${BUSYBOX_IMAGE}"
    if crictl pull "${BUSYBOX_IMAGE}" >/dev/null 2>&1; then
        log_info "busybox 镜像拉取成功"
    elif [ -n "${BUSYBOX_IMAGE_MIRROR}" ] && crictl pull "${BUSYBOX_IMAGE_MIRROR}" >/dev/null 2>&1 \
        && ctr -n k8s.io images tag "${BUSYBOX_IMAGE_MIRROR}" "${BUSYBOX_IMAGE}" >/dev/null 2>&1; then
        log_info "busybox 镜像已从镜像源拉取并重打 tag: ${BUSYBOX_IMAGE_MIRROR}"
    else
        echo "ERROR: 无法获取 ${BUSYBOX_IMAGE}（docker.io 不可达且本地缺失）。"
        echo "  请手工 ctr -n k8s.io images import 或用 BUSYBOX_IMAGE_MIRROR=<可达镜像> 重跑本脚本。"
        exit 1
    fi
fi

echo ""
log_info "环境准备完成"
exit 0
