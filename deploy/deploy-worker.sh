#!/bin/bash
set -euo pipefail
source /opt/e2b-infra/.env
# ========== 全局配置 ==========
SSH_KEY="${HOME}/.ssh/id_rsa"
SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes"
SSH_CONTROL_PATH="${HOME}/.ssh/ctrl-%h-%p-%r"
SSH_MUX_OPTS="-o ControlMaster=auto -o ControlPath=${SSH_CONTROL_PATH} -o ControlPersist=60"
REGISTRY="${SERVER_IP}:30443"
IMAGE="e2b-orchestration/orchestrator:latest"
CERT_SRC="/etc/containerd/certs.d/${REGISTRY}"
INFRA_SRC="${E2B_INFRA_SRC:-/home/e2b}"
REMOTE_INFRA_DIR="/home/e2b"
DEPLOY_DIR="/opt/e2b-infra"
HARBOR_CERTS="/etc/harbor/certs/harbor.crt"
E2B_API_TOKEN="/root/.e2b/config.json"
HTTP_PROXY="${HTTP_PROXY:-}"
CRI_MULTIPLEX_BIN="${CRI_MULTIPLEX_BIN:-/opt/e2b-infra/bin/cri-multiplex}"
CRI_MULTIPLEX_ORCHESTRATOR="${CRI_MULTIPLEX_ORCHESTRATOR:-localhost:5008}"
KUBELET_FLAGS_FILE="${KUBELET_FLAGS_FILE:-/var/lib/kubelet/kubeadm-flags.env}"
PARALLEL=1

# ========== 颜色定义 ==========
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[✓]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
log_error() { echo -e "${RED}[✗]${NC} $*" >&2; }
log_step()  { echo -e "${CYAN}${BOLD}[$1]${NC} $2"; }
log_header(){ echo -e "\n${BOLD}${CYAN}===== $1 =====${NC}"; }

# ========== 函数：SSH 连接管理 ==========
ssh_open() {
    local host="$1"
    mkdir -p "${HOME}/.ssh"
    ssh -i "$SSH_KEY" $SSH_OPTS $SSH_MUX_OPTS "root@${host}" "exit" 2>/dev/null || true
}

ssh_close() {
    local host="$1"
    ssh -i "$SSH_KEY" $SSH_OPTS $SSH_MUX_OPTS -O exit "root@${host}" 2>/dev/null || true
}

ssh_run() {
    ssh -i "$SSH_KEY" $SSH_OPTS $SSH_MUX_OPTS "root@$1" "${@:2}"
}

scp_to() {
    local host="$1"
    shift
    scp -i "$SSH_KEY" $SSH_OPTS $SSH_MUX_OPTS "$@"
}

# ========== 函数：获取节点 IP ==========
get_node_ip() {
    local node="$1"
    kubectl get node "$node" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}'
}

# ========== 函数：配置 SSH 免密 ==========
setup_ssh_key() {
    local node="$1"
    local node_ip
    node_ip=$(get_node_ip "$node")

    if [ ! -f "$SSH_KEY" ]; then
        log_warn "SSH 密钥不存在，正在生成: $SSH_KEY"
        ssh-keygen -t rsa -b 4096 -f "$SSH_KEY" -N "" -q
        log_info "SSH 密钥已生成"
    fi

    if [ ! -f "${SSH_KEY}.pub" ]; then
        log_warn "SSH 公钥不存在，正在从私钥导出: ${SSH_KEY}.pub"
        ssh-keygen -y -f "$SSH_KEY" > "${SSH_KEY}.pub"
    fi

    if ssh -i "$SSH_KEY" $SSH_OPTS "root@${node_ip}" "exit" 2>/dev/null; then
        log_info "SSH 免密已配置: root@${node_ip}"
    else
        log_warn "配置 SSH 免密登录: root@${node_ip} ..."
        ssh-copy-id -i "${SSH_KEY}.pub" "root@${node_ip}" 2>/dev/null
    fi
}

# ========== 函数：前置检查 ==========
preflight_check() {
    local errors=0

    log_step "预检" "检查本地环境..."

    command -v kubectl >/dev/null 2>&1 || { log_error "kubectl 未安装"; ((errors++)); }
    command -v ssh >/dev/null 2>&1 || { log_error "ssh 未安装"; ((errors++)); }
    command -v scp >/dev/null 2>&1 || { log_error "scp 未安装"; ((errors++)); }

    [ -f "$SSH_KEY" ] || { log_error "SSH 私钥不存在: $SSH_KEY"; ((errors++)); }
    [ -d "$INFRA_SRC" ] || { log_error "本地 e2b-infra 目录不存在: $INFRA_SRC"; ((errors++)); }
    [ -d "$CERT_SRC" ] || { log_error "containerd 证书目录不存在: $CERT_SRC"; ((errors++)); }
    [ -f "$HARBOR_CERTS" ] || { log_error "Harbor 证书不存在: $HARBOR_CERTS"; ((errors++)); }
    [ -f "$E2B_API_TOKEN" ] || { log_error "E2B API Token 不存在: $E2B_API_TOKEN"; ((errors++)); }

    if [ "$errors" -gt 0 ]; then
        log_error "前置检查失败，共 ${errors} 项错误"
        return 1
    fi

    log_info "前置检查通过"
}

# ========== 函数：在单个节点上执行部署 ==========
deploy_node() {
    local node="$1"
    local node_ip
    node_ip=$(get_node_ip "$node")
    if [ -z "$node_ip" ]; then
        log_error "无法获取节点 $node 的 IP"
        return 1
    fi

    log_header "部署节点: $node (IP: $node_ip)"

    setup_ssh_key "$node"
    ssh_open "${node_ip}"

    local step_total=6

    # ---------- 步骤 1：分发 e2b-infra 代码包 ----------
    log_step "1/${step_total}" "分发 e2b-infra 代码包到 $node ..."
    ssh_run "${node_ip}" "mkdir -p ${REMOTE_INFRA_DIR}" 2>/dev/null || true
    if scp_to "${node_ip}" -r "${INFRA_SRC}/"* "root@${node_ip}:${REMOTE_INFRA_DIR}/"; then
        log_info "代码包分发完成"
    else
        log_error "代码包分发失败"
        ssh_close "${node_ip}"
        return 1
    fi

    # ---------- 步骤 2：复制 containerd 证书 ----------
    log_step "2/${step_total}" "复制 containerd 证书到 $node ..."
    ssh_run "${node_ip}" "mkdir -p /etc/containerd/certs.d" 2>/dev/null || true
    if scp_to "${node_ip}" -r "$CERT_SRC" "root@${node_ip}:/etc/containerd/certs.d/"; then
        log_info "containerd 证书复制完成"
    else
        log_error "containerd 证书复制失败"
        ssh_close "${node_ip}"
        return 1
    fi

    # ---------- 步骤 3：复制 harbor 证书 ----------
    log_step "3/${step_total}" "复制 harbor 证书到 $node ..."
    ssh_run "${node_ip}" "mkdir -p /etc/harbor/certs" 2>/dev/null || true
    if scp_to "${node_ip}" "$HARBOR_CERTS" "root@${node_ip}:/etc/harbor/certs/"; then
        log_info "Harbor 证书复制完成"
    else
        log_error "Harbor 证书复制失败"
        ssh_close "${node_ip}"
        return 1
    fi

    # ---------- 步骤 4：复制 e2b-api-token ----------
    log_step "4/${step_total}" "复制 e2b-api-token 到 $node ..."
    ssh_run "${node_ip}" "mkdir -p /root/.e2b" 2>/dev/null || true
    if scp_to "${node_ip}" "$E2B_API_TOKEN" "root@${node_ip}:/root/.e2b/"; then
        log_info "E2B API Token 复制完成"
    else
        log_error "E2B API Token 复制失败"
        ssh_close "${node_ip}"
        return 1
    fi

    # ---------- 步骤 5：远程执行构建、重启服务 ----------
    log_step "5/${step_total}" "远程执行部署命令..."
    if ssh_run "${node_ip}" bash -s <<REMOTE_EOF; then
set -euo pipefail
echo "[\$(date '+%H:%M:%S')] 开始远程配置"
export http_proxy="${HTTP_PROXY}"
export https_proxy="${HTTP_PROXY}"
mkdir -p ${DEPLOY_DIR}

if [ "${REMOTE_INFRA_DIR}" != "${DEPLOY_DIR}" ]; then
    cp -r ${REMOTE_INFRA_DIR}/* ${DEPLOY_DIR}/ 2>/dev/null || true
fi

if ls /home/e2b/*.rpm >/dev/null 2>&1; then
    rpm -ivh /home/e2b/*.rpm --force
else
    echo "  无 rpm 包需要安装"
fi

cd ${DEPLOY_DIR}
cp .env ".env.bak.\$(date +%Y%m%d%H%M%S)"
sed -i "s/^export SERVER_IP=.*/export SERVER_IP=${node_ip}/" .env
sed -i 's/^export DEPLOY_MODE=.*/export DEPLOY_MODE=k8s/' .env
echo "  .env 已更新:"
grep -E "^export SERVER_IP|^export DEPLOY_MODE" .env

echo "  执行 build.sh --install-client ..."
bash build.sh --install-client

echo "  执行 init-client.sh ..."
bash init-client.sh || echo "  init-client.sh 执行失败（可能已初始化）"
unset http_proxy
unset https_proxy
mkdir -p /fc-versions/v1.13.1/
cp /home/e2b/firecracker /fc-versions/v1.13.1/ 2>/dev/null || echo "  firecracker 复制失败或不存在"

echo "  重启 kubelet ..."
systemctl restart kubelet

echo "  重启 containerd ..."
systemctl restart containerd

# 部署 cri-multiplex（多 runtime 复用器）
echo "  部署 cri-multiplex ..."
if [ -x "${CRI_MULTIPLEX_BIN}" ]; then
    export CRI_MULTIPLEX_BIN CRI_MULTIPLEX_ORCHESTRATOR KUBELET_FLAGS_FILE
    bash "${DEPLOY_DIR}/k8s-deploy.sh" cri-multiplex
else
    echo "  WARN: 未找到 cri-multiplex 可执行文件: ${CRI_MULTIPLEX_BIN}，跳过部署"
fi

echo "  测试镜像拉取 ${REGISTRY}/${IMAGE} ..."
crictl pull ${REGISTRY}/${IMAGE}
echo "  镜像拉取成功"

exit 0
REMOTE_EOF
        log_info "远程命令执行成功"
    else
        log_error "远程命令执行失败"
        ssh_close "${node_ip}"
        return 1
    fi

    # ---------- 步骤 6：设置节点标签 ----------
    log_step "6/${step_total}" "设置节点标签..."
    kubectl label node "$node" node-role.kubernetes.io/sandbox=true --overwrite
    kubectl label node "$node" node-role.kubernetes.io/${BUILD_NODE_POOL}= --overwrite
    log_info "标签设置完成"

    ssh_close "${node_ip}"
    log_info "$node 部署成功"
}

# ========== 函数：并行部署 ==========
deploy_parallel() {
    local nodes=("$@")
    local pids=()
    local tmpdir
    tmpdir=$(mktemp -d)
    local i=0

    for node in "${nodes[@]}"; do
        (
            deploy_node "$node" > "${tmpdir}/${node}.log" 2>&1
            echo "0" > "${tmpdir}/${node}.status"
        ) &
        pids+=($!)
        ((i++))
    done

    for pid in "${pids[@]}"; do
        wait "$pid" 2>/dev/null || true
    done

    local failed=()
    for node in "${nodes[@]}"; do
        if [ -f "${tmpdir}/${node}.status" ]; then
            cat "${tmpdir}/${node}.log"
            if [ "$(cat "${tmpdir}/${node}.status")" != "0" ]; then
                failed+=("$node")
            fi
        else
            cat "${tmpdir}/${node}.log"
            failed+=("$node")
        fi
    done

    rm -rf "$tmpdir"
    return ${#failed[@]}
}

# ========== 主流程 ==========
show_usage() {
    cat <<EOF
用法: $0 [选项] <节点1> [节点2] [节点3] ...

选项:
  -h, --help       显示帮助信息
  -p, --parallel   并行部署所有节点

示例:
  $0 worker1 worker2 worker8
  $0 --parallel worker1 worker2 worker8

环境变量:
  E2B_INFRA_SRC    本地 e2b-infra 代码路径 (默认: /home/e2b)

部署流程:
  1. 分发 e2b-infra 代码包到目标节点
  2. 复制 containerd 私有仓库证书
  3. 复制 Harbor SSL 证书
  4. 复制 E2B API Token
  5. 远程执行构建、安装、重启服务
  6. 设置 Kubernetes 节点标签 (sandbox, api)
EOF
}

main() {
    local nodes=()
    while [ $# -gt 0 ]; do
        case "$1" in
            -h|--help) show_usage; exit 0 ;;
            -p|--parallel) PARALLEL=0; shift ;;
            *) nodes+=("$1"); shift ;;
        esac
    done

    if [ ${#nodes[@]} -eq 0 ]; then
        show_usage
        exit 1
    fi

    local start_time
    start_time=$(date +%s)

    log_header "多节点部署"
    echo "目标节点: ${nodes[*]}"
    echo "本地 e2b-infra 路径: $INFRA_SRC"
    echo "部署模式: $([ $PARALLEL -eq 0 ] && echo '并行' || echo '串行')"

    preflight_check || exit 1

    local failed=()

    if [ $PARALLEL -eq 0 ] && [ ${#nodes[@]} -gt 1 ]; then
        deploy_parallel "${nodes[@]}" || true
        for node in "${nodes[@]}"; do
            local node_ip
            node_ip=$(get_node_ip "$node" 2>/dev/null) || true
            if ! ssh -i "$SSH_KEY" $SSH_OPTS $SSH_MUX_OPTS "root@${node_ip}" "test -d ${DEPLOY_DIR}" 2>/dev/null; then
                failed+=("$node")
            fi
        done
    else
        for node in "${nodes[@]}"; do
            if ! deploy_node "$node"; then
                failed+=("$node")
            fi
        done
    fi

    local end_time elapsed
    end_time=$(date +%s)
    elapsed=$((end_time - start_time))

    log_header "部署结果"
    echo "总节点数: ${#nodes[@]}"
    echo "成功: $((${#nodes[@]} - ${#failed[@]}))"
    echo "失败: ${#failed[@]}"
    echo "耗时: ${elapsed}s"

    if [ ${#failed[@]} -gt 0 ]; then
        log_error "失败节点: ${failed[*]}"
        exit 1
    else
        log_info "全部节点部署成功"
    fi
}

main "$@"
