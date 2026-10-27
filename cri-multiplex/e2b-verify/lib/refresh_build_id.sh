#!/bin/bash
###############################################################################
# refresh_build_id.sh — 生成/刷新 kubelet Pod YAML（使用当前 E2B 模板元数据）
#
# 历史实现依赖 /home/zrj/build_prod.py + test.py + API 日志，且把 build-id
# 写死；在环境迁移后必然失败。现在改为：
#   1. 从 E2B API /templates 解析当前可用的 template-id/build-id/team-id；
#   2. 生成随机 execution-id + envd-access-token（multiplex 直连创建时
#      不需要预先在 API 注册沙箱，实测可用）；
#   3. 仅当显式设置 E2B_SKIP_BUILD=0 且 API 无可用模板时，才回退到
#      build_prod.py 构建模板。
#
# 用法:
#   bash e2b-verify/lib/refresh_build_id.sh [pod_name]
#
# 输出:
#   ${POD_YAML:-/tmp/e2b-kubelet-pod.yaml}
#   stdout: build_id
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

POD_NAME="${1:-e2b-kubelet-test}"
OUT_YAML="${POD_YAML:-/tmp/e2b-kubelet-pod.yaml}"
E2B_SKIP_BUILD="${E2B_SKIP_BUILD:-1}"

log() { echo "[$(date +%H:%M:%S)] $*" >&2; }

if ! resolve_e2b_template; then
    if [ "${E2B_SKIP_BUILD}" = "0" ] && [ -f "${SCRIPT_DIR}/../build_prod.py" ]; then
        log "API 中无可用模板，尝试执行 build_prod.py ${E2B_TEMPLATE_ALIAS}"
        (
            cd "${SCRIPT_DIR}/.."
            python3 ./build_prod.py "${E2B_TEMPLATE_ALIAS}"
        ) || true
        resolve_e2b_template --force || {
            log "ERROR: 仍无法解析 E2B 模板元数据"
            exit 1
        }
    else
        log "ERROR: 无法从 E2B API 解析模板元数据（API=${E2B_API_URL}）"
        exit 1
    fi
fi

log "Step 1: 写 kubelet Pod YAML: ${OUT_YAML}"
if ! write_e2b_pod_yaml "${POD_NAME}" "${OUT_YAML}"; then
    log "ERROR: 生成 ${OUT_YAML} 失败"
    exit 1
fi

if [ ! -s "${OUT_YAML}" ]; then
    log "ERROR: ${OUT_YAML} 为空"
    exit 1
fi

log "build_id=${E2B_BUILD_ID} template_id=${E2B_TEMPLATE_ID} team_id=${E2B_TEAM_ID}"
log "YAML 已生成: ${OUT_YAML}"
echo "${E2B_BUILD_ID}"
