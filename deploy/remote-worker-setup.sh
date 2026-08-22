#!/bin/bash
# 在 worker 节点上远程执行的部署脚本（由 deploy-worker.sh 通过 ssh + bash -s 调用）
# 所需配置通过环境变量注入：
#   HTTP_PROXY SERVER_IP DEPLOY_DIR REMOTE_INFRA_DIR
#   CRI_MULTIPLEX_BIN CRI_MULTIPLEX_ORCHESTRATOR KUBELET_FLAGS_FILE REGISTRY IMAGE
set -euo pipefail
: "${SERVER_IP:?SERVER_IP 未设置}"
: "${DEPLOY_DIR:?DEPLOY_DIR 未设置}"
: "${REMOTE_INFRA_DIR:?REMOTE_INFRA_DIR 未设置}"

echo "[$(date '+%H:%M:%S')] 开始远程配置"
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
cp .env ".env.bak.$(date +%Y%m%d%H%M%S)"
sed -i "s/^export SERVER_IP=.*/export SERVER_IP=${SERVER_IP}/" .env
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
