#!/usr/bin/env bash
# 构建 template-migrate 唯一发布制品(源端 PostgreSQL + S3/MinIO,目标端
# PostgreSQL + Mooncake)。Mooncake 客户端是 CGO 绑定,必须在已安装 Mooncake
# 头文件与动态库的 Linux/ARM64 主机上原生构建,不支持交叉编译。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${ROOT_DIR}"

# migrate-tool 是独立 Go 模块，不在仓库根 go.work 的 use 列表里（与 cri-multiplex 一致）。
export GOWORK=off

platform="$(go env GOOS)/$(go env GOARCH)"
if [[ "$platform" != "linux/arm64" ]]; then
  printf 'build.sh must run on linux/arm64 with Mooncake libraries installed, got %s\n' "$platform" >&2
  exit 1
fi

# 头文件与链接库路径来自目标环境 E2B build.sh 的 Mooncake 构建配置。
export CGO_ENABLED=1
export CGO_CFLAGS="${CGO_CFLAGS:-} -I/usr/include -I/usr/include/mooncake -I/usr/include/ub/umdk/urma -I/usr/include/ub/umdk/urma/udma"
export CGO_LDFLAGS="${CGO_LDFLAGS:-} -L/usr/lib64/urma -L/usr/lib64 -lmooncake_store -lmooncake_common -lstdc++ -lnuma -lglog -lgflags -libverbs -ljsoncpp -lzstd -lcurl -luring -lurma -letcd_wrapper -lubdiag"

if [[ -d /usr/local/cuda/lib64 ]]; then
  export CGO_LDFLAGS="$CGO_LDFLAGS -L/usr/local/cuda/lib64 -lcudart"
fi

OUTPUT="${ROOT_DIR}/bin/template-migrate"
mkdir -p "${ROOT_DIR}/bin"

go build -buildvcs=false -trimpath -tags mooncake -o "${OUTPUT}" ./cmd/template-migrate

sha256sum "${OUTPUT}"
"${OUTPUT}" version
