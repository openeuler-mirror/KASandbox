#!/bin/bash
###############################################################################
# refresh_build_id.sh — 刷新 E2B build_id 并生成 kubelet Pod YAML
#
# 用法:
#   bash e2b-verify/lib/refresh_build_id.sh [pod_name]
#
# 输出:
#   /tmp/e2b-kubelet-pod.yaml
#   stdout 输出 build_id
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

WORK_DIR="${REFRESH_WORK_DIR:-/home/zrj}"
TEMPLATE="${BUILD_IMAGE_NAME:-ubuntu:22.04-custom}"
POD_NAME="${1:-e2b-kubelet-test}"
OUT_YAML="${POD_YAML:-/tmp/e2b-kubelet-pod.yaml}"
ROOT_BUILD_PROD_PY="${ROOT_BUILD_PROD_PY:-${WORK_DIR}/build_prod.py}"
ROOT_TEST_PY="${ROOT_TEST_PY:-${WORK_DIR}/test.py}"

log() { echo "[$(date +%H:%M:%S)] $*" >&2; }

extract_latest_e2b_log() {
    local api_pod
    if ! api_pod=$(find_e2b_api_pod); then
        log "ERROR: 未找到 ${E2B_API_NS} namespace 下运行中的 api Pod"
        return 1
    fi

    log "Step 3: 从 ${api_pod} 日志提取最新参数"
    kubectl logs "${api_pod}" -n "${E2B_API_NS}" 2>/dev/null | grep "base_template_id" | tail -1 || true
}

if [ ! -f "${ROOT_BUILD_PROD_PY}" ]; then
    log "ERROR: build_prod.py 不存在: ${ROOT_BUILD_PROD_PY}"
    exit 1
fi
if [ ! -f "${ROOT_TEST_PY}" ]; then
    log "ERROR: test.py 不存在: ${ROOT_TEST_PY}"
    exit 1
fi

log "Step 1: 执行 build_prod.py ${TEMPLATE}"
(
    cd "${WORK_DIR}"
    python3 "${ROOT_BUILD_PROD_PY}" "${TEMPLATE}" 2>&1 | tail -20
)
log "build_prod.py 完成"

log "Step 2: 执行 test.py"
for i in 1 2 3; do
    if (
        cd "${WORK_DIR}"
        python3 "${ROOT_TEST_PY}" 2>&1 | tail -10
    ); then
        log "test.py 第 ${i} 次尝试成功"
        break
    fi

    log "test.py 第 ${i} 次失败，等待 5s 重试..."
    sleep 5
    if [ "${i}" -eq 3 ]; then
        log "ERROR: test.py 3 次均失败"
        exit 1
    fi
done

sleep 3
LATEST_LOG=$(extract_latest_e2b_log)
log "最新日志: ${LATEST_LOG}"

BUILD_ID=$(echo "${LATEST_LOG}" | grep -oP '"build_id":\s*"\K[^"]+' || true)
EXEC_ID=$(echo "${LATEST_LOG}" | grep -oP '"execution_id":\s*"\K[^"]+' || true)
TOKEN=$(echo "${LATEST_LOG}" | grep -oP '"envd_access_token":\s*"\K[^"]+' || true)

if [ -z "${BUILD_ID}" ] || [ -z "${EXEC_ID}" ] || [ -z "${TOKEN}" ]; then
    log "ERROR: 未能提取参数 build_id=${BUILD_ID} exec=${EXEC_ID} token=${TOKEN:0:8}..."
    exit 1
fi
log "提取到: build_id=${BUILD_ID} execution_id=${EXEC_ID} token=${TOKEN:0:8}..."

log "Step 4: 生成 ${OUT_YAML}"
cat > "${OUT_YAML}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  annotations:
    e2b.dev/base_template_id: "dqoim7o51k7e89b2s8bl"
    e2b.dev/template-id: "dqoim7o51k7e89b2s8bl"
    e2b.dev/build-id: "${BUILD_ID}"
    e2b.dev/team-id: "db8250ab-9929-48a2-8fb6-c4e992d59288"
    e2b.dev/vcpu: "1"
    e2b.dev/ram-mb: "1024"
    e2b.dev/total-disk-size-mb: "942"
    e2b.dev/max-sandbox-length: "10000"
    e2b.dev/huge-pages: "true"
    e2b.dev/auto-pause: "false"
    e2b.dev/snapshot: "false"
    e2b.dev/allow-internet: "true"
    e2b.dev/envd-version: "0.5.3"
    e2b.dev/kernel-version: "vmlinux-6.1.158"
    e2b.dev/firecracker-version: "v1.13.1"
    e2b.dev/execution-id: "${EXEC_ID}"
    e2b.dev/envd-access-token: "${TOKEN}"
    e2b.dev/env-vars: "{}"
    e2b.dev/network: "{\"egress\":{},\"ingress\":{}}"
    e2b.dev/volume-mounts: "[]"
    e2b.dev/auto-resume: "{\"policy\":\"off\"}"
spec:
  runtimeClassName: e2b
  restartPolicy: Never
  containers:
  - name: app
    image: e2b.dev/dqoim7o51k7e89b2s8bl:${BUILD_ID}
    imagePullPolicy: IfNotPresent
    command: ["sleep", "3600"]
EOF

log "YAML 已生成: ${OUT_YAML}"
log "全部完成"
echo "${BUILD_ID}"
