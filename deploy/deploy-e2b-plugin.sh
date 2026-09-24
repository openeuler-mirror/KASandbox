#!/bin/bash
# OpenClaw E2B 插件自动化部署脚本（Docker & K8s 双模式）
# 退出后强制重启版本
# 用法:
#   Docker: ./deploy-e2b-plugin.sh docker [容器名] [E2B_IP] [API_PORT] [ENVD_PORT] [API_KEY] [超时] [模板]
#   K8s:    ./deploy-e2b-plugin.sh k8s    [Pod/Deployment名] [Namespace] [LabelSelector] [E2B_IP] ...

set -euo pipefail

# ==================== 参数解析 ====================
MODE="${1:-docker}"

if [ "$MODE" = "docker" ]; then
    TARGET="${2:-openclaw-deploy-for-local-exec-6d8696984f-8wb5h}"
    NAMESPACE=""; K8S_SELECTOR=""
    E2B_HOST_IP="${3:-127.0.0.1}"
    E2B_API_PORT="${4:-3000}"
    E2B_ENVD_PORT="${5:-3002}"
    E2B_API_KEY="${6:-${E2B_API_KEY:-}}"
    E2B_TIMEOUT="${7:-400}"
    E2B_TEMPLATE="${8:-base}"
elif [ "$MODE" = "k8s" ]; then
    TARGET="${2:-openclaw-deploy-for-local-exec-6d8696984f-8wb5h}"
    NAMESPACE="${3:-default}"
    K8S_SELECTOR="${4:-app=openclaw-deploy-for-local-exec}"
    E2B_HOST_IP="${5:-192.168.2.22}"
    E2B_API_PORT="${6:-3000}"
    E2B_ENVD_PORT="${7:-3002}"
    E2B_API_KEY="${8:-${E2B_API_KEY:-}}"
    E2B_TIMEOUT="${9:-400}"
    E2B_TEMPLATE="${10:-openclaw}"
else
    echo "错误: 模式必须是 docker 或 k8s" >&2; exit 1
fi

PLUGIN_REPO="https://gitcode.com/zhourenjianz/openclaw-sandbox-exec.git"
PLUGIN_DIR="/app/openclaw-sandbox-exec"
INDEX_JS="/root/.openclaw/extensions/local-exec/node_modules/e2b/dist/index.js"
CONFIG_FILE="/root/.openclaw/openclaw.json"

# ==================== 日志 ====================
log()  { echo -e "\033[1;32m[$(date +%H:%M:%S)]\033[0m $1"; }
warn() { echo -e "\033[1;33m[$(date +%H:%M:%S)] 警告:\033[0m $1"; }
err()  { echo -e "\033[1;31m[$(date +%H:%M:%S)] 错误:\033[0m $1"; exit 1; }

# ==================== K8s Pod 解析 ====================
resolve_pod() {
    if [ "$MODE" != "k8s" ]; then return; fi
    if kubectl get pod -n "$NAMESPACE" "$TARGET" >/dev/null 2>&1; then
        echo "$TARGET"; return
    fi
    local pod
    pod=$(kubectl get pods -n "$NAMESPACE" -l "$K8S_SELECTOR" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    [ -z "$pod" ] && err "找不到符合 selector ($K8S_SELECTOR) 的 Pod"
    echo "$pod"
}
POD_NAME=$(resolve_pod)

# ==================== 执行器封装 ====================
cx() {
    if [ "$MODE" = "docker" ]; then
        docker exec -i "$TARGET" bash -c "$1"
    else
        POD_NAME=$(resolve_pod)
        kubectl exec -n "$NAMESPACE" -i "$POD_NAME" -- bash -c "$1"
    fi
}

copy_and_exec() {
    local host_file="$1"
    local container_path="$2"
    local exec_cmd="$3"
    
    if [ "$MODE" = "docker" ]; then
        docker cp "$host_file" "$TARGET:$container_path"
        docker exec "$TARGET" "$exec_cmd" "$container_path"
    else
        POD_NAME=$(resolve_pod)
        kubectl cp "$host_file" "$NAMESPACE/$POD_NAME:$container_path"
        kubectl exec -n "$NAMESPACE" "$POD_NAME" -- "$exec_cmd" "$container_path"
    fi
}

check_running() {
    if [ "$MODE" = "docker" ]; then
        docker ps --format '{{.Names}}' | grep -q "^${TARGET}$"
    else
        local pod; pod=$(resolve_pod 2>/dev/null || true)
        [ -n "$pod" ] && kubectl get pod -n "$NAMESPACE" "$pod" -o jsonpath='{.status.phase}' 2>/dev/null | grep -q "Running"
    fi
}

# 等待目标就绪（用于强制重启后）
wait_ready() {
    log "等待目标重启后就绪..."
    local max_wait=60 waited=0
    while [ $waited -lt $max_wait ]; do
        if [ "$MODE" = "docker" ]; then
            if docker exec "$TARGET" echo "ok" >/dev/null 2>&1; then
                log "容器已就绪"; return 0
            fi
        else
            POD_NAME=$(resolve_pod)
            local phase
            phase=$(kubectl get pod -n "$NAMESPACE" "$POD_NAME" -o jsonpath='{.status.phase}' 2>/dev/null || true)
            if [ "$phase" = "Running" ]; then
                if kubectl exec -n "$NAMESPACE" -i "$POD_NAME" -- echo ok >/dev/null 2>&1; then
                    log "Pod 已就绪: $POD_NAME"; return 0
                fi
            fi
        fi
        sleep 2; waited=$((waited + 2)); echo -n "."
    done
    err "目标在 ${max_wait}s 内未能恢复"
}

# 强制重启目标
force_restart() {
    if [ "$MODE" = "docker" ]; then
        log "执行 docker restart ${TARGET}..."
        docker restart "$TARGET" >/dev/null 2>&1 || docker start "$TARGET" >/dev/null 2>&1
    else
        log "执行 kubectl delete pod 触发重建..."
        local current_pod
        current_pod=$(resolve_pod)
        kubectl delete pod -n "$NAMESPACE" "$current_pod" --grace-period=30 >/dev/null 2>&1
    fi
}

# 等待目标停止，然后强制重启，再等待就绪
wait_stop_then_force_restart() {
    log "执行强制重启以确保配置生效..."
    force_restart
    wait_ready
}

# ==================== 主流程 ====================
log "========================================"
log "OpenClaw E2B 插件自动化部署 [模式: ${MODE}]"
log "========================================"
[ "$MODE" = "k8s" ] && log "Namespace: ${NAMESPACE} | Selector: ${K8S_SELECTOR}"
log "E2B 服务 : ${E2B_HOST_IP}:${E2B_API_PORT} (API) / ${E2B_ENVD_PORT} (ENVD)"
log "========================================"

if ! check_running; then
    err "目标 ${TARGET} 未运行，请先启动"
fi

# 1. 克隆并编译插件
log "[1/7] 克隆插件源码..."
cx "cd /app && rm -rf ${PLUGIN_DIR} && git clone ${PLUGIN_REPO} ${PLUGIN_DIR}"

log "[2/7] 编译插件..."
cx "cd ${PLUGIN_DIR} && npm install --save-dev typescript && npm run build"

# 3. 安装插件（内部会触发退出）
log "[3/7] 安装插件到 OpenClaw..."
if [ "$MODE" = "docker" ]; then
    docker exec -i "$TARGET" bash -c "cd /app && openclaw plugins install openclaw-sandbox-exec" || true
else
    kubectl exec -n "$NAMESPACE" -i "$POD_NAME" -- bash -c "cd /app && openclaw plugins install openclaw-sandbox-exec" || true
fi

# 等待停止 -> 强制重启 -> 等待就绪
wait_stop_then_force_restart

# 4. 修改 E2B SDK 源码
log "[4/7] 修改 E2B SDK API 地址..."

TMP_SDK_PY=$(mktemp)
cat > "$TMP_SDK_PY" << 'PYEOF'
import re
with open('__INDEX_JS__', 'r') as f:
    content = f.read()

content = re.sub(
    r'this\.apiUrl = this\.debug \? "http://localhost:3000" : `https://api\.\$\{this\.domain\}`',
    'this.apiUrl = this.debug ? "http://localhost:3000" : `http://__E2B_HOST__:__E2B_PORT__`',
    content
)

content = re.sub(
    r'this\.envdApiUrl = `\$\{this\.connectionConfig\.debug \? "http" : "https"\}://\$\{this\.getHost\(this\.envdPort\)\}`',
    'this.envdApiUrl = `http://${this.getHost(this.envdPort)}`',
    content
)

with open('__INDEX_JS__', 'w') as f:
    f.write(content)
print('SDK fix done')
PYEOF

sed -i "s|__INDEX_JS__|${INDEX_JS}|g" "$TMP_SDK_PY"
sed -i "s|__E2B_HOST__|${E2B_HOST_IP}|g" "$TMP_SDK_PY"
sed -i "s|__E2B_PORT__|${E2B_API_PORT}|g" "$TMP_SDK_PY"

copy_and_exec "$TMP_SDK_PY" "/tmp/fix_sdk.py" "python3"
rm -f "$TMP_SDK_PY"

# 5. 修改 openclaw.json
log "[5/7] 注入 local-exec 配置到 openclaw.json..."

TMP_CFG_PY=$(mktemp)
cat > "$TMP_CFG_PY" << 'PYEOF'
import json
with open('__CONFIG_FILE__', 'r') as f:
    cfg = json.load(f)

cfg.setdefault('plugins', {})
cfg['plugins'].setdefault('entries', {})
cfg['plugins'].setdefault('installs', {})

lc = cfg['plugins']['entries'].get('local-exec', {})
lc['enabled'] = True
lc['config'] = {
    'sandboxEnabled': True,
    'e2bApiKey': '__E2B_API_KEY__',
    'e2bTimeout': __E2B_TIMEOUT__,
    'e2bTemplate': '__E2B_TEMPLATE__'
}
cfg['plugins']['entries']['local-exec'] = lc

if 'local-exec' not in cfg['plugins']['installs']:
    cfg['plugins']['installs']['local-exec'] = {
        'source': 'path',
        'sourcePath': '/app/openclaw-sandbox-exec',
        'installPath': '/root/.openclaw/extensions/local-exec',
        'version': '1.0.0',
        'installedAt': '2026-04-20T12:11:51.392Z'
    }

with open('__CONFIG_FILE__', 'w') as f:
    json.dump(cfg, f, indent=2, ensure_ascii=False)
    f.write('\n')
print('Config updated')
PYEOF

sed -i "s|__CONFIG_FILE__|${CONFIG_FILE}|g" "$TMP_CFG_PY"
sed -i "s|__E2B_API_KEY__|${E2B_API_KEY}|g" "$TMP_CFG_PY"
sed -i "s|__E2B_TIMEOUT__|${E2B_TIMEOUT}|g" "$TMP_CFG_PY"
sed -i "s|__E2B_TEMPLATE__|${E2B_TEMPLATE}|g" "$TMP_CFG_PY"

copy_and_exec "$TMP_CFG_PY" "/tmp/fix_config.py" "python3"
rm -f "$TMP_CFG_PY"

# openclaw.json 修改后也会触发退出，同样处理
wait_stop_then_force_restart

# 6. 安装网络工具
log "[6/7] 安装网络工具 (dnsmasq + socat)..."
cx "
    if command -v yum >/dev/null 2>&1; then
        yum install -y dnsmasq socat
    elif command -v apt-get >/dev/null 2>&1; then
        apt-get update -qq && apt-get install -y dnsmasq socat
    else
        echo '错误: 找不到 yum 或 apt-get' >&2; exit 1
    fi
"

# 7. 配置 DNS 劫持 + 端口转发
log "[7/7] 配置网络劫持与端口转发..."

TMP_DNS_SH=$(mktemp)
cat > "$TMP_DNS_SH" << 'SHEOF'
cat > /etc/dnsmasq.conf << 'EOF'
no-resolv
address=/.e2b.app/127.0.0.1
listen-address=127.0.0.1
bind-interfaces
EOF
pkill dnsmasq 2>/dev/null || true
dnsmasq --conf-file=/etc/dnsmasq.conf
SHEOF
copy_and_exec "$TMP_DNS_SH" "/tmp/setup_dnsmasq.sh" "bash"
rm -f "$TMP_DNS_SH"

TMP_RES_SH=$(mktemp)
cat > "$TMP_RES_SH" << 'SHEOF'
{ echo 'nameserver 127.0.0.1'; cat /etc/resolv.conf; } > /tmp/resolv.conf.new
if ! cat /tmp/resolv.conf.new > /etc/resolv.conf 2>/dev/null; then
    echo '警告: /etc/resolv.conf 被挂载，尝试 umount 后写入...' >&2
    umount /etc/resolv.conf 2>/dev/null || true
    cat /tmp/resolv.conf.new > /etc/resolv.conf
fi
SHEOF
copy_and_exec "$TMP_RES_SH" "/tmp/fix_resolv.sh" "bash"
rm -f "$TMP_RES_SH"

TMP_SOC_SH=$(mktemp)
cat > "$TMP_SOC_SH" << SHEOF
pkill -f 'socat TCP-LISTEN:80' 2>/dev/null || true
pkill -f 'socat TCP-LISTEN:443' 2>/dev/null || true
nohup socat TCP-LISTEN:80,fork,reuseaddr TCP:${E2B_HOST_IP}:${E2B_ENVD_PORT} >/dev/null 2>&1 &
nohup socat TCP-LISTEN:443,fork,reuseaddr TCP:${E2B_HOST_IP}:${E2B_ENVD_PORT} >/dev/null 2>&1 &
sleep 1
SHEOF
copy_and_exec "$TMP_SOC_SH" "/tmp/setup_socat.sh" "bash"
rm -f "$TMP_SOC_SH"
log ""
log "========================================"
log "全部步骤执行完毕！"
log "========================================"
