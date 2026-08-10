#!/usr/bin/env bash
set -euo pipefail

# ===================== 基础配置 =====================
# 使用 KubeKey 部署 K8S 集群（支持 x86_64 / arm64）
# 用法:
#   ./k8s-deploy.sh prep     # 安装依赖、下载 kk/cni、生成集群配置（生成后需手动编辑节点信息）
#   ./k8s-deploy.sh create   # 根据配置文件创建集群
#   ./k8s-deploy.sh all      # prep + create（需先通过 -c 指定已编辑的配置文件）

KUBEKEY_VERSION="${KUBEKEY_VERSION:-v3.1.10}"
K8S_VERSION="${K8S_VERSION:-v1.32.5}"
CNI_PLUGINS_VERSION="${CNI_PLUGINS_VERSION:-v1.6.2}"
CLUSTER_NAME="${CLUSTER_NAME:-k8s}"
CONFIG_FILE="${CONFIG_FILE:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ===================== 输出函数 =====================
info()    { echo "==> $*"; }
warn()    { echo "==> WARN: $*"; }
success() { echo "==> OK: $*"; }
error()   { echo "==> ERROR: $*" >&2; exit 1; }

# ===================== 工具函数 =====================
# 检测当前架构并映射为 KubeKey 配置名
detect_arch() {
    local raw_arch
    raw_arch="$(uname -m)"
    case "$raw_arch" in
        x86_64)         echo "amd64" ;;
        aarch64|arm64)  echo "arm64" ;;
        *)              error "不支持的架构: $raw_arch（仅支持 x86_64 / arm64）" ;;
    esac
}

# 识别系统包管理器
detect_pkg_manager() {
    if command -v dnf >/dev/null 2>&1; then
        echo "dnf"
    elif command -v yum >/dev/null 2>&1; then
        echo "yum"
    elif command -v apt-get >/dev/null 2>&1; then
        echo "apt-get"
    else
        error "不支持的发行版（未找到 dnf/yum/apt-get）"
    fi
}

# ===================== 步骤函数 =====================
# 安装 K8S 所需系统依赖
install_system_deps() {
    info "安装系统依赖 ..."
    local pkg_mgr
    pkg_mgr="$(detect_pkg_manager)"
    case "$pkg_mgr" in
        dnf|yum)
            "$pkg_mgr" update -y
            "$pkg_mgr" install -y conntrack socat ipvsadm ipset curl tar
            ;;
        apt-get)
            apt-get update -y
            apt-get install -y conntrack socat ipvsadm ipset curl tar
            ;;
    esac
    # 加载内核模块（ipvs 转发依赖）
    info "加载内核模块 ..."
    modprobe ip_vs ip_vs_rr ip_vs_wrr ip_vs_sh nf_conntrack 2>/dev/null || warn "部分内核模块加载失败，可能已内置"
    success "系统依赖安装完成"
}

# 下载 KubeKey 二进制
download_kubekey() {
    if [ -x "$SCRIPT_DIR/kk" ]; then
        info "kk 已存在，跳过下载"
        return
    fi
    info "下载 KubeKey $KUBEKEY_VERSION ..."
    export KKZONE=cn
    curl -sfL "https://get-kk.kubesphere.io" | VERSION="$KUBEKEY_VERSION" sh -
    chmod +x "$SCRIPT_DIR/kk"
    success "KubeKey 下载完成: $SCRIPT_DIR/kk"
}

# 下载 CNI 插件二进制到 /opt/cni/bin
# KubeKey 默认使用 calico，但节点上仍需 bridge/host-local 等基础 CNI 插件
# 环境变量 CNI_PLUGINS_VERSION 可指定版本（默认 v1.6.2）
download_cni_plugins() {
    local version arch tar_name download_url
    version="${CNI_PLUGINS_VERSION:-v1.9.1}"
    arch="$(detect_arch)"
    tar_name="cni-plugins-linux-${arch}-${version}.tgz"
    download_url="https://github.com/containernetworking/plugins/releases/download/${version}/${tar_name}"

    # 幂等检查：已有核心插件则跳过
    if [ -d "/opt/cni/bin" ] && [ -f "/opt/cni/bin/bridge" ] && [ -f "/opt/cni/bin/host-local" ]; then
        info "CNI 插件已存在，跳过下载"
        return
    fi

    info "下载 CNI 插件 ${version} (${arch}) ..."
    mkdir -p /opt/cni/bin
    curl -sfkL "$download_url" -o "/tmp/${tar_name}" || error "CNI 插件下载失败: $download_url"
    tar -xzf "/tmp/${tar_name}" -C /opt/cni/bin || error "CNI 插件解压失败"
    rm -f "/tmp/${tar_name}"
    success "CNI 插件已安装到 /opt/cni/bin"
}

# 生成集群配置文件
# 使用内置模板，自动填充本机 IP 和架构，无需调用 kk create config
generate_config() {
    local arch config_name host_ip
    arch="$(detect_arch)"
    config_name="config-${CLUSTER_NAME}-${arch}.yaml"
    host_ip="${HOST_IP:-$(hostname -I | awk '{print $1}')}"
    [ -z "$host_ip" ] && error "无法获取本机 IP，请通过 HOST_IP 环境变量指定"

    info "生成集群配置: $config_name（K8S $K8S_VERSION, arch=$arch, ip=$host_ip）"

    cat > "$SCRIPT_DIR/$config_name" << EOF
apiVersion: kubekey.kubesphere.io/v1alpha2
kind: Cluster
metadata:
  name: ${CLUSTER_NAME}-${arch}
spec:
  hosts:
   - {name: node1, address: ${host_ip}, internalAddress: ${host_ip}, user: root, password: "${NODE_PASSWORD:-}", arch: ${arch}}
  roleGroups:
    etcd:
    - node1
    control-plane:
    - node1
    worker:
    - node1
  controlPlaneEndpoint:
    ## Internal loadbalancer for apiservers
    # internalLoadbalancer: haproxy

    domain: lb.kubesphere.local
    address: ""
    port: 6443
  kubernetes:
    version: ${K8S_VERSION}
    clusterName: cluster.local
    autoRenewCerts: true
    containerManager: containerd
  etcd:
    type: kubekey
  network:
    plugin: calico
    kubePodsCIDR: 10.233.64.0/18
    kubeServiceCIDR: 10.233.0.0/18
    ## multus support. https://github.com/k8snetworkplumbingwg/multus-cni
    multusCNI:
      enabled: false
  registry:
    privateRegistry: ""
    namespaceOverride: ""
    registryMirrors: []
    insecureRegistries: []
  addons: []
EOF

    if [ -f "$SCRIPT_DIR/$config_name" ]; then
        success "配置文件已生成: $SCRIPT_DIR/$config_name"
        echo ""
        echo "------> 如需修改，请编辑 $config_name:"
        echo "  - hosts.password: 节点 SSH 密码（通过 NODE_PASSWORD 环境变量提供）"
        echo "  - 多节点: 在 hosts 中追加节点，并在 roleGroups 分配角色"
        echo "  - 离线安装: 配置 registry.privateRegistry"
        echo ""
        echo "------> 编辑完成后执行:"
        echo "  CONFIG_FILE=$config_name $0 create"
        echo ""
        GENERATED_CONFIG="$SCRIPT_DIR/$config_name"
    else
        error "配置文件生成失败: $config_name"
    fi
}

# 解析配置文件路径
resolve_config_file() {
    if [ -n "$CONFIG_FILE" ]; then
        # 用户通过环境变量指定
        if [ ! -f "$CONFIG_FILE" ]; then
            error "配置文件不存在: $CONFIG_FILE"
        fi
        RESOLVED_CONFIG="$CONFIG_FILE"
        return
    fi
    # 默认按集群名 + 架构推断
    local arch config_name
    arch="$(detect_arch)"
    config_name="config-${CLUSTER_NAME}-${arch}.yaml"
    if [ ! -f "$SCRIPT_DIR/$config_name" ]; then
        error "未找到配置文件 $SCRIPT_DIR/$config_name，请先执行 '$0 prep' 生成并编辑"
    fi
    RESOLVED_CONFIG="$SCRIPT_DIR/$config_name"
}

# 创建 K8S 集群
create_cluster() {
    resolve_config_file
    info "根据配置创建集群: $RESOLVED_CONFIG"
    cd "$SCRIPT_DIR"
    export KKZONE=cn
    ./kk create cluster -f "$RESOLVED_CONFIG"
    success "K8S 集群创建完成"
}

# 验证集群状态
verify_cluster() {
    info "验证集群状态 ..."
    # kk 创建完成后 kubeconfig 已写入 ~/.kube/config
    if ! command -v kubectl >/dev/null 2>&1; then
        warn "kubectl 未安装，跳过验证（可在 master 节点手动检查）"
        return
    fi
    echo "------> 节点状态"
    kubectl get nodes -o wide || warn "获取节点失败"
    echo "------> 组件状态"
    kubectl get cs || warn "获取组件状态失败"
    echo "------> Pod 状态（kube-system）"
    kubectl get pods -n kube-system || warn "获取 Pod 失败"
    success "集群验证完成"
}

# 部署 ingress-nginx 控制器（K8S 集群创建后执行）
deploy_ingress() {
    local ingress_yaml="$SCRIPT_DIR/dep/ingress-nginx.yaml"
    if [ ! -f "$ingress_yaml" ]; then
        warn "未找到 $ingress_yaml，跳过 ingress-nginx 部署"
        return
    fi
    if ! command -v kubectl >/dev/null 2>&1; then
        warn "kubectl 未安装，跳过 ingress-nginx 部署"
        return
    fi
    info "部署 ingress-nginx ..."
    # 已安装则跳过（幂等）
    if kubectl get namespace ingress-nginx >/dev/null 2>&1 \
       && kubectl get deployment -n ingress-nginx ingress-nginx-controller >/dev/null 2>&1; then
        success "ingress-nginx 已部署，跳过"
        return
    fi
    kubectl apply -f "$ingress_yaml"
    # 等待 controller 就绪（裸金属环境 LoadBalancer 无外部 IP 时会卡住，故仅等待 Pod Running）
    info "等待 ingress-nginx-controller Pod 就绪（超时 180s）..."
    if kubectl wait --namespace ingress-nginx \
        --for=condition=ready pod \
        --selector=app.kubernetes.io/component=controller \
        --timeout=180s >/dev/null 2>&1; then
        success "ingress-nginx 部署完成"
        kubectl get pods -n ingress-nginx -o wide
    else
        warn "ingress-nginx Pod 未在 180s 内就绪，请检查: kubectl get pods -n ingress-nginx"
    fi
}

# 配置 *.e2b.app 域名访问（三层配置）
# ① 验证 ingress-nginx-controller 暴露 80 端口
# ② CoreDNS rewrite 规则：*.e2b.app -> edge-api.e2b.svc.cluster.local
# ③ 创建 wildcard Ingress（依赖 e2b 命名空间，未部署则跳过）
configure_domain_access() {
    if ! command -v kubectl >/dev/null 2>&1; then
        warn "kubectl 未安装，跳过域名访问配置"
        return
    fi

    # ① 验证 ingress-nginx-controller 80 端口
    info "① 验证 ingress-nginx-controller 80 端口 ..."
    if kubectl get svc -n ingress-nginx ingress-nginx-controller \
        -o jsonpath='{.spec.ports[?(@.name=="http")]}' 2>/dev/null | grep -q "80"; then
        success "ingress-nginx-controller 已暴露 80 端口"
    else
        warn "ingress-nginx-controller 未暴露 80 端口，请手动检查"
    fi

    # ② CoreDNS rewrite 规则
    info "② 配置 CoreDNS 重写规则 ..."
    local tmp_corefile
    tmp_corefile="$(mktemp)"
    if ! kubectl get configmap coredns -n kube-system \
        -o jsonpath='{.data.Corefile}' > "$tmp_corefile" 2>/dev/null; then
        warn "无法获取 CoreDNS Corefile，跳过 rewrite 配置"
        rm -f "$tmp_corefile"
        return
    fi

    if grep -qF 'rewrite name regex .*\.e2b\.app\.$' "$tmp_corefile"; then
        success "CoreDNS rewrite 规则已存在，跳过"
    else
        # 在 'ready' 行后插入（4 空格缩进，KubeKey 默认布局）；找不到则回退到 'errors' 行
        # 注意: sed a\ 命令会吃掉单个反斜杠，故 \. 需写成 \\. 才能原样输出
        if grep -q "^    ready$" "$tmp_corefile"; then
            sed -i '/^    ready$/a\    rewrite name regex .*.e2b.app.$ edge-api.e2b.svc.cluster.local' "$tmp_corefile"
        elif grep -q "^    errors$" "$tmp_corefile"; then
            sed -i '/^    errors$/a\    rewrite name regex .*.e2b.app.$ edge-api.e2b.svc.cluster.local' "$tmp_corefile"
        else
            warn "无法识别 Corefile 结构，请手动添加: rewrite name regex .*.e2b.app.$ edge-api.e2b.svc.cluster.local"
            rm -f "$tmp_corefile"
            return
        fi

        kubectl create configmap coredns -n kube-system \
            --from-file=Corefile="$tmp_corefile" -o yaml --dry-run=client | kubectl apply -f -
        kubectl rollout restart deployment coredns -n kube-system
        success "CoreDNS rewrite 规则已添加，coredns 已重启"
    fi
    rm -f "$tmp_corefile"

    # ③ 创建 wildcard Ingress（依赖 e2b 命名空间）
    info "③ 创建 wildcard Ingress ..."
    local wildcard_yaml="$SCRIPT_DIR/dep/wildcard-ingress.yaml"
    if [ ! -f "$wildcard_yaml" ]; then
        warn "未找到 $wildcard_yaml，跳过"
        return
    fi
    if ! kubectl get namespace e2b >/dev/null 2>&1; then
        warn "e2b 命名空间不存在，跳过 wildcard Ingress（e2b 部署后执行: $0 configure-domain）"
        return
    fi
    kubectl apply -f "$wildcard_yaml"
    kubectl get ingress -n e2b || true
    success "wildcard Ingress 创建完成"
}

# 部署 cri-multiplex（多 runtime 复用器，让 kubelet 通过单一 socket 调度 containerd / 自定义 runtime）
# 流程: 创建 systemd 服务 -> 启动 cri-multiplex -> 切换 kubelet endpoint -> 重启 kubelet -> 创建 RuntimeClass
deploy_cri_multiplex() {
    local bin="${CRI_MULTIPLEX_BIN:-/opt/e2b-infra/bin/cri-multiplex}"
    local mux_socket="/run/cri-multiplex.sock"
    local containerd_socket="/run/containerd/containerd.sock"
    local orchestrator_addr="${CRI_MULTIPLEX_ORCHESTRATOR:-localhost:5008}"
    local flags_file="${KUBELET_FLAGS_FILE:-/var/lib/kubelet/kubeadm-flags.env}"
    local unit_file="/etc/systemd/system/cri-multiplex.service"

    info "部署 cri-multiplex ..."

    # ① 检查二进制
    if [ ! -x "$bin" ]; then
        warn "未找到可执行文件 $bin，跳过 cri-multiplex 部署（可通过 CRI_MULTIPLEX_BIN 指定路径）"
        return
    fi

    # ② 创建 systemd 服务单元（保证开机自启与崩溃重启）
    info "创建 systemd 服务: $unit_file"
    cat > "$unit_file" << EOF
[Unit]
Description=cri-multiplex (CRI multi-runtime multiplexer)
After=containerd.service
Wants=containerd.service

[Service]
ExecStart=${bin} -socket ${mux_socket} -containerd-socket ${containerd_socket} -orchestrator-address ${orchestrator_addr} \
-orchestrator-proxy-address localhost:5007 \
  -state-dir /var/lib/cri-multiplex/state \
  -orphan-reconcile-enabled=1 \
  -orphan-reconcile-interval 60s \
  -orphan-grace-period 120s \
  -cleanup-max-retries 10 \
  -cni-enabled \
  -cni-conf-dir /etc/cni/net.d \
  -cni-bin-dir /opt/cni/bin \
  -cni-ifname eth0 \
  -cni-netns-dir /var/run/netns
Restart=always
RestartSec=3
# 运行时 socket 目录
RuntimeDirectory=cri-multiplex

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable cri-multiplex >/dev/null 2>&1 || true
    systemctl restart cri-multiplex
    # 等待 socket 就绪
    local i
    for i in $(seq 1 10); do
        [ -S "$mux_socket" ] && break
        sleep 0.5
    done
    if [ ! -S "$mux_socket" ]; then
        warn "cri-multiplex socket 未就绪: $mux_socket，请检查: journalctl -u cri-multiplex -n 50"
        return
    fi
    success "cri-multiplex 已启动: $mux_socket"

    # ③ 修改 kubelet endpoint: containerd.sock -> cri-multiplex.sock（幂等）
    info "修改 kubelet 运行时 endpoint ..."
    if [ ! -f "$flags_file" ]; then
        warn "未找到 $flags_file，请确认 kubelet 已通过 kubeadm 初始化"
        return
    fi
    if grep -q "unix://${mux_socket}" "$flags_file"; then
        success "kubelet 已使用 cri-multiplex endpoint，跳过修改"
    else
        # 备份后替换
        cp -a "$flags_file" "${flags_file}.bak.$(date +%s)"
        sed -i "s#unix://${containerd_socket}#unix://${mux_socket}#g" "$flags_file"
        success "kubeadm-flags.env 已更新: endpoint=${mux_socket}"
    fi

    # ④ 重启 kubelet 使配置生效
    info "重启 kubelet ..."
    systemctl restart kubelet
    sleep 3
    if systemctl is-active --quiet kubelet; then
        success "kubelet 已重启并运行中"
    else
        warn "kubelet 未正常运行，请检查: journalctl -u kubelet -n 50"
    fi

    # ⑤ 创建 RuntimeClass（android / e2b）
    if ! command -v kubectl >/dev/null 2>&1; then
        warn "kubectl 未安装，跳过 RuntimeClass 创建（可手动 apply）"
        return
    fi
    info "创建 RuntimeClass (android / e2b) ..."
    cat << 'EOF' | kubectl apply -f -
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: android
handler: android
---
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: e2b
handler: e2b
EOF
    kubectl get runtimeclass 2>/dev/null || true
    success "RuntimeClass 创建完成"
}

install_buildkit() {
    case "$(uname -m)" in
        x86_64)
            ARCH=amd64
            ;;
        aarch64*)
            ARCH=arm64
            ;;
        *)
            echo "$(uname -m), isn't supported"
            exit 1
            ;;
    esac

    wget --no-check-certificate https://openfuyao.obs.cn-north-4.myhuaweicloud.com/moby/buildkit/releases/download/v0.27.1/buildkit-v0.27.1.linux-${ARCH}.tar.gz

    tar -xvzf buildkit-v0.27.1.linux-${ARCH}.tar.gz -C /usr/local/bin/
    mv /usr/local/bin/bin/buildctl /usr/local/bin/bin/buildkitd /usr/local/bin/
    mkdir -p /etc/buildkit
    cat > /etc/buildkit/buildkitd.toml <<'EOF'
[worker.oci]
  enabled = false

[worker.containerd]
  namespace = "default"
  address = "/run/containerd/containerd.sock"
EOF
    cat > /usr/lib/systemd/system/buildkit.service <<'EOF'
[Unit]
Description=BuildKit
Requires=buildkit.socket
After=buildkit.socket
Documentation=https://github.com/moby/buildkit

[Service]
Type=notify
ExecStart=/usr/local/bin/buildkitd --addr fd://

[Install]
WantedBy=multi-user.target
EOF
    cat > /usr/lib/systemd/system/buildkit.socket <<'EOF'
[Unit]
Description=BuildKit
Documentation=https://github.com/moby/buildkit

[Socket]
ListenStream=%t/buildkit/buildkitd.sock
SocketMode=0660

[Install]
WantedBy=sockets.target
EOF
    systemctl enable --now buildkit.service buildkit.socket
}

# ===================== 主流程 =====================
main() {
    local action="${1:-help}"
    # 全局变量（由子函数设置）
    GENERATED_CONFIG=""
    RESOLVED_CONFIG=""

    case "$action" in
        prep)
            install_system_deps
            download_kubekey
            download_cni_plugins
            generate_config
            ;;
        create)
            create_cluster
            verify_cluster
            deploy_ingress
            configure_domain_access
            ;;
        all)
            # all 流程: prep 生成配置后，要求用户通过 CONFIG_FILE 指定已编辑的配置
            if [ -z "$CONFIG_FILE" ]; then
                install_system_deps
                download_kubekey
                download_cni_plugins
                generate_config
                warn "all 流程需要已编辑的配置文件"
                error "请编辑 $GENERATED_CONFIG 后重新执行: CONFIG_FILE=<path> $0 all"
            fi
            create_cluster
            verify_cluster
            deploy_ingress
            configure_domain_access
            ;;
        configure-domain)
            # 单独执行域名访问配置（e2b 部署后补建 wildcard Ingress）
            configure_domain_access
            ;;
        cri-multiplex)
            # 部署 cri-multiplex 并切换 kubelet endpoint（节点级操作，每个节点执行）
            deploy_cri_multiplex
            ;;
        buildkit)
            # 安装 buildkit（节点级操作）
            install_buildkit
            ;;
        download-cni)
            # 单独下载 CNI 插件
            download_cni_plugins
            ;;
        help|--help|-h)
            echo "用法: $0 <prep|create|all|configure-domain|cri-multiplex|buildkit|download-cni>"
            echo ""
            echo "  prep              安装依赖、下载 kk 和 CNI 插件、生成集群配置（需手动编辑节点信息）"
            echo "  create            根据配置文件创建集群、验证状态、部署 ingress-nginx、配置域名访问"
            echo "  all               prep + create（需通过 CONFIG_FILE 指定已编辑的配置）"
            echo "  configure-domain  单独配置 *.e2b.app 域名访问（CoreDNS rewrite + wildcard Ingress）"
            echo "                    （e2b 部署后执行以补建 wildcard Ingress）"
            echo "  cri-multiplex     部署 cri-multiplex、切换 kubelet endpoint、创建 RuntimeClass"
            echo "                    （节点级操作，需在每个节点执行；需先安装 cri-multiplex 二进制）"
            echo "  buildkit          安装并启用 buildkit"
            echo "  download-cni      单独下载并安装 CNI 插件到 /opt/cni/bin"
            echo ""
            echo "环境变量:"
            echo "  KUBEKEY_VERSION      KubeKey 版本（默认 v3.1.10）"
            echo "  K8S_VERSION          K8S 版本（默认 v1.32.5）"
            echo "  CNI_PLUGINS_VERSION  CNI 插件版本（默认 v1.6.2）"
            echo "  CLUSTER_NAME         集群名（默认 k8s）"
            echo "  CONFIG_FILE      指定配置文件路径（create/all 使用）"
            echo "  HOST_IP          本机 IP（默认自动探测，prep 生成配置时使用）"
            echo "  NODE_PASSWORD    节点 SSH 密码（必须通过环境变量提供）"
            echo "  CRI_MULTIPLEX_BIN          cri-multiplex 二进制路径（默认 /opt/e2b-infra/bin/cri-multiplex）"
            echo "  CRI_MULTIPLEX_ORCHESTRATOR orchestrator 地址（默认 localhost:5008）"
            echo "  KUBELET_FLAGS_FILE         kubelet 启动参数文件路径（默认 /var/lib/kubelet/kubeadm-flags.env）"
            echo ""
            echo "示例:"
            echo "  $0 prep                                  # 生成 config-k8s-arm64.yaml（自动填充本机 IP）"
            echo "  vi config-k8s-arm64.yaml                 # 按需修改多节点/密码"
            echo "  $0 create                                # 创建集群"
            echo "  HOST_IP=10.0.0.5 NODE_PASSWORD=secret $0 prep   # 指定 IP 和密码生成配置"
            echo "  $0 buildkit                              # 安装 buildkit"
            echo "  $0 download-cni                          # 下载 CNI 插件"
            echo "  CNI_PLUGINS_VERSION=v1.6.2 $0 download-cni      # 指定版本下载 CNI 插件"
            ;;
        *)
            error "未知操作: $action（支持: prep / create / all / configure-domain / cri-multiplex / buildkit / download-cni / help）"
            ;;
    esac
}

main "$@"
