#!/usr/bin/env bash
# 构建启用 mooncake:// 导入支持的 template-migrate。Mooncake 客户端
# 是 CGO 绑定,必须在已安装 Mooncake 动态库的 Linux/ARM64 主机上原生构建,不支持
# 交叉编译。
#
# 依赖:
#   - 动态库 libmooncake_store.so、libmooncake_common.so(默认 /usr/lib64);
#     二者的传递依赖(transfer_engine、glog、urma 等)由动态
#     链接器按 DT_NEEDED 解析,不需要出现在链接命令行上。
#   - 头文件 store_c.h(Mooncake Go SDK 唯一引用的 C 头文件)。若安装的软件包未提供
#     头文件,用 MOONCAKE_INCLUDE_DIR 指向与动态库版本匹配的 Mooncake 源码树的
#     mooncake-store/include。
#
# 可选环境变量:
#   MOONCAKE_INCLUDE_DIR  store_c.h 所在目录,默认 /usr/include/mooncake
#   MOONCAKE_LIB_DIR      Mooncake 动态库所在目录,默认 /usr/lib64
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${ROOT_DIR}"

# migrate-tool 是独立 Go 模块,不在仓库根 go.work 的 use 列表里(与 cri-multiplex 一致)。
export GOWORK=off

platform="$(go env GOOS)/$(go env GOARCH)"
if [[ "$platform" != "linux/arm64" ]]; then
  printf 'build.sh must run on linux/arm64 with Mooncake libraries installed, got %s\n' "$platform" >&2
  exit 1
fi

include_dir="${MOONCAKE_INCLUDE_DIR:-/usr/include/mooncake}"
lib_dir="${MOONCAKE_LIB_DIR:-/usr/lib64}"
if [[ ! -f "${include_dir}/store_c.h" ]]; then
  printf 'store_c.h not found in %s; set MOONCAKE_INCLUDE_DIR to the directory containing it (e.g. <Mooncake source>/mooncake-store/include)\n' "${include_dir}" >&2
  exit 1
fi
if [[ ! -e "${lib_dir}/libmooncake_store.so" || ! -e "${lib_dir}/libmooncake_common.so" ]]; then
  printf 'libmooncake_store.so / libmooncake_common.so not found in %s; set MOONCAKE_LIB_DIR\n' "${lib_dir}" >&2
  exit 1
fi

export CGO_ENABLED=1
export CGO_CFLAGS="${CGO_CFLAGS:-} -I${include_dir}"
export CGO_LDFLAGS="${CGO_LDFLAGS:-} -L${lib_dir} -lmooncake_store -lmooncake_common"

OUTPUT="${ROOT_DIR}/bin/template-migrate"
mkdir -p "${ROOT_DIR}/bin"

go build -buildvcs=false -trimpath -tags mooncake -o "${OUTPUT}" ./cmd/template-migrate

sha256sum "${OUTPUT}"
"${OUTPUT}" version
