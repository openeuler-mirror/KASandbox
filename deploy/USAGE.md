# E2B 使用文档

## 1. 概述

E2B 是一个基于 Firecracker 微虚拟机的沙箱执行平台，支持在隔离环境中安全运行用户代码。本文档覆盖从部署到使用的完整流程。

### 核心概念

| 概念 | 说明 |
|------|------|
| **Sandbox（沙箱）** | 基于 Firecracker 的隔离微虚拟机，用于执行代码 |
| **Template（模板）** | 沙箱的镜像定义，基于 Dockerfile 构建 |
| **API Service** | 沙箱管理 API 入口（端口 3000） |
| **Client Proxy** | 沙箱连接代理（端口 3002） |
| **Template Manager** | 模板管理/沙箱生命周期管理 |
| **Harbor** | 本地镜像仓库，存储沙箱镜像 |
| **PostgreSQL** | 元数据存储（团队、用户、模板、沙箱配额） |
| **e2b-webhook** | K8S 准入控制器，拦截沙箱 Pod 创建（可选） |
| **cri-multiplex** | CRI 多路复用器，让 K8S 原生调度 E2B 沙箱 Pod（可选） |

### 目录结构

#### 项目目录（源码仓库 `deploy/`）

```
deploy/
├── .env                      # 环境变量配置（必须修改 SERVER_IP）
├── build.sh                  # 主部署脚本（安装/启动/停止/卸载）
├── k8s-deploy.sh             # K8S 集群部署脚本（KubeKey）
├── deploy-worker.sh          # K8S Worker 节点部署脚本
├── create_sandbox.py         # 沙箱创建脚本
├── create_template.py        # 模板创建脚本
├── README.md                 # 项目说明
├── USAGE.md                  # 使用文档（本文件）
├── DEPLOY_DESIGN.md          # 部署设计文档
├── dep/                      # 部署依赖脚本
│   ├── deploy.sh             # K8S 部署执行脚本
│   ├── deploy-e2b-plugin.sh  # E2B 插件部署脚本
│   ├── ingress-nginx.yaml    # Nginx Ingress Controller 离线部署清单
│   ├── wildcard-ingress.yaml # K8S Ingress 规则（*.e2b.app 域名）
│   ├── install-nomad.sh      # Nomad 安装脚本
│   ├── install-consul.sh     # Consul 安装脚本
│   ├── uninstall-nomad.sh    # Nomad 卸载脚本
│   ├── uninstall-consul.sh   # Consul 卸载脚本
│   ├── start-server.sh       # Nomad Server 启动脚本
│   ├── start-client.sh       # Nomad Client 启动脚本
│   ├── init-client.sh        # 客户端初始化脚本
│   ├── run-nomad.sh          # Nomad 运行脚本
│   ├── run-consul.sh         # Consul 运行脚本
│   ├── harbor.cnf            # Harbor SSL 证书配置模板
│   └── openclaw.yaml         # OpenClaw 配置
└── nomad/                    # Nomad 任务定义
    ├── api.hcl               # API 服务任务
    ├── edge.hcl              # Edge 服务任务
    ├── redis.hcl             # Redis 任务
    └── template-manager.hcl  # Template Manager 任务
```

> **说明**：Helm 模板（`helm/`）、二进制与 Dockerfile（`bin/`）不在 `deploy/` 源码目录中，由根目录 `build.sh` 从 `helm/` 与 `packages/*/Dockerfile` 打包进 RPM，安装后落在 `/opt/e2b-infra/` 下（见下文）。

#### 部署目标目录（`/opt/e2b-infra/`）

通过 RPM 包安装（`rpm -ivh KASandbox-*.aarch64.rpm`），目录结构如下（以 `rpm -qlp KASandbox-1.0.0-1.aarch64.rpm` 实际内容为准）：

```
/opt/e2b-infra/
├── .env                      # 环境变量配置（必须修改 SERVER_IP）
├── build.sh                  # 主部署脚本（安装/启动/停止/卸载）
├── k8s-deploy.sh             # K8S 集群部署脚本（KubeKey）
├── deploy.sh                 # K8S 部署执行脚本（dep/deploy.sh 的副本）
├── deploy-worker.sh          # K8S Worker 节点部署脚本
├── deploy-e2b-plugin.sh      # E2B 插件部署脚本（dep/ 副本）
├── create_sandbox.py         # 沙箱创建脚本
├── create_template.py        # 模板创建脚本
├── init-client.sh            # 客户端初始化脚本（dep/ 副本）
├── install-nomad.sh          # Nomad 安装脚本（dep/ 副本）
├── install-consul.sh         # Consul 安装脚本（dep/ 副本）
├── uninstall-nomad.sh        # Nomad 卸载脚本（dep/ 副本）
├── uninstall-consul.sh       # Consul 卸载脚本（dep/ 副本）
├── start-server.sh           # Nomad Server 启动脚本（dep/ 副本）
├── start-client.sh           # Nomad Client 启动脚本（dep/ 副本）
├── run-nomad.sh              # Nomad 运行脚本（dep/ 副本）
├── run-consul.sh             # Consul 运行脚本（dep/ 副本）
├── harbor.cnf                # Harbor SSL 证书配置模板（dep/ 副本）
├── openclaw.yaml             # OpenClaw 配置（dep/ 副本）
├── ingress-nginx.yaml        # Nginx Ingress Controller 离线部署清单（dep/ 副本）
├── wildcard-ingress.yaml     # K8S Ingress 规则（*.e2b.app 域名，dep/ 副本）
├── README.md                 # 项目说明
├── USAGE.md                  # 使用文档（本文件）
├── DEPLOY_DESIGN.md          # 部署设计文档
├── bin/                      # 二进制与 Dockerfile（根 build.sh 构建后打包）
│   ├── api.Dockerfile        # API 服务镜像构建文件
│   ├── client-proxy.Dockerfile
│   ├── orchestrator.Dockerfile
│   ├── e2b-webhook.Dockerfile
│   ├── db-migrator.Dockerfile
│   ├── migrations/           # PostgreSQL 迁移 SQL 文件
│   ├── migrations-clickhouse/ # ClickHouse 迁移 SQL 文件
│   ├── api                   # API 服务二进制
│   ├── orchestrator          # Orchestrator 二进制
│   ├── client-proxy          # Client Proxy 二进制
│   ├── envd                  # envd 二进制
│   ├── e2b-webhook           # Webhook 二进制
│   ├── migrator              # 数据库迁移工具
│   ├── seed-db               # 数据库种子工具
│   ├── fc-netns-exec         # 网络命名空间工具
│   └── cri-multiplex         # CRI 多路复用器二进制
├── dep/                      # 部署依赖脚本（原始副本，build.sh 引用此目录）
│   ├── deploy.sh
│   ├── deploy-e2b-plugin.sh
│   ├── init-client.sh
│   ├── install-nomad.sh
│   ├── install-consul.sh
│   ├── uninstall-nomad.sh
│   ├── uninstall-consul.sh
│   ├── start-server.sh
│   ├── start-client.sh
│   ├── run-nomad.sh
│   ├── run-consul.sh
│   ├── harbor.cnf
│   ├── nginx.conf
│   ├── openclaw.yaml
│   ├── ingress-nginx.yaml
│   ├── wildcard-ingress.yaml
│   └── main.py
├── helm/                     # Helm 部署模板
│   ├── Chart.yaml            # Helm Chart 定义
│   ├── values-template.yaml  # Helm values 模板
│   └── templates/
│       ├── api.yaml          # API 服务 Deployment
│       ├── edge.yaml         # Edge/Client-Proxy Deployment
│       ├── postgres.yaml     # PostgreSQL Deployment
│       ├── redis.yaml        # Redis Deployment
│       ├── template-manager.yaml  # Template Manager DaemonSet
│       └── rbac-orchestrator.yaml # Orchestrator RBAC
└── nomad/                    # Nomad 任务定义
    ├── api.hcl               # API 服务任务
    ├── edge.hcl              # Edge 服务任务
    ├── redis.hcl             # Redis 任务
    └── template-manager.hcl  # Template Manager 任务
```

> **说明**：根目录下的 `deploy.sh`、`main.py`、`nginx.conf` 等与 `dep/` 下同名文件内容一致，为方便顶层脚本直接调用而放置在根目录；`dep/` 保留原始副本供 `build.sh --install` 引用。

---

## 2. 环境准备

### 2.1 系统要求

| 项目 | 要求 |
|------|------|
| 操作系统 | openEuler2403sp3 |
| 架构 | arm64 |
| 内存 | ≥ 16GB（推荐 32GB） |
| 磁盘 | ≥ 100GB |
| 网络 | 可访问外网（下载依赖包时） |

### 2.2 修改配置文件

编辑 `.env`，将 `SERVER_IP` 修改为本机 IP 地址：

```bash
vi .env
```

```bash
# 必须修改
export SERVER_IP="10.10.10.10"    # 改为本机 IP

# 部署模式选择
export DEPLOY_MODE=nomad          # nomad 或 k8s
```

### 2.3 关闭 SELinux

```bash
setenforce 0
```

### 2.4 组件下载

> **重要**：安装前**必须**先下载好所有组件包，否则 `--install` 会失败。

#### 2.4.1 自动下载（推荐）

```bash
./build.sh --download
```

自动下载所有必需组件到 `dep/` 目录，包括二进制包、Docker 镜像、Python 依赖等。

#### 2.4.2 手动下载

如网络受限，可手动下载以下组件到 `dep/` 目录。

#### 2.4.2.1 Nomad 模式组件

| 组件 | 版本 | x86_64 下载地址 | arm64 下载地址 |
|------|------|----------------|---------------|
| Docker | 25.0.5 | https://download.docker.com/linux/static/stable/x86_64/docker-25.0.5.tgz | https://download.docker.com/linux/static/stable/aarch64/docker-25.0.5.tgz |
| Docker Compose | 2.40.2 | https://github.com/docker/compose/releases/download/v2.40.2/docker-compose-linux-x86_64 | https://github.com/docker/compose/releases/download/v2.40.2/docker-compose-linux-aarch64 |
| Nomad | 1.10.4 | https://releases.hashicorp.com/nomad/1.10.4/nomad_1.10.4_linux_amd64.zip | https://releases.hashicorp.com/nomad/1.10.4/nomad_1.10.4_linux_arm64.zip |
| Consul | 1.21.4 | https://releases.hashicorp.com/consul/1.21.4/consul_1.21.4_linux_amd64.zip | https://releases.hashicorp.com/consul/1.21.4/consul_1.21.4_linux_arm64.zip |
| Firecracker | 1.13.1 | https://github.com/firecracker-microvm/firecracker/releases/download/v1.13.1/firecracker-v1.13.1-x86_64.tgz | https://github.com/firecracker-microvm/firecracker/releases/download/v1.13.1/firecracker-v1.13.1-aarch64.tgz |
| Harbor | 2.13.0 | https://github.com/goharbor/harbor/releases/download/v2.13.0/harbor-offline-installer-v2.13.0.tgz | https://github.com/wise2c-devops/build-harbor-aarch64/releases/download/v2.13.0/harbor-offline-installer-aarch64-v2.13.0.tgz |

#### 2.4.2.2 K8S 模式组件

K8S 模式包含 Nomad 模式的全部组件（除 Docker/Docker Compose 外），额外需要：

| 组件 | 说明 | 安装方式 |
|------|------|----------|
| Kubernetes | K8S 集群（kubelet, kubectl, kubeadm） | 需预先安装，脚本不负责部署 |
| Nginx Ingress Controller | Ingress 路由控制器 | 内置清单 `dep/ingress-nginx.yaml`，未安装时手动 apply（见 [3.2.4](#324-前置条件)） |
| containerd | 容器运行时 | K8S 节点自带 |
| nerdctl | containerd CLI | 替代 Docker 命令 |
| helm | K8S 包管理器 | 用于卸载 e2b-api |

> **注意**：K8S 集群、kubectl 需在运行脚本前自行安装配置。Ingress Controller 可使用内置清单部署。

#### 2.4.2.3 Docker 镜像

以下镜像从华为云 SWR 镜像仓库拉取，部署时自动处理：

| 架构 | 镜像 | 本地标签 | 模式 | 下载地址 |
|------|------|----------|------|----------|
| x86_64 | Redis | redis:7.4.4-alpine | 通用 | swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/redis:7.4.4-alpine |
| x86_64 | Debian | debian:bookworm-slim | 通用 | swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/debian:bookworm-slim |
| x86_64 | PostgreSQL | postgres:latest | 通用 | swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/postgres:latest |
| arm64 | Redis | redis:7.4.4-alpine | 通用 | swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/redis:7.4.4-alpine-linuxarm64 |
| arm64 | Debian | debian:bookworm-slim | 通用 | swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/debian:bookworm-slim-linuxarm64 |
| arm64 | PostgreSQL | postgres:latest | 通用 | swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/postgres:latest-linuxarm64 |
| arm64 | BusyBox | busybox:latest | K8S | swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/busybox:latest-linuxarm64 |
| arm64 | Ubuntu | ubuntu:24.04 | K8S | swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/ubuntu:24.04-linuxarm64 |

#### 2.4.2.4 e2b-webhook 镜像（K8S 模式可选）

仅当 K8S 模式下启用 webhook（`ENABLE_WEBHOOK=true`）时需要，`--download` 会自动下载：

| 组件 | 版本 | 下载地址 | 说明 |
|------|------|----------|------|
| e2b-webhook | 1.0.0 | https://gitcode.com/fly_1997/e2b-webhook/releases/download/1.0.0/e2b-webhook.tar | 镜像 tar 包，`--install` 时自动 `docker load` 导入 |

> **说明**：
> - 下载后保存为 `dep/e2b-webhook.tar`，`pull_docker_images` 会自动通过 `docker load -i` 导入为本地镜像 `e2b-webhook`
> - 仅 K8S 模式下载（nomad 模式不需要）
> - 部署时由 `deploy.sh` 推送到 Harbor，详见 [3.2.3 e2b-webhook](#323-e2b-webhook可选组件)

#### 2.4.2.5 Python 依赖

| 包 | 版本 | 安装命令 |
|-----|------|----------|
| e2b | 2.20.0 | `pip install e2b==2.20.0` |
| e2b_code_interpreter | 2.4.1 | `pip install e2b_code_interpreter==2.4.1` |

#### 2.4.2.6 系统依赖

| 包 | 用途 | 安装命令 |
|----|------|----------|
| curl | HTTP 请求 | `yum install -y curl` |
| unzip | 解压 zip | `yum install -y unzip` |
| jq | JSON 处理 | `yum install -y jq` |
| tar | 解压 tar | `yum install -y tar` |
| rsync | 文件同步 | `yum install -y rsync` |
| dnsmasq | DNS 服务 | `yum install -y dnsmasq` |
| openssl | SSL 证书 | `yum install -y openssl` |
| python3 / pip | E2B SDK | `yum install -y python3 python3-pip` |
| socat | 端口转发 | `yum install -y socat` |
| websocat | WebSocket 代理 | https://github.com/vi/websocat/releases/latest/download/websocat.aarch64-unknown-linux-musl |

### 2.5 Mooncake 配置（可选组件）

Mooncake 是分布式内存语义层组件，用于加速跨节点内存共享。**仅当 `STORAGE_PROVIDER=MooncakeBucket` 时需要配置**，其他存储后端可跳过本节。

在 `.env` 中设置：

```bash
export STORAGE_PROVIDER=MooncakeBucket
```

#### 2.5.1 必须配置

| 变量 | 说明 | 示例 |
|------|------|------|
| `MOONCAKE_MASTER_ADDR` | 集群 Master 地址（固定节点） | `141.61.17.196:50055` |
| `MOONCAKE_METADATA_SERVER` | 元数据服务地址（固定节点） | `http://141.61.17.196:8015` |

#### 2.5.2 自动获取（无需手动配置）

以下变量由调度平台自动获取节点 IP，**无需在 `.env` 中设置**：

| 变量 | Nomad 模式 | K8S 模式 |
|------|-----------|----------|
| `MOONCAKE_LOCAL_HOSTNAME` | `$${attr.unique.network.ip-address}` | `status.hostIP`（Downward API） |
| `MC_TCP_BIND_ADDRESS` | `$${attr.unique.network.ip-address}` | `status.hostIP`（Downward API） |

#### 2.5.3 可选配置

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `GLOG_logtostderr` | 日志输出到 stderr | `1` |
| `MOONCAKE_LOCAL_BUFFER_SIZE` | 本地缓冲区大小 | `536870912`（512MB） |
| `MOONCAKE_GLOBAL_SEGMENT_SIZE` | 全局段大小 | `0` |
| `MOONCAKE_PROTOCOL` | 传输协议 | `ub` |
| `MC_URMA_TRANS_MODE` | URMA 传输模式 | `RM` |
| `MOONCAKE_DEVICE_NAME` | 设备名 | `bonding_dev_0` |
| `MC_LOG_ENABLE` | 启用日志 | `1` |
| `MC_LOG_DIR` | 日志目录 | `/var/log/mooncake` |
| `MC_LOG_LEVEL` | 日志级别 | `TRACE` |
| `MC_STORE_LOCAL_HOT_CACHE_USE_SHM` | 热缓存使用共享内存 | `0` |
| `MC_STORE_LOCAL_HOT_BLOCK_SIZE` | 热缓存块大小 | `67108864`（64MB） |
| `MC_STORE_LOCAL_HOT_ADMISSION_THRESHOLD` | 热缓存准入阈值 | `1` |
| `MC_SLICE_SIZE` | 分片大小 | `1048576`（1MB） |
| `MC_WORKERS_PER_CTX` | 每上下文工作线程数 | `8` |
| `MC_MAX_WR` | 最大写并发 | `4` |

#### 2.5.4 配置示例

```bash
# 编辑 .env，修改 Master 地址和元数据服务地址
vi .env

# --- Mooncake ---
export MOONCAKE_MASTER_ADDR="10.10.10.10:50055"       # 改为 Master 节点 IP
export MOONCAKE_METADATA_SERVER="http://10.10.10.10:8015"  # 改为元数据服务地址
```

> **注意**：`MOONCAKE_LOCAL_HOSTNAME` 和 `MC_TCP_BIND_ADDRESS` 在 Nomad 模式下通过 Nomad 属性自动获取节点 IP，在 K8S 模式下通过 Downward API 获取 `status.hostIP`，均无需手动配置。

---

## 3. 部署

### 3.1 Nomad 模式部署

适用于单机或小规模环境，使用 Docker + Nomad + Consul 调度。

#### 3.1.1 下载组件

```bash
./build.sh --download
```

#### 3.1.2 安装

```bash
./build.sh --install
```

安装过程包括：
- 系统依赖安装（curl, jq, dnsmasq 等）
- E2B SDK 及配置文件部署
- Docker / Consul / Nomad 安装
- 基础镜像拉取（Redis, PostgreSQL, Debian 等）
- PostgreSQL 容器启动
- Harbor 镜像仓库安装
- Harbor SSL 证书生成（/etc/harbor/certs/）

#### 3.1.3 启动服务

```bash
./build.sh --start
```

启动过程包括：
- 客户端初始化
- PostgreSQL 启动
- Harbor 启动并等待健康检查
- Harbor 登录并创建项目
- Nomad Server 启动
- Nomad 客户端配置追加
- E2B 业务服务部署（API, Template Manager, Redis 等）

#### 3.1.4 验证

```bash
# 检查 Nomad 状态
ss -tlnp | grep 4646

# 访问 Nomad Web 界面
# http://<SERVER_IP>:4646
# Token 在 /opt/e2b-infra/.env 的 NOMAD_ACL_TOKEN 字段

# 检查 Harbor 状态
curl -sk http://<SERVER_IP>:2900/api/v2.0/health | jq .
```

#### 3.1.5 Harbor 协议配置

Harbor 支持 HTTP、HTTPS、两者并存三种模式，通过环境变量 `HARBOR_PROTOCOL` 控制：

| 值 | HTTP (2900) | HTTPS (30443) | 说明 |
|----|-------------|---------------|------|
| `http` | 启用 | 禁用 | 仅 HTTP，需配置 Docker insecure-registries |
| `https` | 禁用 | 启用 | 仅 HTTPS，K8S 模式需配置 containerd 证书 |
| `both` | 启用 | 启用 | 同时支持（默认） |

```bash
# 使用默认值 both（HTTP + HTTPS 同时启用）
./build.sh --start

# 仅 HTTP 模式
HARBOR_PROTOCOL=http ./build.sh --start

# 仅 HTTPS 模式
HARBOR_PROTOCOL=https ./build.sh --start
```

HTTP 模式下需配置 Docker 信任（脚本自动完成）：

```bash
# /etc/docker/daemon.json
{
    "insecure-registries": ["<SERVER_IP>:2900"]
}
```

---

### 3.2 K8S 模式部署

适用于多节点/生产环境，使用 Kubernetes + containerd + nerdctl。

> **可选组件**：cri-multiplex 是 K8S 模式下的可选组件。如需让 K8S 原生调度 DaemonSet/Deployment 管理 E2B 沙箱 Pod，请在集群就绪后、部署业务服务前完成本节操作；否则可直接跳过。

#### 3.2.1 K8S 集群部署

通过 `k8s-deploy.sh` 使用 KubeKey 部署 K8S 集群（支持 x86_64 / arm64）。

**前置条件**：
- 系统：openEuler2403sp3（或兼容发行版）
- 已安装 RPM 包（`k8s-deploy.sh` 位于 `/opt/e2b-infra/`）
- 可访问外网（下载 KubeKey、CNI 插件）
- 多节点时需在配置文件中配置节点 SSH 密码

**步骤一：生成集群配置**（`prep`：安装依赖、下载 kk/CNI、生成配置）

```bash
./k8s-deploy.sh prep
```

`prep` 会自动：
- 安装系统依赖（conntrack, socat, ipvsadm, ipset, curl, tar）
- 下载 KubeKey（默认 v3.1.10，可用 `KUBEKEY_VERSION` 指定）
- 下载 CNI 插件（默认 v1.6.2，可用 `CNI_PLUGINS_VERSION` 指定）
- 生成集群配置文件 `config-k8s-arm64.yaml`（自动填充本机 IP）

多节点时可通过环境变量指定 IP 和 SSH 密码：

```bash
HOST_IP=10.0.0.5 NODE_PASSWORD=secret ./k8s-deploy.sh prep
```

**步骤二：编辑配置文件**

编辑生成的 `config-k8s-arm64.yaml`，按需修改节点列表、SSH 密码、K8S 版本等。

**步骤三：创建集群**（`create`：创建集群、验证状态、部署 ingress-nginx、配置域名）

```bash
./k8s-deploy.sh create
```

或使用 `all` 一步完成（需通过 `CONFIG_FILE` 指定已编辑的配置）：

```bash
CONFIG_FILE=config-k8s-arm64.yaml ./k8s-deploy.sh all
```

**可用的子命令**：

| 命令 | 说明 |
|------|------|
| `prep` | 安装依赖、下载 kk/CNI、生成集群配置 |
| `create` | 根据配置创建集群、验证状态、部署 ingress-nginx、配置域名 |
| `all` | prep + create（需通过 `CONFIG_FILE` 指定已编辑配置） |
| `configure-domain` | 单独配置 `*.e2b.app` 域名访问 |
| `cri-multiplex` | 部署 cri-multiplex（见 [3.2.2](#322-cri-multiplex可选组件)） |
| `buildkit` | 安装并启用 buildkit |
| `download-cni` | 单独下载并安装 CNI 插件到 `/opt/cni/bin` |

**常用环境变量**：

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `KUBEKEY_VERSION` | KubeKey 版本 | v3.1.10 |
| `K8S_VERSION` | K8S 版本 | v1.32.5 |
| `CNI_PLUGINS_VERSION` | CNI 插件版本 | v1.6.2 |
| `CLUSTER_NAME` | 集群名 | k8s |
| `CONFIG_FILE` | 配置文件路径（create/all 使用） | - |
| `HOST_IP` | 本机 IP | 自动探测 |
| `NODE_PASSWORD` | 节点 SSH 密码（必须提供） | - |

**验证集群**：

```bash
kubectl get nodes
kubectl get pods -A
```

#### 3.2.2 cri-multiplex（可选组件）

cri-multiplex 是 CRI gRPC 多路复用器，让 kubelet 通过单一 Unix socket 调度 **containerd**（普通 Pod）和 **E2B orchestrator**（沙箱 Pod）。

```
Kubelet ──Unix socket──▶ cri-multiplex
                           ├── RuntimeHandler != "e2b" ──▶ ContainerEngine ──▶ containerd
                           └── RuntimeHandler == "e2b" ──▶ E2BEngine ──▶ orchestrator:5008
```

**前置条件**：K8S 集群已就绪、kubelet 通过 kubeadm 初始化、containerd 运行中、orchestrator 可达（默认 `localhost:5008`）、二进制已随 RPM 安装至 `/opt/e2b-infra/bin/cri-multiplex`。

**部署**（每个节点执行一次）：

```bash
./k8s-deploy.sh cri-multiplex
```

| 步骤 | 操作 |
|------|------|
| ① 检查二进制 | `/opt/e2b-infra/bin/cri-multiplex`，不存在则跳过 |
| ② 创建 systemd 服务 | 开机自启 + 崩溃自动重启 |
| ③ 切换 kubelet endpoint | `containerd.sock` → `cri-multiplex.sock`，自动备份原文件 |
| ④ 重启 kubelet | 使新 endpoint 生效 |
| ⑤ 创建 RuntimeClass | `android`、`e2b`，Pod 通过 `runtimeClassName` 选择后端 |

**验证**：

```bash
systemctl status cri-multiplex
ls -l /run/cri-multiplex.sock
grep "cri-multiplex" /var/lib/kubelet/kubeadm-flags.env
kubectl get runtimeclass
```

**故障恢复**：

```bash
# 紧急回滚到 containerd
sed -i 's#unix:///run/cri-multiplex.sock#unix:///run/containerd/containerd.sock#g' \
    /var/lib/kubelet/kubeadm-flags.env
systemctl restart kubelet
```

> **注意**：切换 kubelet endpoint 会短暂影响节点上所有 Pod，建议在维护窗口操作；回滚只需改回 `containerd.sock` 并重启 kubelet。


#### 3.2.3 e2b-webhook（可选组件）

> **可选组件**：e2b-webhook 是 K8S 模式下的可选准入控制器。用于拦截 BatchSandbox CR 和特定 Pod 的创建请求，自动注入沙箱配置注解。默认关闭，通过 `ENABLE_WEBHOOK` 控制。

在 `.env` 中设置：

```bash
export ENABLE_WEBHOOK=true
```

启用后，部署脚本自动完成：

| 步骤 | 操作 |
|------|------|
| ① 镜像推送 | 将本地 `e2b-webhook` 镜像推送到 Harbor |
| ② Helm 安装 | Deployment（2 副本）、Service（443→8443）、MutatingWebhookConfiguration（pod + batchsandbox） |
| ③ TLS 证书 | 自签证书（10 年有效），创建 Secret `e2b-webhook-tls` 并注入 caBundle |
| ④ API Key | 从 `/root/.e2b/config.json` 读取 `teamApiKey`，创建 Secret `e2b-api-key` |

**验证**：

```bash
kubectl get pods -n e2b -l app.kubernetes.io/name=e2b-webhook
kubectl get mutatingwebhookconfiguration e2b-webhook -o jsonpath='{.webhooks[0].clientConfig.caBundle}' | base64 -d | openssl x509 -text -noout
kubectl -n e2b get secret e2b-webhook-tls e2b-api-key
```

> **注意**：关闭时（`ENABLE_WEBHOOK=false`），部署脚本自动清理 Secret 和 MutatingWebhookConfiguration，Helm 资源由 `.Values.webhook.enabled` 同步控制。

#### 3.2.4 前置条件

- K8S 集群已就绪
- kubectl 可正常访问集群
- Nginx Ingress Controller 已安装（检查与部署见下文）

##### 检查 Nginx Ingress Controller

部署前需确认 Ingress Controller 是否已安装：

```bash
# 方式一：检查命名空间
kubectl get namespace ingress-nginx

# 方式二：检查 Deployment
kubectl get deployment -n ingress-nginx ingress-nginx-controller

# 方式三：检查 IngressClass
kubectl get ingressclass nginx
```

若上述命令均返回有效资源，说明已安装，可跳过下文部署步骤。

##### 部署 Nginx Ingress Controller

未安装时，使用项目内置的离线清单 [dep/ingress-nginx.yaml](dep/ingress-nginx.yaml) 部署（v1.15.1，镜像源 `k8s.dockerproxy.net`）：

```bash
kubectl apply -f dep/ingress-nginx.yaml
```

部署后等待 Pod 就绪：

```bash
kubectl wait --namespace ingress-nginx \
  --for=condition=ready pod \
  --selector=app.kubernetes.io/component=controller \
  --timeout=120s

# 验证
kubectl get pods -n ingress-nginx
kubectl get svc -n ingress-nginx ingress-nginx-controller
```

> **注意**：
> - 该清单中 Service 类型为 `LoadBalancer`。裸金属环境若无外部 LB，可改为 `NodePort` 或配合 MetalLB 使用
> - 默认监听 80/443 端口，与 [3.2.9 配置域名访问](#329-配置域名访问) 中要求的 80 端口监听一致
> - 镜像需从 `k8s.dockerproxy.net` 拉取，离线环境请预先导入镜像

#### 3.2.5 下载组件

```bash
./build.sh --download
```

#### 3.2.6 Master 节点安装

```bash
./build.sh --k8s <节点名> --install
```

不指定节点名时自动选择第一个节点：

```bash
./build.sh --k8s --install
```

#### 3.2.7 Master 节点启动

```bash
./build.sh --k8s <节点名> --start
```

启动过程额外步骤（相比 Nomad 模式）：
- Harbor 根据 `HARBOR_PROTOCOL` 配置协议（默认 both）
- HTTPS 启用时配置 containerd 证书和仓库
- Kubelet 重启应用大页配置
- 节点标签设置（sandbox=true, api=）
- K8S Deployment 部署

#### 3.2.8 Worker 节点部署

```bash
# 单节点部署
./deploy-worker.sh worker1

# 多节点部署
./deploy-worker.sh worker1 worker2 worker3

# 并行部署
./deploy-worker.sh --parallel worker1 worker2 worker3
```

Worker 节点部署内容：
- 分发 e2b-infra 代码包
- 复制 containerd/Harbor 证书
- 复制 E2B API Token
- 远程执行安装和初始化
- 设置节点标签

#### 3.2.9 配置域名访问

K8S 模式下需配置三层域名解析，确保宿主机和集群内部 Pod 均可通过 `*.e2b.app` 访问 API。

**① Ingress Controller 监听 80 端口**

```bash
kubectl edit svc -n ingress-nginx ingress-nginx-controller
# 在 ports 中增加 HTTP 80 端口映射
```

**② CoreDNS 重写规则**

```bash
kubectl edit configmap coredns -n kube-system
# 添加：rewrite name regex .*\.e2b\.app\.$ edge-api.e2b.svc.cluster.local

kubectl rollout restart deployment coredns -n kube-system
```

#### 3.2.10 验证

```bash
# 检查 K8S 节点状态
kubectl get nodes --show-labels

# 检查 Ingress
kubectl get ingress -n e2b

# 检查 Pod 状态
kubectl get pods -n e2b

# 检查域名解析
dig *.e2b.app @127.0.0.1
```

---

## 4. 模板管理

模板是沙箱的镜像定义。创建沙箱前，必须先构建模板。

### 4.1 制作沙箱镜像

沙箱镜像需要推送到 Harbor 仓库，供后续模板构建使用。整个流程分为**可选的镜像制作**和**必需的 Harbor 上传**两部分。

#### 前置条件

- Harbor 已启动并可访问（Nomad 模式端口 2900/30443，K8S 模式端口 30443）
- 已执行 `docker login` 或 containerd 已配置 Harbor 证书
- 本地或镜像仓库中已有基础镜像（如 `ubuntu:22.04`）

#### 4.1.1（可选）制作沙箱镜像

如果已有安装好必要组件的镜像，可跳过本步，直接进入 [4.1.2 上传镜像到 Harbor](#412-必需上传镜像到-harbor)。

沙箱镜像需包含以下组件：systemd、openssh-server、websocat、socat、curl 等。以下以 `ubuntu:22.04` 为例。

**方式一：使用脚本制作（推荐）**

```bash
# 自动完成安装组件、导出、推送全流程
./build.sh --make ubuntu:22.04
```

脚本内部流程：
1. 保存原镜像的 ENTRYPOINT 和 CMD
2. 启动临时容器，安装 systemd、openssh-server、websocat 等组件
3. 导出容器并恢复原始 ENTRYPOINT/CMD
4. 推送到 Harbor：`<HARBOR_URL>/e2b-orchestration/ubuntu:22.04`
5. 验证并清理临时容器

**方式二：手动制作 — Ubuntu**

以 `ubuntu:22.04` 为例，使用 apt-get 安装组件：

```bash
# 1. 保存原镜像的 Entrypoint 和 Cmd
ORIG_ENTRY=$(docker inspect ubuntu:22.04 --format='{{json .Config.Entrypoint}}')
ORIG_CMD=$(docker inspect ubuntu:22.04 --format='{{json .Config.Cmd}}')
echo "原 ENTRYPOINT: $ORIG_ENTRY"
echo "原 CMD: $ORIG_CMD"

# 2. 启动临时容器
docker rm -f temp-images 2>/dev/null
docker run -d --name temp-images --privileged --entrypoint tail \
    ubuntu:22.04 -f /dev/null

# 3. 安装必要组件
docker exec temp-images bash -c " \
    apt-get update && \
    apt-get install -y systemd systemd-sysv openssh-server sudo chrony \
    linuxptp socat curl wget iputils iproute2 netcat-openbsd tcpdump passwd && \
    apt-get clean && rm -rf /var/lib/apt/lists/* /var/tmp/* /tmp/*"

# 4. 安装 websocat（用于 SSH 代理连接沙箱）
docker exec temp-images bash -c ' \
    wget -O /usr/local/bin/websocat https://github.com/vi/websocat/releases/latest/download/websocat.aarch64-unknown-linux-musl && \
    chmod a+x /usr/local/bin/websocat && \
    websocat --version'

# 5. 导出容器并恢复原始 ENTRYPOINT/CMD
docker stop temp-images
docker export temp-images | docker import \
    --change "ENTRYPOINT $ORIG_ENTRY" \
    --change "CMD $ORIG_CMD" \
    - ubuntu:22.04-custom

# 6. 清理临时容器
docker rm -f temp-images
```

**方式二：手动制作 — openEuler**

以 `openeuler/openeuler:24.03` 为例，使用 yum 安装组件：

```bash
# 1. 保存原镜像的 Entrypoint 和 Cmd
ORIG_ENTRY=$(docker inspect openeuler/openeuler:24.03 --format='{{json .Config.Entrypoint}}')
ORIG_CMD=$(docker inspect openeuler/openeuler:24.03 --format='{{json .Config.Cmd}}')
echo "原 ENTRYPOINT: $ORIG_ENTRY"
echo "原 CMD: $ORIG_CMD"

# 2. 启动临时容器
docker rm -f temp-images 2>/dev/null
docker run -d --name temp-images --privileged --entrypoint tail \
    openeuler/openeuler:24.03 -f /dev/null

# 3. 安装必要组件
docker exec temp-images bash -c " \
    yum install -y systemd systemd-sysv openssh-server sudo chrony \
    linuxptp socat curl wget iputils bind-utils iproute nc tcpdump passwd && \
    yum clean all && rm -rf /var/cache/yum /var/tmp/* /tmp/*"

# 4. 安装 websocat（用于 SSH 代理连接沙箱）
docker exec temp-images bash -c ' \
    wget -O /usr/local/bin/websocat https://github.com/vi/websocat/releases/latest/download/websocat.aarch64-unknown-linux-musl && \
    chmod a+x /usr/local/bin/websocat && \
    websocat --version'

# 5. 导出容器并恢复原始 ENTRYPOINT/CMD
docker stop temp-images
docker export temp-images | docker import \
    --change "ENTRYPOINT $ORIG_ENTRY" \
    --change "CMD $ORIG_CMD" \
    - openeuler:24.03-custom

# 6. 清理临时容器
docker rm -f temp-images
```

**方式三：直接使用测试镜像**

如果仅用于测试，可直接使用基础镜像跳过制作步骤。`--download` 阶段已拉取 `ubuntu:22.04`，可直接上传到 Harbor 使用：

```bash
# 直接使用已有的基础镜像作为沙箱镜像
docker tag ubuntu:22.04 <SERVER_IP>:30443/e2b-orchestration/ubuntu:22.04
```

> **注意**：直接使用基础镜像缺少 systemd、sshd、websocat 等组件，沙箱功能受限（无法 SSH 连接、无法使用 websocat 代理）。仅推荐测试用途。

#### 4.1.2（必需）上传镜像到 Harbor

无论镜像是通过制作得到还是直接使用测试镜像，都必须上传到 Harbor 的 `e2b-orchestration` 项目下。

**登录 Harbor**

```bash
# Nomad 模式（HTTP 端口 2900）
docker login <SERVER_IP>:2900 -u admin -p "${HARBOR_PASSWORD}"

# K8S 模式（HTTPS 端口 30443）
docker login <SERVER_IP>:30443 -u admin -p "${HARBOR_PASSWORD}"
```

**打标签并推送**

```bash
# 以制作好的 ubuntu:22.04-custom 为例
docker tag ubuntu:22.04-custom <SERVER_IP>:30443/e2b-orchestration/ubuntu:22.04-custom
docker push <SERVER_IP>:30443/e2b-orchestration/ubuntu:22.04-custom

# Nomad 模式使用 HTTP 端口
docker tag ubuntu:22.04-custom <SERVER_IP>:2900/e2b-orchestration/ubuntu:22.04-custom
docker push <SERVER_IP>:2900/e2b-orchestration/ubuntu:22.04-custom
```

> `--make` 脚本已自动完成登录和推送，无需手动执行。

**验证镜像已上传**

```bash
# 查看 Harbor 仓库中的镜像列表
curl -sk https://<SERVER_IP>:30443/api/v2.0/projects/e2b-orchestration/repositories \
    -u admin:"${HARBOR_PASSWORD}" -H "accept: application/json" | jq '.[].name'

# 或通过 docker pull 验证
docker pull <SERVER_IP>:30443/e2b-orchestration/ubuntu:22.04-custom
```

### 4.2 构建模板

使用 Python 脚本从 Harbor 镜像构建 E2B 模板：

```bash
# Nomad 模式
python3 create_template.py --server-ip <SERVER_IP> --harbor-ip <SERVER_IP>

# K8S 模式（使用 HTTPS 端口）
python3 create_template.py --server-ip <SERVER_IP> --harbor-ip <SERVER_IP>
```

脚本内部流程：
1. 从 `/root/.e2b/config.json` 读取 API Token
2. 设置环境变量 `E2B_API_URL`、`E2B_API_KEY` 等
3. 调用 `Template.build()` 从 Harbor 镜像构建模板
4. 模板别名为 `openclaw`

**自定义模板构建**：

```python
from e2b import Template, default_build_logger, wait_for_port

Template.build(
    Template().from_dockerfile(
        'FROM <SERVER_IP>:2900/e2b-orchestration/ubuntu:22.04-custom'
    ),
    alias="my-template",
    cpu_count=2,
    memory_mb=2048,
    on_build_logs=default_build_logger(),
    skip_cache=True
)
```

**带启动命令的模板**：

```python
Template.build(
    Template().from_dockerfile(
        'FROM harbor:443/e2b-orchestration/openclaw-openviking:custom'
    ).set_start_cmd(
        'sudo websocat -b --exit-on-eof ws-l:0.0.0.0:8081 tcp:127.0.0.1:22',
        wait_for_port(8081)
    ),
    alias="openclaw",
    cpu_count=2,
    memory_mb=2048,
    on_build_logs=default_build_logger(),
    skip_cache=True
)
```

### 4.3 快速构建模板

`create_template.py` 默认构建别名为 `openclaw` 的模板（固定从 `<HARBOR_IP>:30443/e2b-orchestration/ubuntu:22.04-custom` 镜像构建）：

```bash
# 使用默认 IP（10.10.10.10），适用于本地快速验证
python3 create_template.py

# 指定 SERVER_IP 和 HARBOR_IP
python3 create_template.py --server-ip <SERVER_IP> --harbor-ip <SERVER_IP>
```

如需修改模板别名、镜像或资源配置，直接编辑 [create_template.py](create_template.py) 中的 `Template.build()` 调用。

---

## 5. 沙箱使用

### 5.1 创建沙箱

```bash
python3 create_sandbox.py --server-ip <SERVER_IP>
```

脚本内部流程：
1. 读取 `/root/.e2b/config.json` 获取认证信息
2. 设置环境变量
3. 调用 `Sandbox.create("openclaw")` 创建沙箱
4. 返回沙箱 ID

**Python SDK 方式**：

```python
import os
import json
from e2b import Sandbox

# 设置环境变量
os.environ["E2B_API_URL"] = "http://<SERVER_IP>:3000"
os.environ["E2B_HTTP_SSL"] = "false"
os.environ["E2B_DOMAIN"] = "e2b.app"

# 读取认证信息
with open("/root/.e2b/config.json") as f:
    data = json.load(f)
os.environ["E2B_ACCESS_TOKEN"] = data["accessToken"]
os.environ["E2B_API_KEY"] = data["teamApiKey"]

# 创建沙箱
sbx = Sandbox.create("openclaw")
print(f"沙箱 ID: {sbx.sandbox_id}")

# 执行命令
result = sbx.commands.run("whoami")
print(result)

# 关闭沙箱
sbx.close()
```

### 5.2 SSH 连接沙箱

通过 websocat 代理连接沙箱的 SSH 服务：

```bash
# 设置沙箱 ID
export SANDBOX_ID=<sandbox_id>

# 添加域名解析（如未配置 dnsmasq）
echo "127.0.0.1 8081-${SANDBOX_ID}.e2b.app" | sudo tee -a /etc/hosts

# SSH 连接
ssh -o "ProxyCommand=websocat --binary -B 65536 ws://8081-${SANDBOX_ID}.e2b.app" \
    -o "StrictHostKeyChecking=no" \
    -o "UserKnownHostsFile=/dev/null" \
    user@8081-${SANDBOX_ID}.e2b.app
```

### 5.3 环境变量说明

| 变量 | 说明 | 示例 |
|------|------|------|
| `E2B_API_URL` | E2B API 地址 | `http://<IP>:3000` |
| `E2B_HTTP_SSL` | 是否启用 SSL | `false`（本地部署） |
| `E2B_DOMAIN` | E2B 域名 | `e2b.app` |
| `E2B_ACCESS_TOKEN` | 访问令牌 | 从 `/root/.e2b/config.json` 获取 |
| `E2B_API_KEY` | 团队 API Key | 从 `/root/.e2b/config.json` 获取 |

认证信息存储在 `/root/.e2b/config.json`：

```json
{
    "accessToken": "<E2B_ACCESS_TOKEN>",
    "teamApiKey": "e2b_xxxxx"
}
```

---

## 6. E2B 插件部署

将 E2B 沙箱能力集成到 OpenClaw 容器中。

### 6.1 部署插件

```bash
# Nomad 模式（Docker 容器）
./build.sh --deploy-plugin

# K8S 模式
./build.sh --deploy-plugin <pod名> <模板名> <selector> <namespace>
```

### 6.2 插件部署流程

```
[1/7] 克隆插件源码 (openclaw-sandbox-exec)
    │
    ▼
[2/7] 编译插件 (npm install && npm run build)
    │
    ▼
[3/7] 安装插件到 OpenClaw (openclaw plugins install)
    │
    ▼
[4/7] 修改 E2B SDK API 地址 (指向本地 E2B 服务)
    │
    ▼
[5/7] 注入 local-exec 配置到 openclaw.json
    │
    ▼
[6/7] 安装网络工具 (dnsmasq + socat)
    │
    ▼
[7/7] 配置 DNS 劫持 + 端口转发
```

### 6.3 插件配置参数

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `target` | 容器/Pod 名 | 自动获取 |
| `template` | E2B 模板名 | `base`（Nomad）/ `openclaw`（K8S） |
| `selector` | K8S 标签选择器 | `app=openclaw-deploy-for-local-exec` |
| `namespace` | K8S 命名空间 | `default` |

---

## 7. 运维操作

### 7.1 服务管理

```bash
# 启动服务
./build.sh --start

# 停止服务
./build.sh --stop

# 重启服务
./build.sh --stop && ./build.sh --start

# 重新部署（不重启基础设施）
./build.sh --deploy
```

### 7.2 单独部署组件

```bash
./build.sh --deploy nomad       # 重新部署 Nomad
./build.sh --deploy consul      # 重新部署 Consul
./build.sh --deploy postgres    # 重新部署 PostgreSQL
./build.sh --deploy harbor      # 重新部署 Harbor
./build.sh --deploy services    # 构建镜像并部署 E2B 服务（Nomad/K8S 任务 + 生成 Token）
```

### 7.3 单独卸载组件

```bash
./build.sh --remove nomad       # 卸载 Nomad
./build.sh --remove consul      # 卸载 Consul
./build.sh --remove harbor      # 卸载 Harbor
./build.sh --remove postgres    # 卸载 PostgreSQL
```

### 7.4 Harbor 项目管理

单独创建 Harbor 项目，无需重新部署 Harbor：

```bash
# 创建默认项目 e2b-orchestration
./build.sh --create-harbor-project ''

# 创建指定项目
./build.sh --create-harbor-project my-project
```

脚本会自动等待 Harbor 就绪、登录、然后创建项目。

### 7.5 Nomad 任务管理

对 Nomad 任务进行独立操作，不影响基础设施：

```bash
# 查看所有任务
./build.sh --nomad-job list

# 部署指定任务
./build.sh --nomad-job deploy redis
./build.sh --nomad-job deploy api
./build.sh --nomad-job deploy all      # 部署所有任务（构建镜像 + 部署 + 生成 Token）

# 停止任务（保留任务记录）
./build.sh --nomad-job stop api

# 启动已停止的任务
./build.sh --nomad-job start api

# 删除任务（彻底清除）
./build.sh --nomad-job delete api
```

| 操作 | 说明 | 底层命令 |
|------|------|----------|
| `deploy` | 部署任务 | `nomad job run` |
| `stop` | 停止任务（保留记录） | `nomad job stop` |
| `start` | 启动已停止的任务 | `nomad job run` |
| `delete` | 删除任务（彻底清除） | `nomad job stop -purge` |
| `list` | 查看所有任务 | `nomad job status` |

支持的任务名：`redis`、`template-manager`、`edge`、`api`、`all`（默认）

### 7.6 全量卸载

```bash
./build.sh --uninstall
```

### 7.7 修改沙箱配置

**修改默认超时时间**：

```bash
# 默认 24 小时
docker exec postgres psql -U postgres -d mydatabase \
    -c "UPDATE tiers SET max_length_hours = 24 WHERE id = 'base_v1';"
```

**修改最大并发数**：

```bash
# 默认 50
docker exec postgres psql -U postgres -d mydatabase \
    -c "UPDATE tiers SET concurrent_instances = 50 WHERE id = 'base_v1';"
```

### 7.8 下载离线包

```bash
# x86_64
ARCH=x86 ./build.sh --download

# arm64
ARCH=arm64 ./build.sh --download
```

---

## 8. 常见问题

### 8.1 部署脚本失败（Nomad 403 错误）

**现象**：

```
Error submitting job: Unexpected response code: 403 (Permission denied)
```

**原因**：Nomad 未完全启动。

**解决**：

```bash
# 检查 Nomad 端口
ss -tlnp | grep 4646

# 等待后重试
./build.sh --deploy

# 仍失败则重启
./build.sh --stop && ./build.sh --start
```

### 8.2 模板构建失败（连接拒绝）

**现象**：

```
BuildException: dial tcp xxx:5008: connect: connection refused
```

**原因**：Template Manager 内存不足。

**解决**：通过 Nomad Web 界面增加 Template Manager 资源：

```hcl
resources {
    memory = 81920
    cpu    = 20480
}
```

### 8.3 Consul 启动失败

**原因**：代理干扰。

**解决**：关闭系统代理后重启。

### 8.4 API 部署失败

**原因**：PostgreSQL 连接异常。

**解决**（详见 [3.3 PostgreSQL 数据库部署](#33-postgresql-数据库部署)）：

```bash
# 检查 PostgreSQL 状态
docker exec -it postgres psql -U postgres -d mydatabase -c "\q"

# 重启 PostgreSQL
docker restart postgres
```

K8S 模式下：

```bash
# 检查 PostgreSQL Pod
kubectl get pods -n e2b -l app=postgres

# 检查持久化目录
ls -ld /data/postgres

# 重启 Pod
kubectl delete pod -n e2b -l app=postgres
```

### 8.5 Template 启动失败

**原因**：`.env` 配置缺失。

**解决**：确保 `.env` 中包含：

```bash
export API_NODE_POOL=api
```

### 8.6 Harbor 镜像拉取失败

根据 `HARBOR_PROTOCOL` 配置，检查对应协议：

**HTTP 模式（默认端口 2900）**：

```bash
# 检查 Docker 信任配置
cat /etc/docker/daemon.json | jq .

# 检查 /etc/hosts
grep harbor /etc/hosts

# 手动登录
docker login <SERVER_IP>:2900 -u admin -p "${HARBOR_PASSWORD}"
```

**HTTPS 模式（默认端口 30443，K8S 模式）**：

```bash
# 检查 containerd 证书
ls /etc/containerd/certs.d/<IP>:30443/

# 检查 containerd 配置
grep -A5 "<IP>:30443" /etc/containerd/config.toml

# 手动登录
nerdctl login <SERVER_IP>:30443 -u admin -p "${HARBOR_PASSWORD}"
```

> 提示：使用 `HARBOR_PROTOCOL=both` 可同时支持两种协议，便于排查问题。

### 8.7 K8S 域名解析失败

```bash
# 检查 dnsmasq
dig *.e2b.app @127.0.0.1

# 检查 CoreDNS
kubectl get configmap coredns -n kube-system -o yaml | grep rewrite

# 检查 Ingress
kubectl get ingress -n e2b
kubectl describe ingress wildcard-e2b-app -n e2b

# 检查 Ingress Controller 端口
kubectl get svc ingress-nginx-controller -n ingress-nginx
```

---

## 9. 命令速查

| 操作 | Nomad 模式 | K8S 模式 |
|------|-----------|----------|
| 下载组件 | `./build.sh --download` | `./build.sh --download` |
| 安装 | `./build.sh --install` | `./build.sh --k8s <node> --install` |
| 启动 | `./build.sh --start` | `./build.sh --k8s <node> --start` |
| 停止 | `./build.sh --stop` | `./build.sh --stop` |
| 部署 Worker | - | `./deploy-worker.sh worker1` |
| 构建模板 | `python3 create_template.py` | `python3 create_template.py` |
| 创建沙箱 | `python3 create_sandbox.py` | `python3 create_sandbox.py` |
| 部署插件 | `./build.sh --deploy-plugin` | `./build.sh --deploy-plugin` |
| 部署 E2B 服务 | `./build.sh --deploy services` | `./build.sh --deploy services` |
| 单独部署 PostgreSQL | `./build.sh --deploy postgres` | `./build.sh --deploy postgres` |
| 启用 webhook | - | `ENABLE_WEBHOOK=true` 后 `--start` |
| 创建 Harbor 项目 | `./build.sh --create-harbor-project ''` | `./build.sh --create-harbor-project ''` |
| Nomad 任务管理 | `./build.sh --nomad-job list` | - |
| 单独卸载组件 | `./build.sh --remove harbor` | `./build.sh --remove harbor` |
| 全量卸载 | `./build.sh --uninstall` | `./build.sh --uninstall` |
| 下载离线包 | `./build.sh --download` | `./build.sh --download` |
| 查看帮助 | `./build.sh --help` | `./build.sh --help` |
