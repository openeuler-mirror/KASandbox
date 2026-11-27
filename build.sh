#!/bin/bash

# e2b-infra 构建脚本（门禁/本地构建入口）
# 仅构建 e2b-infra 的 Go 组件（api/client-proxy/envd/orchestrator + db 工具 + fc-netns-exec），
# 可选 Docker 镜像与源码打包。参考 KASandbox android 分支 build.sh 改编，
# 移除了 cri-multiplex / e2b-webhook / firecracker 等 KASandbox 专属部分。

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
ENABLE_IMAGES=false
ENABLE_PACKAGE=false
ENABLE_PACKAGE_SLIM=false
PACKAGE_OUTPUT_NAME=""
BUILD_TAGS=""
IMAGES_FILTER=""   # 逗号分隔的镜像名（空 = 全部），由 -i/--images 解析

# 镜像构建配置
readonly API_IMAGE_NAME="api"
readonly ORCHESTRATOR_IMAGE_NAME="orchestrator"
readonly CLIENT_PROXY_IMAGE_NAME="client-proxy"

# ============================================================================
# 命令行参数解析
# ============================================================================

show_usage() {
    cat << 'EOF'
用法: ./build.sh [选项]

选项:
  -m, --mooncake         启用 Mooncake 存储后端编译 (包含 CGo 链接)
  -i, --images [名称]  构建 Docker 镜像；不带参数构建全部 (api/orchestrator/client-proxy)，
                         带参数仅构建指定镜像（可用逗号分隔多个）
  -p, --package          打包源码 (包含所有源代码)
  --package-slim         打包最小化构建包 (仅构建所需文件)
  -n, --package-name NAME 指定打包输出文件名 (不含扩展名，自动追加 .tar.*)
  -h, --help             显示此帮助信息

示例:
  ./build.sh                              # 标准构建（全部 Go 二进制）
  ./build.sh -m -i                        # 使用 Mooncake + 构建镜像
  ./build.sh -i                           # 构建全部 Docker 镜像
  ./build.sh -i orchestrator              # 仅构建 orchestrator 镜像
  ./build.sh -p                           # 打包源码
  ./build.sh --package-slim               # 打包最小化构建包
  ./build.sh --package-slim -n my-release # 指定输出名为 my-release.tar.xz
EOF
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -m|--mooncake)      ENABLE_MOONCAKE=true ;;
        -i|--images)
            ENABLE_IMAGES=true
            # 可选参数：下一个 token 不是以 - 开头则视为镜像名列表
            if [[ $# -ge 2 && ! "$2" =~ ^- ]]; then
                IMAGES_FILTER="$2"
                shift
            fi
            ;;
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
        "-lurma -letcd_wrapper -lspdiag"
    )

    # 可选：CUDA 支持
    if [ -d "/usr/local/cuda/lib64" ]; then
        cgo_ldflags+=("-L/usr/local/cuda/lib64 -lcudart")
    fi

    # 合并用户手动传入的 CGO flags（追加到内置 flags 之后）
    MOONCAKE_CGO_CFLAGS="${cgo_cflags[*]} ${CGO_CFLAGS:-}"
    MOONCAKE_CGO_LDFLAGS="${cgo_ldflags[*]} ${CGO_LDFLAGS:-}"
    export BUILD_TAGS="$BUILD_TAGS"

    log_info "BUILD_TAGS=${BUILD_TAGS}"
    log_info "MOONCAKE_CGO_CFLAGS=${MOONCAKE_CGO_CFLAGS}"
    log_info "MOONCAKE_CGO_LDFLAGS=${MOONCAKE_CGO_LDFLAGS}"
}

setup_standard_build() {
    log_section "标准构建（无 Mooncake）"
    log_warn "提示: 使用 -m 启用 Mooncake，-i 构建 Docker 镜像"
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
    )
    
    # 注意：不要像 KASandbox android 分支那样在非 mooncake 模式下临时移走
    # storage_mooncake.go——该文件无构建标签、定义了 mooncakeKV/mooncakeObject
    # 等类型，被 write/cache 等同样无标签的文件引用。移走它会让随后并行的
    # `go mod vendor` 把缺少定义的包拷进 vendor/，导致非 mooncake 构建
    # "undefined: mooncakeKV"。mooncake 外部模块依赖只由带 //go:build mooncake
    # 标签的 client.go 引入，非 mooncake 构建不会编译它。

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
    local module_tags="$BUILD_TAGS"
    local module_cgo_cflags=""
    local module_cgo_ldflags=""

    # Mooncake 仅 orchestrator 依赖
    if $ENABLE_MOONCAKE; then
        if [[ "$module_dir" == *"orchestrator"* ]]; then
            module_cgo_cflags="$MOONCAKE_CGO_CFLAGS"
            module_cgo_ldflags="$MOONCAKE_CGO_LDFLAGS"
        else
            module_tags=""
        fi
    fi

    if ! check_dir "$module_dir"; then
        return 1
    fi

    log_warn "→ 构建 $module_dir -> $output"
    (
        cd "$module_dir"
        export CGO_CFLAGS="$module_cgo_cflags"
        export CGO_LDFLAGS="$module_cgo_ldflags"
        # 使用 GOWORK=off：脱离 workspace，按各自 go.mod 构建
        GOWORK=off go build $module_tags -o "$output" $build_args
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
        export CGO_CFLAGS=""
        export CGO_LDFLAGS=""
        go build -o "../../${BIN_DIR}/migrator" ./scripts/migrator.go
        log_info "✓ ${BIN_DIR}/migrator 构建完成"
    ) &
    pids+=($!)

    # seed-db 后台构建
    (
        log_warn "→ 构建 $db_dir 的 seed-db"
        cd "$db_dir"
        export CGO_CFLAGS=""
        export CGO_LDFLAGS=""
        go build -o "../../${BIN_DIR}/seed-db" ./scripts/seed/postgres/seed-db.go
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
    
    # 定义镜像构建任务：镜像名|Dockerfile|构建上下文|需要的二进制
    local image_tasks=(
        "${API_IMAGE_NAME}|packages/api/Dockerfile|packages/api|api"
        "${ORCHESTRATOR_IMAGE_NAME}|packages/orchestrator/Dockerfile|packages/orchestrator|orchestrator,fc-netns-exec"
        "${CLIENT_PROXY_IMAGE_NAME}|packages/client-proxy/Dockerfile|packages/client-proxy|client-proxy"
    )

    # 应用 -i 后的镜像过滤（IMAGES_FILTER 为空 = 全部）
    local selected_tasks=()
    if [ -n "$IMAGES_FILTER" ]; then
        local wanted normalized
        normalized=",${IMAGES_FILTER},"
        for task in "${image_tasks[@]}"; do
            local name="${task%%|*}"
            if [[ "$normalized" == *",${name},"* ]]; then
                selected_tasks+=("$task")
            fi
        done
        if [ ${#selected_tasks[@]} -eq 0 ]; then
            log_error "未匹配任何镜像：$IMAGES_FILTER（可选：${API_IMAGE_NAME}/${ORCHESTRATOR_IMAGE_NAME}/${CLIENT_PROXY_IMAGE_NAME}）"
            return 1
        fi
        log_info "仅构建指定镜像：$IMAGES_FILTER"
    else
        selected_tasks=("${image_tasks[@]}")
    fi

    local pids=()
    local task
    for task in "${selected_tasks[@]}"; do
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
    
    # 显示本次构建涉及的镜像
    local selected_names=()
    for task in "${selected_tasks[@]}"; do
        selected_names+=("${task%%|*}")
    done
    local name_pattern
    name_pattern=$(IFS='|'; echo "${selected_names[*]}")
    echo -e "${CYAN}已构建镜像:${NC}"
    docker images --format "  {{.Repository}}:{{.Tag}}  ({{.Size}})" \
        | grep -E "(${name_pattern}) " || true

    # 导出镜像为 tar 包，便于离线分发 / docker load
    save_images_to_tar "${selected_names[@]}"
}

save_images_to_tar() {
    local names=("$@")
    local out_dir="${BIN_DIR}/images"
    mkdir -p "$out_dir"
    local name
    for name in "${names[@]}"; do
        local tar_path="${out_dir}/${name}.tar"
        log_warn "→ 导出镜像 ${name} 到 ${tar_path}"
        if docker save -o "$tar_path" "$name"; then
            log_info "✓ ${tar_path} ($(du -h "$tar_path" | cut -f1))"
        else
            log_error "✗ 镜像 ${name} 导出失败"
            rm -f "$tar_path"
        fi
    done
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
        package_name="e2b-infra-${project_version}-${timestamp}${compress_ext}"
        package_dir_name="e2b-infra-${project_version}"
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
            --exclude='e2b-infra-*.tar.*' \
            --warning=no-file-changed \
            --transform "s,^$(basename "$SCRIPT_DIR"),$package_dir_name," \
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
        package_name="e2b-infra-build-${project_version}-${timestamp}${compress_ext}"
        package_dir_name="e2b-infra-build-${project_version}"
    fi
    
    log_warn "→ 创建最小化构建包: $package_name"
    log_warn "  压缩算法: $compression"
    log_warn "  包含: build.sh、packages/{api,client-proxy,envd,orchestrator,db,auth,clickhouse,shared}"
    
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
            --exclude='e2b-infra-*.tar.*' \
            --exclude='*/bin' \
            --warning=no-file-changed \
            --transform "s,^$repo_dir,$package_dir_name," \
            -cf "$SCRIPT_DIR/$package_name" \
            "$repo_dir/build.sh" \
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
    if $ENABLE_IMAGES || $ENABLE_PACKAGE || $ENABLE_PACKAGE_SLIM; then
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
