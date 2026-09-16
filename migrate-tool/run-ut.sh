#!/usr/bin/env bash
# migrate-tool 单元测试
#   ./run-ut.sh                 # 全部 UT
#   ./run-ut.sh -p importer     # 只跑 internal/importer
#   ./run-ut.sh --no-race       # 关闭 race（无 C 工具链的环境）
#   ./run-ut.sh --cover         # 附带覆盖率
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${ROOT_DIR}"

# migrate-tool 是独立 Go 模块，不在仓库根 go.work 的 use 列表里（与 cri-multiplex 一致）。
export GOWORK=off

RACE_FLAG="-race"
COVER_FLAG=""
TARGET="./..."

while [[ $# -gt 0 ]]; do
  case "$1" in
    -p|--package)
      [[ $# -lt 2 || "$2" == -* ]] && { echo "Error: -p requires a package name" >&2; exit 1; }
      TARGET="./internal/$2"
      shift 2
      ;;
    --no-race) RACE_FLAG=""; shift ;;
    --cover)   COVER_FLAG="-cover"; shift ;;
    -h|--help) sed -n '2,6p' "$0"; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done

echo "==> $(go version)"
echo "==> GOWORK=${GOWORK}  target=${TARGET}"

# PostgreSQL Catalog 的集成测试需要 TM_TEST_POSTGRES_DSN 指向一次性测试库，
# 未设置时这些用例自动 Skip，其余 UT 不依赖任何外部服务。
go test -mod=readonly ${RACE_FLAG} ${COVER_FLAG} -count=1 -timeout 10m "${TARGET}"
