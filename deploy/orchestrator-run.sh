#!/bin/bash
# e2b-orchestrator / template-manager systemd wrapper
# 职责：解析节点标识与本机 IP → 加载 /opt/e2b-infra/.env → 映射兼容环境变量 → 启动进程
# 双模式：DEPLOY_MODE=nomad|k8s（由 .env 提供，默认 nomad）
#   - nomad：NODE_ID 通过 Consul agent 反查（对齐 nomad 插值 ${node.unique.name}）
#   - k8s  ：NODE_ID = 主机名（对齐 Downward API spec.nodeName；
#            kubelet --hostname-override 场景须在 .env 显式配置 NODE_ID）
set -euo pipefail

E2B_DIR="/opt/e2b-infra"

# 加载 .env 并全部导出（orchestrator 依赖的数据库/存储/OTEL 等变量均在其中）
cd "$E2B_DIR"
set -a
# shellcheck disable=SC1091
source ./.env
set +a

# ---- 解析本机 IP ----
# 多节点集群中 .env 的 SERVER_IP 可能是其他节点（如 Master）地址，不能直接作为 Mooncake 本机地址
resolve_local_ip() {
    # 1) .env 显式指定（多网卡场景）
    if [ -n "${NODE_LOCAL_IP:-}" ]; then
        echo "$NODE_LOCAL_IP"
        return
    fi
    # 2) 默认路由出接口地址
    local ip
    ip=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.*src \([0-9.]\+\).*/\1/p' | head -n1 || true)
    # 3) 兜底：SERVER_IP（单机场景 == 本机）
    echo "${ip:-${SERVER_IP:-127.0.0.1}}"
}

# ---- 解析 NODE_ID（沙箱归属标识，必须与集群发现侧一致）----
resolve_node_id() {
    if [ -n "${NODE_ID:-}" ]; then
        echo "$NODE_ID"
        return
    fi
    if [ "${DEPLOY_MODE:-nomad}" = "k8s" ]; then
        hostname
        return
    fi
    # Nomad 模式：查询本机 Nomad agent 取 client 节点名（严格对齐插值 ${node.unique.name}）
    local nomad_addr="${NOMAD_ADDR:-http://127.0.0.1:4646}"
    local node_name=""
    local args=(-sf --max-time 3 "${nomad_addr}/v1/agent/self")
    if [ -n "${NOMAD_ACL_TOKEN:-}" ]; then
        args=(-H "X-Nomad-Token: ${NOMAD_ACL_TOKEN}" "${args[@]}")
    fi
    node_name=$(curl "${args[@]}" 2>/dev/null | jq -r '.config.Name // empty' 2>/dev/null || true)
    echo "${node_name:-$(hostname -s)}"
}

LOCAL_IP="$(resolve_local_ip)"
export NODE_ID="$(resolve_node_id)"
export HOST_IP="$LOCAL_IP"
export LOCAL_IP="$LOCAL_IP"

# ---- 与 Nomad job / K8S DaemonSet 对齐的兼容变量映射（.env 显式配置优先）----
# Redis 可选：ENABLE_REDIS=false 时清空注入地址（触发代码内 ErrRedisDisabled 降级），存储后端强制 memory
if [ "${ENABLE_REDIS:-true}" != "true" ]; then
    unset REDIS_URL REDIS_CLUSTER_URL REDIS_ENDPOINT
    if [ "${SANDBOX_STORAGE_BACKEND:-redis}" = "redis" ]; then
        SANDBOX_STORAGE_BACKEND="memory"
    fi
    export SANDBOX_STORAGE_BACKEND
# K8S 模式 redis 走集群内 Service（.env 的 *.service.consul 为 nomad/consul 寻址，K8S 节点不可解析）
elif [ "${DEPLOY_MODE:-nomad}" = "k8s" ]; then
    case "${REDIS_URL:-}" in
        ""|*.service.consul|*.service.consul:*)
            # 集群 DNS（*.svc.cluster.local）对宿主机不可见，且 redis Service 为 headless（无 ClusterIP），
            # 经 apiserver 取 redis Endpoints 节点地址（redis 为 hostNetwork 部署，地址稳定）
            redis_host=$(kubectl get endpoints redis -n e2b -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null || true)
            export REDIS_URL="${redis_host:-redis.e2b.svc.cluster.local}:${REDIS_PORT:-6379}" ;;
    esac
fi
export MOONCAKE_LOCAL_HOSTNAME="${MOONCAKE_LOCAL_HOSTNAME:-$LOCAL_IP}"
export MC_TCP_BIND_ADDRESS="${MC_TCP_BIND_ADDRESS:-$LOCAL_IP}"
export CONSUL_TOKEN="${CONSUL_TOKEN:-${CONSUL_ACL_TOKEN:-}}"
export GCP_DOCKER_REPOSITORY_NAME="${GCP_DOCKER_REPOSITORY_NAME:-${HARBOR_HOST:-}}"
export API_SECRET="${API_SECRET:-${EDGE_API_SECRET:-}}"
export ORCHESTRATOR_SERVICES="${ORCHESTRATOR_SERVICES:-orchestrator,template-manager}"
# Harbor 自签 CA：Go（go-containerregistry）经 SSL_CERT_FILE 加载，对齐 DaemonSet 时代的同名变量
if [ -z "${SSL_CERT_FILE:-}" ] && [ -f /etc/harbor/certs/harbor.crt ]; then
    export SSL_CERT_FILE=/etc/harbor/certs/harbor.crt
fi

exec "$E2B_DIR/bin/orchestrator" --port "${TEMPLATE_MANAGER_PORT:-5008}"
