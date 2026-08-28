#!/usr/bin/env bash
# 构建 e2b py-sdk 为 wheel（.whl）
# 基于 pyproject.toml 的 poetry-core 构建后端，产物输出到 dist/
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# 脚本位于 scripts/ 下，项目根为上一级（py-sdk/，含 pyproject.toml）
cd "$SCRIPT_DIR/.."

# 1. 检查/安装构建工具（python-build 会按 pyproject.toml 的 build-system 自动拉起 poetry-core）
if ! python3 -c "import build" >/dev/null 2>&1; then
    echo "==> 安装 python-build ..."
    python3 -m pip install --quiet build
fi

# 2. 清理旧构建产物
rm -rf dist build e2b.egg-info e2b_connect.egg-info

# 3. 构建 wheel（隔离环境，无需预先安装运行时依赖）
echo "==> 构建 wheel ..."
python3 -m build --wheel

# 4. 输出结果
echo ""
echo "==> 构建完成，产物:"
ls -lh dist/*.whl
