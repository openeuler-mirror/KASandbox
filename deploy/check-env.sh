#!/bin/bash

# ===================== 基础配置 =====================
# 部署前置检查脚本
# 用法:
#   ./check-env.sh             # 检查 install + start 的全部前置条件
#   ./check-env.sh --install   # 仅检查 ./build.sh --install 的前置条件
#   ./check-env.sh --start     # 仅检查 ./build.sh --start 的前置条件
#
# 作用: 确保 ./build.sh --install --start 能够成功执行。
# 任何 FAIL 都会给出对应的修复提示；退出码为失败检查项数量。

WORK_DIR=$(cd "$(dirname "$0")" && pwd)
DEP_DIR="$WORK_DIR/dep"
E2B_DIR="/opt/e2b-infra"

# 架构检测
RAW_ARCH=$(uname -m)
case "$RAW_ARCH" in
    x86_64)   ARCH="x86_64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)        ARCH="unknown" ;;
esac

# ===================== 输出函数 =====================
RED='\033[31m'; GREEN='\033[32m'; YELLOW='\033[33m'; NC='\033[0m'
info()  { echo -e "${YELLOW}[检查] $*${NC}"; }
ok()    { echo -e "  ${GREEN}✔ $*${NC}"; }
fail()  { echo -e "  ${RED}✘ $*${NC}"; }
warn()  { echo -e "  ${YELLOW}⚠ $*${NC}"; }

FAIL_COUNT=0
WARN_COUNT=0

# 通过/失败检查：$1=描述 $2=结果(0通过) $3=失败提示
check() {
    local desc="$1" result="$2" hint="$3"
    if [ "$result" -eq 0 ]; then
        ok "$desc"
    else
        fail "$desc"
        [ -n "$hint" ] && echo -e "    ${RED}-> $hint${NC}"
        FAIL_COUNT=$((FAIL_COUNT + 1))
    fi
}

# ===================== 通用条件（install 与 start 均需要）=====================
check_common() {
    info "===== 通用条件 ====="

    check "以 root 权限运行" \
        "$([ "$(id -u)" -eq 0 ]; echo $?)" \
        "请使用 root 或 sudo 执行"

    check "架构受支持（当前: $ARCH）" \
        "$([ "$ARCH" != "unknown" ]; echo $?)" \
        "仅支持 x86_64 / arm64"

    check "配置文件 .env 存在" \
        "$([ -f "$WORK_DIR/.env" ]; echo $?)" \
        "缺少 $WORK_DIR/.env，请参考 .env 模板创建"

    if [ -f "$WORK_DIR/.env" ]; then
        # shellcheck disable=SC1091
        source "$WORK_DIR/.env"
        local ip
        ip="${SERVER_IP:-${SERVER_IPS:-}}"
        check "SERVER_IP 已配置（当前: ${ip:-空}）" \
            "$([ -n "$ip" ]; echo $?)" \
            "请编辑 $WORK_DIR/.env，将 SERVER_IP 修改为本机 IP"
    fi

    # 磁盘空间检查（Harbor 镜像 + 沙箱镜像需较大空间）
    local disk_avail_mb
    disk_avail_mb=$(df -m "$WORK_DIR" 2>/dev/null | awk 'NR==2{print $4}')
    if [ -n "$disk_avail_mb" ] && [ "$disk_avail_mb" -lt 51200 ]; then
        warn "磁盘可用空间不足 50GB（当前: $((disk_avail_mb/1024))GB），Harbor 与沙箱镜像可能空间不足"
        WARN_COUNT=$((WARN_COUNT + 1))
    else
        ok "磁盘可用空间充足（当前: $((disk_avail_mb/1024))GB）"
    fi

    # SELinux 提示（build.sh install 会执行 setenforce 0）
    if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce)" != "Disabled" ]; then
        warn "SELinux 未关闭（当前: $(getenforce)），建议执行 setenforce 0"
        WARN_COUNT=$((WARN_COUNT + 1))
    else
        ok "SELinux 状态正常"
    fi
}

# ===================== --install 前置条件 =====================
check_install() {
    info "===== --install 前置条件 ====="

    # 包管理器
    local pkg_mgr=""
    command -v yum >/dev/null 2>&1 && pkg_mgr="yum"
    command -v dnf >/dev/null 2>&1 && pkg_mgr="dnf"
    command -v apt-get >/dev/null 2>&1 && pkg_mgr="apt"
    check "包管理器可用（当前: ${pkg_mgr:-无}）" \
        "$([ -n "$pkg_mgr" ]; echo $?)" \
        "build.sh 仅支持 yum/dnf/apt，请先安装其中之一"

    # python3 + pip（install_e2b 需要）
    check "python3 + pip 可用" \
        "$(command -v python3 >/dev/null 2>&1 && command -v pip >/dev/null 2>&1; echo $?)" \
        "请安装 python3 与 pip: $pkg_mgr install -y python3 python3-pip"

    # 容器运行时（build.sh 在 install 前调用 set_container_runtime）
    local rt=""
    command -v docker >/dev/null 2>&1 && rt="docker"
    [ -z "$rt" ] && command -v nerdctl >/dev/null 2>&1 && rt="nerdctl"
    check "容器运行时可用（当前: ${rt:-无}）" \
        "$([ -n "$rt" ]; echo $?)" \
        "build.sh 需要 docker 或 nerdctl。可先执行 ./build.sh --deploy docker 安装"

    # dep/ 目录
    check "依赖目录存在（$DEP_DIR）" \
        "$([ -d "$DEP_DIR" ]; echo $?)" \
        "请先执行 ./build.sh --download 下载依赖包"

    if [ -d "$DEP_DIR" ]; then
        info "----- 下载包完整性检查（按架构 $ARCH）-----"
        local required_files=()
        case "$ARCH" in
            x86_64)
                required_files=(
                    "docker-25.0.5.tgz|Docker 二进制"
                    "docker-compose-linux-x86_64|Docker Compose"
                    "nomad_1.10.4_linux_amd64.zip|Nomad"
                    "consul_1.21.4_linux_amd64.zip|Consul"
                    "firecracker-v1.13.1-x86_64.tgz|Firecracker"
                    "openEuler-docker.x86_64.tar.xz|openEuler 镜像"
                    "harbor-offline-installer-v2.13.0.tgz|Harbor"
                )
                ;;
            arm64)
                required_files=(
                    "docker-25.0.5.tgz|Docker 二进制"
                    "docker-compose-linux-aarch64|Docker Compose"
                    "nomad_1.10.4_linux_arm64.zip|Nomad"
                    "consul_1.21.4_linux_arm64.zip|Consul"
                    "firecracker-v1.13.1-aarch64.tgz|Firecracker"
                    "openEuler-docker.aarch64.tar.xz|openEuler 镜像"
                    "harbor-offline-installer-aarch64-v2.13.0.tgz|Harbor"
                )
                ;;
        esac

        local entry fname desc
        for entry in "${required_files[@]}"; do
            fname="${entry%%|*}"; desc="${entry##*|}"
            check "$desc 存在（dep/$fname）" \
                "$([ -f "$DEP_DIR/$fname" ]; echo $?)" \
                "运行 ./build.sh --download 重新下载，或手动放入 $DEP_DIR/$fname"
        done

        # k8s 模式 + 启用 webhook 时额外需要 e2b-webhook.tar
        if [ "${DEPLOY_MODE:-nomad}" = "k8s" ] && [ "${ENABLE_WEBHOOK:-false}" = "true" ]; then
            check "e2b-webhook 镜像包存在（dep/e2b-webhook.tar）" \
                "$([ -f "$DEP_DIR/e2b-webhook.tar" ]; echo $?)" \
                "运行 ./build.sh --download 下载 e2b-webhook 镜像"
        fi
    fi

    # 外网连通性（拉取基础镜像需要，离线环境可跳过）
    local image_registry="swr.cn-north-4.myhuaweicloud.com"
    if curl -s -m 5 -o /dev/null "https://$image_registry" 2>/dev/null; then
        ok "镜像仓库可达（$image_registry）"
    else
        warn "镜像仓库不可达（$image_registry），install 拉取基础镜像可能失败；离线环境请预先导入镜像"
        WARN_COUNT=$((WARN_COUNT + 1))
    fi
}

# ===================== --start 前置条件 =====================
check_start() {
    info "===== --start 前置条件 ====="

    # 已执行 install（e2b-infra 目录）
    check "/opt/e2b-infra 已初始化（需先 --install）" \
        "$([ -d "$E2B_DIR" ]; echo $?)" \
        "请先执行 ./build.sh --install"

    # 容器运行时
    local rt=""
    command -v docker >/dev/null 2>&1 && rt="docker"
    [ -z "$rt" ] && command -v nerdctl >/dev/null 2>&1 && rt="nerdctl"
    check "容器运行时可用（当前: ${rt:-无}）" \
        "$([ -n "$rt" ]; echo $?)" \
        "请先执行 ./build.sh --install 或 --deploy docker"

    # Harbor 目录（install 时解压生成）
    check "Harbor 已解压（$WORK_DIR/harbor）" \
        "$([ -d "$WORK_DIR/harbor" ]; echo $?)" \
        "请先执行 ./build.sh --install"

    if [ "$DEPLOY_MODE" = "k8s" ]; then
        info "----- K8S 模式检查 -----"
        check "kubectl 已安装" \
            "$(command -v kubectl >/dev/null 2>&1; echo $?)" \
            "请安装 kubectl（见 USAGE.md 3.2.1 K8S 集群部署）"
        if command -v kubectl >/dev/null 2>&1; then
            check "K8S 集群可访问（kubectl cluster-info）" \
                "$(kubectl cluster-info >/dev/null 2>&1; echo $?)" \
                "请检查 kubeconfig 与集群状态: kubectl get nodes"
        fi
    else
        info "----- Nomad 模式检查 -----"
        # postgres 容器（start_postgres 依赖）
        local pg_name
        pg_name=$("$rt" ps -a --format '{{.Names}}' 2>/dev/null | grep -x postgres || true)
        check "PostgreSQL 容器存在（postgres）" \
            "$([ -n "$pg_name" ]; echo $?)" \
            "install 阶段应创建 postgres 容器，请先执行 ./build.sh --install"

        # nomad/consul systemd 服务
        check "Nomad 服务可用（systemd 或二进制）" \
            "$([ -f /etc/systemd/system/nomad.service ] || command -v nomad >/dev/null 2>&1; echo $?)" \
            "install 阶段应安装 Nomad，请先执行 ./build.sh --install"
        check "Consul 服务可用（systemd 或二进制）" \
            "$([ -f /etc/systemd/system/consul.service ] || command -v consul >/dev/null 2>&1; echo $?)" \
            "install 阶段应安装 Consul，请先执行 ./build.sh --install"
    fi

    info "----- 端口占用检查 -----"
    check "Harbor 端口 2900/30443 空闲" \
        "$(ss -tln 2>/dev/null | grep -qE ':2900|:30443'; [ $? -ne 0 ]; echo $?)" \
        "端口已被占用，请释放或修改 .env 中 HARBOR_HTTP_PORT/HARBOR_HTTPS_PORT"
    check "Nomad 端口 4646 空闲" \
        "$(ss -tln 2>/dev/null | grep -q ':4646'; [ $? -ne 0 ]; echo $?)" \
        "端口已被占用，请释放"
    check "Consul 端口 8500 空闲" \
        "$(ss -tln 2>/dev/null | grep -q ':8500'; [ $? -ne 0 ]; echo $?)" \
        "端口已被占用，请释放"
}

# ===================== 入口 =====================
CHECK_INSTALL=false
CHECK_START=false
case "${1:-all}" in
    --install) CHECK_INSTALL=true ;;
    --start)   CHECK_START=true ;;
    all|"")    CHECK_INSTALL=true; CHECK_START=true ;;
    --help|-h)
        echo "用法: $0 [--install|--start|all]"
        echo "  all        检查 install + start 全部前置条件（默认）"
        echo "  --install  仅检查 ./build.sh --install 的前置条件"
        echo "  --start    仅检查 ./build.sh --start 的前置条件"
        exit 0
        ;;
    *) echo "未知参数: $1（使用 --help 查看用法）" >&2; exit 2 ;;
esac

check_common
if [ "$CHECK_INSTALL" = true ]; then check_install; fi
if [ "$CHECK_START" = true ]; then check_start; fi

echo ""
if [ "$FAIL_COUNT" -eq 0 ]; then
    echo -e "${GREEN}✅ 全部检查通过，可以执行 ./build.sh --install --start${NC}"
else
    echo -e "${RED}❌ 共 $FAIL_COUNT 项检查未通过，请根据上方提示修复后重试${NC}"
    [ "$WARN_COUNT" -gt 0 ] && echo -e "${YELLOW}⚠ 另有 $WARN_COUNT 项警告，不影响执行但建议处理${NC}"
fi
exit "$FAIL_COUNT"
