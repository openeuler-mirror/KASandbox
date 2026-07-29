#!/usr/bin/env bash
# e2b-webhook 单元测试执行脚本
# 用法:
#   ./run-tests.sh              # 运行所有测试
#   ./run-tests.sh -v           # verbose 模式
#   ./run-tests.sh -c           # 输出覆盖率报告
#   ./run-tests.sh -r <regex>   # 运行匹配的测试用例

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# e2b-webhook 是独立 Go 模块, 未列入 go.work, 需关闭 workspace
export GOWORK=off

VERBOSE=""
COVERAGE=""
RUN_REGEX=""

while getopts ":vcr:" opt; do
  case "$opt" in
    v) VERBOSE="-v" ;;
    c) COVERAGE="-coverprofile=coverage.out -covermode=atomic" ;;
    r) RUN_REGEX="-run=$OPTARG" ;;
    \?) echo "未知选项: -$OPTARG" >&2; exit 1 ;;
  esac
done

echo "=== e2b-webhook 单元测试 ==="
echo "工作目录: $SCRIPT_DIR"
echo "GOWORK=$GOWORK"
echo

# 1. go vet 静态检查
echo "[1/3] go vet 静态检查..."
if ! go vet ./... 2>&1; then
  echo "✗ go vet 失败"
  exit 1
fi
echo "✓ go vet 通过"
echo

# 2. 编译检查
echo "[2/3] 编译检查..."
if ! go build ./... 2>&1; then
  echo "✗ 编译失败"
  exit 1
fi
echo "✓ 编译通过"
echo

# 3. 运行测试
echo "[3/3] 运行测试..."
# shellcheck disable=SC2086
go test ./... $VERBOSE $COVERAGE $RUN_REGEX

TEST_EXIT=$?
if [ $TEST_EXIT -ne 0 ]; then
  echo "✗ 测试失败"
  exit $TEST_EXIT
fi

# 统计结果
if [ -n "$VERBOSE" ]; then
  PASS_COUNT=$(go test ./... -v 2>&1 | grep -cE '^\s*--- PASS' || true)
  FAIL_COUNT=$(go test ./... -v 2>&1 | grep -cE '^\s*--- FAIL' || true)
  echo
  echo "=== 测试统计 ==="
  echo "通过: $PASS_COUNT"
  echo "失败: $FAIL_COUNT"
fi

# 覆盖率报告
if [ -n "$COVERAGE" ] && [ -f coverage.out ]; then
  echo
  echo "=== 覆盖率报告 ==="
  go tool cover -func=coverage.out | tail -1
  echo "详细 HTML 报告: go tool cover -html=coverage.out"
fi

echo
echo "✓ 全部测试通过"
