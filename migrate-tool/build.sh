#!/usr/bin/env bash
# 构建 template-migrate 普通制品（File / PostgreSQL / S3 / MinIO）。
# Mooncake 制品需在目标 Linux/ARM64 主机上执行 scripts/build-mooncake.sh。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${ROOT_DIR}"

# migrate-tool 是独立 Go 模块，不在仓库根 go.work 的 use 列表里（与 cri-multiplex 一致）。
export GOWORK=off

OUTPUT="${ROOT_DIR}/bin/template-migrate"
mkdir -p "${ROOT_DIR}/bin"

echo "==> GOWORK=${GOWORK}"
echo "==> output: ${OUTPUT}"

go build -buildvcs=false -trimpath -o "${OUTPUT}" ./cmd/template-migrate

echo "==> built: ${OUTPUT}"
"${OUTPUT}" version
