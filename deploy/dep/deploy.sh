#!/usr/bin/env bash
set -euo pipefail

# ===================== 基础配置 =====================
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="$SCRIPT_DIR/bin"
DEPLOY_TYPE="nomad"
REGISTRY_URL=""

# ===================== 输出函数 =====================
info()  { echo "==> $*"; }
warn()  { echo "==> WARN: $*"; }
step()  { echo "------> $*"; }
step2() { echo "======  $*  ======"; }

# ===================== 参数解析 =====================
parse_args() {
    while [[ "$#" -gt 0 ]]; do
        case "$1" in
            --type)
                DEPLOY_TYPE="$2"
                shift 2
                ;;
            *)
                echo "Unknown parameter: $1"
                exit 1
                ;;
        esac
    done
}

# ===================== 环境加载 =====================
load_env() {
    if [ ! -f "$SCRIPT_DIR/.env" ]; then
        echo "==> please cp .env.example .env and fill it with your current variable"
        exit 1
    fi
    # set -a 使 source 进来的变量自动 export，供 envsubst/helm 等子进程使用
    set -a
    source "$SCRIPT_DIR/.env"
    set +a
    REGISTRY_URL="$SERVER_IP:$HARBOR_HTTPS_PORT/$REGISTRY_PROJECT"
    export REGISTRY_URL
}

# ===================== 镜像构建与推送 =====================
# 构建 bin 目录下的 *.Dockerfile 并推送到 Harbor
build_and_push_dockerfiles() {
    cd "$BIN_DIR"
    local dockerfile name tag
    for dockerfile in *.Dockerfile; do
        # 无 Dockerfile 时 glob 返回字面量，跳过
        [ -e "$dockerfile" ] || continue
        name="${dockerfile%.Dockerfile}"
        tag="${REGISTRY_URL}/${name,,}"
        step "检查镜像 $tag 是否存在"
        if docker image inspect "$name" >/dev/null 2>&1; then
            docker tag "$name" "$tag"
            step "镜像 $name 已存在，跳过构建"
        elif docker image inspect "$tag" >/dev/null 2>&1; then
            step "镜像 $tag 已存在，跳过构建"
        else
            step "构建镜像 $tag"
            docker build --pull=false -t "$tag" -f "$dockerfile" .
        fi
        step "推送镜像 $tag"
        docker push "$tag"
    done
}

# 推送预构建镜像（redis/postgres/busybox）到 Harbor
push_prebuilt_images() {
    declare -A imgs=(
        [redis]="redis:${REDIS_VERSION}"
        [postgres]="postgres:latest"
        [busybox]="busybox:latest"
        # [vector]="timberio/vector:${LOGS_COLLECTOR_VERSION}"
        # [loki]="grafana/loki:${LOKI_VERSION}"
        # [otel]="otel/opentelemetry-collector-contrib:${OTEL_COLLECTOR_VERSION}"
        # [clickhouse]="clickhouse/clickhouse-server:${CLICKHOUSE_VERSION}"
    )
    local name src tag
    for name in "${!imgs[@]}"; do
        src="${imgs[$name]}"
        tag="${REGISTRY_URL}/${src}"
        step2 "$src  ->  $tag"
        docker tag "$src" "$tag"
        docker push "$tag"
    done
}

# 推送 webhook 镜像到 Harbor（受 ENABLE_WEBHOOK 开关控制）
# helm 模板期望镜像格式为 ${REGISTRY_URL}/${WEBHOOK_DOCKER_IMAGE}（不带版本标签）
push_webhook_image() {
    if [ "${ENABLE_WEBHOOK:-false}" != "true" ]; then
        return
    fi
    local webhook_tag="${REGISTRY_URL}/${WEBHOOK_DOCKER_IMAGE}"
    step2 "${WEBHOOK_DOCKER_IMAGE}  ->  ${webhook_tag}"
    if docker image inspect "$WEBHOOK_DOCKER_IMAGE" >/dev/null 2>&1; then
        docker tag "$WEBHOOK_DOCKER_IMAGE" "$webhook_tag"
        docker push "$webhook_tag"
    else
        warn "webhook 镜像 ${WEBHOOK_DOCKER_IMAGE} 本地不存在，跳过推送"
    fi
}

# ===================== K8S 部署 =====================
render_helm_values() {
    info "rendering helm values.yaml ..."
    envsubst < "$SCRIPT_DIR/helm/values-template.yaml" > "$SCRIPT_DIR/helm/values.yaml"
    # 删除未启用的可选组件模板
    rm -f "$SCRIPT_DIR/helm/templates/clickhouse.yaml" \
          "$SCRIPT_DIR/helm/templates/logs-collector.yaml" \
          "$SCRIPT_DIR/helm/templates/otel-collector.yaml" \
          "$SCRIPT_DIR/helm/templates/loki.yaml"
}

install_helm_chart() {
    info "installing with helm ..."
    helm install e2b-api "$SCRIPT_DIR/helm" --create-namespace -n e2b
}

# webhook 的 Deployment/Service/MutatingWebhookConfiguration 已由 helm 模板安装（受 .Values.webhook.enabled 控制）
# 此函数仅当 ENABLE_WEBHOOK=true 时负责:
#   1) 生成自签 TLS 证书并创建 Secret
#   2) 注入 caBundle 到 MutatingWebhookConfiguration
#   3) 创建 API Key Secret
setup_webhook_certificates() {
    if [ "${ENABLE_WEBHOOK:-false}" != "true" ]; then
        info "ENABLE_WEBHOOK=false, 跳过 e2b-webhook 证书配置"
        # 开关关闭时清理可能遗留的 webhook 资源
        kubectl -n e2b delete secret e2b-webhook-tls --ignore-not-found=true >/dev/null 2>&1 || true
        kubectl -n e2b delete secret e2b-api-key --ignore-not-found=true >/dev/null 2>&1 || true
        kubectl delete mutatingwebhookconfiguration e2b-webhook --ignore-not-found=true >/dev/null 2>&1 || true
        return
    fi

    info "ENABLE_WEBHOOK=true, 开始配置 e2b-webhook 证书 ..."

    local tls_dir webhook_svc webhook_svc_fqdn
    tls_dir="$(mktemp -d)"
    webhook_svc="e2b-webhook.e2b"
    webhook_svc_fqdn="e2b-webhook.e2b.svc"

    # 1. 生成自签 TLS 证书（用于 webhook 服务 HTTPS）
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
        -keyout "$tls_dir/tls.key" \
        -out "$tls_dir/tls.crt" \
        -subj "/CN=${webhook_svc}" \
        -addext "subjectAltName=DNS:${webhook_svc},DNS:${webhook_svc_fqdn},DNS:${webhook_svc_fqdn}.cluster.local" \
        >/dev/null 2>&1

    # 2. 创建 TLS Secret（幂等：先删后建）
    kubectl -n e2b delete secret e2b-webhook-tls --ignore-not-found=true >/dev/null 2>&1 || true
    kubectl -n e2b create secret tls e2b-webhook-tls \
        --cert="$tls_dir/tls.crt" \
        --key="$tls_dir/tls.key" >/dev/null

    # 3. 创建 E2B_API_KEY Secret（如 config.json 存在则注入）
    local api_key=""
    if [ -f /root/.e2b/config.json ]; then
        api_key=$(python3 -c "import json; print(json.load(open('/root/.e2b/config.json')).get('teamApiKey',''))" 2>/dev/null || true)
    fi
    kubectl -n e2b delete secret e2b-api-key --ignore-not-found=true >/dev/null 2>&1 || true
    if [ -n "$api_key" ]; then
        kubectl -n e2b create secret generic e2b-api-key \
            --from-literal=api-key="$api_key" >/dev/null
    fi

    # 4. 注入 caBundle 到 MutatingWebhookConfiguration（从 Secret 读取 tls.crt）
    local ca_bundle
    ca_bundle=$(kubectl -n e2b get secret e2b-webhook-tls -o jsonpath='{.data.tls\.crt}' 2>/dev/null || true)
    if [ -n "$ca_bundle" ]; then
        kubectl patch mutatingwebhookconfiguration e2b-webhook \
            --type='json' \
            -p="[{\"op\":\"replace\",\"path\":\"/webhooks/0/clientConfig/caBundle\",\"value\":\"${ca_bundle}\"},{\"op\":\"replace\",\"path\":\"/webhooks/1/clientConfig/caBundle\",\"value\":\"${ca_bundle}\"}]" \
            >/dev/null
        info "caBundle injected into e2b-webhook"
    else
        warn "failed to read caBundle from secret e2b-webhook-tls"
    fi
    rm -rf "$tls_dir"
}

# 等待所有 Pod 就绪，超时 5 分钟
wait_for_pods() {
    info "waiting for all pods to be running (timeout: 5min)..."
    local timeout=300 start_time current_time elapsed pod_status not_ready
    start_time=$(date +%s)

    while true; do
        current_time=$(date +%s)
        elapsed=$((current_time - start_time))
        if [ "$elapsed" -gt "$timeout" ]; then
            echo "==> ERROR: timeout waiting for pods to be running"
            echo "==> current pod status:"
            kubectl get pods -n e2b || true
            exit 1
        fi

        pod_status=$(kubectl get pods -n e2b --no-headers 2>/dev/null || true)
        if [ -z "$pod_status" ]; then
            echo "----> no pods found yet, waiting..."
            sleep 5
            continue
        fi

        # 统计未就绪 Pod 数量（ready 列 x/y 不相等）
        not_ready=$(echo "$pod_status" | awk '
            NF {
                split($2, ready, "/")
                if (ready[1] != ready[2]) { count++ }
            }
            END { print count+0 }
        ')

        if [ "$not_ready" -eq 0 ]; then
            info "all pods are running!"
            kubectl get pods -n e2b || true
            break
        fi
        echo "----> waiting for pods... ($not_ready not ready, elapsed: ${elapsed}s)"
        sleep 5
    done
}

deploy_k8s() {
    render_helm_values
    install_helm_chart
    setup_webhook_certificates
    wait_for_pods
}

# ===================== Nomad 部署 =====================
# envsubst 渲染所需的全部环境变量（新增 .env 变量时需同步追加到此列表）
render_hcl_files() {
    mkdir -p "$SCRIPT_DIR/rendered"
    info "rendering hcl ..."
    local env_vars='$ENVIRONMENT $DOMAIN_NAME $GCP_ZONE $CONSUL_ACL_TOKEN $NOMAD_ACL_TOKEN $EDGE_SECRET $LOKI_URL $CLIENT_PROXY_DOCKER_IMAGE $LOKI_SERVICE_PORT_NAME $LOKI_PROXY_MAX_RESOURCES_MEMORY_MB $LOKI_PROXY_RESOURCES_MEMORY_MB $LOKI_RESOURCES_CPU_COUNT
$POSTGRES_CONNECTION_STRING $REDIS_URL $REDIS_PORT $CLIENT_PROXY_COUNT $EDGE_PROXY_PORT_NAME $EDGE_API_PORT_NAME $CLIENT_PROXY_MAX_RESOURCES_MEMORY_MB $CLIENT_PROXY_RESOURCES_MEMORY_MB $CLIENT_PROXY_RESOURCES_CPU_COUNT
$SUPABASE_JWT_SECRETS $POSTHOG_API_KEY $ANALYTICS_COLLECTOR_HOST $ANALYTICS_COLLECTOR_API_TOKEN $HARBOR_HOST $HARBOR_PROJECT $HARBOR_USERNAME $HARBOR_PASSWORD $DOCKER_REVERSE_PROXY_DOCKER_IMAGE $DOCKER_REVERSE_PROXY_PORT_NAME
$LAUNCH_DARKLY_API_KEY $API_ADMIN_TOKEN $EDGE_API_SECRET $SANDBOX_ACCESS_TOKEN_HASH_SEED $CLICKHOUSE_RESOURCES_CPU_COUNT $CLICKHOUSE_RESOURCES_MEMORY_MB $CLICKHOUSE_SERVER_SECRET $REDIS_PORT_NAME $BUILD_CACHE_BUCKET_NAME
$CLICKHOUSE_SERVER_COUNT $CLICKHOUSE_BACKUPS_BUCKET_NAME $CLICKHOUSE_USERNAME $CLICKHOUSE_DATABASE $DNS_PORT $LOCAL_CLUSTER_ENDPOINT $API_DOCKER_IMAGE $API_PORT_NAME $DB_MIGRATOR_DOCKER_IMAGE $CLICKHOUSE_NODE_POOL $CLICKHOUSE_VERSION $MINIO_ENDPOINT $MINIO_ACCESS_KEY $MINIO_SECRET_KEY
$LOKI_BUCKET_NAME $LOGS_COLLECTOR_PUBLIC_IP $TEMPLATE_MANAGER_HOST $CLICKHOUSE_PASSWORD $OTEL_TRACING_PRINT $LOGS_COLLECTOR_ADDRESS $OTEL_COLLECTOR_GRPC_ENDPOINT $REDIS_CLUSTER_URL $OTEL_COLLECTOR_GRPC_PORT $REDIS_VERSION
$API_PORT $EDGE_API_PORT $EDGE_PROXY_PORT $ORCHESTRATOR_PORT $ORCHESTRATOR_PROXY_PORT $ENVD_TIMEOUT $TEMPLATE_BUCKET_NAME $ALLOW_SANDBOX_INTERNET $SHARED_CHUNK_CACHE_PATH $GRAFANA_OTLP_URL $CLICKHOUSE_HOST $REGISTRY_URL
$TEMPLATE_MANAGER_PORT $DOCKER_REVERSE_PROXY_PORT $LOKI_SERVICE_PORT $OTEL_COLLECTOR_PROXY_MAX_RESOURCES_MEMORY_MB $OTEL_COLLECTOR_PROXY_RESOURCES_MEMORY_MB $OTEL_COLLECTOR_RESOURCES_CPU_COUNT $GRAFANA_USERNAME $GRAFANA_OTEL_COLLECTOR_TOKEN
$LOGS_PROXY_PORT $LOGS_HEALTH_PROXY_PORT $STORAGE_PROVIDER $ARTIFACTS_REGISTRY_PROVIDER $API_NODE_POOL $BUILD_NODE_POOL $LOGS_COLLECTOR_VERSION $LOKI_VERSION $OTEL_COLLECTOR_VERSION $CLICKHOUSE_SERVER_PORT $CLICKHOUSE_METRICS_PORT $API_GRPC_PORT $EDGE_HEALTH_PORT $API_GRPC_ADDRESS $DOMAIN_NAME $SANDBOX_STORAGE_BACKEND
$HARBOR_CERTS_DIR $NODE_ID $GLOG_logtostderr $MOONCAKE_MASTER_ADDR $MOONCAKE_METADATA_SERVER $MOONCAKE_LOCAL_BUFFER_SIZE $MOONCAKE_GLOBAL_SEGMENT_SIZE $MOONCAKE_PROTOCOL $MC_URMA_TRANS_MODE $MOONCAKE_DEVICE_NAME $MC_LOG_ENABLE $MC_LOG_DIR $MC_LOG_LEVEL $MC_STORE_LOCAL_HOT_CACHE_USE_SHM $MC_STORE_LOCAL_HOT_BLOCK_SIZE $MC_STORE_LOCAL_HOT_ADMISSION_THRESHOLD $MC_SLICE_SIZE $MC_WORKERS_PER_CTX $MC_MAX_WR
$E2B_FC_NETNS_EXEC_HELPER $E2B_USE_FC_NETNS_EXEC_HELPER
$API_RESOURCES_CPU_COUNT $API_RESOURCES_MEMORY_MB $API_LIMITS_CPU_COUNT $API_LIMITS_MEMORY_MB
$TEMPLATE_MANAGER_RESOURCES_CPU_COUNT $TEMPLATE_MANAGER_RESOURCES_MEMORY_MB $TEMPLATE_MANAGER_LIMITS_CPU_COUNT $TEMPLATE_MANAGER_LIMITS_MEMORY_MB'

    local hcl outfile
    for hcl in "$SCRIPT_DIR"/nomad/*.hcl; do
        [ -e "$hcl" ] || continue
        outfile="$SCRIPT_DIR/rendered/$(basename "$hcl")"
        envsubst "$env_vars" < "$hcl" > "$outfile"
    done
}

submit_nomad_jobs() {
    local jobs=(redis template-manager edge api)
    info "submitting nomad job..."
    local j
    for j in "${jobs[@]}"; do
        if [ -f "$SCRIPT_DIR/rendered/${j}.hcl" ]; then
            echo "-----> $j"
            nomad job run --token "$NOMAD_ACL_TOKEN" "$SCRIPT_DIR/rendered/${j}.hcl"
        fi
    done
}

deploy_nomad() {
    render_hcl_files
    submit_nomad_jobs
}

# ===================== 数据库初始化 =====================
# 检查 E2B 团队是否已初始化，未初始化则导入默认用户并调整 sandbox 配额
init_database() {
    local pg_container="postgres"
    local pg_user="postgres"
    local pg_db="mydatabase"
    local query="SELECT EXISTS (SELECT 1 FROM teams WHERE name = 'E2B');"

    # --tuples-only 只输出数据行；-q 静默模式；xargs 清理首尾空格
    local result
    result=$(docker exec "$pg_container" psql -U "$pg_user" -d "$pg_db" -q --tuples-only -c "$query" 2>/dev/null | xargs)

    if [ "$result" != "t" ]; then
        info "importing default E2B user..."
        mkdir -p /root/.e2b
        cat > /root/.e2b/config.json <<'EOF'
{
    "email": "admin@e2b.dev",
    "accessToken": "",
    "teamName": "E2B",
    "teamId": "",
    "teamApiKey": ""
}
EOF

        info "initializing user into postgres..."
        chmod +x "$BIN_DIR/seed-db"
        POSTGRES_CONNECTION_STRING="$POSTGRES_CONNECTION_STRING" "$BIN_DIR/seed-db" | tee "$SCRIPT_DIR/user.txt"

        # 从 user.txt 中提取变量
        local team_id token api_key
        team_id=$(grep "Team ID:" "$SCRIPT_DIR/user.txt" | awk -F': ' '{print $2}')
        token=$(grep "Access Token:" "$SCRIPT_DIR/user.txt" | awk -F': ' '{print $2}')
        api_key=$(grep "Team API Key:" "$SCRIPT_DIR/user.txt" | awk -F': ' '{print $2}')

        cat > /root/.e2b/config.json <<EOF
{
    "teamId": "$team_id",
    "accessToken": "$token",
    "teamApiKey": "$api_key"
}
EOF
        info "config.json 写入完成"

        info "Setting sandbox timeout to 10000 hours"
        docker exec "$pg_container" psql -U "$pg_user" -d "$pg_db" -c "UPDATE tiers SET max_length_hours = 10000 WHERE id = 'base_v1';"
        info "Setting sandbox timeout to 10000 hours done!"

        info "Setting sandbox concurrent instances to 10000"
        docker exec "$pg_container" psql -U "$pg_user" -d "$pg_db" -c "UPDATE tiers SET concurrent_instances = 10000 WHERE id = 'base_v1';"
        info "Setting sandbox concurrent instances to 10000 done!"
    fi
}

# ===================== 主流程 =====================
main() {
    parse_args "$@"
    load_env
    build_and_push_dockerfiles
    push_prebuilt_images
    push_webhook_image

    case "$DEPLOY_TYPE" in
        k8s)   deploy_k8s ;;
        nomad) deploy_nomad ;;
        *)
            echo "Unknown deploy type: $DEPLOY_TYPE"
            exit 1
            ;;
    esac

    init_database
    info "E2B Local deploy complete!"
}

main "$@"
