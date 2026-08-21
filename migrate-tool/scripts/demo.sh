#!/usr/bin/env sh
set -eu

# migrate-tool 是独立 Go 模块，不在仓库根 go.work 的 use 列表里（与 cri-multiplex 一致）。
export GOWORK=off

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/template-migrate-demo.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

if [ -n "${TEMPLATE_MIGRATE_BIN:-}" ]; then
  migrate() {
    "$TEMPLATE_MIGRATE_BIN" "$@"
  }
else
  migrate() {
    go run "$repo_dir/cmd/template-migrate" "$@"
  }
fi

mkdir -p "$work_dir/target"
cp "$repo_dir/testdata/demo/target/catalog.json" "$work_dir/target/catalog.json"
mkdir -p "$work_dir/target/objects"

migrate export \
  --catalog "$repo_dir/testdata/demo/source/catalog.json" \
  --store "$repo_dir/testdata/demo/source/objects" \
  --all \
  --out "$work_dir/bundle"

migrate inspect "$work_dir/bundle"
migrate verify "$work_dir/bundle"

migrate import "$work_dir/bundle" \
  --catalog "$work_dir/target/catalog.json" \
  --store "$work_dir/target/objects" \
  --target-team slug:runtime \
  --literal-namespace legacy=archive

migrate import "$work_dir/bundle" \
  --catalog "$work_dir/target/catalog.json" \
  --store "$work_dir/target/objects" \
  --target-team slug:runtime \
  --literal-namespace legacy=archive \
  --apply

migrate import "$work_dir/bundle" \
  --catalog "$work_dir/target/catalog.json" \
  --store "$work_dir/target/objects" \
  --target-team slug:runtime \
  --literal-namespace legacy=archive \
  --conflict-policy skip-identical
