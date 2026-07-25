# E2B Infra 部署设计文档

## 1. 系统概述

E2B Infra 是一个基于沙箱（Sandbox）的代码执行平台，支持在隔离的微虚拟机（Firecracker）中安全运行用户代码。平台提供模板管理、沙箱编排、API 网关等核心能力。

### 1.1 两种部署模式

| 模式 | 调度器 | 容器运行时 | 适用场景 |
|------|--------|------------|----------|
| **Nomad** | HashiCorp Nomad | Docker / docker-compose | 单机/小规模部署 |
| **K8S** | Kubernetes | nerdctl / containerd | 多节点/生产环境 |

### 1.2 架构图

```
┌──────────────────────────────────────────────────────────────────────┐
│                           用户请求                                    │
└──────────────────────────────┬───────────────────────────────────────┘
                               │
              ┌────────────────┴────────────────┐
              │                                 │
   ┌──────────▼──────────┐          ┌───────────▼──────────┐
   │    API Service      │          │   Client Proxy       │
   │   (orchestrator)    │          │   (envd 代理)        │
   │   沙箱 API 入口     │          │   沙箱连接代理       │
   └──────────┬──────────┘          └───────────┬──────────┘
              │                                 │
              └────────────────┬────────────────┘
                               │ 调用
              ┌────────────────▼────────────────┐
              │      Template Manager           │
              │    (模板管理/沙箱生命周期)        │
              └────────────────┬────────────────┘
                               │ 创建沙箱
              ┌────────────────▼────────────────┐
              │     Firecracker 微虚拟机         │
              │  (沙箱实例: 隔离的代码执行环境)    │
              └─────────────────────────────────┘


═══════════════════════════════════════════════════════════════════════
                     调度层 & 基础设施层
═══════════════════════════════════════════════════════════════════════

  ┌─────────────────────────────────────────────────────────────────┐
  │              Nomad (nomad模式) / Kubernetes (k8s模式)            │
  │         统一管理: API, Client Proxy, Template Manager, Redis    │
  └─────────────────────────────────────────────────────────────────┘

  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
  │  PostgreSQL  │  │    Redis     │  │   Harbor     │  │  dnsmasq     │
  │   :5432     │  │ (Nomad任务)  │  │ (本地镜像仓库)│  │ (DNS解析)    │
  │  元数据存储  │  │  缓存/会话   │  │  镜像获取    │  │ .e2b.app     │
  └──────────────┘  └──────────────┘  └──────────────┘  └──────────────┘

  ┌─────────────────────────────────────────────────────────────────┐
  │                    Nginx (反向代理/SSL)                          │
  │              HTTP:80 → 3002  /  HTTPS:443 → 30443              │
  └─────────────────────────────────────────────────────────────────┘
```

### 1.3 请求调用链路

```
用户请求 ──→ API / Client Proxy ──→ Template Manager ──→ Firecracker
                                          │
                                          │ 拉取沙箱镜像
                                          ▼
                                      Harbor (本地镜像仓库)
```

1. **用户请求** 发送到 API Service 或 Client Proxy
2. **API / Client Proxy** 调用 **Template Manager** 处理沙箱生命周期
3. **Template Manager** 从 **Harbor** 拉取沙箱镜像，调用 **Firecracker** 创建微虚拟机
4. **Nomad / K8S** 统一调度管理 API、Client Proxy、Template Manager、Redis 四个服务

## 2. 组件清单

### 2.1 核心组件

| 组件 | 版本 | 部署方式 | 端口 | 说明 |
|------|------|----------|------|------|
| **Consul** | 1.21.4 | systemd 服务 | 8500 | 服务发现/配置中心（Nomad 模式） |
| **Nomad** | 1.10.4 | systemd 服务 | 4646 | 任务调度器（Nomad 模式） |
| **Kubernetes** | - | 集群 | - | 容器编排（K8S 模式） |
| **PostgreSQL** | latest | Docker 容器 | 5432 | 元数据存储 |
| **Redis** | 7.4.4-alpine | Nomad 任务 | - | 缓存/会话存储 |
| **Harbor** | 2.13.0 | docker-compose | 2900(HTTP) / 30443(HTTPS) | 镜像仓库 |
| **Nginx** | yum 安装 | systemd 服务 | 80 / 443 | 反向代理 / SSL 终端 |
| **dnsmasq** | yum 安装 | systemd 服务 | 53 | DNS 解析（.e2b.app 域名） |
| **Firecracker** | 1.13.1 | 二进制 | - | 微虚拟机运行时 |

### 2.2 E2B 业务组件

| 组件 | 二进制 | 说明 |
|------|--------|------|
| **orchestrator** | /usr/bin/orchestrator | 沙箱编排服务（API） |
| **template-manager** | /usr/bin/template-manager | 模板管理服务 |

### 2.3 基础依赖

| 包 | 用途 |
|----|------|
| curl, unzip, jq, tar, rsync | 系统工具 |
| python3, pip | e2b SDK 安装 |
| openssl | SSL 证书生成 |
| wget | 离线包下载 |

## 3. 目录结构

```
/root/e2b-deploy/                    # 脚本工作目录
├── build.sh                         # 主管理脚本
├── deploy-worker.sh                 # K8S Worker 节点部署脚本
├── template-ubuntu.py               # Ubuntu 模板制作
├── create_sandbox.py                # 沙箱创建工具
├── create_template.py               # 模板创建工具
├── build_prod.py                    # 生产镜像构建
├── test_build.sh                    # 测试用例
└── dep/                             # 依赖文件目录
    ├── .env                         # 环境变量配置
    ├── install-nomad.sh             # Nomad 安装脚本
    ├── install-consul.sh            # Consul 安装脚本
    ├── uninstall-nomad.sh           # Nomad 卸载脚本
    ├── start-server.sh              # 服务端启动脚本
    ├── start-client.sh              # 客户端启动脚本
    ├── init-client.sh               # 客户端初始化脚本
    ├── run-nomad.sh                 # Nomad 运行脚本
    ├── run-consul.sh                # Consul 运行脚本
    ├── deploy.sh                    # 部署脚本
    ├── deploy-e2b-plugin.sh         # E2B 插件部署脚本
    ├── nginx.conf                   # Nginx 配置
    ├── harbor.cnf                   # Harbor SSL 证书配置
    ├── daemon.json                  # Docker daemon 配置
    ├── default.hcl                  # Nomad 默认配置
    ├── template-manager.hcl         # Nomad Job 配置
    ├── template-manager.yaml        # K8S Deployment 配置
    ├── wildcard-ingress.yaml        # K8S Ingress 配置
    ├── openclaw.yaml                # OpenClaw 部署配置
    ├── *.tar / *.tar.gz             # 离线镜像包
    └── *.rpm                        # RPM 安装包

/opt/e2b-infra/                      # E2B 运行目录
├── bin/
│   ├── orchestrator                 # 编排器二进制
│   └── template-manager             # 模板管理器二进制
├── nomad/
│   └── template-manager.hcl         # Nomad Job 定义
├── .env                             # 运行时环境变量
├── start-server.sh                  # 服务端启动
├── start-client.sh                  # 客户端启动
├── init-client.sh                   # 客户端初始化
├── deploy.sh                        # 部署入口
└── patch_e2b.py                     # E2B 补丁脚本

/etc/nginx/ssl/                      # SSL 证书目录
├── harbor.cnf                       # 证书请求配置
├── harbor.crt                       # 证书文件
└── harbor.key                       # 私钥文件

/etc/containerd/certs.d/             # containerd 证书目录（K8S 模式）
└── <IP>:<PORT>/
    ├── harbor.crt
    ├── harbor.key
    └── hosts.toml

/etc/docker/certs.d/                 # Docker 证书目录（Nomad 模式）
└── harbor:443/
    └── ca.crt

/fc-versions/v1.13.1/                # Firecracker 二进制
└── firecracker
```

## 4. 环境变量

配置文件：`dep/.env`

### 4.1 核心变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `SERVER_IP` | 空（必填） | 服务器 IP 地址 |
| `DEPLOY_MODE` | `nomad` | 部署模式：nomad / k8s |
| `ENVIRONMENT` | `local` | 环境标识 |
| `NUM_SERVERS` | `1` | Nomad Server 数量 |
| `CONSUL_VERSION` | `1.21.4` | Consul 版本 |
| `NOMAD_VERSION` | `1.10.4` | Nomad 版本 |

### 4.2 端口配置

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `HARBOR_HTTP_PORT` | `2900` | Harbor HTTP 端口 |
| `HARBOR_HTTPS_PORT` | `30443` | Harbor HTTPS 端口 |
| `PG_PORT` | `5432` | PostgreSQL 端口 |
| `NOMAD_PORT` | `4646` | Nomad API 端口 |
| `MINIO_PORT` | `9000` | MinIO API 端口 |
| `MINIO_CONSOLE_PORT` | `9001` | MinIO 控制台端口 |

### 4.3 凭证配置

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `HARBOR_USER` | `admin` | Harbor 管理员 |
| `HARBOR_PASSWORD` | 必填 | Harbor 密码 |
| `POSTGRES_CONNECTION_STRING` | `postgresql://postgres:local@...` | PostgreSQL 连接串 |
| `NOMAD_ACL_TOKEN` | 空 | Nomad ACL Token |
| `CONSUL_ACL_TOKEN` | 空 | Consul ACL Token |

## 5. 部署流程

### 5.1 Nomad 模式部署流程

```
--install                          --start
    │                                  │
    ▼                                  ▼
yum_install                     init-client.sh
    │                                  │
    ▼                                  ▼
setenforce 0                    start_postgres
    │                                  │
    ▼                                  ▼
install_e2b                     deploy_harbor
    │                           ┌──────┴──────┐
    ├── pip install e2b         │             │
    ├── 复制脚本到 /opt/e2b     ▼             ▼
    ├── modify_nomad       start_harbor   harbor_wait_healthy
    ├── patch_e2b.py           │             │
    └── dnsmasq配置            ▼             ▼
    │                      harbor_login  harbor_create_project
    ▼                                  │
install_docker                        ▼
    │                          start-server.sh
    ▼                                  │
install_consul                        ▼
    │                          append_nomad_client_config
    ▼                                  │
install_nomad                         ▼
    │                          systemctl restart nomad
    ▼                                  │
pull_docker_images                    ▼
    │                          wait_for_port 4646
    ▼                                  │
install_postgres                      ▼
    │                          deploy.sh
    ▼                                  │
install_harbor                        ▼
    │                          iptable_clean (可选)
    ▼
install_nginx
```

### 5.2 K8S 模式部署流程

```
--install --k8s <node>             --start --k8s <node>
    │                                  │
    ▼                                  ▼
yum_install                     init-client.sh
    │                                  │
    ▼                                  ▼
setenforce 0                    start_postgres
    │                                  │
    ▼                                  ▼
install_e2b                     deploy_harbor
    │                           ┌──────┴──────┐
    ├── pip install e2b         │             │
    ├── 复制脚本到 /opt/e2b     ▼             ▼
    └── dnsmasq配置        start_harbor   harbor_wait_healthy
    │                              │             │
    ▼                              ▼             ▼
pull_docker_images            harbor_login  harbor_create_project
    │                                  │
    ▼                                  ▼
install_postgres                systemctl restart kubelet
    │                                  │
    ▼                                  ▼
install_harbor                  kubectl label node
    │                                  │
    ▼                                  ▼
install_nginx                   deploy.sh --type k8s
                                       │
                                       ▼
                               配置 Ingress 域名访问
                            (wildcard-ingress.yaml)
```

### 5.3 K8S 模式域名访问配置

K8S 模式下，宿主机需要通过 `*.e2b.app` 域名访问 API 服务，涉及三层配置：Ingress Controller 端口监听、CoreDNS 域名重写、Ingress 路由规则。

#### 5.3.1 Ingress Controller 监听 80 端口

默认 Ingress Nginx Controller 的 Service 可能未映射 HTTP 80 端口，需手动添加：

```bash
kubectl edit svc -n ingress-nginx ingress-nginx-controller
```

在 `ports` 列表中增加 HTTP 80 端口映射，确保宿主机可通过 80 端口访问 Ingress Controller。

**验证端口监听**：

```bash
# 检查 Ingress Controller Service 端口
kubectl get svc ingress-nginx-controller -n ingress-nginx

# 检查 80 端口是否可达
curl -s -o /dev/null -w "%{http_code}" http://<INGRESS_IP>:80
```

#### 5.3.2 CoreDNS 重写 *.e2b.app 规则

K8S 集群内部 Pod 需要通过 CoreDNS 将 `*.e2b.app` 请求重写到 `edge-api` 服务：

```bash
kubectl edit configmap coredns -n kube-system
```

在 `Corefile` 中添加重写规则：

```
rewrite name regex .*\.e2b\.app\.$ edge-api.e2b.svc.cluster.local
```

重启 CoreDNS 使配置生效：

```bash
kubectl rollout restart deployment coredns -n kube-system
```

#### 5.3.3 创建 Ingress 域名转发

**Ingress 配置** (`dep/wildcard-ingress.yaml`)：

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: wildcard-e2b-app
  namespace: e2b
  annotations:
    kubernetes.io/ingress.class: "nginx"
spec:
  ingressClassName: nginx
  rules:
  - host: "*.e2b.app"
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: edge-api      # headless 服务名
            port:
              number: 3002      # 转发到 API 端口
```

部署并验证：

```bash
kubectl apply -f dep/wildcard-ingress.yaml
kubectl describe ingress wildcard-e2b-app -n e2b
```

#### 5.3.4 宿主机 dnsmasq 配置

将 `*.e2b.app` 解析到 Ingress Controller 的 ClusterIP：

```bash
# 获取 Ingress Controller ClusterIP
DNS_IP=$(kubectl get svc ingress-nginx-controller -n ingress-nginx \
    -o jsonpath='{.spec.clusterIP}')

# 写入 dnsmasq 配置
echo "address=/.e2b.app/$DNS_IP" >> /etc/dnsmasq.conf
systemctl restart dnsmasq
```

> 脚本 `init-client.sh` 已自动完成此配置，K8S 模式下会自动获取 Ingress Controller ClusterIP 并写入 dnsmasq。

#### 5.3.5 域名解析完整链路

```
宿主机请求 *.e2b.app
    │
    ▼
dnsmasq 解析 → Ingress Controller ClusterIP (自动获取)
    │
    ▼
K8S Nginx Ingress (监听 80 端口，匹配 *.e2b.app)
    │
    ▼
edge-api Service (namespace: e2b, port: 3002)
    │
    ▼
API Pod

--- K8S 集群内部 ---

Pod 请求 *.e2b.app
    │
    ▼
CoreDNS (rewrite 规则)
    │
    ▼
edge-api.e2b.svc.cluster.local
    │
    ▼
API Pod
```

#### 5.3.6 部署步骤汇总

1. 确保 K8S 集群已安装 Nginx Ingress Controller
2. 检查 Ingress Controller Service 是否监听 80 端口，未监听则手动添加
3. 修改 CoreDNS ConfigMap 添加 `*.e2b.app` 重写规则并重启
4. 创建 `e2b` 命名空间：`kubectl create namespace e2b`
5. 部署 Ingress：`kubectl apply -f wildcard-ingress.yaml`
6. 运行 `init-client.sh`，自动配置宿主机 dnsmasq（解析到 Ingress Controller ClusterIP）

### 5.4 K8S Worker 节点部署流程

通过 `deploy-worker.sh` 在 K8S 集群的 Worker 节点上部署：

```
deploy-worker.sh worker1 worker2 ...
    │
    ▼
[1/6] 分发 e2b-infra 代码包
    │
    ▼
[2/6] 复制 containerd 证书
    │
    ▼
[3/6] 复制 Harbor SSL 证书
    │
    ▼
[4/6] 复制 E2B API Token
    │
    ▼
[5/6] 远程执行部署
    ├── 同步代码到 /opt/e2b-infra
    ├── 安装 RPM 包
    ├── 修改 .env (SERVER_IP, DEPLOY_MODE=k8s)
    ├── build.sh --install-client
    ├── init-client.sh
    ├── 复制 firecracker
    ├── 重启 kubelet / containerd
    └── 测试镜像拉取
    │
    ▼
[6/6] 设置节点标签
    ├── node-role.kubernetes.io/sandbox=true
    └── node-role.kubernetes.io/api=
```

## 6. 命令参考

### 6.1 主脚本 build.sh

```bash
./build.sh [选项]
```

| 选项 | 参数 | 说明 |
|------|------|------|
| `--download` | - | 下载离线依赖包 |
| `--install` | - | 安装所有组件 |
| `--install-client` | - | 安装客户端组件（K8S Worker） |
| `--uninstall` | - | 卸载所有组件 |
| `--remove <组件>` | nomad/consul/harbor/postgres/nginx | 单独卸载指定组件 |
| `--start` | - | 启动服务 |
| `--stop` | - | 停止服务 |
| `--deploy <组件>` | nomad/consul/postgres/harbor/all | 单独部署指定组件 |
| `--deploy-plugin` | [target] [template] [selector] [ns] | 部署 E2B 插件 |
| `--make <target>` | 镜像名 | 制作并推送镜像 |
| `--k8s [node]` | 节点名（可选） | 启用 K8S 模式 |
| `-h / --help` | - | 显示帮助 |

### 6.2 Worker 部署脚本 deploy-worker.sh

```bash
./deploy-worker.sh <节点1> [节点2] ... [-p/--parallel]
```

| 选项 | 说明 |
|------|------|
| 节点名 | K8S 节点名称（必填，支持多个） |
| `-p / --parallel` | 并行部署多节点 |

### 6.3 常用命令组合

```bash
# 初次完整安装（Nomad 模式）
./build.sh --install --start

# 初次完整安装（K8S 模式）
./build.sh --k8s worker1 --install --start

# 快速重启
./build.sh --stop --start

# 单独重新部署 Harbor
./build.sh --deploy harbor

# 单独卸载 Nomad
./build.sh --remove nomad

# 下载离线包
./build.sh --download

# 制作镜像
./build.sh --make api:latest

# 部署 E2B 插件
./build.sh --deploy-plugin

# 部署 Worker 节点
./deploy-worker.sh worker1 worker2
./deploy-worker.sh --parallel worker1 worker2 worker3
```

## 7. Harbor 配置详解

### 7.1 Nomad 模式

```
Harbor (HTTP:2900)
    │
    ├── Nginx 反向代理 (80/443 → 2900)
    ├── Docker daemon.json 添加 insecure-registries
    ├── /etc/hosts 添加 harbor → 127.0.0.1
    └── docker login 登录
```

- HTTPS 配置块被注释
- HTTP 端口设为 2900
- Docker daemon 添加 `insecure-registries: ["<IP>:2900"]`

### 7.2 K8S 模式

```
Harbor (HTTPS:30443)
    │
    ├── Nginx SSL 终端 (443 → 30443)
    ├── containerd 证书配置
    │   ├── /etc/containerd/certs.d/<IP>:<PORT>/harbor.crt
    │   ├── /etc/containerd/certs.d/<IP>:<PORT>/harbor.key
    │   └── /etc/containerd/certs.d/<IP>:<PORT>/hosts.toml
    ├── containerd config.toml 添加 registry 配置
    └── nerdctl login 登录
```

- HTTPS 端口设为 30443
- SSL 证书路径指向 `/etc/nginx/ssl/`
- containerd 配置镜像仓库认证

### 7.3 Harbor 健康检查

```bash
# API 端点
GET /api/v2.0/health

# 响应格式
{
  "components": [
    {"name": "core", "status": "healthy"},
    {"name": "database", "status": "healthy"},
    {"name": "jobservice", "status": "healthy"},
    {"name": "portal", "status": "healthy"},
    {"name": "redis", "status": "healthy"},
    {"name": "registry", "status": "healthy"},
    {"name": "registryctl", "status": "healthy"}
  ],
  "status": "healthy"    # 7 个组件全部 healthy 才返回 healthy
}
```

## 8. 镜像管理

### 8.1 基础镜像

| 架构 | 镜像 | 本地标签 |
|------|------|----------|
| x86_64 | swr.cn-north-4.myhuaweicloud.com/.../redis:7.4.4-alpine | redis:7.4.4-alpine |
| x86_64 | swr.cn-north-4.myhuaweicloud.com/.../debian:bookworm-slim | debian:bookworm-slim |
| x86_64 | swr.cn-north-4.myhuaweicloud.com/.../postgres:latest | postgres:latest |
| arm64 | ...-linuxarm64 | 同上 + busybox:latest, ubuntu:24.04 |

### 8.2 镜像制作流程（--make）

```
原始镜像 (如 api:latest)
    │
    ▼
启动临时容器 (temp-images, --privileged, entrypoint=tail)
    │
    ▼
安装系统组件 (systemd, openssh, socat, websocat...)
    │
    ▼
导出容器 → 重新导入 (保留原始 ENTRYPOINT/CMD)
    │
    ▼
推送到 Harbor (<IP>:<PORT>/e2b-orchestration/<image>)
    │
    ▼
清理临时容器
```

## 9. 网络配置

### 9.1 DNS 配置

**Nomad 模式**：dnsmasq 将 `.e2b.app` 域名解析到本地：

```
address=/.e2b.app/127.0.0.1
```

**K8S 模式**：dnsmasq 将 `.e2b.app` 域名解析到 K8S Ingress Controller ClusterIP：

```
address=/.e2b.app/<INGRESS_CONTROLLER_CLUSTER_IP>
```

获取 ClusterIP：

```bash
kubectl get svc ingress-nginx-controller -n ingress-nginx -o jsonpath='{.spec.clusterIP}'
```

> **注意**：K8S 集群内部 Pod 的 DNS 由 CoreDNS 处理，需额外配置 rewrite 规则（见 5.3.2 节），与宿主机 dnsmasq 互不干扰。

请求通过 Ingress 路由到 `edge-api` Service（端口 3002），实现域名访问 API 服务。

### 9.2 iptables 规则

```bash
# HTTP 80 → 3002 重定向（本地访问）
iptables -t nat -A OUTPUT -p tcp -d 127.0.0.1 --dport 80 -j REDIRECT --to-port 3002
```

### 9.3 端口映射

| 外部端口 | 内部端口 | 服务 | 模式 |
|----------|----------|------|------|
| 80 | 3002 | API (via iptables REDIRECT) | Nomad |
| 80/443 | 3002 | API (via K8S Ingress → edge-api) | K8S |
| 2900 | Harbor HTTP | 镜像仓库 | Nomad |
| 30443 | Harbor HTTPS | 镜像仓库 | K8S |
| 5432 | PostgreSQL | 数据库 | 通用 |
| 4646 | Nomad | 调度器 API | Nomad |
| 8500 | Consul | 服务发现 API | Nomad |

## 10. 安全配置

### 10.1 SELinux

安装时自动关闭：
```bash
setenforce 0
```

### 10.2 SSL 证书

- 自签名证书，有效期 10 年（3650 天）
- CN 设置为本机 IP
- 证书配置文件：`dep/harbor.cnf`（`{ip}` 占位符自动替换）

### 10.3 Harbor 认证

- 默认管理员：`admin` / 环境变量 `HARBOR_PASSWORD`
- containerd 配置中硬编码认证信息

## 11. 卸载流程

### 11.1 全量卸载（--uninstall）

```
stop()
    ├── 停止 Nomad/Consul 服务
    ├── 终止业务进程 (redis, client-proxy, api, template-manager)
    ├── 停止 Harbor 容器
    └── 清理模板文件
    │
uninstall_e2b()
    ├── uninstall_nomad()
    ├── pip uninstall e2b
    └── modprobe -r nbd
    │
uninstall_nginx()
    ├── systemctl stop nginx
    ├── yum remove nginx
    └── rm -rf /etc/nginx
    │
uninstall_harbor()
    ├── docker-compose down -v
    ├── 删除 Harbor/goharbor 镜像
    ├── rm -rf harbor 目录
    └── 清理证书配置
    │
uninstall_postgres()
    ├── 停止/删除容器
    └── 删除镜像
    │
uninstall_docker_resources()
    ├── 删除脚本创建的容器
    ├── 删除脚本拉取的镜像
    └── 清理悬空镜像
```

### 11.2 单独卸载（--remove）

| 组件 | 卸载内容 |
|------|----------|
| nomad | 调用卸载脚本 + pkill |
| consul | 停止服务 + pkill + 删除二进制和服务文件 |
| harbor | 停止容器 + 删除镜像 + 清理目录和证书 |
| postgres | 停止/删除容器 + 删除镜像 |
| nginx | 停止服务 + pkill + yum remove + 清理配置 |

## 12. 故障排查

### 12.1 常见问题

| 问题 | 排查方法 |
|------|----------|
| Harbor 启动失败 | `curl -sk https://<IP>:30443/api/v2.0/health` 检查健康状态 |
| Nomad 连接失败 | `ss -tln | grep 4646` 检查端口监听 |
| 镜像拉取失败 | 检查 containerd/Docker 证书配置和 insecure-registries |
| DNS 解析失败 | `dig .e2b.app @127.0.0.1` 检查 dnsmasq |
| PostgreSQL 连接失败 | `docker exec -it postgres psql -U postgres -d mydatabase -c "\q"` |
| K8S 节点标签缺失 | `kubectl get nodes --show-labels` 检查 sandbox/api 标签 |

### 12.2 日志位置

| 组件 | 日志命令 |
|------|----------|
| Nomad | `journalctl -u nomad -f` |
| Consul | `journalctl -u consul -f` |
| PostgreSQL | `docker logs postgres` |
| Harbor | `cd harbor && docker-compose logs` |
| Nginx | `journalctl -u nginx -f` / `/var/log/nginx/` |
| MinIO | `journalctl -u minio -f` |
