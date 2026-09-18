#!/usr/bin/env bash
# 构建 RPM 包（自动检查并安装构建依赖：git/rpm-build/gcc/make/golang，
# dnf golang 仅满足 spec BuildRequires，实际编译自动使用满足 go.mod 要求的新版 Go）
# 用法: ./scripts/build-rpm.sh [选项]
#   -v VERSION    指定版本号（默认: <spec_version>.git<8位commit>）
#   -r RELEASE    指定 Release 号（默认 1）
#   -o OUTPUT     指定输出目录（默认 /tmp/rpmbuild/RPMS）
#   -m, --mooncake 启用 Mooncake 存储后端编译（对应 build.sh -m，需构建机已安装 Mooncake 依赖库）
#   --srpm        同时构建 SRPM
#   -h, --help    显示帮助

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# ── 默认值 ──
VERSION=""
RELEASE="1"
OUTPUT_DIR="/tmp/rpmbuild/RPMS"
BUILD_SRPM=false
BUILD_MOONCAKE=false

# ── 参数解析 ──
while [[ $# -gt 0 ]]; do
    case "$1" in
        -v|--version) VERSION="$2"; shift 2 ;;
        -r|--release) RELEASE="$2"; shift 2 ;;
        -o|--output)  OUTPUT_DIR="$2"; shift 2 ;;
        -m|--mooncake) BUILD_MOONCAKE=true; shift ;;
        --srpm)       BUILD_SRPM=true; shift ;;
        -h|--help)
            echo "用法: $0 [选项]"
            echo "  -v VERSION    指定版本号"
            echo "  -r RELEASE    指定 Release 号（默认 1）"
            echo "  -o OUTPUT     指定输出目录（默认 /tmp/rpmbuild/RPMS）"
            echo "  -m, --mooncake 启用 Mooncake 存储后端编译（需构建机已安装依赖库）"
            echo "  --srpm        同时构建 SRPM"
            exit 0
            ;;
        *) echo "未知选项: $1"; exit 1 ;;
    esac
done

# -o 支持相对路径：以调用脚本时的 cwd 为基准转为绝对路径
#（脚本后续会 cd 到 PROJECT_DIR，不先规范化的话相对路径基准就变了）
OUTPUT_DIR="$(realpath -m "$OUTPUT_DIR")"

# ── 0. 构建依赖检查与安装 ──
echo "[0/4] 检查构建依赖..."

need_root() {
    [ "$(id -u)" -eq 0 ] || { echo "ERROR: 安装依赖需要 root 权限，请以 root 重跑" >&2; exit 1; }
}

# /usr/local/bin 须先于 /usr/bin（dnf golang 的 /usr/bin/go 可能是旧版本）
export PATH="/usr/local/bin:$PATH"

# 基础工具（rpmbuild 由 rpm-build 包提供）
missing_pkgs=()
command -v git      >/dev/null 2>&1 || missing_pkgs+=(git)
command -v rpmbuild >/dev/null 2>&1 || missing_pkgs+=(rpm-build)
command -v gcc      >/dev/null 2>&1 || missing_pkgs+=(gcc)
command -v make     >/dev/null 2>&1 || missing_pkgs+=(make)
command -v curl     >/dev/null 2>&1 || missing_pkgs+=(curl)
if ((${#missing_pkgs[@]})); then
    need_root
    echo "  → 安装: ${missing_pkgs[*]}"
    dnf install -y -q "${missing_pkgs[@]}"
fi

# spec 的 BuildRequires golang 需由 rpm 数据库满足；dnf 仓 golang（如 1.21）
# 低于 go.mod 要求，仅用于占位，实际编译使用下方安装的新版 Go
if ! rpm -q golang >/dev/null 2>&1; then
    need_root
    echo "  → 安装 dnf golang（满足 BuildRequires，实际编译使用新版 Go）"
    dnf install -y -q golang
fi

# 所有 go.mod 中最高的 go 指令版本即最低编译版本
REQUIRED_GO=$(git -C "$PROJECT_DIR" ls-files \
    | grep -E '(^|/)go\.mod$' \
    | xargs -r grep -hoP '^go \K[0-9.]+' 2>/dev/null \
    | sort -V | tail -1 || true)
REQUIRED_GO="${REQUIRED_GO:-1.25.4}"

installed_go=""
if command -v go >/dev/null 2>&1; then
    installed_go=$(go version | grep -oP 'go\K[0-9.]+' | head -1 || true)
fi

# installed_go 低于 REQUIRED_GO 时安装官方 Go 发行包
if [ -z "$installed_go" ] \
    || [ "$(printf '%s\n%s\n' "$REQUIRED_GO" "$installed_go" | sort -V | head -1)" != "$REQUIRED_GO" ]; then
    need_root
    case "$(uname -m)" in
        aarch64) go_arch=arm64 ;;
        x86_64)  go_arch=amd64 ;;
        *) echo "ERROR: 不支持的架构 $(uname -m)" >&2; exit 1 ;;
    esac
    echo "  → 当前 Go ${installed_go:-未安装} 低于 go.mod 要求 ${REQUIRED_GO}，下载安装 go${REQUIRED_GO} ..."
    download_ok=false
    for mirror in "https://mirrors.aliyun.com/golang"; do
        if curl -fsSL -o /tmp/go.tgz "${mirror}/go${REQUIRED_GO}.linux-${go_arch}.tar.gz"; then
            download_ok=true
            break
        fi
        echo "  → 镜像不可用，尝试下一个: $mirror"
    done
    $download_ok || { echo "ERROR: 无法下载 go${REQUIRED_GO}，请手动安装后重试" >&2; exit 1; }
    tar -C /usr/local -xzf /tmp/go.tgz
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
    rm -f /tmp/go.tgz
fi
echo "  → 使用 $(go version)"

# ── 确定版本号 ──
if [ -z "$VERSION" ]; then
    # 从 spec 中提取 %global pkg_version X.Y.Z
    base_ver=$(grep -oP '^%global\s+pkg_version\s+\K\S+' "$SCRIPT_DIR/KASandbox.spec" | head -1)
    if [ -z "$base_ver" ]; then
        echo "ERROR: 无法从 spec 中提取版本号，请使用 -v 指定" >&2
        exit 1
    fi
    # 取当前 HEAD 的短 commit hash 作为后缀
    commit_sha=$(git -C "$PROJECT_DIR" rev-parse --short=8 HEAD 2>/dev/null || echo "unknown")
    VERSION="${base_ver}.git${commit_sha}"
fi

echo "=== KASandbox RPM 构建 ==="
echo "版本:  $VERSION"
echo "Release: $RELEASE"
echo "Mooncake: $BUILD_MOONCAKE"
echo "项目目录: $PROJECT_DIR"
echo ""

# ── 1. 打包源码为 tar.gz ──
PACKAGE_NAME="KASandbox-${VERSION}"
TARBALL="${PACKAGE_NAME}.tar.gz"
SPEC_FILE="$SCRIPT_DIR/KASandbox.spec"

echo "[1/4] 打包源码..."

cd "$PROJECT_DIR"

# 确保工作区干净
if ! git diff-index --quiet HEAD -- 2>/dev/null; then
    echo "WARNING: 工作区有未提交的修改，git archive 将使用 HEAD 的快照"
    echo "         未提交的修改不会被包含在 RPM 包中"
    echo ""
fi

# 使用 git archive 打包当前 HEAD，自动遵循 .gitignore
git archive --format=tar.gz \
    --prefix="${PACKAGE_NAME}/" \
    --output="/tmp/${TARBALL}" \
    HEAD

echo "  → 源码包: /tmp/${TARBALL}"
echo "  → 大小: $(du -h "/tmp/${TARBALL}" | cut -f1)"

# ── 2. 准备 rpmbuild 目录并复制文件 ──
echo ""
echo "[2/4] 准备 rpmbuild 环境..."

RPMBUILD_DIR="/tmp/rpmbuild"
mkdir -p "$RPMBUILD_DIR"/{SOURCES,SPECS,RPMS,SRPMS,BUILD,BUILDROOT}

# 清理旧源码包避免 Source0 文件名冲突
rm -f "$RPMBUILD_DIR/SOURCES/KASandbox-"*.tar.gz

# 复制源码包到 SOURCES
cp "/tmp/${TARBALL}" "$RPMBUILD_DIR/SOURCES/"

# 复制 spec 到 SPECS，并注入版本号（替换 %global pkg_version 行）
sed "s/^%global pkg_version .*/%global pkg_version ${VERSION}/" "$SPEC_FILE" \
    > "$RPMBUILD_DIR/SPECS/$(basename "$SPEC_FILE")"

echo "  → 源码包已复制到: $RPMBUILD_DIR/SOURCES/${TARBALL}"
echo "  → Spec 已复制到:  $RPMBUILD_DIR/SPECS/$(basename "$SPEC_FILE")"

# ── 3. 构建 RPM ──
echo ""
echo "[3/4] 构建 RPM 包..."

RPMBUILD_OPTS=()
if $BUILD_MOONCAKE; then
    # spec 中 %bcond_with mooncake，--with 开启后 %build 走 ./build.sh -m
    RPMBUILD_OPTS+=("--with" "mooncake")
fi

if $BUILD_SRPM; then
    echo "  → 构建 SRPM + RPM..."
    rpmbuild -ba \
        --define "_topdir $RPMBUILD_DIR" \
        "${RPMBUILD_OPTS[@]}" \
        "$RPMBUILD_DIR/SPECS/$(basename "$SPEC_FILE")"
else
    echo "  → 构建 RPM..."
    rpmbuild -bb \
        --define "_topdir $RPMBUILD_DIR" \
        "${RPMBUILD_OPTS[@]}" \
        "$RPMBUILD_DIR/SPECS/$(basename "$SPEC_FILE")"
fi

# ── 4. 输出结果 ──
echo ""
echo "[4/4] 构建完成!"

ARCH=$(uname -m)
RPM_FILE="$RPMBUILD_DIR/RPMS/${ARCH}/KASandbox-${VERSION}-${RELEASE}.${ARCH}.rpm"

if [ -f "$RPM_FILE" ]; then
    echo ""
    echo "=== RPM 包信息 ==="
    rpm -qpi "$RPM_FILE" 2>/dev/null || true
    echo ""
    echo "文件列表:"
    # head 提前退出会让 rpm 收到 SIGPIPE(141)，pipefail+set -e 下会静默终止脚本
    rpm -qlp "$RPM_FILE" | head -40 || true
    echo "  ... (共 $(rpm -qlp "$RPM_FILE" | wc -l) 个文件)"
    echo ""
    echo "RPM 包位置: $RPM_FILE"
    echo "大小: $(du -h "$RPM_FILE" | cut -f1)"

    if [ -n "$OUTPUT_DIR" ] && [ "$OUTPUT_DIR" != "$RPMBUILD_DIR/RPMS" ]; then
        mkdir -p "$OUTPUT_DIR"
        cp "$RPM_FILE" "$OUTPUT_DIR/"
        echo "已复制到: $OUTPUT_DIR/$(basename "$RPM_FILE")"
    fi
else
    echo "ERROR: RPM 构建失败，未找到产物 $RPM_FILE" >&2
    exit 1
fi

if $BUILD_SRPM; then
    SRPM_FILE="$RPMBUILD_DIR/SRPMS/KASandbox-${VERSION}-${RELEASE}.src.rpm"
    if [ -f "$SRPM_FILE" ]; then
        echo ""
        echo "SRPM 包位置: $SRPM_FILE"
    fi
fi

echo ""
echo "✓ RPM 构建成功"
