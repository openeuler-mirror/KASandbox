#!/usr/bin/env bash
set -euo pipefail

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"

# migrate-tool 是独立 Go 模块，不在仓库根 go.work 的 use 列表里（与 cri-multiplex 一致）。
export GOWORK=off

platform="$(go env GOOS)/$(go env GOARCH)"
if [[ "$platform" != "linux/arm64" ]]; then
  printf 'build-mooncake.sh must run on linux/arm64, got %s\n' "$platform" >&2
  exit 1
fi

# 这些目录和链接库来自目标 E2B build.sh 的 Mooncake 构建配置。脚本在目标机
# 原生编译 CGO 制品，不尝试从 Windows 交叉链接目标机的 native libraries。
export CGO_ENABLED=1
export CGO_CFLAGS="${CGO_CFLAGS:-} -I/usr/include -I/usr/include/mooncake -I/usr/include/ub/umdk/urma -I/usr/include/ub/umdk/urma/udma"
export CGO_LDFLAGS="${CGO_LDFLAGS:-} -L/usr/lib64/urma -L/usr/lib64 -lmooncake_store -lmooncake_common -lstdc++ -lnuma -lglog -lgflags -libverbs -ljsoncpp -lzstd -lcurl -luring -lurma -letcd_wrapper -lubdiag"

if [[ -d /usr/local/cuda/lib64 ]]; then
  export CGO_LDFLAGS="$CGO_LDFLAGS -L/usr/local/cuda/lib64 -lcudart"
fi

mkdir -p bin
go build -buildvcs=false -trimpath -tags mooncake \
  -o bin/template-migrate-mooncake ./cmd/template-migrate

sha256sum bin/template-migrate-mooncake
