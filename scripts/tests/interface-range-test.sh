#!/usr/bin/env bash
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/interface-range.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
unset BASE TARGET CHANGE_TARGET gitlabTargetBranch JOB_NAME
git init -q "$tmp"
cd "$tmp"
git config user.name test
git config user.email test@example.invalid
git config commit.gpgsign false
git commit -q --allow-empty -m base
base=$(git rev-parse HEAD)
git update-ref refs/remotes/origin/master "$base"
mkdir -p spec
echo source > spec/openapi.yml
git add .
git commit -qm source
echo generated > generated.go
git add .
git commit -qm generated
head=$(git rev-parse HEAD)

# 本地默认、位置参数和环境变量覆盖均保持兼容。
resolve_interface_range
test "$BASE_REF" = 'HEAD^'
test "$TARGET_REF" = HEAD
resolve_interface_range "$base" "$head"
test "$BASE_REF" = "$base"
test "$TARGET_REF" = "$head"
BASE="$head" TARGET="$base" resolve_interface_range "$base" "$head"
test "$BASE_REF" = "$head"
test "$TARGET_REF" = "$base"

# 多提交 PR 同时包含源文件与生成文件；新增文档提交不缩小检查范围。
JOB_NAME=multiarch/openeuler/check_interface/KASandbox resolve_interface_range
test "$BASE_REF" = "$base"
test "$(git diff --name-only "$BASE_REF" "$TARGET_REF" | wc -l)" -eq 2
git commit -q --allow-empty -m followup
CHANGE_TARGET=master resolve_interface_range
test "$BASE_REF" = "$base"
gitlabTargetBranch=master resolve_interface_range
test "$BASE_REF" = "$base"

# 目标分支独立前进时取共同祖先，不把目标分支新增内容算作 PR 删除。
other=$(echo target | git commit-tree "${base}^{tree}" -p "$base")
git update-ref refs/remotes/origin/master "$other"
CHANGE_TARGET=master resolve_interface_range
test "$BASE_REF" = "$base"

# 显式 BASE 优先；缺失或无共同祖先的 PR 引用必须失败，不能缩小范围。
CHANGE_TARGET=missing BASE="$base" resolve_interface_range
test "$BASE_REF" = "$base"
if CHANGE_TARGET=missing resolve_interface_range; then exit 1; fi
if CHANGE_TARGET='../bad' resolve_interface_range; then exit 1; fi
unrelated=$(echo unrelated | git commit-tree "${base}^{tree}")
git update-ref refs/remotes/origin/unrelated "$unrelated"
if CHANGE_TARGET=unrelated resolve_interface_range; then exit 1; fi
JOB_NAME=other-job resolve_interface_range
test "$BASE_REF" = 'HEAD^'
echo 'PASS: interface range regression'
