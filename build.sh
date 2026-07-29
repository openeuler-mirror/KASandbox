#!/bin/bash

# 设置错误时退出
set -e

# ============================================================================
# 常量定义
# ============================================================================

# 颜色定义
readonly RED='\033[0;31m'
readonly GREEN='\033[0;32m'
readonly YELLOW='\033[1;33m'
readonly CYAN='\033[0;36m'
readonly NC='\033[0m'

# 脚本目录和时间戳
readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly START_TIME=$(date +%s)

cd "${SCRIPT_DIR}"

# ============================================================================
# 通用函数库
# ============================================================================

# 日志函数
log_info() {
    echo -e "${GREEN}[$(date +'%H:%M:%S')]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[$(date +'%H:%M:%S')]${NC} $*"
}

log_error() {
    echo -e "${RED}[$(date +'%H:%M:%S')]${NC} $*" >&2
}

log_section() {
    echo -e "\n${GREEN}=== $* ===${NC}"
}

# 检查目录是否存在
check_dir() {
    local dir=$1
    if [ ! -d "$dir" ]; then
        log_error "✗ 目录不存在: $dir"
        return 1
    fi
    return 0
}

# 检查命令是否存在
check_command() {
    local cmd=$1
    if ! command -v "$cmd" >/dev/null 2>&1; then
        log_error "✗ 命令不存在: $cmd"
        return 1
    fi
}

# 执行命令并处理错误
run_cmd() {
    local msg=$1
    shift
    log_warn "→ $msg"
    "$@" || { log_error "✗ 命令失败: $msg"; return 1; }
}

# 计算耗时
print_elapsed_time() {
    local end_time=$(date +%s)
    local elapsed=$((end_time - START_TIME))
    local mins=$((elapsed / 60))
    local secs=$((elapsed % 60))
    log_info "总耗时: ${mins}m${secs}s"
}

# ============================================================================
# 配置变量
# ============================================================================

ENABLE_MOONCAKE=false
ENABLE_FIRECRACKER=false
ENABLE_CRI_MUX=false
ENABLE_IMAGES=false
ENABLE_PACKAGE=false
ENABLE_PACKAGE_SLIM=false
PACKAGE_OUTPUT_NAME=""
BUILD_TAGS=""

# 镜像构建配置
readonly API_IMAGE_NAME="api"
readonly ORCHESTRATOR_IMAGE_NAME="orchestrator"
readonly CLIENT_PROXY_IMAGE_NAME="client-proxy"
readonly WEBHOOK_IMAGE_NAME="e2b-webhook"

# ============================================================================
# 命令行参数解析
# ============================================================================

show_usage() {
    cat << 'EOF'
用法: ./build.sh [选项]

选项:
  -m, --mooncake         启用 Mooncake 存储后端编译 (包含 CGo 链接)
  -f, --firecracker      构建 Firecracker 微虚拟机 (需要 Docker)
  -c, --cri-multiplex    构建 CRI 多路复用器
  -i, --images           构建 Docker 镜像 (api/orchestrator/webhook/client-proxy)
  -p, --package          打包源码 (包含所有源代码，15MB)
  --package-slim         打包最小化构建包 (仅构建所需文件，< 5MB)
  -n, --package-name NAME 指定打包输出文件名 (不含扩展名，自动追加 .tar.*)
  -h, --help             显示此帮助信息

示例:
  ./build.sh                              # 标准构建
  ./build.sh -m -i                        # 使用 Mooncake + 构建镜像
  ./build.sh -f                           # 构建 Firecracker
  ./build.sh -c -p                        # 构建 CRI 多路复用 + 打包源码
  ./build.sh --package-slim               # 打包最小化构建包（推荐）
  ./build.sh --package-slim -n my-release # 指定输出名为 my-release.tar.xz
EOF
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -m|--mooncake)      ENABLE_MOONCAKE=true ;;
        -f|--firecracker)   ENABLE_FIRECRACKER=true ;;
        -c|--cri-multiplex) ENABLE_CRI_MUX=true ;;
        -i|--images)        ENABLE_IMAGES=true ;;
        -p|--package)       ENABLE_PACKAGE=true ;;
        --package-slim)     ENABLE_PACKAGE_SLIM=true ;;
        -n|--package-name)
            if [[ $# -lt 2 ]]; then
                log_error "--package-name 需要指定文件名"
                exit 1
            fi
            PACKAGE_OUTPUT_NAME="$2"
            shift
            ;;
        -h|--help)          show_usage ;;
        *)
            log_error "未知参数: $1"
            show_usage
            ;;
    esac
    shift
done

# ============================================================================
# Mooncake 编译配置
# ============================================================================

setup_mooncake() {
    log_section "Mooncake Store 依赖准备"
    
    BUILD_TAGS="-tags mooncake"
    
    # 构建 CGo 编译器标志
    local cgo_cflags=(
        "-I/usr/include"
        "-I/usr/include/mooncake"
        "-I/usr/include/ub/umdk/urma"
        "-I/usr/include/ub/umdk/urma/udma"
    )
    
    # 构建 CGo 链接器标志
    local cgo_ldflags=(
        "-L/usr/lib64/urma"
        "-L/usr/lib64"
        "-lmooncake_store -lmooncake_common"
        "-lstdc++ -lnuma -lglog -lgflags -libverbs -ljsoncpp -lzstd -lcurl -luring"
        "-lurma -letcd_wrapper -lubdiag"
    )
    
    # 可选：CUDA 支持
    if [ -d "/usr/local/cuda/lib64" ]; then
        cgo_ldflags+=("-L/usr/local/cuda/lib64 -lcudart")
    fi
    
    export CGO_CFLAGS="${cgo_cflags[*]}"
    export CGO_LDFLAGS="${cgo_ldflags[*]}"
    export BUILD_TAGS="$BUILD_TAGS"
    
    log_info "CGO_CFLAGS=${CGO_CFLAGS}"
    log_info "CGO_LDFLAGS=${CGO_LDFLAGS}"
    log_info "BUILD_TAGS=${BUILD_TAGS}"
}

setup_standard_build() {
    log_section "标准构建（无 Mooncake）"
    log_warn "提示: 使用 -m 启用 Mooncake，-f 构建 Firecracker，-c 构建 CRI 多路复用器，-i 构建 Docker 镜像"
}

if $ENABLE_MOONCAKE; then
    setup_mooncake
else
    setup_standard_build
fi

export CGO_ENABLED=1

# ============================================================================
# Go 模块依赖整理
# ============================================================================

setup_go_dependencies() {
    log_section "Go Modules 整理和依赖处理"
    
    local tidy_dirs=(
        "packages/shared"
        "packages/api"
        "packages/client-proxy"
        "packages/envd"
        "packages/orchestrator"
        "packages/db"
        "e2b-webhook"
    )
    
    local mooncake_src="packages/shared/pkg/storage/storage_mooncake.go"
    
    # 非 mooncake 模式下，临时移除 mooncake 源文件避免 tidy 将其依赖加入
    if ! $ENABLE_MOONCAKE && [ -f "$mooncake_src" ]; then
        log_warn "移除临时 mooncake 源文件..."
        mv "$mooncake_src" "${mooncake_src}.bak"
        # 注册清理函数
        trap "restore_mooncake_src '$mooncake_src'" EXIT
    fi
    
    # 执行 go mod tidy 和 vendor（并行执行）
    local dir
    local pids=()
    
    for dir in "${tidy_dirs[@]}"; do
        if ! check_dir "$dir"; then
            continue
        fi
        
        log_warn "→ 进入 $dir 执行 go mod tidy 和 go mod vendor..."
        (
            cd "$dir"
            GOWORK="off" go mod tidy
            GOWORK="off" go mod vendor
            log_info "✓ $dir 完成"
        ) &
        pids+=($!)
    done
    
    # 等待所有后台任务完成
    local pid
    for pid in "${pids[@]}"; do
        wait "$pid" || log_error "进程 $pid 执行失败"
    done
    
    # 恢复 mooncake 源文件
    restore_mooncake_src "$mooncake_src"
    trap - EXIT
}

restore_mooncake_src() {
    local src=$1
    local backup="${src}.bak"
    if [ -f "$backup" ]; then
        mv "$backup" "$src"
        log_warn "已恢复 mooncake 源文件"
    fi
}

# ============================================================================
# 架构和输出目录设置
# ============================================================================

detect_architecture() {
    local arch=${GOARCH:-$(go env GOARCH 2>/dev/null || uname -m)}
    # 规范化架构名
    case "$arch" in
        x86_64)  echo "amd64" ;;
        aarch64) echo "arm64" ;;
        *)       echo "$arch" ;;
    esac
}

GOARCH=$(detect_architecture)
readonly BIN_DIR="bin/${GOARCH}"

# ============================================================================
# 二进制构建
# ============================================================================

# 构建单个 Go 模块
build_go_module() {
    local module_dir=$1
    local output=$2
    local build_args=${3:-.}
    
    if ! check_dir "$module_dir"; then
        return 1
    fi
    
    log_warn "→ 构建 $module_dir -> $output"
    (
        cd "$module_dir"
        # 使用 GOWORK=off：部分模块（如 e2b-webhook）不在 go.work 中，
        # 必须脱离 workspace 才能按各自 go.mod 构建
        GOWORK=off go build $BUILD_TAGS -o "$output" $build_args
    )
    log_info "✓ $output 构建完成"
}

# 构建主要二进制文件（并行）
build_main_binaries() {
    local tasks=(
        "packages/api:../../${BIN_DIR}/api:."
        "packages/client-proxy:../../${BIN_DIR}/client-proxy:."
        "packages/envd:../../${BIN_DIR}/envd:./main.go"
        "packages/orchestrator:../../${BIN_DIR}/orchestrator:."
        "e2b-webhook:../${BIN_DIR}/e2b-webhook:."
    )
    
    local pids=()
    local task
    for task in "${tasks[@]}"; do
        IFS=':' read -r dir output args <<< "$task"
        (
            build_go_module "$dir" "$output" "$args"
        ) &
        pids+=($!)
    done
    
    # 等待所有构建完成
    local pid
    for pid in "${pids[@]}"; do
        wait "$pid" || log_error "构建进程 $pid 失败"
    done
}

# 构建数据库相关工具（并行）
build_db_tools() {
    local db_dir="packages/db"
    
    if ! check_dir "$db_dir"; then
        return
    fi
    
    local pids=()
    
    # migrator 后台构建
    (
        log_warn "→ 构建 $db_dir 的 migrator"
        cd "$db_dir"
        go build $BUILD_TAGS -o "../../${BIN_DIR}/migrator" ./scripts/migrator.go
        log_info "✓ ${BIN_DIR}/migrator 构建完成"
    ) &
    pids+=($!)
    
    # seed-db 后台构建
    (
        log_warn "→ 构建 $db_dir 的 seed-db"
        cd "$db_dir"
        go build $BUILD_TAGS -o "../../${BIN_DIR}/seed-db" ./scripts/seed/postgres/seed-db.go
        log_info "✓ ${BIN_DIR}/seed-db 构建完成"
    ) &
    pids+=($!)
    
    # 等待两个构建完成
    local pid
    for pid in "${pids[@]}"; do
        wait "$pid" || log_error "DB 工具构建进程 $pid 失败"
    done
}

# 构建 fc-netns-exec（不需要 CGO）
build_fc_netns_exec() {
    local orch_dir="packages/orchestrator"
    local exec_dir="$orch_dir/cmd/fc-netns-exec"
    
    if [ ! -d "$exec_dir" ]; then
        log_error "✗ 目录不存在: $exec_dir"
        return
    fi
    
    log_warn "→ 构建 $orch_dir 的 fc-netns-exec"
    (
        cd "$orch_dir"
        CGO_ENABLED=0 GOOS=linux go build -o "../../${BIN_DIR}/fc-netns-exec" ./cmd/fc-netns-exec
    )
    log_info "✓ ${BIN_DIR}/fc-netns-exec 构建完成"
}

# 构建 CRI 多路复用器
build_cri_multiplex() {
    if ! $ENABLE_CRI_MUX; then
        return
    fi
    
    local cri_dir="cri-multiplex"
    
    if ! check_dir "$cri_dir"; then
        return
    fi
    
    log_warn "→ 整理 $cri_dir 依赖 (go mod tidy/vendor)"
    (
        cd "$cri_dir"
        GOWORK=off go mod tidy
        GOWORK=off go mod vendor
    )
    
    log_warn "→ 构建 $cri_dir"
    (
        cd "$cri_dir"
        GOWORK=off go build -o "../${BIN_DIR}/cri-multiplex" ./cmd/cri-multiplex
    )
    log_info "✓ ${BIN_DIR}/cri-multiplex 构建完成"
}

# ============================================================================
# Docker 镜像构建
# ============================================================================

build_docker_images() {
    if ! $ENABLE_IMAGES; then
        return
    fi
    
    log_section "构建 Docker 镜像"
    
    if ! check_command "docker"; then
        log_error "镜像构建需要 Docker"
        return
    fi
    
    # webhook 二进制未在主循环中构建，单独处理
    ensure_webhook_binary
    
    # 定义镜像构建任务：镜像名|Dockerfile|构建上下文|需要的二进制
    local image_tasks=(
        "${API_IMAGE_NAME}|packages/api/Dockerfile|packages/api|api"
        "${ORCHESTRATOR_IMAGE_NAME}|packages/orchestrator/Dockerfile|packages/orchestrator|orchestrator,fc-netns-exec"
        "${CLIENT_PROXY_IMAGE_NAME}|packages/client-proxy/Dockerfile|packages/client-proxy|client-proxy"
        "${WEBHOOK_IMAGE_NAME}|e2b-webhook/Dockerfile.scratch|e2b-webhook|e2b-webhook"
    )
    
    local pids=()
    local task
    for task in "${image_tasks[@]}"; do
        (
            build_docker_image "$task"
        ) &
        pids+=($!)
    done
    
    # 等待所有镜像构建完成
    local pid
    for pid in "${pids[@]}"; do
        wait "$pid" || log_error "镜像构建进程 $pid 失败"
    done
    
    # 显示已构建的镜像
    echo -e "${CYAN}已构建镜像:${NC}"
    docker images --format "  {{.Repository}}:{{.Tag}}  ({{.Size}})" \
        | grep -E "(${API_IMAGE_NAME}|${ORCHESTRATOR_IMAGE_NAME}|${CLIENT_PROXY_IMAGE_NAME}|${WEBHOOK_IMAGE_NAME}) " || true
}

ensure_webhook_binary() {
    local webhook_dir="e2b-webhook"
    local webhook_bin="${BIN_DIR}/e2b-webhook"
    
    if [ -f "$webhook_bin" ]; then
        return
    fi
    
    if ! check_dir "$webhook_dir"; then
        return
    fi
    
    log_warn "→ 构建 $webhook_dir 二进制"
    (
        cd "$webhook_dir"
        GOWORK=off go mod tidy
        GOWORK=off CGO_ENABLED=0 GOOS=linux go build -o "../${webhook_bin}" .
    )
    log_info "✓ $webhook_bin 构建完成"
}

build_docker_image() {
    local task=$1
    IFS='|' read -r img_name dockerfile ctx binaries <<< "$task"
    
    log_warn "→ 构建镜像 $img_name"
    
    # 校验所需二进制文件
    if ! validate_binaries "$binaries"; then
        log_error "✗ 缺少必要二进制文件，跳过 $img_name"
        return 1
    fi
    
    # 拷贝二进制到构建上下文
    copy_binaries_to_context "$ctx" "$binaries"
    
    # 执行 Docker 构建
    docker build --platform "linux/${GOARCH}" \
        -f "$dockerfile" \
        -t "$img_name" \
        "$ctx"
    
    # 清理拷贝的二进制
    cleanup_binaries_from_context "$ctx" "$binaries"
    
    log_info "✓ 镜像 $img_name 构建完成"
}

validate_binaries() {
    local binaries=$1
    local bin
    IFS=',' read -ra bin_arr <<< "$binaries"
    
    for bin in "${bin_arr[@]}"; do
        if [ ! -f "${BIN_DIR}/$bin" ]; then
            log_error "缺少二进制: ${BIN_DIR}/$bin"
            return 1
        fi
    done
}

copy_binaries_to_context() {
    local ctx=$1
    local binaries=$2
    local bin
    IFS=',' read -ra bin_arr <<< "$binaries"
    
    for bin in "${bin_arr[@]}"; do
        cp -f "${BIN_DIR}/$bin" "$ctx/$bin"
    done
}

cleanup_binaries_from_context() {
    local ctx=$1
    local binaries=$2
    local bin
    IFS=',' read -ra bin_arr <<< "$binaries"
    
    for bin in "${bin_arr[@]}"; do
        rm -f "$ctx/$bin"
    done
}

# ============================================================================
# Firecracker 构建
# ============================================================================

build_firecracker() {
    if ! $ENABLE_FIRECRACKER; then
        return
    fi
    
    local fc_dir="firecracker"
    
    log_section "构建 Firecracker ${BUILD_TAGS:+($BUILD_TAGS)}"
    
    if ! check_dir "$fc_dir"; then
        return
    fi
    
    if ! check_command "docker"; then
        log_error "Firecracker 构建需要 Docker"
        return
    fi
    
    log_warn "→ 通过 devtool 构建 Firecracker (release)"
    (
        cd "$fc_dir"
        ./scripts/build.sh
    )
    log_info "✓ Firecracker 构建完成"
}
# ============================================================================
# 源码打包
# ============================================================================

# 检查压缩工具可用性
detect_compression() {
    # 优先使用 xz（最高压缩率），其次 bzip2，最后 gzip
    if command -v xz >/dev/null 2>&1; then
        echo "xz"
    elif command -v bzip2 >/dev/null 2>&1; then
        echo "bzip2"
    else
        echo "gzip"
    fi
}

package_source_code() {
    if ! $ENABLE_PACKAGE; then
        return
    fi
    
    log_section "打包源码"
    
    local project_version=$(cat VERSION 2>/dev/null || echo "latest")
    local timestamp=$(date +%Y%m%d_%H%M%S)
    local compression=$(detect_compression)
    
    # 根据压缩方式设置文件扩展名和 tar 选项
    local compress_flag=""
    local compress_ext=""
    case "$compression" in
        xz)
            compress_flag="-J"
            compress_ext=".tar.xz"
            ;;
        bzip2)
            compress_flag="-j"
            compress_ext=".tar.bz2"
            ;;
        *)
            compress_flag="-z"
            compress_ext=".tar.gz"
            ;;
    esac
    
    local package_name package_dir_name
    if [ -n "$PACKAGE_OUTPUT_NAME" ]; then
        package_name="${PACKAGE_OUTPUT_NAME}${compress_ext}"
        package_dir_name="${PACKAGE_OUTPUT_NAME}"
    else
        package_name="KASandbox-${project_version}-${timestamp}${compress_ext}"
        package_dir_name="KASandbox-${project_version}"
    fi
    
    log_warn "→ 创建源码压缩包: $package_name"
    log_warn "  压缩算法: $compression"
    log_warn "  排除项: vendor/、.cache/、.local/、生成文件等"
    
    # 从父目录执行 tar，避免打包时新生成的文件
    (
        cd "$(dirname "$SCRIPT_DIR")"
        tar $compress_flag \
            --exclude=doc \
            --exclude=tests \
            --exclude=firecracker \
            --exclude=.git \
            --exclude=bin \
            --exclude=.opencode \
            --exclude=node_modules \
            --exclude='*/vendor' \
            --exclude='*/.cache' \
            --exclude='*/.local' \
            --exclude='*/.pytest_cache' \
            --exclude='*.pb.go' \
            --exclude='*_gen.go' \
            --exclude='*.o' \
            --exclude='*.a' \
            --exclude=.DS_Store \
            --exclude='*.log' \
            --exclude=.env \
            --exclude=.env.* \
            --exclude='KASandbox-*.tar.*' \
            --warning=no-file-changed \
            --transform "s,^KASandbox,$package_dir_name," \
            -cf "$SCRIPT_DIR/$package_name" \
            "$(basename "$SCRIPT_DIR")"
    )
    
    if [ -f "$SCRIPT_DIR/$package_name" ]; then
        local package_size=$(du -h "$SCRIPT_DIR/$package_name" | cut -f1)
        log_info "✓ 源码打包完成"
        log_info "  文件名: $package_name"
        log_info "  大小: $package_size"
        log_info "  位置: $SCRIPT_DIR/$package_name"
        log_info "  压缩算法: $compression"
    else
        log_error "✗ 源码打包失败"
        return 1
    fi
}

# ============================================================================
# 最小化打包（仅构建所需文件）
# ============================================================================

package_source_code_slim() {
    if ! $ENABLE_PACKAGE_SLIM; then
        return
    fi
    
    log_section "打包最小化构建包"
    
    local project_version=$(cat VERSION 2>/dev/null || echo "latest")
    local timestamp=$(date +%Y%m%d_%H%M%S)
    local compression=$(detect_compression)
    
    # 根据压缩方式设置文件扩展名和 tar 选项
    local compress_flag=""
    local compress_ext=""
    case "$compression" in
        xz)
            compress_flag="-J"
            compress_ext=".tar.xz"
            ;;
        bzip2)
            compress_flag="-j"
            compress_ext=".tar.bz2"
            ;;
        *)
            compress_flag="-z"
            compress_ext=".tar.gz"
            ;;
    esac
    
    local package_name package_dir_name
    local repo_dir="$(basename "$SCRIPT_DIR")"
    if [ -n "$PACKAGE_OUTPUT_NAME" ]; then
        package_name="${PACKAGE_OUTPUT_NAME}${compress_ext}"
        package_dir_name="${PACKAGE_OUTPUT_NAME}"
    else
        package_name="KASandbox-build-${project_version}-${timestamp}${compress_ext}"
        package_dir_name="KASandbox-build-${project_version}"
    fi
    
    log_warn "→ 创建最小化构建包: $package_name"
    log_warn "  压缩算法: $compression"
    log_warn "  包含: build.sh、cri-multiplex、e2b-webhook、deploy、packages/{api,client-proxy,envd,orchestrator,shared}"
    
    # 显式指定打包内容（而非排除法），仅包含构建与部署所需文件
    # 同时排除各目录内的 vendor、缓存、生成文件等
    (
        cd "$(dirname "$SCRIPT_DIR")"
        tar $compress_flag \
            --exclude='*/vendor' \
            --exclude='*/.cache' \
            --exclude='*/.local' \
            --exclude='*/.pytest_cache' \
            --exclude='*.o' \
            --exclude='*.a' \
            --exclude=.DS_Store \
            --exclude='*.log' \
            --exclude=.env \
            --exclude=.env.* \
            --exclude='KASandbox-*.tar.*' \
            --exclude='*/bin' \
            --warning=no-file-changed \
            --transform "s,^$repo_dir,$package_dir_name," \
            -cf "$SCRIPT_DIR/$package_name" \
            "$repo_dir/build.sh" \
            "$repo_dir/cri-multiplex" \
            "$repo_dir/e2b-webhook" \
            "$repo_dir/deploy" \
            "$repo_dir/packages/api" \
            "$repo_dir/packages/client-proxy" \
            "$repo_dir/packages/envd" \
            "$repo_dir/packages/orchestrator" \
            "$repo_dir/packages/db" \
            "$repo_dir/packages/auth" \
            "$repo_dir/packages/clickhouse" \
            "$repo_dir/packages/shared"
    )
    
    if [ -f "$SCRIPT_DIR/$package_name" ]; then
        local package_size=$(du -h "$SCRIPT_DIR/$package_name" | cut -f1)
        log_info "✓ 最小化打包完成"
        log_info "  文件名: $package_name"
        log_info "  大小: $package_size"
        log_info "  位置: $SCRIPT_DIR/$package_name"
        log_info "  压缩算法: $compression"
        log_info "  用途: 构建与部署用源码包"
    else
        log_error "✗ 最小化打包失败"
        return 1
    fi
}

# ============================================================================
# 主执行逻辑：各命令独立并行
# ============================================================================

# 执行选项对应的任务（并行）
run_tasks() {
    local pids=()
    
    # -i 选项：仅构建 Docker 镜像（不构建二进制）
    if $ENABLE_IMAGES; then
        (
            log_section "Docker 镜像构建流程"
            build_docker_images
        ) &
        pids+=($!)
    fi

    # -c 选项：仅构建 CRI 多路复用器
    if $ENABLE_CRI_MUX; then
        (
            log_section "CRI 多路复用器构建流程"
            build_cri_multiplex
        ) &
        pids+=($!)
    fi

    # -f 选项：仅构建 Firecracker
    if $ENABLE_FIRECRACKER; then
        (
            log_section "Firecracker 构建流程"
            build_firecracker
        ) &
        pids+=($!)
    fi

    # -p 选项：仅打包源码
    if $ENABLE_PACKAGE; then
        (
            log_section "源码打包流程"
            package_source_code
        ) &
        pids+=($!)
    fi

    # --package-slim 选项：仅打包最小化构建包
    if $ENABLE_PACKAGE_SLIM; then
        (
            log_section "最小化打包流程"
            package_source_code_slim
        ) &
        pids+=($!)
    fi

    # 等待所有后台任务完成
    if [ ${#pids[@]} -gt 0 ]; then
        local pid
        for pid in "${pids[@]}"; do
            if wait "$pid"; then
                : # 成功
            else
                log_warn "⚠️  任务进程 $pid 返回非零状态"
            fi
        done
    fi
}

# 检查是否指定了任何执行选项（除 -m/--mooncake 外）
check_has_execute_options() {
    if $ENABLE_IMAGES || $ENABLE_CRI_MUX || $ENABLE_FIRECRACKER || \
       $ENABLE_PACKAGE || $ENABLE_PACKAGE_SLIM; then
        return 0  # 有执行选项
    fi
    return 1  # 没有执行选项
}

# 主逻辑判断
if check_has_execute_options "$@"; then
    # 有指定特定任务，并行执行这些任务
    run_tasks "$@"
else
    # 没有指定特定任务，执行标准构建流程
    log_section "标准构建流程"
    setup_go_dependencies
    mkdir -p "$BIN_DIR"
    log_section "构建二进制文件 ${BUILD_TAGS:+($BUILD_TAGS)} [${GOARCH}]"
    build_main_binaries
    build_db_tools
    build_fc_netns_exec
fi

# ============================================================================
# 总结
# ============================================================================

log_section "所有任务执行完毕"
if [ -d "$BIN_DIR" ]; then
    log_info "生成的二进制文件位于: ${BIN_DIR}/"
    ls -lh "${BIN_DIR}/" 2>/dev/null || log_error "目录为空或不存在"
fi
print_elapsed_time
