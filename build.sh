#!/bin/bash

# 设置错误时退出
set -e

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# 当前脚本目录
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

# ---------------------------------------------------------------------------
# 解析命令行参数
# ---------------------------------------------------------------------------
ENABLE_MOONCAKE=false
BUILD_TAGS=""

show_usage() {
    echo "用法: $0 [--mooncake]"
    echo ""
    echo "选项:"
    echo "  --mooncake, -m    启用 Mooncake 存储后端编译"
    echo "                     (传递 -tags mooncake + 链接 Mooncake CGo 库)"
    echo "  --help, -h        显示此帮助信息"
    exit 0
}

for arg in "$@"; do
    case "$arg" in
        --mooncake|-m)
            ENABLE_MOONCAKE=true
            ;;
        --help|-h)
            show_usage
            ;;
        *)
            echo -e "${RED}未知参数: $arg${NC}"
            show_usage
            ;;
    esac
done

# ---------------------------------------------------------------------------
# Mooncake 编译标志 (仅在 --mooncake 时启用)
# ---------------------------------------------------------------------------
if $ENABLE_MOONCAKE; then
    echo -e "${GREEN}=== Mooncake Store 依赖准备 ===${NC}"

    BUILD_TAGS="-tags mooncake"

    # CGo 编译器标志 — 指向 Mooncake 头文件
    CGO_CFLAGS="-I/usr/include"
    CGO_CFLAGS+=" -I/usr/include/mooncake"
    CGO_CFLAGS+=" -I/usr/include/ub/umdk/urma"
    CGO_CFLAGS+=" -I/usr/include/ub/umdk/urma/udma"

    # CGo 链接器标志 — Mooncake 库
    CGO_LDFLAGS=""
    CGO_LDFLAGS+=" -L/usr/lib64/urma"
    CGO_LDFLAGS+=" -L/usr/lib64"
    CGO_LDFLAGS+=" -lmooncake_store -lmooncake_common"
    CGO_LDFLAGS+=" -lstdc++ -lnuma -lglog -lgflags -libverbs -ljsoncpp -lzstd -lcurl -luring"
    CGO_LDFLAGS+=" -lurma -letcd_wrapper -lubdiag"

    # 可选：CUDA 支持
    if [ -d "/usr/local/cuda/lib64" ]; then
        CGO_LDFLAGS+=" -L/usr/local/cuda/lib64 -lcudart"
    fi

    export CGO_CFLAGS="${CGO_CFLAGS}"
    export CGO_LDFLAGS="${CGO_LDFLAGS}"
    echo -e "${GREEN}=== CGO 环境变量 ===${NC}"
    echo -e "CGO_CFLAGS=${CGO_CFLAGS}"
    echo -e "CGO_LDFLAGS=${CGO_LDFLAGS}"
    echo -e "BUILD_TAGS=${BUILD_TAGS}"
else
    echo -e "${CYAN}=== 标准构建（无 Mooncake） ===${NC}"
    echo -e "${CYAN}提示: 使用 --mooncake 启用 Mooncake 存储后端${NC}"
    # 非 mooncake 模式仍需 CGO (orchestrator 的 userfaultfd 包依赖 Linux 内核头文件)
fi
export CGO_ENABLED=1
echo -e "\n${GREEN}=== Go Modules 整理和依赖处理 ===${NC}"

# 定义需要执行 go mod tidy 和 go mod vendor 的目录
TIDY_DIRS=(
    "packages/shared"
    "packages/api"
    "packages/client-proxy"
    "packages/envd"
    "packages/orchestrator"
    "packages/db"
)

# 非 mooncake 模式下，go mod tidy 会因 //go:build mooncake 标签文件
# 仍然把 mooncakestore 依赖加入 go.mod（tidy 视所有 build tag 为启用）。
# 由于 orchestrator/api 等通过 replace => ../shared 引用本地 shared 模块，
# tidy 这些模块时也会扫描 shared 源码，因此需在整个 tidy 循环期间移走
# mooncake 源文件，循环结束后统一恢复。
MOONCAKE_SRC="packages/shared/pkg/storage/storage_mooncake.go"
MOONCAKE_MOVED=false

# 恢复 mooncake 源文件的清理函数
restore_mooncake() {
    if $MOONCAKE_MOVED && [ -f "${MOONCAKE_SRC}.bak" ]; then
        mv "${MOONCAKE_SRC}.bak" "$MOONCAKE_SRC"
        echo -e "${YELLOW}→ 已恢复 mooncake 源文件${NC}"
    fi
}

if ! $ENABLE_MOONCAKE && [ -f "$MOONCAKE_SRC" ]; then
    mv "$MOONCAKE_SRC" "${MOONCAKE_SRC}.bak"
    MOONCAKE_MOVED=true
    # 注册 trap：脚本退出时（含 set -e 触发的异常退出）恢复 mooncake 源文件
    trap restore_mooncake EXIT
fi

# 第一步：执行 go mod tidy 和 go mod vendor
for dir in "${TIDY_DIRS[@]}"; do
    if [ -d "$dir" ]; then
        echo -e "${YELLOW}→ 进入 $dir 执行 go mod tidy 和 go mod vendor...${NC}"
        (
            cd "$dir"
            GOWORK="off" go mod tidy
            GOWORK="off" go mod vendor
        )
        echo -e "${GREEN}✓ $dir 完成${NC}"
    else
        echo -e "${RED}✗ 目录 $dir 不存在，跳过${NC}"
    fi
done

# 恢复 mooncake 源文件（trap 也会兜底，此处显式调用并解除 trap）
restore_mooncake
trap - EXIT

# 根据主机架构确定输出目录
GOARCH=${GOARCH:-$(go env GOARCH 2>/dev/null || uname -m)}
# 规范化架构名 (uname -m 输出 x86_64 -> amd64, aarch64 -> arm64)
case "$GOARCH" in
    x86_64) GOARCH=amd64 ;;
    aarch64) GOARCH=arm64 ;;
esac
BIN_DIR="bin/${GOARCH}"

echo -e "\n${GREEN}=== 构建二进制文件 ${BUILD_TAGS:+($BUILD_TAGS)} [${GOARCH}]===${NC}"

# 创建输出目录
mkdir -p "$BIN_DIR"

# 定义构建任务（目录、输出路径、构建参数）
declare -A BUILD_TASKS=(
    ["packages/api"]="../../${BIN_DIR}/api ."
    ["packages/client-proxy"]="../../${BIN_DIR}/client-proxy ."
    ["packages/envd"]="../../${BIN_DIR}/envd ./main.go"
    ["packages/orchestrator"]="../../${BIN_DIR}/orchestrator ."
)

# 执行常规构建
for dir in "${!BUILD_TASKS[@]}"; do
    if [ -d "$dir" ]; then
        # 解析构建参数
        read -r output args <<< "${BUILD_TASKS[$dir]}"
        echo -e "${YELLOW}→ 构建 $dir -> $output${NC}"
        (
            cd "$dir"
            go build $BUILD_TAGS -o "$output" $args
        )
        echo -e "${GREEN}✓ $output 构建完成${NC}"
    else
        echo -e "${RED}✗ 目录 $dir 不存在，跳过${NC}"
    fi
done

# 处理 packages/db 的特殊构建（两个二进制文件）
DB_DIR="packages/db"
if [ -d "$DB_DIR" ]; then
    echo -e "${YELLOW}→ 构建 $DB_DIR 的 migrator...${NC}"
    (
        cd "$DB_DIR"
       go build $BUILD_TAGS -o ../../${BIN_DIR}/migrator ./scripts/migrator.go
    )
    echo -e "${GREEN}✓ ${BIN_DIR}/migrator 构建完成${NC}"

    echo -e "${YELLOW}→ 构建 $DB_DIR 的 seed-db...${NC}"
    (
        cd "$DB_DIR"
        go build $BUILD_TAGS -o ../../${BIN_DIR}/seed-db ./scripts/seed/postgres/seed-db.go
    )
    echo -e "${GREEN}✓ ${BIN_DIR}/seed-db 构建完成${NC}"
else
    echo -e "${RED}✗ 目录 $DB_DIR 不存在${NC}"
fi

# 处理 packages/orchestrator 的 fc-netns-exec 构建（不需要 CGO）
ORCHESTRATOR_DIR="packages/orchestrator"
if [ -d "$ORCHESTRATOR_DIR/cmd/fc-netns-exec" ]; then
    echo -e "${YELLOW}→ 构建 $ORCHESTRATOR_DIR 的 fc-netns-exec...${NC}"
    (
        cd "$ORCHESTRATOR_DIR"
        CGO_ENABLED=0 GOOS=linux go build -o ../../${BIN_DIR}/fc-netns-exec ./cmd/fc-netns-exec
    )
    echo -e "${GREEN}✓ ${BIN_DIR}/fc-netns-exec 构建完成${NC}"
else
    echo -e "${RED}✗ 目录 $ORCHESTRATOR_DIR/cmd/fc-netns-exec 不存在${NC}"
fi

echo -e "\n${GREEN}=== 所有任务执行完毕 ===${NC}"
echo -e "${GREEN}生成的二进制文件位于: ${BIN_DIR}/${NC}"
ls -lh "${BIN_DIR}/" 2>/dev/null || echo "目录为空或不存在"
