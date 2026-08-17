#!/bin/bash

# ===================== 基础配置 =====================
WORK_DIR=$(cd "$(dirname "$0")" && pwd)  # 转为绝对路径，避免相对路径问题
DEP_DIR="$WORK_DIR/dep"
E2B_DIR="/opt/e2b-infra"
mkdir -p "$DEP_DIR"
# 定义默认端口（可通过参数覆盖，后续扩展）
PG_PORT=5432
MINIO_PORT=9000
MINIO_CONSOLE_PORT=9001
NOMAD_PORT=4646
# Harbor 登录凭证（根据实际情况修改）
HARBOR_USER="admin"
HARBOR_PASSWORD="${HARBOR_PASSWORD:-}"
source ".env"
# Harbor 协议: http, https, both
HARBOR_PROTOCOL="${HARBOR_PROTOCOL:-both}"
# Nomad/Consul 健康检查端口
CONSUL_HTTP_PORT=8500
HOST_IP="$SERVER_IPS"
# 架构检测
RAW_ARCH=$(uname -m)
case "$RAW_ARCH" in
    x86_64)
        DEFAULT_ARCH="x86_64"
        ;;
    aarch64|arm64)
        DEFAULT_ARCH="arm64"
        ;;
    *)
        DEFAULT_ARCH="unknown"
        ;;
esac

# 设置初始架构为自动检测值
ARCH="$DEFAULT_ARCH"
CONTAINER_RUNTIME=""

# 设置容器运行时命令
# 优先使用 docker，若不存在则回退到 nerdctl（通常用于 k8s 节点）
# 可通过 --runtime docker|nerdctl 显式指定
set_container_runtime() {
    local explicit_runtime="${1:-}"

    if [ -n "$explicit_runtime" ]; then
        case "$explicit_runtime" in
            docker)
                if ! command -v docker >/dev/null 2>&1; then
                    error "显式指定 docker，但系统中未找到 docker 命令"
                    exit 1
                fi
                DOCKER_CMD="docker"
                DOCKER_COMPOSE_CMD="docker-compose"
                CONTAINERD_SERVICE="docker"
                ;;
            nerdctl)
                if ! command -v nerdctl >/dev/null 2>&1; then
                    error "显式指定 nerdctl，但系统中未找到 nerdctl 命令"
                    exit 1
                fi
                DOCKER_CMD="nerdctl"
                DOCKER_COMPOSE_CMD="nerdctl compose"
                CONTAINERD_SERVICE="containerd"
                ;;
            *)
                error "不支持的容器运行时: $explicit_runtime（仅支持 docker/nerdctl）"
                exit 1
                ;;
        esac
        info "使用显式指定的容器运行时: $DOCKER_CMD"
        return
    fi

    if command -v docker >/dev/null 2>&1; then
        DOCKER_CMD="docker"
        DOCKER_COMPOSE_CMD="docker-compose"
        CONTAINERD_SERVICE="docker"
    elif command -v nerdctl >/dev/null 2>&1; then
        DOCKER_CMD="nerdctl"
        DOCKER_COMPOSE_CMD="nerdctl compose"
        CONTAINERD_SERVICE="containerd"
    else
        error "未检测到可用的容器运行时（docker/nerdctl）"
        exit 1
    fi
}

# ===================== 颜色输出函数（增强可读性）=====================
info() {
    echo -e "\033[34mℹ️ $1\033[0m"
}
success() {
    echo -e "\033[32m✅ $1\033[0m"
}
error() {
    echo -e "\033[31m❌ $1\033[0m"
    exit 1
}
warn() {
    echo -e "\033[33m⚠️ $1\033[0m"
}

# ===================== 安装函数 =====================
install_base_packages() {
    # 自动识别包管理器，兼容 yum/apt
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update && apt-get install -y curl unzip jq tar rsync dnsmasq
    elif command -v yum >/dev/null 2>&1; then
        yum install -y curl unzip jq tar rsync dnsmasq
    else
        error "不支持的包管理器，仅支持 yum / apt"
    fi
}

# --- 函数：拉取并重命名 Docker 镜像 (带存在性检查) ---
pull_docker_images() {
    # 加载本地镜像包（.tar / .tar.gz）
    local file
    for file in "$DEP_DIR"/*.tar "$DEP_DIR"/*.tar.gz; do
        [ -f "$file" ] || continue
        echo "正在加载镜像: $file"
        $DOCKER_CMD load -i "$file"
    done

    echo ">>> 正在检查并拉取 Docker 镜像..."
    # 架构后缀映射：arm64 镜像带 -linuxarm64 后缀，x86_64 无后缀
    local arch_suffix=""
    [ "$ARCH" = "arm64" ] && arch_suffix="-linuxarm64"

    local images=(
        "swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/redis:7.4.4-alpine${arch_suffix}|redis:7.4.4-alpine"
        "swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/debian:bookworm-slim${arch_suffix}|debian:bookworm-slim"
        "swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/postgres:latest${arch_suffix}|postgres:latest"
    )

    # K8S 模式额外拉取 busybox 和 ubuntu（两种架构均需要）
    if [ "$DEPLOY_MODE" = "k8s" ]; then
        images+=(
            "swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/busybox:latest${arch_suffix}|busybox:latest"
            "swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/ubuntu:24.04${arch_suffix}|ubuntu:24.04"
        )
    fi
    
    for item in "${images[@]}"; do
        local remote_img="${item%|*}"
        local local_tag="${item#*|}"
        
        # 1. 检查本地是否已经存在目标标签 (local_tag)
        if $DOCKER_CMD inspect --type=image "$local_tag" >/dev/null 2>&1; then
            echo "  [跳过] 镜像 $local_tag 已存在，无需拉取。"
            continue
        fi
        if $DOCKER_CMD inspect --type=image "$remote_img" >/dev/null 2>&1; then
            echo "  [发现] 本地已存在原始镜像，正在直接重命名..."
            $DOCKER_CMD tag "$remote_img" "$local_tag"
            $DOCKER_CMD rmi "$remote_img" >/dev/null 2>&1
            continue
        fi

        # 2. 如果不存在，则拉取并重命名
        echo "  [执行] 正在从远程拉取: $remote_img"
        if $DOCKER_CMD pull "$remote_img"; then
            echo "  [成功] 重命名为: $local_tag"
            $DOCKER_CMD tag "$remote_img" "$local_tag"
            $DOCKER_CMD rmi "$remote_img" >/dev/null 2>&1
        else
            error "  [错误] 无法拉取镜像: $remote_img"
        fi
    done
    # K8S 模式：将 busybox 导入 containerd
    if [ "$DEPLOY_MODE" = "k8s" ]; then
        $DOCKER_CMD save -o busybox.tar busybox:latest
        ctr -n k8s.io images import busybox.tar
    fi
}

install_consul() {
    info "开始安装 Consul..."
    ./install-consul.sh --version "${CONSUL_VERSION}" || error "Consul 安装失败"
}

install_nomad() {
    info "开始安装 Nomad..."
    ./install-nomad.sh --version "${NOMAD_VERSION}" || error "Nomad 安装失败"
}

install_docker() {
    info "开始安装 Docker..."
    # 本机可能尚无任何容器运行时（--deploy docker 场景），docker 由本函数自行安装
    DOCKER_CMD="${DOCKER_CMD:-docker}"
    DOCKER_COMPOSE_CMD="${DOCKER_COMPOSE_CMD:-docker-compose}"

    # 1. 安装 docker 运行依赖（仅依赖包，docker 本体由下方 tgz 安装）
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update -qq && apt-get install -y iptables ca-certificates xz-utils tar || error "安装 docker 依赖失败"
    elif command -v yum >/dev/null 2>&1; then
        yum install -y iptables ca-certificates xz tar || error "安装 docker 依赖失败"
    else
        error "不支持的包管理器，仅支持 yum / apt"
    fi

    # 2. 按架构选择 docker-compose 二进制
    local compose_file
    case "$ARCH" in
        x86_64) compose_file="docker-compose-linux-x86_64" ;;
        arm64)  compose_file="docker-compose-linux-aarch64" ;;
        *)      error "不支持的架构: $ARCH（仅支持 x86_64 / arm64）" ;;
    esac

    # 3. 校验依赖文件齐全
    if [ ! -f "$DEP_DIR/$compose_file" ] || [ ! -f "$DEP_DIR/docker-25.0.5.tgz" ]; then
        error "Docker 依赖文件缺失，请检查 $DEP_DIR 目录"
    fi

    # 4. 安装 docker-compose（标准路径 /usr/local/bin/）
    cp -f "$DEP_DIR/$compose_file" /usr/local/bin/docker-compose || error "复制 docker-compose 失败"
    chmod +x /usr/local/bin/docker-compose || error "添加 docker-compose 执行权限失败"

    # 5. 解压并安装 docker 二进制
    tar -xzf "$DEP_DIR/docker-25.0.5.tgz" -C "$DEP_DIR" || error "解压 docker 包失败"
    cp -f "$DEP_DIR/docker/"* /usr/bin/ || error "复制 docker 二进制文件失败"
    rm -rf "$DEP_DIR/docker" || true  # 清理解压残留

    # 6. 确保 docker systemd unit 存在（全新机器默认无 docker.service）
    if [ ! -f /etc/systemd/system/docker.service ] && [ ! -f /usr/lib/systemd/system/docker.service ]; then
        cat > /etc/systemd/system/docker.service <<'EOF'
[Unit]
Description=Docker Application Container Engine
Documentation=https://docs.docker.com
After=network-online.target firewalld.service
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/bin/dockerd
ExecReload=/bin/kill -s HUP $MAINPID
LimitNOFILE=infinity
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
Delegate=yes
KillMode=process
Restart=always
RestartSec=2
TimeoutStartSec=0

[Install]
WantedBy=multi-user.target
EOF
        warn "系统无 docker.service，已生成默认 unit（/etc/systemd/system/docker.service）"
    fi
    systemctl daemon-reload || error "systemd daemon-reload 失败"

    # 7. 启动并设置自启
    systemctl enable docker >/dev/null 2>&1 || warn "设置 docker 开机自启失败（非致命）"
    systemctl restart docker || error "重启 docker 服务失败"

    # 8. 验证 docker 守护进程可用（info 校验 daemon 可达，version 只校验客户端）
    if ! $DOCKER_CMD info >/dev/null 2>&1; then
        error "Docker 守护进程不可达，建议查看日志：journalctl -u docker -n 50"
    fi
    success "Docker 安装成功！版本：$($DOCKER_CMD --version | awk '{print $3}')"
    info "docker-compose 版本：$($DOCKER_COMPOSE_CMD --version)"
}

install_minio() {
    info "开始安装 MinIO..."
    # 检查依赖文件
    if [ ! -f "$DEP_DIR/minio" ] || [ ! -f "$DEP_DIR/minio.yml" ] || [ ! -f "$DEP_DIR/minio.service" ]; then
        error "MinIO 依赖文件缺失，请检查 $DEP_DIR 目录"
    fi
    
    # 安装 minio 二进制文件
    cp -f "$DEP_DIR/minio" /usr/local/bin || error "复制 minio 二进制文件失败"
    chmod +x /usr/local/bin/minio || error "添加 minio 执行权限失败"
    
    # 创建数据目录
    mkdir -p /root/data/minio || error "创建 MinIO 数据目录失败"
    
    # 复制配置文件
    cp -f "$DEP_DIR/minio.yml" /etc/default/minio || error "复制 minio 配置文件失败"
    cp -f "$DEP_DIR/minio.service" /etc/systemd/system/minio.service || error "复制 minio 服务文件失败"
    
    # 启动并设置自启
    systemctl daemon-reload || error "daemon-reload 失败"
    systemctl enable minio || warn "设置 minio 开机自启失败（非致命）"
    systemctl start minio || error "启动 minio 服务失败"
    
    # 健康检查
    info "等待 MinIO 健康检查通过..."
    HEALTH_CHECK=""
    for ((i=0; i<10; i++)); do
        HEALTH_CHECK=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 2 "http://$HOST_IP:$MINIO_PORT/minio/health/ready")
        if [ "$HEALTH_CHECK" = "200" ]; then
            break
        fi
        sleep 2
    done
    
    if [ "$HEALTH_CHECK" = "200" ]; then
        success "MinIO 安装并启动成功！健康检查通过（HTTP 200）"
        info "控制台访问地址：http://$HOST_IP:$MINIO_CONSOLE_PORT"
        return 0
    else
        error "MinIO 启动失败！健康检查返回码：$HEALTH_CHECK\n建议查看日志：journalctl -u minio -f"
    fi
}

install_harbor() {
    info "开始安装 Harbor..."
    # 检查安装包（按架构选择）
    local harbor_tar
    case "$ARCH" in
        x86_64) harbor_tar="$DEP_DIR/harbor-offline-installer-v2.13.0.tgz" ;;
        arm64)  harbor_tar="$DEP_DIR/harbor-offline-installer-aarch64-v2.13.0.tgz" ;;
    esac
    if [ ! -f "$harbor_tar" ]; then
        error "Harbor 安装包不存在：$harbor_tar"
    fi

    # 解压安装包
    tar -xvf "$harbor_tar" -C "$WORK_DIR" || error "解压 Harbor 安装包失败"
    cd "$WORK_DIR/harbor" || error "进入 Harbor 目录失败"
}

install_harbor_certs() {
    info "生成 Harbor SSL 证书..."
    mkdir -p "$HARBOR_CERTS_DIR" || error "创建 Harbor 证书目录失败"

    if [ ! -f "$DEP_DIR/harbor.cnf" ]; then
        error "证书配置文件缺失，请检查 $DEP_DIR/harbor.cnf"
    fi
    cp -f "$DEP_DIR/harbor.cnf" "$HARBOR_CERTS_DIR/" || error "复制 harbor.cnf 失败"
    sed -i "s/{ip}/$HOST_IP/" "$HARBOR_CERTS_DIR/harbor.cnf" || error "修改 harbor.cnf 中的 IP 地址失败"

    # 生成 SSL 证书
    openssl req -x509 -nodes -days 3650 -newkey rsa:2048 \
        -keyout "$HARBOR_CERTS_DIR/harbor.key" \
        -out "$HARBOR_CERTS_DIR/harbor.crt" \
        -config "$HARBOR_CERTS_DIR/harbor.cnf" \
        -extensions v3_req || error "生成 SSL 证书失败"

    success "Harbor SSL 证书生成完成: $HARBOR_CERTS_DIR/"

    if [ "$DEPLOY_MODE" = "k8s" ]; then
        if ! kubectl create configmap harbor-ca-cert \
            --from-file=harbor.crt="$HARBOR_CERTS_DIR/harbor.crt" \
            -n e2b \
            --dry-run=client -o yaml | kubectl apply -f -; then
            error "创建 harbor-ca-cert ConfigMap 失败，请检查 kubectl 连接与证书路径: $HARBOR_CERTS_DIR/harbor.crt"
        fi
    fi
}

install_postgres() {
    # 停止并删除已有容器（避免冲突）
    if $DOCKER_CMD ps -a --format "{{.Names}}" | grep -q "^postgres$"; then
        warn "检测到已有 postgres 容器，先停止并删除"
        $DOCKER_CMD stop postgres || true
        $DOCKER_CMD rm postgres || true
    fi
    
    # 启动容器（添加 --restart=always 保证开机自启）
    $DOCKER_CMD run -d --name postgres \
        -e POSTGRES_USER=postgres \
        -e POSTGRES_PASSWORD=local \
        -e POSTGRES_DB=mydatabase \
        -p "$PG_PORT:$PG_PORT" \
        --restart=always \
        postgres:latest || error "启动 PostgreSQL 容器失败"
}


dnsmasq_add_address() {
    local domain="${1:?请提供域名}"
    local ip="${2:?请提供IP地址}"
    local config_file="/etc/dnsmasq.conf"
    local entry="address=/${domain}/${ip}"
    
    if ! grep -qF "$entry" "$config_file"; then
        echo "$entry" >> "$config_file"
        info "DNS 配置已添加: $entry"
        systemctl restart dnsmasq
    else
        info "DNS 配置已存在，无需重复添加: $entry"
    fi
}

# 兼容 yum/dnf 安装 e2b-infra
install_e2b() {
    info "开始安装 e2b-infra..."
    pip install e2b==2.20.0
    pip install e2b_code_interpreter==2.4.1
    # 单机替换相关文件
    local e2b_dir="/opt/e2b-infra"
    local files=(
        "install-nomad.sh"
        "install-consul.sh"
        "uninstall-nomad.sh"
        "start-client.sh"
        "start-server.sh"
        "init-client.sh"
        "run-nomad.sh"
        "run-consul.sh"
        "deploy.sh"
        "deploy-e2b-plugin.sh"
    )
    local f
    for f in "${files[@]}"; do
        cp -fv "$DEP_DIR/$f" "$e2b_dir/$f"
    done
    
    # helm 目录随 RPM 包安装到 /opt/e2b-infra/helm，无需在此拷贝
    python3 "$e2b_dir/patch_e2b.py"
    cp -fv "$e2b_dir/bin/orchestrator" /usr/bin/orchestrator
    cp -fv "$e2b_dir/bin/orchestrator" /usr/bin/template-manager
    chmod +x /usr/bin/orchestrator /usr/bin/template-manager
}

# ===================== 卸载函数 =====================
uninstall_postgres() {
    info "开始卸载 PostgreSQL..."
    if [ "$DEPLOY_MODE" = "k8s" ]; then
        # k8s 模式：PostgreSQL 由 helm 管理，stop() 中 helm uninstall 已删除 Deployment
        # 仅清理宿主机持久化数据（可选，默认保留以防误删）
        if [ -d /data/postgres ]; then
            warn "K8S 模式 PostgreSQL 数据目录 /data/postgres 仍保留，如需清除请手动 rm -rf"
        fi
    else
        # nomad 模式：清理 Docker 容器
        $DOCKER_CMD stop postgres || true
        $DOCKER_CMD rm postgres || true
        $DOCKER_CMD rmi postgres:latest || true
        pkill postgres 2>/dev/null || true
    fi
    success "PostgreSQL 卸载完成"
}

uninstall_docker() {
    info "开始卸载 Docker..."
    dnf remove docker -y
    success "Docker 卸载完成"
}

uninstall_minio() {
    info "开始卸载 MinIO..."
    systemctl stop minio || true
    systemctl disable minio || true
    rm -rf /usr/local/bin/minio /etc/default/minio /etc/systemd/system/minio.service /root/data/minio || true
    success "MinIO 卸载完成"
}

uninstall_harbor() {
    info "开始卸载 Harbor..."

    # Harbor 是容器集群，先停止并删除容器
    cd "$WORK_DIR/harbor" 2>/dev/null || {
        warn "Harbor 目录不存在，跳过卸载"
        return
    }

    info "停止 Harbor 容器..."
    $DOCKER_COMPOSE_CMD down -v 2>/dev/null || true

    # 删除 Harbor 相关镜像
    info "删除 Harbor 相关镜像..."
    local harbor_images
    harbor_images=$(docker images --format "{{.Repository}}:{{.Tag}}" | grep -E 'harbor|goharbor')
    if [ -n "$harbor_images" ]; then
        while IFS= read -r image; do
            info "删除镜像: $image"
            docker rmi -f "$image" 2>/dev/null || true
        done <<< "$harbor_images"
        success "Harbor 镜像清理完成"
    else
        info "未找到 Harbor 相关镜像"
    fi

    # 删除 Harbor 安装目录
    info "删除 Harbor 安装目录: $WORK_DIR/harbor"
    rm -rf "$WORK_DIR/harbor" 2>/dev/null || true

    # 清理 Harbor 相关的容器运行时证书配置
    info "清理 Docker/Containerd 证书配置: /etc/docker/certs.d/$HOST_IP:$HARBOR_HTTPS_PORT 及 /etc/containerd/certs.d/$HOST_IP:$HARBOR_HTTPS_PORT"
    rm -rf /etc/docker/certs.d/${HOST_IP}:$HARBOR_HTTPS_PORT 2>/dev/null || true
    rm -rf /etc/containerd/certs.d/${HOST_IP}:*$HARBOR_HTTPS_PORT 2>/dev/null || true
    info "删除 Harbor 证书目录: $HARBOR_CERTS_DIR"
    rm -rf $HARBOR_CERTS_DIR 2>/dev/null || true
    # 删除 Harbor 数据目录
    info "删除 Harbor 数据目录: $HARBOR_DATA_DIR"
    rm -rf "$HARBOR_DATA_DIR" 2>/dev/null || true

    success "Harbor 卸载完成"
}

uninstall_nomad() {
    info "开始卸载 Nomad/Consul..."
    local uninstall_nomad_sh="/opt/e2b-infra/uninstall-nomad.sh"
    local uninstall_consul_sh="/opt/e2b-infra/uninstall-consul.sh"

    if [ -f "$uninstall_nomad_sh" ]; then
        bash "$uninstall_nomad_sh" --force || warn "卸载 Nomad 失败"
    else
        warn "Nomad 卸载脚本不存在：$uninstall_nomad_sh"
    fi

    if [ -f "$uninstall_consul_sh" ]; then
        bash "$uninstall_consul_sh" --force || warn "卸载 Consul 失败"
    else
        warn "Consul 卸载脚本不存在：$uninstall_consul_sh"
    fi
    pkill nomad
    pkill consul
}

uninstall_e2b() {
    uninstall_nomad
    pip uninstall e2b==2.15.3 -y
    pip uninstall e2b_code_interpreter==2.4.1 -y
    info "remove nbd"
    modprobe -r nbd
}

# 清理 Firecracker 运行时目录（由 init-client.sh 创建）
uninstall_fc_directories() {
    info "开始清理 Firecracker 运行时目录..."
    local fc_dirs=(
        "/fc-envd"
        "/fc-kernels"
        "/fc-versions"
        "/fc-vm"
    )
    local d
    for d in "${fc_dirs[@]}"; do
        if [ -d "$d" ]; then
            info "删除目录: $d"
            rm -rf "$d" || warn "删除 $d 失败（可能权限不足）"
        else
            info "目录不存在，跳过: $d"
        fi
    done
    success "Firecracker 运行时目录清理完成"
}

# ===================== 主函数 =====================
install() {
    info "===== 开始批量安装组件（本机IP：$HOST_IP）====="
    install_base_packages
    command -v setenforce >/dev/null 2>&1 && setenforce 0
    install_e2b
    if [ "$DEPLOY_MODE" = "nomad" ]; then
        install_docker
        install_consul
        install_nomad
    fi
    pull_docker_images
    # PostgreSQL：nomad 模式通过 Docker 容器启动；k8s 模式由 helm postgres.yaml 部署为 Pod
    if [ "$DEPLOY_MODE" = "nomad" ]; then
        install_postgres
    fi
    # install_minio
    install_harbor
    install_harbor_certs
    
    success "===== 所有组件安装完成 ====="
}

# 在k8s其他节点部署template-manager
install_client() {
    info "===== 开始批量安装客户端组件（本机IP：$HOST_IP）====="
    install_base_packages
    command -v setenforce >/dev/null 2>&1 && setenforce 0
    pip install e2b==2.20.0
    pip install e2b_code_interpreter==2.4.1
    python3 $E2B_DIR/patch_e2b.py
    
    cp -fv "$DEP_DIR/init-client.sh" "$E2B_DIR/init-client.sh"
    success "===== 所有客户端组件安装完成 ====="
}

start_client() {
    info "===== 开始启动客户端组件（本机IP：$HOST_IP）====="
    bash "$E2B_DIR/init-client.sh" api "$HOST_IP" || error "启动客户端组件失败"
    systemctl restart kubelet || true
    success "===== 所有客户端组件启动完成 ====="
}


download_packages() {
    local pkg_dir="$DEP_DIR"
    # 架构相关变量映射
    local docker_arch nomad_arch consul_arch fc_arch oe_arch harbor_pkg
    case "$ARCH" in
        x86_64)
            docker_arch="x86_64"
            nomad_arch="amd64"
            consul_arch="amd64"
            fc_arch="x86_64"
            oe_arch="x86_64"
            harbor_pkg="harbor-offline-installer-v2.13.0.tgz"
            harbor_url="https://github.com/goharbor/harbor/releases/download/v2.13.0"
            ;;
        arm64)
            docker_arch="aarch64"
            nomad_arch="arm64"
            consul_arch="arm64"
            fc_arch="aarch64"
            oe_arch="aarch64"
            harbor_pkg="harbor-offline-installer-aarch64-v2.13.0.tgz"
            harbor_url="https://github.com/wise2c-devops/build-harbor-aarch64/releases/download/v2.13.0"
            ;;
        *)
            error "不支持的架构 $ARCH，仅支持 x86_64/arm64"
            ;;
    esac

    echo "开始下载 $ARCH 架构软件包..."

    # 定义下载列表: URL|目标文件名|描述
    local downloads=(
        "https://download.docker.com/linux/static/stable/${docker_arch}/docker-25.0.5.tgz|docker-25.0.5.tgz|docker"
        "https://github.com/docker/compose/releases/download/v2.40.2/docker-compose-linux-${docker_arch}|docker-compose-linux-${docker_arch}|docker-compose"
        "https://releases.hashicorp.com/nomad/1.10.4/nomad_1.10.4_linux_${nomad_arch}.zip|nomad_1.10.4_linux_${nomad_arch}.zip|nomad"
        "https://releases.hashicorp.com/consul/1.21.4/consul_1.21.4_linux_${consul_arch}.zip|consul_1.21.4_linux_${consul_arch}.zip|consul"
        "https://github.com/firecracker-microvm/firecracker/releases/download/v1.13.1/firecracker-v1.13.1-${fc_arch}.tgz|firecracker-v1.13.1-${fc_arch}.tgz|firecracker"
        "https://dl-cdn.openeuler.openatom.cn/openEuler-24.03-LTS-SP3/docker_img/${oe_arch}/openEuler-docker.${oe_arch}.tar.xz|openEuler-docker.${oe_arch}.tar.xz|docker"
        "${harbor_url}/${harbor_pkg}|${harbor_pkg}|harbor"
    )

    local entry url filename desc
    for entry in "${downloads[@]}"; do
        url="${entry%%|*}"
        local rest="${entry#*|}"
        filename="${rest%%|*}"
        desc="${rest##*|}"
        echo "正在下载 $desc: $url"
        wget -q --show-progress --no-check-certificate "$url" -O "$pkg_dir/$filename" || error "$desc 下载失败"
    done

    # e2b-webhook 镜像 tar 包（K8S 模式下启用 webhook 时需要）
    # 下载后由 pull_docker_images 通过 docker load 导入
    if [ "$DEPLOY_MODE" = "k8s" ]; then
        local webhook_url="https://gitcode.com/fly_1997/e2b-webhook/releases/download/1.0.0/e2b-webhook.tar"
        local webhook_pkg="e2b-webhook.tar"
        echo "正在下载 e2b-webhook: $webhook_url"
        wget -q --show-progress --no-check-certificate "$webhook_url" -O "$pkg_dir/$webhook_pkg" || error "e2b-webhook 下载失败"
    fi

    echo "所有软件包下载完成，保存路径：$pkg_dir"
    return 0
}

uninstall_docker_resources() {
    info "清理脚本创建的 Docker 容器和镜像..."
    
    # 定义脚本创建的容器名称列表
    local script_containers=(
        "temp-images"
    )
    
    # 停止并删除脚本创建的容器
    for container in "${script_containers[@]}"; do
        if $DOCKER_CMD ps -a --format "{{.Names}}" | grep -q "^${container}$"; then
            info "停止并删除容器: $container"
            $DOCKER_CMD stop "$container" 2>/dev/null || true
            $DOCKER_CMD rm -f "$container" 2>/dev/null || true
            success "容器 $container 已清理"
        else
            info "容器 $container 不存在，跳过"
        fi
    done
    
    # 定义脚本拉取的镜像列表（根据 pull_docker_images 函数中的定义）
    local script_images=(
        "redis:7.4.4-alpine"
        "debian:bookworm-slim"
        "api:latest"
        "client-proxy:latest"
    )
    # K8S 模式额外清理
    if [ "$DEPLOY_MODE" = "k8s" ]; then
        script_images+=("busybox:latest" "ubuntu:24.04")
    fi
    
    # 删除脚本拉取的镜像
    for image in "${script_images[@]}"; do
        if $DOCKER_CMD images --format "{{.Repository}}:{{.Tag}}" | grep -q "^${image}$"; then
            info "删除镜像: $image"
            $DOCKER_CMD rmi -f "$image" 2>/dev/null || true
            success "镜像 $image 已清理"
        else
            info "镜像 $image 不存在，跳过"
        fi
    done
    
    # 清理推送到 Harbor 的 orchestration 镜像（如 193.12.7.2:30443/e2b-orchestration/o*）
    local registry_prefix="${SERVER_IP}:${HARBOR_HTTPS_PORT}/${REGISTRY_PROJECT}"
    info "清理 Harbor 镜像: ${registry_prefix}/..."
    local harbor_images
    harbor_images=$($DOCKER_CMD images --format "{{.Repository}}:{{.Tag}}" | grep "^${registry_prefix}/" || true)
    if [ -n "$harbor_images" ]; then
        while IFS= read -r image; do
            [ -n "$image" ] || continue
            info "删除 Harbor 镜像: $image"
            $DOCKER_CMD rmi -f "$image" 2>/dev/null || true
        done <<< "$harbor_images"
        success "Harbor 镜像已清理"
    else
        info "未找到 Harbor 镜像，跳过"
    fi

    # 清理悬空镜像
    info "清理悬空镜像..."
    $DOCKER_CMD image prune -f 2>/dev/null || true
}

uninstall() {
    info "===== 开始批量卸载组件 ====="
    stop
    uninstall_e2b
    uninstall_fc_directories
    uninstall_harbor
    # uninstall_minio
    uninstall_postgres
    #uninstall_docker
    uninstall_docker_resources
    success "===== 所有组件卸载完成 ====="
}


# ===================== 追加 Nomad 客户端配置到 default.hcl =====================
append_nomad_client_config() {
    local nomad_config_file="/etc/nomad.d/default.hcl"
    local node_pool_name="api"  # 可根据实际需求修改节点池名称
    
    # 1. 定义要追加的配置内容（替换变量）
    local client_config
    client_config=$(cat <<EOF
# === 自动追加的客户端配置 ===
client {
enabled = true
node_pool = "$node_pool_name"
max_kill_timeout = "10m"
meta {
    node_pool = "$node_pool_name"
}
}

plugin "raw_exec" {
config {
    enabled = true
}
}
# === 客户端配置结束 ===
EOF
)

    # 2. 检查配置是否已存在（避免重复追加）
    if grep -q "# === 自动追加的客户端配置 ===" "$nomad_config_file" 2>/dev/null; then
        info "Nomad 客户端配置已存在于 $nomad_config_file，跳过追加"
        return 0
    fi

    # 3. 确保配置文件目录存在
    mkdir -p "$(dirname "$nomad_config_file")" || error "创建 Nomad 配置目录失败"

    # 4. 追加配置到文件末尾
    info "追加 Nomad 客户端配置到 $nomad_config_file..."
    echo -e "\n$client_config" >> "$nomad_config_file" || error "追加配置到 $nomad_config_file 失败"
}

# ===================== 通用等待端口启动函数 =====================
# 函数名：wait_for_port
# 功能：等待指定端口启动（监听状态），直到端口可用或超时
# 参数说明：
#   $1 - 目标端口号（必填，如4646）
#   $2 - 端口类型（可选，默认tcp，支持tcp/udp/all）
#   $3 - 检查间隔（可选，默认2秒，单位：秒）
#   $4 - 超时时间（可选，默认300秒，0表示永不超时，单位：秒）
# 返回值：
#   0 - 端口成功启动
#   1 - 超时/参数错误
# 使用示例：
#   wait_for_port 4646 tcp 2 300  # 等待TCP 4646端口，间隔2秒，超时300秒
#   wait_for_port 8080 udp 1 0    # 等待UDP 8080端口，间隔1秒，永不超时
# =================================================================
wait_for_port() {
    # 解析参数（设置默认值）
    local TARGET_PORT=${1:?"参数错误：必须指定目标端口号！"}  # 必填参数，无则报错
    local PORT_TYPE=${2:-tcp}
    local CHECK_INTERVAL=${3:-2}
    local TIMEOUT_SECONDS=${4:-300}

    # 校验参数合法性
    if ! [[ "$TARGET_PORT" =~ ^[0-9]+$ ]]; then
        echo "❌ 错误：端口号必须是数字（当前值：$TARGET_PORT）"
        return 1
    fi
    if ! [[ "$PORT_TYPE" =~ ^(tcp|udp|all)$ ]]; then
        echo "❌ 错误：端口类型只能是 tcp/udp/all（当前值：$PORT_TYPE）"
        return 1
    fi
    if ! [[ "$CHECK_INTERVAL" =~ ^[0-9]+$ ]]; then
        echo "❌ 错误：检查间隔必须是数字（当前值：$CHECK_INTERVAL）"
        return 1
    fi
    if ! [[ "$TIMEOUT_SECONDS" =~ ^[0-9]+$ ]]; then
        echo "❌ 错误：超时时间必须是数字（当前值：$TIMEOUT_SECONDS）"
        return 1
    fi

    # 初始化变量
    local start_time
    start_time=$(date +%s)
    local elapsed_time=0

    # 内部函数：检查端口监听状态
    check_port_listening() {
        local port=$1
        local type=$2
        case $type in
            tcp)  ss -tln | grep -q ":$port\b" ;;
            udp)  ss -uln | grep -q ":$port\b" ;;
            all)  ss -tuln | grep -q ":$port\b" ;;
        esac
        return $?
    }

    # 打印启动信息
    echo "========================================"
    echo "开始等待 $PORT_TYPE 端口 $TARGET_PORT 启动..."
    echo "检查间隔：$CHECK_INTERVAL 秒 | 超时时间：${TIMEOUT_SECONDS:-永不超时} 秒"
    echo "========================================"

    # 循环检查端口
    while true; do
        # 检查端口是否已启动
        if check_port_listening "$TARGET_PORT" "$PORT_TYPE"; then
            echo -e "\n✅ 端口 $TARGET_PORT ($PORT_TYPE) 已成功启动！"
            return 0
        fi

        # 超时判断（0表示永不超时）
        if [ "$TIMEOUT_SECONDS" -ne 0 ]; then
            local current_time
            current_time=$(date +%s)
            elapsed_time=$((current_time - start_time))
            if [ "$elapsed_time" -ge "$TIMEOUT_SECONDS" ]; then
                echo -e "\n❌ 超时错误：等待 $TIMEOUT_SECONDS 秒后，端口 $TARGET_PORT ($PORT_TYPE) 仍未启动！"
                return 1
            fi
            # 打印等待进度
            echo -n "⏳ 已等待 $elapsed_time 秒，端口仍未启动... "
            echo "剩余超时时间：$((TIMEOUT_SECONDS - elapsed_time)) 秒"
        else
            echo "⏳ 端口未启动，继续等待...（永不超时）"
        fi

        # 等待指定间隔后重试
        sleep "$CHECK_INTERVAL"
    done
}

start_postgres() {
    info "开始启动 PostgreSQL..."
    $DOCKER_CMD start postgres || error "启动 PostgreSQL 容器失败"
    sleep 5
    local pg_container="postgres"
    local pg_user="postgres"
    local pg_db="mydatabase"
    $DOCKER_CMD exec -it "$pg_container" psql -U "$pg_user" -d "$pg_db" -c "\q" > /dev/null 2>&1
    if [ $? -eq 0 ]; then
        success "PostgreSQL 启动成功！可正常连接和使用"
    else
        echo "--- 最近20行错误日志 ---"
        $DOCKER_CMD logs --tail 20 "$pg_container"
        error "PostgreSQL 启动失败！无法连接数据库"
    fi
}

start_harbor() {
    cd "$WORK_DIR/harbor" || error "进入 Harbor 目录失败"

    # 检查 Harbor 是否已经启动，若已启动且健康则跳过重启
    if [ -n "$($DOCKER_COMPOSE_CMD ps -q 2>/dev/null)" ]; then
        local harbor_url
        harbor_url=$(harbor_get_url)
        local health_json
        health_json=$(set +e; curl -sk "${harbor_url}/api/v2.0/health" 2>/dev/null)
        if echo "$health_json" | grep -q '"status":"healthy"'; then
            success "Harbor 已启动且状态健康，跳过重启"
            return 0
        else
            warn "Harbor 容器在运行但状态不健康，将重新启动..."
            $DOCKER_COMPOSE_CMD down -v 2>/dev/null || true
        fi
    fi

    # 修改 Harbor 配置文件
    echo ">>> 正在将 Harbor 域名/IP 修改为: $HOST_IP"
    local harbor_config="harbor.yml"
    if [ ! -f "$harbor_config" ]; then
        cp -f harbor.yml.tmpl harbor.yml || error "生成 harbor.yml 失败"
    fi
    sed -i "s/^hostname: .*/hostname: $HOST_IP/" "$harbor_config" || error "修改 hostname 失败"
    mkdir -p "$HARBOR_DATA_DIR" || error "创建 Harbor 数据目录失败"
    sed -i "s|^#*data_volume: .*|data_volume: $HARBOR_DATA_DIR|" "$harbor_config" || error "修改 data_volume 失败"
    # 根据当前使用的容器运行时修改 harbor/install.sh 和 prepare
    # 若使用 nerdctl（通常为 k8s 节点），则需跳过 install.sh 自带的 docker 检查
    sed -i "s|^DOCKER_COMPOSE=.*$|DOCKER_COMPOSE='$DOCKER_COMPOSE_CMD'|" install.sh
    sed -i "s|docker load|$DOCKER_CMD load|g" install.sh
    if [ -f "prepare" ]; then
        sed -i "s|docker run|$DOCKER_CMD run|g" prepare
    fi
    if [ "$DOCKER_CMD" = "nerdctl" ]; then
        sed -i "/check_docker/d" install.sh
        sed -i "/check_dockercompose/d" install.sh
    fi

    local enable_http=false
    local enable_https=false
    case "$HARBOR_PROTOCOL" in
        http)  enable_http=true ;;
        https) enable_https=true ;;
        both)  enable_http=true; enable_https=true ;;
        *)     enable_http=true; enable_https=true ;;
    esac
    info "Harbor 协议: $HARBOR_PROTOCOL (HTTP=$enable_http, HTTPS=$enable_https)"

    # --- 配置 HTTP ---
    harbor_set_http_port "$harbor_config" "$HARBOR_HTTP_PORT"
    info "Harbor HTTP 端口已设置为: $HARBOR_HTTP_PORT"

    # --- 配置 HTTPS ---
    if [ "$enable_https" = true ]; then
        info "配置 Harbor HTTPS (端口: $HARBOR_HTTPS_PORT)"
        sed -i 's/^#\(https:\)/\1/' "$harbor_config"
        sed -i "s/^  #*port: 443/  port: $HARBOR_HTTPS_PORT/" "$harbor_config"
        sed -i "s|^  #*certificate: .*|  certificate: /etc/harbor/certs/harbor.crt|" "$harbor_config"
        sed -i "s|^  #*private_key: .*|  private_key: /etc/harbor/certs/harbor.key|" "$harbor_config"

        # 根据容器运行时配置 HTTPS 证书信任
        # nerdctl（通常为 k8s 节点）直接配置 containerd；docker 则配置 Docker daemon
        if [ "$DEPLOY_MODE" = "k8s" ]; then
            # 1. 准备 certs.d 目录与 hosts.toml（containerd 现代配置方式）
            mkdir -p "/etc/containerd/certs.d/$HOST_IP:$HARBOR_HTTPS_PORT"
            cp -f "$HARBOR_CERTS_DIR/harbor.crt" "/etc/containerd/certs.d/$HOST_IP:$HARBOR_HTTPS_PORT/harbor.crt" || error "复制 harbor.crt 失败"
            cp -f "$HARBOR_CERTS_DIR/harbor.key" "/etc/containerd/certs.d/$HOST_IP:$HARBOR_HTTPS_PORT/harbor.key" || error "复制 harbor.key 失败"
            cat > "/etc/containerd/certs.d/$HOST_IP:$HARBOR_HTTPS_PORT/hosts.toml" << EOF
server = "https://$HOST_IP:$HARBOR_HTTPS_PORT"
[host."https://$HOST_IP:$HARBOR_HTTPS_PORT"]
  capabilities = ["pull", "resolve", "push"]
  ca = "/etc/containerd/certs.d/$HOST_IP:$HARBOR_HTTPS_PORT/harbor.crt"
  skip_verify = false
EOF

            # 2. 检查 containerd config.toml 是否已配置 config_path 指向 certs.d 目录
            #    只有配置了 config_path，上面的 hosts.toml 才会生效
            info "检查 containerd config_path 配置..."
            local containerd_config="/etc/containerd/config.toml"
            [ ! -f "$containerd_config" ] && containerd config default > "$containerd_config"
            cp "$containerd_config" "$containerd_config.bak"
            local need_restart=false
            if ! grep -q 'config_path = "/etc/containerd/certs.d"' "$containerd_config"; then
                info "containerd 未配置 config_path，正在添加..."
                # 确保存在 [plugins."io.containerd.grpc.v1.cri".registry] 段
                if ! grep -q '\[plugins."io.containerd.grpc.v1.cri".registry\]' "$containerd_config"; then
                    cat >> "$containerd_config" << EOF

[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d"
EOF
                else
                    # 段已存在但缺少 config_path，在段内追加
                    sed -i '/\[plugins."io.containerd.grpc.v1.cri".registry\]/a\  config_path = "/etc/containerd/certs.d"' "$containerd_config"
                fi
                need_restart=true
            else
                info "containerd config_path 已配置，跳过"
            fi

            # 3. 清理旧的 registry.mirrors/configs 配置（与 config_path 方式冲突）
            if grep -q '\[plugins."io.containerd.grpc.v1.cri".registry.mirrors.' "$containerd_config"; then
                warn "发现旧的 registry.mirrors 配置，正在清理（与 config_path 方式冲突）..."
                sed -i '/\[plugins."io.containerd.grpc.v1.cri".registry.mirrors\./,/^$/d' "$containerd_config"
                sed -i '/\[plugins."io.containerd.grpc.v1.cri".registry.configs\./,/^$/d' "$containerd_config"
                need_restart=true
            fi

            if [ "$need_restart" = true ]; then
                systemctl daemon-reload
                systemctl restart containerd
            fi
        fi
        if [ "$DOCKER_CMD" = "docker" ]; then
            # Docker 模式：配置 Docker 信任 Harbor HTTPS 证书
            info "配置 Docker 信任 Harbor HTTPS 证书..."
            mkdir -p "/etc/docker/certs.d/$HOST_IP:$HARBOR_HTTPS_PORT"
            cp -f "$HARBOR_CERTS_DIR/harbor.crt" "/etc/docker/certs.d/$HOST_IP:$HARBOR_HTTPS_PORT/ca.crt" || error "复制 ca.crt 失败"
            systemctl restart "$CONTAINERD_SERVICE"
            info "Docker Harbor HTTPS 证书已配置: /etc/docker/certs.d/$HOST_IP:$HARBOR_HTTPS_PORT/ca.crt"
        fi
    else
        sed -i '/^https:/,/^$/ s/^/#/' "$harbor_config"
    fi

    # --- 配置 Docker insecure registries (HTTP 模式需要) ---
    if [ "$enable_http" = true ]; then
        local daemon_json="/etc/docker/daemon.json"
        [ ! -f "$daemon_json" ] && echo "{}" > "$daemon_json"
        local registry_entry="$HOST_IP:$HARBOR_HTTP_PORT"
        if jq -e --arg ip "$registry_entry" '."insecure-registries" // [] | index($ip)' "$daemon_json" >/dev/null 2>&1; then
            info "insecure-registries 已包含 $registry_entry，跳过"
        else
            local tmp
            tmp=$(mktemp)
            jq --arg ip "$registry_entry" '."insecure-registries" += [$ip] | ."insecure-registries" |= unique' "$daemon_json" > "$tmp" && mv "$tmp" "$daemon_json"
            systemctl daemon-reload
            systemctl restart "$CONTAINERD_SERVICE"
            info "Docker insecure-registries 已添加: $registry_entry"
        fi
    fi

    # 修正 /etc/hosts (追加 harbor 映射)
    echo ">>> 正在配置 /etc/hosts..."
    if ! grep -q "harbor" /etc/hosts; then
        echo "127.0.0.1 harbor" | sudo tee -a /etc/hosts
        echo "[OK] 已追加 127.0.0.1 harbor"
    else
        echo "[SKIP] harbor 已存在于 /etc/hosts"
    fi

    bash install.sh || error "Harbor 安装脚本执行失败"
}

harbor_get_url() {
    case "${HARBOR_PROTOCOL:-https}" in
        http)  echo "http://$HOST_IP:$HARBOR_HTTP_PORT" ;;
        *)     echo "https://$HOST_IP:$HARBOR_HTTPS_PORT" ;;
    esac
}

harbor_wait_healthy() {
    local harbor_url
    harbor_url=$(harbor_get_url)
    info "等待 Harbor 就绪: $harbor_url ..."
    while true; do
        local health_json
        health_json=$(set +e; curl -sk "${harbor_url}/api/v2.0/health" 2>/dev/null)
        if echo "$health_json" | grep -q '"status":"healthy"'; then
            info "Harbor 已就绪"
            local comp
            for comp in core database jobservice portal redis registry registryctl; do
                if ! echo "$health_json" | grep -q "\"name\":\"$comp\",\"status\":\"healthy\""; then
                    warn "Harbor 组件 $comp 未就绪（不影响整体状态）"
                fi
            done
            break
        fi
        warn "Harbor 未就绪，等待 10 秒后重试..."
        sleep 10
    done
}

harbor_login() {
    local harbor_url
    harbor_url=$(harbor_get_url)
    info "登录 Harbor 仓库：$harbor_url ..."
    if ! $DOCKER_CMD login -u "$HARBOR_USER" -p "$HARBOR_PASSWORD" "$harbor_url"; then
        warn "Harbor 登录失败（可能凭证错误或Harbor未就绪），继续执行部署..."
    fi
}

harbor_create_project() {
    local project_name="${1:-e2b-orchestration}"
    local harbor_url
    harbor_url=$(harbor_get_url)
    info "创建 Harbor 项目: $project_name"
    local resp
    resp=$(curl -sk -o /dev/null -w "%{http_code}" \
        -X POST "${harbor_url}/api/v2.0/projects" \
        -u "${HARBOR_USER}:${HARBOR_PASSWORD}" \
        -H "Content-Type: application/json" \
        -d "{\"project_name\": \"${project_name}\", \"public\": true}")
    if [ "$resp" -eq 201 ] || [ "$resp" -eq 409 ]; then
        info "Harbor 项目 $project_name 已就绪 (HTTP $resp)"
    else
        warn "Harbor 项目 $project_name 创建返回 HTTP $resp"
    fi
}

harbor_set_http_port() {
    local config_file="${1:?请提供 Harbor 配置文件路径}"
    local http_port="${2:?请提供 HTTP 端口号}"
    
    # 修改 HTTP 端口（确保只修改 http: 块下的 port）
    # 先检查 http: 块下的 port 是否存在，不存在则添加
    if grep -q "^http:" "$config_file" && ! grep "^http:" -A 5 "$config_file" | grep -q "port:"; then
        # http: 块存在但没有 port，在 http: 后添加
        sed -i "/^http:/a\  port: $http_port" "$config_file"
    elif grep -q "^http:" "$config_file"; then
        # http: 块存在且有 port，精确匹配 http: 块下的 port 并替换
        awk -v new_port="$http_port" '
            /^http:/ { in_http=1 }
            /^https:/ { in_http=0 }
            in_http && /^  port:/ { sub(/[0-9]+$/, new_port); in_http=0 }
            { print }
        ' "$config_file" > "${config_file}.tmp" && mv "${config_file}.tmp" "$config_file"
    else
        # http: 块不存在，添加整个 http: 块
        sed -i "/^hostname:/a\\nhttp:\\n  port: $http_port" "$config_file"
    fi
}

deploy_harbor() {
    start_harbor
    harbor_wait_healthy
    harbor_login
    harbor_create_project
}

start() {
    info "开始启动 e2b-infra 服务（本机IP：$HOST_IP）..."
    bash "$E2B_DIR/init-client.sh" || error "初始化客户端失败"

    deploy_harbor

    # 检查关键目录/文件
    [ ! -d "$E2B_DIR" ] && error "e2b-infra 目录不存在：$E2B_DIR"

    cd "$E2B_DIR" || error "进入 $E2B_DIR 目录失败"

    if [ "$DEPLOY_MODE" = "k8s" ]; then
        # 重启 K8S 应用大页
        info "重启 K8S 应用大页..."
        systemctl restart kubelet || error "重启 K8S 应用大页失败"

        # 检查 kubectl 是否安装
        if ! command -v kubectl &> /dev/null; then
            error "kubectl 未安装，请先安装 Kubernetes 命令行工具"
        fi

        # 检查 K8S 集群状态
        info "检查 K8S 集群状态..."
        if ! kubectl cluster-info > /dev/null 2>&1; then
            error "K8S 集群未就绪，请确保集群已正确配置"
        fi

        # 使用传入的节点名，如果未传入则使用默认的第一个节点
        local node_name
        if [ -n "$K8S_NODE_NAME" ]; then
            node_name="$K8S_NODE_NAME"
            info "使用指定的节点名：$node_name"
        else
            node_name=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
            info "未指定节点名，使用默认节点：$node_name"
        fi

        # 检查节点是否存在
        if ! kubectl get node "$node_name" > /dev/null 2>&1; then
            error "节点 $node_name 不存在，请检查节点名是否正确"
        fi

        echo "当前节点名称：$node_name"
        kubectl label node "$node_name" node-role.kubernetes.io/sandbox=true --overwrite
        kubectl label node "$node_name" node-role.kubernetes.io/api= --overwrite
        kubectl label node "$node_name" node-role.kubernetes.io/postgres= --overwrite
        bash deploy.sh --type k8s || error "K8S 模式部署失败"
        success "K8S 模式启动完成！"

    else
        start_postgres
        # 检查 nomad 和 consul 是否已安装（systemd unit 文件存在）
        if [[ -f /etc/systemd/system/nomad.service && -f /etc/systemd/system/consul.service ]]; then
            info "Nomad 和 Consul 已安装，通过 systemctl 启动..."
            systemctl daemon-reload
            systemctl restart consul || error "启动 Consul 服务失败"
            systemctl restart nomad || error "启动 Nomad 服务失败"
        else
            info "Nomad/Consul 尚未安装 systemd 服务，通过 start-server.sh 启动..."
            bash "$E2B_DIR/start-server.sh" "$HOST_IP" || error "启动 Nomad 服务端失败"
        fi

        append_nomad_client_config

        # 重启 nomad 服务
        systemctl restart nomad

        wait_for_port 4646 tcp 1 0
        # 删掉服务
        rm -fv "$E2B_DIR/bin/orchestrator.Dockerfile"
        info "执行部署脚本..."
        bash "$E2B_DIR/deploy.sh" || error "执行部署脚本失败"
        iptable_clean
        success "e2b-infra 服务启动完成！所有组件健康检查通过"
    fi
    # iptables -t nat -A PREROUTING -p tcp --dport 80 -j REDIRECT --to-port 3002
    iptables -t nat -A OUTPUT -p tcp -d 127.0.0.1 --dport 80 -j REDIRECT --to-port 3002
}

iptable_clean() {
    source /opt/e2b-infra/.env
    local nomad_token="$NOMAD_ACL_TOKEN"

    local jobs
    jobs=$(nomad job status -token "$nomad_token" -json | jq -r '.[].Allocations[].JobID')
    local job
    for job in $jobs; do
        nomad job stop -token "$nomad_token" "$job"
    done
    iptables -F
    systemctl restart "$CONTAINERD_SERVICE"
    bash "$WORK_DIR/harbor/install.sh" || error "Harbor 安装脚本执行失败"
    bash "$E2B_DIR/deploy.sh" || error "执行部署脚本失败"
}

deploy() {
    info "执行部署脚本..."
    bash "$E2B_DIR/deploy.sh" || error "执行部署脚本失败"
}

# Nomad 任务管理
# 支持的任务名: redis, template-manager, edge, api, all
NOMAD_JOBS=("redis" "template-manager" "edge" "api")

nomad_get_token() {
    source /opt/e2b-infra/.env 2>/dev/null
    echo "${NOMAD_ACL_TOKEN:-}"
}

nomad_job_list() {
    local token
    token=$(nomad_get_token)
    info "Nomad 任务列表:"
    nomad job status -token "$token" 2>/dev/null || warn "无法获取 Nomad 任务列表"
}

nomad_job_deploy() {
    local job="$1"
    local token
    token=$(nomad_get_token)
    local rendered_dir="$E2B_DIR/rendered"

    if [ ! -d "$rendered_dir" ]; then
        error "渲染目录不存在: $rendered_dir，请先执行 --start 完成初始部署"
    fi

    if [ "$job" = "all" ]; then
        for j in "${NOMAD_JOBS[@]}"; do
            nomad_job_deploy "$j"
        done
        return
    fi

    local hcl_file="$rendered_dir/${job}.hcl"
    if [ ! -f "$hcl_file" ]; then
        error "任务文件不存在: $hcl_file"
    fi

    info "部署 Nomad 任务: $job"
    nomad job run -token "$token" "$hcl_file" || error "部署任务失败: $job"
    success "任务 $job 部署完成"
}

nomad_job_stop() {
    local job="$1"
    local token
    token=$(nomad_get_token)

    if [ "$job" = "all" ]; then
        for j in "${NOMAD_JOBS[@]}"; do
            nomad_job_stop "$j"
        done
        return
    fi

    info "停止 Nomad 任务: $job"
    nomad job stop -token "$token" "$job" || warn "停止任务失败: $job"
    success "任务 $job 已停止"
}

nomad_job_start() {
    local job="$1"
    local token
    token=$(nomad_get_token)
    local rendered_dir="$E2B_DIR/rendered"

    if [ "$job" = "all" ]; then
        for j in "${NOMAD_JOBS[@]}"; do
            nomad_job_start "$j"
        done
        return
    fi

    local hcl_file="$rendered_dir/${job}.hcl"
    if [ ! -f "$hcl_file" ]; then
        error "任务文件不存在: $hcl_file"
    fi

    info "启动 Nomad 任务: $job"
    nomad job run -token "$token" "$hcl_file" || error "启动任务失败: $job"
    success "任务 $job 已启动"
}

nomad_job_delete() {
    local job="$1"
    local token
    token=$(nomad_get_token)

    if [ "$job" = "all" ]; then
        for j in "${NOMAD_JOBS[@]}"; do
            nomad_job_delete "$j"
        done
        return
    fi

    info "删除 Nomad 任务: $job"
    nomad job stop -purge -token "$token" "$job" || warn "删除任务失败: $job"
    success "任务 $job 已删除"
}

# 部署 E2B 插件
# 参数: [target] [template] [selector] [namespace]
deploy_plugin() {
    local target="$1"
    local template="$2"
    local selector="$3"
    local namespace="$4"
    
    info "部署 E2B 插件..."
    local plugin_script="$DEP_DIR/deploy-e2b-plugin.sh"
    if [ ! -f "$plugin_script" ]; then
        error "插件部署脚本不存在：$plugin_script"
    fi

    local E2B_HOST_IP="${SERVER_IP}"
    local E2B_API_PORT="${API_PORT}"
    local E2B_ENVD_PORT="${EDGE_PROXY_PORT}"
    local E2B_API_KEY
    E2B_API_KEY=$(python3 -c "import json; print(json.load(open('/root/.e2b/config.json')).get('teamApiKey',''))" 2>/dev/null || true)
    local E2B_TIMEOUT="${E2B_TIMEOUT:-400}"
    local E2B_TEMPLATE="${template:-base}"
    local E2B_TARGET="${target:-}"
    local E2B_K8S_SELECTOR="${selector:-app=openclaw-deploy-for-local-exec}"
    local E2B_K8S_NAMESPACE="${namespace:-default}"
    echo "部署插件参数：target=$E2B_TARGET, template=$E2B_TEMPLATE, selector=$E2B_K8S_SELECTOR, namespace=$E2B_K8S_NAMESPACE"
    if [ "$DEPLOY_MODE" = "k8s" ]; then
        if [ -z "$E2B_TARGET" ]; then
            E2B_TARGET=$(kubectl get pods -n "$E2B_K8S_NAMESPACE" -l "$E2B_K8S_SELECTOR" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        fi
        bash "$plugin_script" k8s \
            "$E2B_TARGET" "$E2B_K8S_NAMESPACE" "$E2B_K8S_SELECTOR" \
            "$E2B_HOST_IP" "$E2B_API_PORT" "$E2B_ENVD_PORT" \
            "$E2B_API_KEY" "$E2B_TIMEOUT" "$E2B_TEMPLATE"
    else
        E2B_TARGET="${E2B_TARGET:-openclaw-deploy-for-local-exec-6d8696984f-8wb5h}"
        bash "$plugin_script" docker \
            "$E2B_TARGET" \
            "$E2B_HOST_IP" "$E2B_API_PORT" "$E2B_ENVD_PORT" \
            "$E2B_API_KEY" "$E2B_TIMEOUT" "$E2B_TEMPLATE"
    fi
}

stop() {
    info "开始停止 e2b-infra 服务..."
    local e2b_dir="/opt/e2b-infra"

    # 检查目录是否存在
    if [ ! -d "$e2b_dir" ]; then
        warn "e2b-infra 目录不存在：$e2b_dir，跳过停止操作"
        return 0
    fi
    if [ "$DEPLOY_MODE" = "nomad" ]; then
        cd "$e2b_dir" || error "进入 $e2b_dir 目录失败"
        systemctl stop nomad
        systemctl stop consul

        local tasks=("redis" "/client-proxy" "/api" "template-manager")
        local task pids remaining_pids
        for task in "${tasks[@]}"; do
            # 查找进程PID：排除当前脚本PID（$$），避免误杀自己
            pids=$(pgrep -f "^$task" | grep -v $$)
            if [ -n "$pids" ]; then
                echo "正在关闭 $task 进程，PID列表：$pids"
                echo "$pids" | xargs -r kill 2>/dev/null
                sleep 1
                remaining_pids=$(pgrep -f "^$task" | grep -v $$)
                if [ -z "$remaining_pids" ]; then
                    success "$task 进程已全部关闭"
                else
                    warn "部分 $task 进程未优雅关闭，强制终止（PID：$remaining_pids）"
                    echo "$remaining_pids" | xargs -r kill -9 2>/dev/null
                fi
            else
                info "未找到运行中的 $task 进程"
            fi
        done

    else
        helm uninstall e2b-api -n e2b 2>/dev/null || true
        # 清理 e2b-webhook 手动创建的 Secret（helm uninstall 不管理这些）
        info "清理 e2b-webhook Secret 资源..."
        kubectl -n e2b delete secret e2b-webhook-tls --ignore-not-found=true 2>/dev/null || true
        kubectl -n e2b delete secret e2b-api-key --ignore-not-found=true 2>/dev/null || true
    fi
    # 关闭harbor容器
    cd "$WORK_DIR/harbor" 2>/dev/null || return 0
    if [ -n "$($DOCKER_COMPOSE_CMD ps -q 2>/dev/null)" ]; then
        info "stopping existing Harbor instance ..."
        $DOCKER_COMPOSE_CMD down -v
    fi

    # 清理创建的template
    rm -rf "$ORCHESTRATOR_DIR/template/*"
    rm -rf /tmp/templates/*
    success "e2b-infra 服务停止完成！"
}

make_images() {
    # 1. 保存原镜像的 Entrypoint 和 Cmd 配置
    local image_name="$1"
    local orig_entry orig_cmd
    orig_entry=$($DOCKER_CMD inspect "$image_name" --format='{{json .Config.Entrypoint}}')
    orig_cmd=$($DOCKER_CMD inspect "$image_name" --format='{{json .Config.Cmd}}')

    echo "原 ENTRYPOINT: $orig_entry"
    echo "原 CMD: $orig_cmd"

    local temp_image="temp-images"
    # 2. 清理并启动临时容器（用 tail 保持运行，覆盖原 entrypoint）
    $DOCKER_CMD rm -f "$temp_image" 2>/dev/null
    $DOCKER_CMD run -d --name "$temp_image" --privileged --network host --entrypoint tail \
        "$image_name" \
        -f /dev/null

    # 3. 进入容器安装组件（自动识别 yum / apt 包管理器）
    $DOCKER_CMD exec "$temp_image" bash -c '
    if command -v yum >/dev/null 2>&1; then
        PKG_MGR="yum"
        PKGS="systemd systemd-sysv openssh-server sudo chrony linuxptp socat curl wget iputils bind-utils iproute nc tcpdump passwd"
        INSTALL_CMD="yum install -y ${PKGS} && yum clean all && rm -rf /var/cache/yum /var/tmp/* /tmp/*"
    elif command -v apt-get >/dev/null 2>&1; then
        PKG_MGR="apt"
        PKGS="systemd systemd-sysv openssh-server sudo chrony linuxptp socat curl wget iputils-ping bind9-utils iproute2 netcat-openbsd tcpdump passwd"
        INSTALL_CMD="apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y ${PKGS} && apt-get clean && rm -rf /var/lib/apt/lists/* /var/tmp/* /tmp/*"
    else
        echo "不支持的包管理器，仅支持 yum / apt" >&2
        exit 1
    fi
    echo "使用包管理器: ${PKG_MGR}"
    eval "${INSTALL_CMD}"
    '

    $DOCKER_CMD exec "$temp_image" bash -c '
        ARCH=$(uname -m)
        case "$ARCH" in
            x86_64)  WS_ARCH="x86_64-unknown-linux-musl" ;;
            aarch64) WS_ARCH="aarch64-unknown-linux-musl" ;;
            *) echo "不支持的架构: $ARCH" >&2; exit 1 ;;
        esac
        echo "下载 websocat for $ARCH ($WS_ARCH)"
        wget -O /usr/local/bin/websocat "https://ghfast.top/https://github.com/vi/websocat/releases/latest/download/websocat.${WS_ARCH}" && \
        chmod a+x /usr/local/bin/websocat && \
        websocat --version'

    # 4. 停止容器并导出导入（关键：恢复原来的 Entrypoint 和 Cmd）
    local harbor_url
    harbor_url=$(harbor_get_url)
    local registry_addr="${harbor_url#*://}"
    $DOCKER_CMD stop "$temp_image"
    $DOCKER_CMD export "$temp_image" | $DOCKER_CMD import \
        --change "ENTRYPOINT $orig_entry" \
        --change "CMD $orig_cmd" \
        - "${registry_addr}/e2b-orchestration/${image_name}"

    # 5. 推送新镜像
    $DOCKER_CMD push "${registry_addr}/e2b-orchestration/${image_name}"

    # 6. 验证配置是否与原镜像一致
    $DOCKER_CMD inspect "${registry_addr}/e2b-orchestration/${image_name}" \
        --format='Entrypoint: {{.Config.Entrypoint}} Cmd: {{.Config.Cmd}}'

    # 7. 清理临时容器
    $DOCKER_CMD rm -f "$temp_image"
}

# =============================================================================
# 单独部署组件函数
# =============================================================================
deploy_component() {
    local comp="$1"
    case "$comp" in
        nomad)
            info "单独部署 Nomad..."
            systemctl stop nomad 2>/dev/null || true
            pkill nomad 2>/dev/null || true
            cd "$E2B_DIR" || error "进入 $E2B_DIR 目录失败"
            bash install-nomad.sh --version "${NOMAD_VERSION}"
            bash run-nomad.sh --server --num-servers "${NUM_SERVERS}" --consul-token "${CONSUL_ACL_TOKEN}" --instance-ip-address "${HOST_IP}"
            append_nomad_client_config
            systemctl restart nomad 2>/dev/null || true
            wait_for_port "$NOMAD_PORT"
            success "Nomad 部署完成，监听端口 $NOMAD_PORT"
            ;;
        consul)
            info "单独部署 Consul..."
            systemctl stop consul 2>/dev/null || true
            pkill consul 2>/dev/null || true
            cd "$E2B_DIR" || error "进入 $E2B_DIR 目录失败"
            bash install-consul.sh --version "${CONSUL_VERSION}"
            bash run-consul.sh --server --server-ips "${SERVER_IPS}" --instance-ip-address "${HOST_IP}"
            systemctl restart consul 2>/dev/null || true
            success "Consul 部署完成，监听端口 $CONSUL_HTTP_PORT"
            ;;
        postgres)
            info "单独部署 PostgreSQL..."
            if [ "$DEPLOY_MODE" = "k8s" ]; then
                # k8s 模式：重启 postgres Deployment（helm release 管理的资源）
                kubectl rollout restart deployment postgres -n e2b || error "重启 PostgreSQL Deployment 失败"
                success "PostgreSQL Deployment 重启完成"
            else
                install_postgres
                success "PostgreSQL 部署完成，监听端口 $PG_PORT"
            fi
            ;;
        docker)
            info "单独部署 Docker & Docker Compose..."
            install_docker
            success "Docker & Docker Compose 部署完成"
            ;;
        harbor)
            info "单独部署 Harbor..."
            install_harbor
            install_harbor_certs
            deploy_harbor
            success "Harbor 部署完成"
            ;;
        services|"")
            info "构建镜像并部署服务 (Nomad/K8S 任务 + 生成 E2B Token)..."
            deploy
            success "服务部署完成"
            ;;
        *)
            error "未知组件: $comp。支持: docker, nomad, consul, postgres, harbor, services"
            ;;
    esac
}

undeploy_component() {
    local comp="$1"
    case "$comp" in
        nomad)
            info "单独卸载 Nomad..."
            uninstall_nomad
            success "Nomad 卸载完成"
            ;;
        consul)
            info "单独卸载 Consul..."
            systemctl stop consul 2>/dev/null || true
            pkill consul 2>/dev/null || true
            rm -f /usr/local/bin/consul /etc/systemd/system/consul.service 2>/dev/null || true
            systemctl daemon-reload 2>/dev/null || true
            success "Consul 卸载完成"
            ;;
        harbor)
            info "单独卸载 Harbor..."
            uninstall_harbor
            success "Harbor 卸载完成"
            ;;
        postgres)
            info "单独卸载 PostgreSQL..."
            uninstall_postgres
            success "PostgreSQL 卸载完成"
            ;;
        *)
            error "未知组件: $comp。支持: nomad, consul, harbor, postgres"
            ;;
    esac
}

# =============================================================================
# E2B Infra 管理脚本
# 支持长参数: --install, --uninstall, --start, --stop, --deploy, --make <target>
# =============================================================================
show_help() {
    echo -e "${GREEN}E2B Infra 管理工具 v1.0 (ARM64 Support)${NC}"
    echo -e "${YELLOW}用法:${NC}"
    echo -e "  $0 [选项]"
    echo ""
    echo -e "${YELLOW}核心选项:${NC}"
    echo -e "  ${GREEN}--download${NC}      下载并准备离线依赖包 (Docker images, Binaries)"
    echo -e "  ${GREEN}--install${NC}       安装 Consul, Nomad, DNS 引导及环境初始化"
    echo -e "  ${GREEN}--install-client${NC}  安装客户端组件"
    echo -e "  ${GREEN}--uninstall${NC}     卸载所有组件"
    echo -e "  ${GREEN}--remove <组件名>${NC}  单独卸载指定组件 (支持: nomad, consul, harbor, postgres)"
    echo -e "  ${GREEN}--start${NC}         启动基础服务 (Consul/Nomad/Dnsmasq)"
    echo -e "  ${GREEN}--stop${NC}          停止服务并清理残留的沙箱实例"
    echo -e "  ${GREEN}--deploy <组件名>${NC}  单独部署指定组件 (支持: docker, nomad, consul, postgres, harbor, services)"
    echo -e "  ${GREEN}--deploy-plugin${NC} [<target>] [<template>] [<selector>] [<namespace>] 部署 E2B 插件到 OpenClaw 容器 (需先 --start)"
    echo -e "    <target>: 容器/Pod 名 (自动获取)"
    echo -e "    <template>: 模板名 (默认: base)"
    echo -e "    <selector>: K8s 选择器 (默认: app=openclaw-deploy-for-local-exec)"
    echo -e "    <namespace>: K8s 命名空间 (默认: default)"
    echo -e "  ${GREEN}--make <target>${NC}  在当前环境下制作镜像"
    echo -e "  ${GREEN}--nomad-job <操作> [任务名]${NC}  管理 Nomad 任务"
    echo -e "    操作: deploy(部署) | stop(停止) | start(启动) | delete(删除) | list(列表)"
    echo -e "    任务名: redis | template-manager | edge | api | all (默认: all)"
    echo -e "  ${GREEN}--create-harbor-project [项目名]${NC}  创建 Harbor 项目 (默认: e2b-orchestration)"
    echo -e "  ${GREEN}--k8s [node-name]${NC}  启用 K8S 模式启动服务"
    echo -e "                      不指定节点名时自动选择第一个节点"
    echo -e "  ${GREEN}--runtime docker|nerdctl${NC}  显式指定容器运行时（默认自动检测）"
    echo ""
    echo -e "${YELLOW}使用示例:${NC}"
    echo -e "  ${GREEN}# 初次完整安装:${NC}"
    echo -e "  $0 --download --install --start"
    echo ""
    echo -e "  ${GREEN}# 快速重启环境:${NC}"
    echo -e "  $0 --stop --start"
    echo ""
    echo -e "  ${GREEN}# 单独部署组件:${NC}"
    echo -e "  $0 --deploy docker        # 安装 Docker & Docker Compose (需先 --download)"
    echo -e "  $0 --deploy postgres      # 重新部署 PostgreSQL"
    echo -e "  $0 --deploy harbor        # 重新部署 Harbor (含 Docker/Nginx)"
    echo -e "  $0 --deploy nomad         # 重新部署 Nomad"
    echo -e "  $0 --deploy consul        # 重新部署 Consul"
    echo -e "  $0 --deploy services      # 构建镜像并部署 E2B 服务 (Nomad/K8S 任务 + 生成 Token)"
    echo ""
    echo -e "  ${GREEN}# K8S 模式部署:${NC}"
    echo -e "  $0 --k8s worker1 --start   # 指定节点名部署"
    echo -e "  $0 --k8s --start          # 自动选择第一个节点"
    echo ""
    echo -e "  ${GREEN}# 指定容器运行时:${NC}"
    echo -e "  $0 --runtime nerdctl --install --start  # 强制使用 nerdctl"
    echo -e "  $0 --runtime docker --install --start   # 强制使用 docker"
    echo ""
    echo -e "  ${GREEN}# 卸载组件:${NC}"
    echo -e "  $0 --uninstall              # 卸载所有组件"
    echo -e "  $0 --remove harbor          # 单独卸载 Harbor"
    echo -e "  $0 --remove nomad           # 单独卸载 Nomad"
    echo -e "  $0 --remove consul          # 单独卸载 Consul"
    echo ""
    echo -e "  ${GREEN}# Harbor 项目管理:${NC}"
    echo -e "  $0 --create-harbor-project ''                  # 创建默认项目 e2b-orchestration"
    echo -e "  $0 --create-harbor-project my-project          # 创建指定项目"
    echo ""
    echo -e "  ${GREEN}# Nomad 任务管理:${NC}"
    echo -e "  $0 --nomad-job list                            # 查看所有任务"
    echo -e "  $0 --nomad-job deploy redis                    # 部署指定任务"
    echo -e "  $0 --nomad-job deploy all                      # 部署所有任务"
    echo -e "  $0 --nomad-job stop api                        # 停止指定任务"
    echo -e "  $0 --nomad-job start api                       # 启动指定任务"
    echo -e "  $0 --nomad-job delete api                      # 删除指定任务"
    echo ""
    echo -e "${YELLOW}说明:${NC}"
    echo "  - 脚本必须以 root 权限运行。"
}
# 1. 使用 getopt 解析长参数
# 注意：deploy-plugin 不带冒号，因为它需要处理多个可选参数
PARSED_ARGUMENTS=$(getopt \
  --options "h" \
  --longoptions "help,download,install,install-client,uninstall,remove:,start,stop,deploy:,deploy-plugin,nomad-job,make:,k8s:,create-harbor-project:,runtime:" \
  --name "$0" \
  -- "$@")

# 检查解析是否成功
if [ $? -ne 0 ]; then
    echo "错误: 参数解析失败。请检查输入是否正确。"
    exit 1
fi

# 重新设置位置参数
eval set -- "$PARSED_ARGUMENTS"

# ===================== 参数解析阶段 =====================
# 初始化动作标志
ACTION_DOWNLOAD=false
ACTION_INSTALL=false
ACTION_INSTALL_CLIENT=false
ACTION_UNINSTALL=false
REMOVE_COMPONENT=""
ACTION_START=false
ACTION_STOP=false
DEPLOY_COMPONENT=""
DEPLOY_PLUGIN=false
DEPLOY_PLUGIN_ARGS=()
NOMAD_JOB=false
NOMAD_JOB_ARGS=()
MAKE_TARGET=""
K8S_NODE_NAME=""
CREATE_PROJECT=""

# 解析参数
while true; do
    case "$1" in
        -h|--help)
            ACTION_HELP=true
            ;;
        --download)
            ACTION_DOWNLOAD=true
            ;;
        --install)
            ACTION_INSTALL=true
            ;;
        --install-client)
            ACTION_INSTALL_CLIENT=true
            ;;
        --uninstall)
            ACTION_UNINSTALL=true
            ;;
        --remove)
            REMOVE_COMPONENT="$2"
            shift
            ;;
        --start)
            ACTION_START=true
            ;;
        --stop)
            ACTION_STOP=true
            ;;
        --deploy)
            DEPLOY_COMPONENT="$2"
            shift
            ;;
        --deploy-plugin)
            DEPLOY_PLUGIN=true
            # 收集所有后续参数直到遇到选项或结束
            shift
            if [ $# -gt 0 ] && [[ "$1" == -- ]]; then
                shift
            fi
            while [ $# -gt 0 ] && [[ "$1" != --* ]]; do
                DEPLOY_PLUGIN_ARGS+=("$1")
                shift
            done
            continue  # 跳过默认的 shift
            ;;
        --nomad-job)
            NOMAD_JOB=true
            shift
            if [ $# -gt 0 ] && [[ "$1" == -- ]]; then
                shift
            fi
            while [ $# -gt 0 ] && [[ "$1" != --* ]]; do
                NOMAD_JOB_ARGS+=("$1")
                shift
            done
            continue
            ;;
        --make)
            MAKE_TARGET="$2"
            shift
            ;;
        --k8s)
            export DEPLOY_MODE="k8s"
            # getopt 带 : 会将参数设为空字符串（--k8s ''）或实际值（--k8s worker1）
            if [ -n "$2" ] && [[ "$2" != --* ]]; then
                K8S_NODE_NAME="$2"
                shift
            else
                K8S_NODE_NAME=""
            fi
            echo "K8S 节点名: ${K8S_NODE_NAME:-自动选择}"
            ;;
        --create-harbor-project)
            # 类似 --k8s：getopt 带 : 会将参数设为空字符串或实际值
            if [ -n "$2" ] && [[ "$2" != --* ]]; then
                CREATE_PROJECT="$2"
                shift
            else
                CREATE_PROJECT="e2b-orchestration"
            fi
            ;;
        --runtime)
            CONTAINER_RUNTIME="$2"
            shift
            ;;
        --)
            shift
            break
            ;;
        *)
            break
            ;;
    esac
    shift
done

# ===================== 执行逻辑阶段 =====================
echo "------------------------------$DEPLOY_MODE---------------------------------"

# 显示帮助
if [ "${ACTION_HELP:-false}" = true ]; then
    show_help
    exit 0
fi

# 仅在需要容器运行时的操作前初始化
# install-client、download、help 等不依赖容器运行时，避免在无 docker/nerdctl 环境报错
# 部署 docker 组件本身不需要预装容器运行时（install_docker 会自行安装 docker），故排除
if [ "$ACTION_INSTALL" = true ] || \
   [ "$ACTION_UNINSTALL" = true ] || \
   [ "$ACTION_START" = true ] || \
   [ "$ACTION_STOP" = true ] || \
   [ "$DEPLOY_PLUGIN" = true ] || \
   [ "$NOMAD_JOB" = true ] || \
   [ -n "$MAKE_TARGET" ] || \
   [ -n "$CREATE_PROJECT" ] || \
   [ -n "$REMOVE_COMPONENT" ] || \
   ( [ -n "$DEPLOY_COMPONENT" ] && [ "$DEPLOY_COMPONENT" != "docker" ] ); then
    set_container_runtime "$CONTAINER_RUNTIME"
fi


# 执行下载
if [ "$ACTION_DOWNLOAD" = true ]; then
    echo "[Action] 下载依赖包..."
    download_packages
fi

# 执行安装
if [ "$ACTION_INSTALL" = true ]; then
    echo "[Action] 执行安装逻辑..."
    install
fi

# 执行客户端安装
if [ "$ACTION_INSTALL_CLIENT" = true ]; then
    echo "[Action] 执行安装客户端逻辑..."
    install_client
fi

# 执行卸载
if [ "$ACTION_UNINSTALL" = true ]; then
    echo "[Action] 卸载所有组件..."
    uninstall
fi

# 执行单独卸载组件
if [ -n "$REMOVE_COMPONENT" ]; then
    echo "[Action] 单独卸载组件: $REMOVE_COMPONENT"
    undeploy_component "$REMOVE_COMPONENT"
fi

# 执行停止
if [ "$ACTION_STOP" = true ]; then
    echo "[Action] 停止服务..."
    stop
fi

# 执行启动
if [ "$ACTION_START" = true ]; then
    echo "[Action] 启动服务..."
    unset http_proxy https_proxy
    start
fi

# 执行部署组件
if [ -n "$DEPLOY_COMPONENT" ]; then
    echo "[Action] 单独部署组件: $DEPLOY_COMPONENT"
    unset http_proxy https_proxy
    deploy_component "$DEPLOY_COMPONENT"
fi

# 执行部署插件
if [ "$DEPLOY_PLUGIN" = true ]; then
    echo "[Action] 部署 E2B 插件..."
    deploy_plugin "${DEPLOY_PLUGIN_ARGS[@]}"
fi

# 执行 Nomad 任务管理
if [ "$NOMAD_JOB" = true ]; then
    NOMAD_ACTION="${NOMAD_JOB_ARGS[0]:-}"
    NOMAD_TARGET="${NOMAD_JOB_ARGS[1]:-all}"
    case "$NOMAD_ACTION" in
        deploy)
            echo "[Action] 部署 Nomad 任务: $NOMAD_TARGET"
            nomad_job_deploy "$NOMAD_TARGET"
            ;;
        stop)
            echo "[Action] 停止 Nomad 任务: $NOMAD_TARGET"
            nomad_job_stop "$NOMAD_TARGET"
            ;;
        start)
            echo "[Action] 启动 Nomad 任务: $NOMAD_TARGET"
            nomad_job_start "$NOMAD_TARGET"
            ;;
        delete)
            echo "[Action] 删除 Nomad 任务: $NOMAD_TARGET"
            nomad_job_delete "$NOMAD_TARGET"
            ;;
        list)
            echo "[Action] 查看 Nomad 任务列表"
            nomad_job_list
            ;;
        *)
            echo "错误: 未知 Nomad 任务操作: $NOMAD_ACTION"
            echo "支持的操作: deploy, stop, start, delete, list"
            echo "用法: $0 --nomad-job <操作> [任务名|all]"
            exit 1
            ;;
    esac
fi

# 执行镜像制作
if [ -n "$MAKE_TARGET" ]; then
    echo "[Action] 镜像制作，目标: $MAKE_TARGET"
    make_images "$MAKE_TARGET"
fi

# 执行创建 Harbor 项目
if [ -n "$CREATE_PROJECT" ]; then
    echo "[Action] 创建 Harbor 项目: $CREATE_PROJECT"
    harbor_wait_healthy
    harbor_login
    harbor_create_project "$CREATE_PROJECT"
fi

# 兜底逻辑：无参数时自动显示 Help
if [ "$ACTION_DOWNLOAD" = false ] && \
   [ "$ACTION_INSTALL" = false ] && \
   [ "$ACTION_INSTALL_CLIENT" = false ] && \
   [ "$ACTION_UNINSTALL" = false ] && \
   [ -z "$REMOVE_COMPONENT" ] && \
   [ "$ACTION_START" = false ] && \
   [ "$ACTION_STOP" = false ] && \
   [ -z "$DEPLOY_COMPONENT" ] && \
   [ "$DEPLOY_PLUGIN" = false ] && \
   [ "$NOMAD_JOB" = false ] && \
   [ -z "$MAKE_TARGET" ] && \
   [ -z "$CREATE_PROJECT" ] && \
   [ -z "${ACTION_HELP:-}" ]; then
    show_help
    exit 0
fi
