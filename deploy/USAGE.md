# E2B 使用文档

> E2B 是基于 Firecracker 微虚拟机的沙箱执行平台，可在隔离环境中安全运行用户代码。本文档覆盖从环境准备、两种模式部署（Nomad / K8S）、模板管理与沙箱创建、插件集成，到运维操作与故障排查的完整流程。

## 目录

- [1. 快速开始](#1-快速开始)
  - [1.1 部署模式选择](#11-部署模式选择)
  - [1.2 Nomad 模式快速上手](#12-nomad-模式快速上手)
  - [1.3 K8S 模式快速上手](#13-k8s-模式快速上手)
- [2. 概述](#2-概述)
  - [2.1 核心概念](#21-核心概念)
  - [2.2 架构与端口一览](#22-架构与端口一览)
  - [2.3 目录结构](#23-目录结构)
- [3. 环境准备](#3-环境准备)
  - [3.1 系统要求](#31-系统要求)
  - [3.2 修改配置文件](#32-修改配置文件)
  - [3.3 关闭 SELinux](#33-关闭-selinux)
  - [3.4 组件下载](#34-组件下载)
  - [3.5 Mooncake 配置（可选）](#35-mooncake-配置可选)
- [4. 部署：Nomad 模式（单机）](#4-部署nomad-模式单机)
  - [4.1 下载组件](#41-下载组件)
  - [4.2 安装](#42-安装)
  - [4.3 启动服务](#43-启动服务)
  - [4.4 Harbor 协议配置](#44-harbor-协议配置)
  - [4.5 验证](#45-验证)
- [5. 部署：K8S 模式（生产）](#5-部署k8s-模式生产)
  - [5.1 前置条件](#51-前置条件)
  - [5.2 集群部署](#52-集群部署)
  - [5.3 Master 节点部署](#53-master-节点部署)
  - [5.4 Worker 节点部署](#54-worker-节点部署)
  - [5.5 配置域名访问](#55-配置域名访问)
  - [5.6 验证](#56-验证)
  - [5.7 可选组件：cri-multiplex](#57-可选组件cri-multiplex)
  - [5.8 可选组件：e2b-webhook](#58-可选组件e2b-webhook)
- [6. 模板管理](#6-模板管理)
  - [6.1 制作沙箱镜像](#61-制作沙箱镜像)
  - [6.2 上传镜像到 Harbor](#62-上传镜像到-harbor)
  - [6.3 构建模板](#63-构建模板)
- [7. 创建沙箱](#7-创建沙箱)
  - [7.1 方式一：SDK 创建](#71-方式一sdk-创建)
  - [7.2 方式二：K8S 沙箱 Pod 创建](#72-方式二k8s-沙箱-pod-创建)
  - [7.3 环境变量说明](#73-环境变量说明)
  - [7.4 认证信息](#74-认证信息)
- [8. E2B 插件部署](#8-e2b-插件部署)
  - [8.1 部署插件](#81-部署插件)
  - [8.2 插件部署流程](#82-插件部署流程)
  - [8.3 插件配置参数](#83-插件配置参数)
- [9. 运维操作](#9-运维操作)
  - [9.1 服务管理](#91-服务管理)
  - [9.2 单独部署组件](#92-单独部署组件)
  - [9.3 单独卸载组件](#93-单独卸载组件)
  - [9.4 Harbor 项目管理](#94-harbor-项目管理)
  - [9.5 Nomad 任务管理](#95-nomad-任务管理)
  - [9.6 修改沙箱配置](#96-修改沙箱配置)
  - [9.7 下载离线包](#97-下载离线包)
  - [9.8 全量卸载](#98-全量卸载)
- [10. 常见问题](#10-常见问题)
  - [10.1 部署脚本失败（Nomad 403 错误）](#101-部署脚本失败nomad-403-错误)
  - [10.2 模板构建失败（连接拒绝）](#102-模板构建失败连接拒绝)
  - [10.3 Consul 启动失败](#103-consul-启动失败)
  - [10.4 API 部署失败](#104-api-部署失败)
  - [10.5 Template 启动失败](#105-template-启动失败)
  - [10.6 Harbor 镜像拉取失败](#106-harbor-镜像拉取失败)
  - [10.7 K8S 域名解析失败](#107-k8s-域名解析失败)
- [11. 命令速查](#11-命令速查)
- [附录 A：环境变量全览](#附录-a环境变量全览)
- [附录 B：组件端口与地址](#附录-b组件端口与地址)
- [附录 C：目录结构](#附录-c目录结构)

---

## 1. 快速开始

### 1.1 部署模式选择

| 场景 | 推荐路径 | 说明 |
|------|----------|------|
| 单机 / 小规模 / 快速体验 | [4. 部署：Nomad 模式（单机）](#4-部署nomad-模式单机) | Docker + Nomad + Consul，脚本一键部署 |
| 多节点 / 生产 / 需要 K8S 原生调度 | [5. 部署：K8S 模式（生产）](#5-部署k8s-模式生产) | Kubernetes + containerd + nerdctl，可选 cri-multiplex 与 e2b-webhook |

### 1.2 Nomad 模式快速上手

```bash
# 1. 修改 .env 中的 SERVER_IP 为本机 IP
vi .env

# 2. 下载组件
./build.sh --download

# 3. 安装
./build.sh --install

# 4. 启动
./build.sh --start

# 5. 构建模板并创建沙箱
python3 create_template.py
python3 create_sandbox.py --server-ip <SERVER_IP>
```

### 1.3 K8S 模式快速上手

```bash
# 1. 修改 .env：SERVER_IP 为本机 IP，DEPLOY_MODE=k8s
vi .env

# 2. 生成集群配置并创建集群（自动部署 ingress-nginx、配置域名）
./k8s-deploy.sh prep && ./k8s-deploy.sh create

# 3. 下载组件、安装并启动 Master 节点
./build.sh --download
./build.sh --k8s <节点名> --install --start

# 4. 部署 Worker 节点（可多个）
./deploy-worker.sh worker1 worker2

# 5. 配置 *.e2b.app 域名访问
./k8s-deploy.sh configure-domain
```

> **说明**：上述两条快速路径省略了大量可选项（Harbor 协议、Mooncake、cri-multiplex、e2b-webhook 等），完整步骤见对应章节。

---

## 2. 概述

### 2.1 核心概念

| 概念 | 说明 |
|------|------|
| **Sandbox（沙箱）** | 基于 Firecracker 的隔离微虚拟机，用于执行代码 |
| **Template（模板）** | 沙箱的镜像定义，基于 Dockerfile 构建 |
| **API Service** | 沙箱管理 API 入口（端口 3000） |
| **Client Proxy（edge）** | 沙箱连接代理（端口 3002），K8S 模式下对应 Service 名 `edge-api` |
| **Template Manager / Orchestrator** | 模板构建与沙箱生命周期管理（端口 5008），两名称指同一服务 |
| **Harbor** | 本地镜像仓库，存储沙箱镜像 |
| **PostgreSQL** | 元数据存储（团队、用户、模板、沙箱配额） |
| **e2b-webhook** | K8S 准入控制器，拦截沙箱 Pod 创建（可选） |
| **cri-multiplex** | CRI 多路复用器，让 K8S 原生调度 E2B 沙箱 Pod（可选） |

### 2.2 架构与端口一览

```
┌────────────────────────────────────────────────────────────┐
│ API Service (3000) ─ Client Proxy / edge (3002)             │
│        │                                                     │
│  Template Manager / Orchestrator (5008)                     │
│        │                                                     │
│  Nomad (4646) + Consul         ── Nomad 模式                 │
│  Kubernetes + containerd       ── K8S 模式                   │
│        │                                                     │
│  Firecracker 微VM ── envd (49983)  ── 沙箱内代理              │
└────────────────────────────────────────────────────────────┘
```

完整端口与地址清单见 [附录 B：组件端口与地址](#附录-b组件端口与地址)，此处仅列核心服务：

| 组件 | 端口 / 地址 | 说明 |
|------|-------------|------|
| API Service | 3000 | 沙箱管理 API 入口 |
| Client Proxy（edge） | 3002 | 沙箱连接代理，配合 `*.e2b.app` 域名使用 |
| Template Manager / Orchestrator | 5008 | 模板构建与沙箱生命周期管理 |
| Nomad | 4646 | 调度器（Nomad 模式），Web UI |
| Harbor | 2900（HTTP）/ 30443（HTTPS） | 镜像仓库，协议由 `HARBOR_PROTOCOL` 控制 |
| cri-multiplex | `/run/cri-multiplex.sock` | CRI gRPC 多路复用（K8S 模式可选） |
| envd | 49983（沙箱内） | 沙箱内守护进程，供 SDK 操作沙箱 |

### 2.3 目录结构

完整的源码目录树与部署目标目录树见 [附录 C：目录结构](#附录-c目录结构)。要点如下：

- 源码仓库 `deploy/` 含构建 / 部署脚本（`build.sh`、`k8s-deploy.sh`、`deploy-worker.sh` 等）、配置模板（`.env`）与 Nomad 任务定义（`nomad/`）。
- 通过 RPM 安装后落地到 `/opt/e2b-infra/`，包含脚本、`bin/`（二进制与 Dockerfile）、`helm/`（Helm 模板）、`dep/`（部署依赖脚本副本）。
- 所有环境变量统一在 `.env` 中配置，详见 [3.2 修改配置文件](#32-修改配置文件) 与 [附录 A：环境变量全览](#附录-a环境变量全览)。

---

## 3. 环境准备

### 3.1 系统要求

| 项目 | 要求 |
|------|------|
| 操作系统 | openEuler2403sp3 |
| 架构 | arm64 |
| 内存 | ≥ 16GB（推荐 32GB） |
| 磁盘 | ≥ 100GB |
| 网络 | 可访问外网（下载依赖包时） |

### 3.2 修改配置文件

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

> 资源配额在 Nomad 与 K8S 两种模式下均生效（Nomad 任务资源限制 / K8S Deployment `resources`）。

可选：按机器规格调整 API 与 Template Manager 的资源配额（`RESOURCES` 为初始分配值，`LIMITS` 为上限，默认值按 32GB 内存 / 高核数机器设计）：

```bash
# API 服务（CPU 单位：K8S 为毫核 m，Nomad 为 MHz，20000m = 20 核）
export API_RESOURCES_CPU_COUNT=20000           # 初始 CPU 20 核
export API_RESOURCES_MEMORY_MB=20480           # 初始内存 20GB
export API_LIMITS_CPU_COUNT=20000              # CPU 上限 20 核
export API_LIMITS_MEMORY_MB=40960              # 内存上限 40GB

# Template Manager（2048m ≈ 2 核）
export TEMPLATE_MANAGER_RESOURCES_CPU_COUNT=2048        # 初始 CPU ≈2 核
export TEMPLATE_MANAGER_RESOURCES_MEMORY_MB=8192        # 初始内存 8GB
export TEMPLATE_MANAGER_LIMITS_CPU_COUNT=2048           # CPU 上限 ≈2 核
export TEMPLATE_MANAGER_LIMITS_MEMORY_MB=8192           # 内存上限 8GB
```

### 3.3 关闭 SELinux

```bash
setenforce 0
```

### 3.4 组件下载

> **重要**：安装前**必须**先下载好所有组件包，否则 `--install` 会失败。

#### 3.4.1 自动下载（推荐）

```bash
./build.sh --download
```

自动下载所有必需组件到 `dep/` 目录，包括二进制包、Docker 镜像、Python 依赖等。

#### 3.4.2 手动下载（二进制组件）

如网络受限，可手动下载以下组件到 `dep/` 目录。

**Nomad 模式组件**

| 组件 | 版本 | x86_64 下载地址 | arm64 下载地址 |
|------|------|----------------|---------------|
| Docker | 25.0.5 | https://download.docker.com/linux/static/stable/x86_64/docker-25.0.5.tgz | https://download.docker.com/linux/static/stable/aarch64/docker-25.0.5.tgz |
| Docker Compose | 2.40.2 | https://github.com/docker/compose/releases/download/v2.40.2/docker-compose-linux-x86_64 | https://github.com/docker/compose/releases/download/v2.40.2/docker-compose-linux-aarch64 |
| Nomad | 1.10.4 | https://releases.hashicorp.com/nomad/1.10.4/nomad_1.10.4_linux_amd64.zip | https://releases.hashicorp.com/nomad/1.10.4/nomad_1.10.4_linux_arm64.zip |
| Consul | 1.21.4 | https://releases.hashicorp.com/consul/1.21.4/consul_1.21.4_linux_amd64.zip | https://releases.hashicorp.com/consul/1.21.4/consul_1.21.4_linux_arm64.zip |
| Firecracker | 1.13.1 | https://github.com/firecracker-microvm/firecracker/releases/download/v1.13.1/firecracker-v1.13.1-x86_64.tgz | https://github.com/firecracker-microvm/firecracker/releases/download/v1.13.1/firecracker-v1.13.1-aarch64.tgz |
| Harbor | 2.13.0 | https://github.com/goharbor/harbor/releases/download/v2.13.0/harbor-offline-installer-v2.13.0.tgz | https://github.com/wise2c-devops/build-harbor-aarch64/releases/download/v2.13.0/harbor-offline-installer-aarch64-v2.13.0.tgz |

**K8S 模式额外组件**

K8S 模式包含 Nomad 模式的全部组件（除 Docker / Docker Compose 外），额外需要：

| 组件 | 说明 | 安装方式 |
|------|------|----------|
| Kubernetes | K8S 集群（kubelet, kubectl, kubeadm） | 需预先安装，脚本不负责部署 |
| Nginx Ingress Controller | Ingress 路由控制器 | 内置清单 `dep/ingress-nginx.yaml`，未安装时手动 apply（见 [5.1 前置条件](#51-前置条件)） |
| containerd | 容器运行时 | K8S 节点自带 |
| nerdctl | containerd CLI | 替代 Docker 命令 |
| helm | K8S 包管理器 | 用于卸载 e2b-api |

> **注意**：K8S 集群、kubectl 需在运行脚本前自行安装配置。Ingress Controller 可使用内置清单部署。

#### 3.4.3 容器镜像

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

**e2b-webhook 镜像（K8S 模式可选）**

仅当 K8S 模式下启用 webhook（`ENABLE_WEBHOOK=true`）时需要，`--download` 会自动下载：

| 组件 | 版本 | 下载地址 | 说明 |
|------|------|----------|------|
| e2b-webhook | 1.0.0 | https://gitcode.com/fly_1997/e2b-webhook/releases/download/1.0.0/e2b-webhook.tar | 镜像 tar 包，`--install` 时自动 `docker load` 导入 |

> **说明**：
> - 下载后保存为 `dep/e2b-webhook.tar`，`pull_docker_images` 会自动通过 `docker load -i` 导入为本地镜像 `e2b-webhook`。
> - 仅 K8S 模式下载（nomad 模式不需要）。
> - 部署时由 `deploy.sh` 推送到 Harbor，详见 [5.8 可选组件：e2b-webhook](#58-可选组件e2b-webhook)。

#### 3.4.4 Python 与系统依赖

**Python 依赖**

| 包 | 版本 | 安装命令 |
|-----|------|----------|
| e2b | 2.20.0 | `pip install e2b==2.20.0` |
| e2b_code_interpreter | 2.4.1 | `pip install e2b_code_interpreter==2.4.1` |

**系统依赖**

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

### 3.5 Mooncake 配置（可选）

Mooncake 是分布式内存语义层组件，用于加速跨节点内存共享。**仅当 `STORAGE_PROVIDER=MooncakeBucket` 时需要配置**，其他存储后端可跳过本节。

```bash
export STORAGE_PROVIDER=MooncakeBucket
```

**必须配置**

| 变量 | 说明 | 示例 |
|------|------|------|
| `MOONCAKE_MASTER_ADDR` | 集群 Master 地址（固定节点） | `141.61.17.196:50055` |
| `MOONCAKE_METADATA_SERVER` | 元数据服务地址（固定节点） | `http://141.61.17.196:8015` |

**自动获取（无需手动配置）**

以下变量由调度平台自动获取节点 IP，**无需在 `.env` 中设置**：

| 变量 | Nomad 模式 | K8S 模式 |
|------|-----------|----------|
| `MOONCAKE_LOCAL_HOSTNAME` | `$${attr.unique.network.ip-address}` | `status.hostIP`（Downward API） |
| `MC_TCP_BIND_ADDRESS` | `$${attr.unique.network.ip-address}` | `status.hostIP`（Downward API） |

**可选配置**

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

**配置示例**

```bash
# 编辑 .env，修改 Master 地址和元数据服务地址
vi .env

# --- Mooncake ---
export MOONCAKE_MASTER_ADDR="10.10.10.10:50055"             # 改为 Master 节点 IP
export MOONCAKE_METADATA_SERVER="http://10.10.10.10:8015"   # 改为元数据服务地址
```

> **注意**：`MOONCAKE_LOCAL_HOSTNAME` 和 `MC_TCP_BIND_ADDRESS` 在 Nomad 模式下通过 Nomad 属性自动获取节点 IP，在 K8S 模式下通过 Downward API 获取 `status.hostIP`，均无需手动配置。

---

## 4. 部署：Nomad 模式（单机）

适用于单机或小规模环境，使用 Docker + Nomad + Consul 调度。

### 4.1 下载组件

```bash
./build.sh --download
```

### 4.2 安装

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

### 4.3 启动服务

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

### 4.4 Harbor 协议配置

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

```json
// /etc/docker/daemon.json
{
    "insecure-registries": ["<SERVER_IP>:2900"]
}
```

### 4.5 验证

```bash
# 检查 Nomad 状态
ss -tlnp | grep 4646
```

示例输出：

```
LISTEN 0      4096         0.0.0.0:4646       0.0.0.0:*   users:(("nomad",pid=1234,fd=11))
```

```bash
# 检查 Harbor 状态
curl -sk http://<SERVER_IP>:2900/api/v2.0/health | jq .
```

示例输出：

```json
{
  "status": "healthy",
  "components": [
    { "name": "core", "status": "healthy" },
    { "name": "portal", "status": "healthy" }
  ]
}
```

访问 Nomad Web 界面：`http://<SERVER_IP>:4646`，Token 在 `/opt/e2b-infra/.env` 的 `NOMAD_ACL_TOKEN` 字段。

---

## 5. 部署：K8S 模式（生产）

适用于多节点/生产环境，使用 Kubernetes + containerd + nerdctl。

> **可选组件**：ingress-nginx（`*.e2b.app` 域名访问，通过 E2B SDK 执行沙箱命令时需要，见 [5.5 配置域名访问](#55-配置域名访问)）、cri-multiplex 与 e2b-webhook 均为 K8S 模式下的可选组件，分别见 [5.7 可选组件：cri-multiplex](#57-可选组件cri-multiplex) 与 [5.8 可选组件：e2b-webhook](#58-可选组件e2b-webhook)。如不需要 K8S 原生调度沙箱 Pod，可直接跳过。

### 5.1 前置条件

- K8S 集群已就绪
- kubectl 可正常访问集群

### 5.2 集群部署

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
| `configure-domain` | 单独配置 `*.e2b.app` 域名访问（见 [5.5 配置域名访问](#55-配置域名访问)） |
| `cri-multiplex` | 部署 cri-multiplex（见 [5.7 可选组件：cri-multiplex](#57-可选组件cri-multiplex)） |
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

### 5.3 Master 节点部署

依次执行组件下载、安装与启动三步。

**步骤一：组件下载**

```bash
./build.sh --download
```

**步骤二：Master 节点安装**

```bash
./build.sh --k8s <节点名> --install
```

不指定节点名时自动选择第一个节点：

```bash
./build.sh --k8s --install
```

**步骤三：Master 节点启动**

```bash
./build.sh --k8s <节点名> --start
```

启动过程额外步骤（相比 Nomad 模式）：

- Harbor 根据 `HARBOR_PROTOCOL` 配置协议（默认 both）
- HTTPS 启用时配置 containerd 证书和仓库
- Kubelet 重启应用大页配置
- 节点标签设置（如 `sandbox=true`）
- K8S Deployment 部署

### 5.4 Worker 节点部署

**前置条件确认**

Worker 部署依赖 Master 节点生成的安装包与证书，执行前请确认以下产物已就绪。脚本启动时会自动执行预检（`preflight_check`），任一缺失即报错退出：

| 项目 | 默认路径 | 说明 |
|------|----------|------|
| e2b-infra 代码包 | `/home/e2b`（`E2B_INFRA_SRC` 可覆盖） | 待分发的部署代码包，由 [5.3 Master 节点部署](#53-master-节点部署) 步骤二生成 |
| containerd 证书 | `/etc/containerd/certs.d/<SERVER_IP>:30443` | containerd 拉取 Harbor 私有仓库所需，由 [5.3 Master 节点部署](#53-master-节点部署) 步骤三生成 |
| Harbor 证书 | `/etc/harbor/certs/harbor.crt` | Harbor SSL 证书，由 [5.3 Master 节点部署](#53-master-节点部署) 步骤三生成 |
| E2B API Token | `/root/.e2b/config.json` | E2B API 访问令牌，由 [5.3 Master 节点部署](#53-master-节点部署) 步骤三生成 |
| SSH 私钥 | `~/.ssh/id_rsa` | 免密登录目标节点（不存在时脚本自动生成） |
| 本地工具 | `kubectl` / `ssh` / `scp` | 本地环境依赖 |

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

### 5.5 配置域名访问

通过 E2B SDK 执行沙箱命令时，需配置三层域名解析，确保宿主机和集群内部 Pod 均可通过 `*.e2b.app` 访问沙箱。可选自动或手动两种方式。

**前置步骤：部署 Nginx Ingress Controller**

`*.e2b.app` 域名流量经由 Ingress 转发到 Client Proxy（edge-api:3002），因此首先需确保 Ingress Controller 已部署。确认是否已安装：

```bash
# 方式一：检查命名空间
kubectl get namespace ingress-nginx

# 方式二：检查 Deployment
kubectl get deployment -n ingress-nginx ingress-nginx-controller

# 方式三：检查 IngressClass
kubectl get ingressclass nginx
```

若上述命令均返回有效资源，说明已安装，可跳过部署步骤。

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
> - 该清单中 Service 类型为 `LoadBalancer`。裸金属环境若无外部 LB，可改为 `NodePort` 或配合 MetalLB 使用。
> - 部署后需确保 Controller 暴露 80 端口（见下方方式二步骤一）。
> - 镜像需从 `k8s.dockerproxy.net` 拉取，离线环境请预先导入镜像。

**方式一：自动部署（推荐）**

```bash
./k8s-deploy.sh configure-domain
```

脚本一键完成三层配置：

| 步骤 | 操作 |
|------|------|
| 1 | 检查 Ingress Controller：验证 `ingress-nginx-controller` 是否暴露 80 端口，未暴露则告警提示 |
| 2 | 配置 CoreDNS 重写：自动在 Corefile 中添加 `rewrite name regex .*\.e2b\.app\.$ edge-api.e2b.svc.cluster.local` 并重启 coredns（幂等） |
| 3 | 创建 wildcard Ingress：`kubectl apply -f dep/wildcard-ingress.yaml`，将 `*.e2b.app` 转发到 `edge-api:3002`（依赖 `e2b` 命名空间，未部署则跳过） |

> 若 `e2b` 命名空间尚未创建，第 3 步会跳过并提示；待 e2b 部署完成后重新执行 `./k8s-deploy.sh configure-domain` 补建即可。

**方式二：手动部署（分三步）**

**步骤一：Ingress Controller 监听 80 端口**

```bash
kubectl edit svc -n ingress-nginx ingress-nginx-controller
# 在 ports 中增加 HTTP 80 端口映射
```

**步骤二：部署 wildcard Ingress**

创建 `*.e2b.app` 通配符 Ingress，将请求转发到 `edge-api:3002`：

```bash
# 按实际环境修改 dep/wildcard-ingress.yaml 中的 namespace（edge-api 所在命名空间）和服务名后应用
kubectl apply -f dep/wildcard-ingress.yaml
```

**步骤三：CoreDNS 重写规则**

```bash
kubectl edit configmap coredns -n kube-system
# 添加：rewrite name regex .*\.e2b\.app\.$ edge-api.e2b.svc.cluster.local

kubectl rollout restart deployment coredns -n kube-system
```

### 5.6 验证

验证 K8S 是否部署成功，检查以下组件是否正常运行。

**核心组件（必须正常运行，位于 `e2b` 命名空间）**

```bash
kubectl get pods -n e2b -o wide
```

期望看到以下 Pod 均为 `Running` 且 `READY 1/1`：

| Pod | 说明 |
|-----|------|
| `postgres-*` | 元数据存储（teams / users / templates / 沙箱配额） |
| `redis-*` | 缓存 / 会话存储 |
| `api-*` | API 服务（端口 3000） |
| `edge-*` | 客户端代理（端口 3002） |
| `template-manager-*` | 模板管理 / 沙箱生命周期管理（端口 5008） |

**可选组件（按需启用/部署）**

| 组件 | 命名空间 | 检查命令 | 期望 |
|------|---------|----------|------|
| e2b-webhook | `e2b` | `kubectl get pods -n e2b -l app.kubernetes.io/name=e2b-webhook` | Running（启用 `ENABLE_WEBHOOK=true` 时） |
| ingress-nginx | `ingress-nginx` | `kubectl get pods -n ingress-nginx` | Running |
| coredns | `kube-system` | `kubectl get pods -n kube-system \| grep coredns` | Running |
| wildcard-e2b-app | `e2b`（Ingress） | `kubectl get ingress -n e2b` | 存在 `wildcard-e2b-app`（域名访问配置后） |

### 5.7 可选组件：cri-multiplex

cri-multiplex 是 CRI gRPC 多路复用器，让 kubelet 通过单一 Unix socket 调度 **containerd**（普通 Pod）和 **E2B orchestrator**（沙箱 Pod）。

```
Kubelet ──Unix socket──▶ cri-multiplex
                           ├── RuntimeHandler != "e2b" ──▶ ContainerEngine ──▶ containerd
                           └── RuntimeHandler == "e2b" ──▶ E2BEngine ──▶ orchestrator:5008
```

**前置条件**：K8S 集群已就绪、kubelet 通过 kubeadm 初始化、containerd 运行中、orchestrator 可达（默认 `localhost:5008`）、二进制已随 RPM 安装至 `/opt/e2b-infra/bin/cri-multiplex`。

**部署流程**（拆分为两步，每个节点依次执行）：

**步骤一：部署 cri-multiplex 服务**（检查二进制、创建 systemd 服务、创建 RuntimeClass）：

```bash
./k8s-deploy.sh cri-multiplex
```

| 步骤 | 操作 |
|------|------|
| 1 | 检查二进制 `/opt/e2b-infra/bin/cri-multiplex`，不存在则跳过 |
| 2 | 创建 systemd 服务：开机自启 + 崩溃自动重启，等待 `/run/cri-multiplex.sock` 就绪 |
| 3 | 创建 RuntimeClass：`android`、`e2b`，Pod 通过 `runtimeClassName` 选择后端 |

**步骤二：切换 kubelet endpoint**（把 kubelet 从 `containerd.sock` 切到 `cri-multiplex.sock` 并重启，使调度走 cri-multiplex）：

```bash
./k8s-deploy.sh kubelet-endpoint
```

| 步骤 | 操作 |
|------|------|
| 1 | 修改 kubelet 启动参数：将 runtime-endpoint 切到 `unix:///run/cri-multiplex.sock`（幂等，自动备份原文件）。默认改 `/var/lib/kubelet/kubeadm-flags.env`，**具体路径取决于 kubelet 启动方式**，见下方说明 |
| 2 | 重启 kubelet：使新 endpoint 生效，并确认 kubelet 运行中 |

> **说明**：
>
> 1. kubelet 启动方式不同，配置位置/参数形式可能不同——kubeadm 初始化的一般是 `/var/lib/kubelet/kubeadm-flags.env`（默认路径）；若是自定义 systemd unit 或其它方式启动，runtime-endpoint 可能写在启动参数或独立配置文件中。脚本默认处理 kubeadm 方式，其它场景需按实际启动方式调整，也可用 `KUBELET_FLAGS_FILE` 指定配置文件路径。
> 2. `cri-multiplex` 只负责部署服务，不切换 endpoint；`kubelet-endpoint` 幂等，若已指向 `cri-multiplex.sock` 则跳过修改。两者配合使用即为完整的 cri-multiplex 接入。也可通过 SSH 将 `kubelet-endpoint` 单独分发给其它节点执行。

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

### 5.8 可选组件：e2b-webhook

e2b-webhook 是 K8S 模式下的可选准入控制器，用于拦截沙箱 Pod（以及 BatchSandbox CR）的创建请求，自动注入沙箱配置注解。默认关闭，通过 `ENABLE_WEBHOOK` 控制。

在 `.env` 中设置：

```bash
export ENABLE_WEBHOOK=true
```

启用后，部署脚本自动完成：

| 步骤 | 操作 |
|------|------|
| 1 | 镜像推送：将本地 `e2b-webhook` 镜像推送到 Harbor |
| 2 | Helm 安装：Deployment（2 副本）、Service（443→8443）、MutatingWebhookConfiguration（pod + batchsandbox） |
| 3 | TLS 证书：自签证书（10 年有效），创建 Secret `e2b-webhook-tls` 并注入 caBundle |
| 4 | API Key：从 `/root/.e2b/config.json` 读取 `teamApiKey`，创建 Secret `e2b-api-key` |

**验证**：

```bash
kubectl get pods -n e2b -l app.kubernetes.io/name=e2b-webhook
kubectl get mutatingwebhookconfiguration e2b-webhook -o jsonpath='{.webhooks[0].clientConfig.caBundle}' | base64 -d | openssl x509 -text -noout
kubectl -n e2b get secret e2b-webhook-tls e2b-api-key
```

> **注意**：关闭时（`ENABLE_WEBHOOK=false`），部署脚本自动清理 Secret 和 MutatingWebhookConfiguration，Helm 资源由 `.Values.webhook.enabled` 同步控制。

**忘记部署怎么办**：若主流程部署时未启用或未成功部署 e2b-webhook，可后续单独部署，不影响其它组件：

```bash
# 1. 确保 .env 中 ENABLE_WEBHOOK=true
# 2. 单独部署 e2b-webhook（构建推送镜像 → 渲染 e2b-webhook.yaml → kubectl apply，
#    不经 helm install/upgrade，避免影响其它 Pod）
./deploy.sh deploy-webhook
```

命令内部会自动完成：构建并推送镜像、生成自签证书并注入 caBundle、用 `/root/.e2b/config.json` 的 `teamApiKey` 更新 `e2b-api-key` Secret、等待 Deployment 就绪。

---

## 6. 模板管理

模板是沙箱的镜像定义。创建沙箱前，必须先构建模板。

### 6.1 制作沙箱镜像

沙箱镜像需要推送到 Harbor 仓库，供后续模板构建使用。整个流程分为**可选的镜像制作**和**必需的 Harbor 上传**两部分。

**前置条件**

- Harbor 已启动并可访问（Nomad 模式端口 2900/30443，K8S 模式端口 30443）
- 已执行 `docker login` 或 containerd 已配置 Harbor 证书
- 本地或镜像仓库中已有基础镜像（如 `ubuntu:22.04`）

#### 6.1.1 方式一：脚本制作（推荐）

如果已有安装好必要组件的镜像，可跳过本步，直接进入 [6.2 上传镜像到 Harbor](#62-上传镜像到-harbor)。

沙箱镜像需包含以下组件：systemd、openssh-server、websocat、socat、curl 等。以下以 `ubuntu:22.04` 为例。

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

#### 6.1.2 方式二：手动制作

手动流程与脚本一致（保存 ENTRYPOINT/CMD → 启动临时容器 → 安装组件 → websocat → 导出恢复 → 清理），仅安装命令因发行版而异。以下给出 Ubuntu 与 openEuler 两个示例。

**Ubuntu 示例**

```bash
# 1. 保存原镜像的 Entrypoint 和 Cmd
ORIG_ENTRY=$(docker inspect ubuntu:22.04 --format='{{json .Config.Entrypoint}}')
ORIG_CMD=$(docker inspect ubuntu:22.04 --format='{{json .Config.Cmd}}')
echo "原 ENTRYPOINT: $ORIG_ENTRY"
echo "原 CMD: $ORIG_CMD"
```

```bash
# 2. 启动临时容器
docker rm -f temp-images 2>/dev/null
docker run -d --name temp-images --privileged --entrypoint tail \
    ubuntu:22.04 -f /dev/null
```

```bash
# 3. 安装必要组件
docker exec temp-images bash -c " \
    apt-get update && \
    apt-get install -y systemd systemd-sysv openssh-server sudo chrony \
    linuxptp socat curl wget iputils iproute2 netcat-openbsd tcpdump passwd && \
    apt-get clean && rm -rf /var/lib/apt/lists/* /var/tmp/* /tmp/*"
```

```bash
# 4. 安装 websocat（用于 SSH 代理连接沙箱）
docker exec temp-images bash -c ' \
    wget -O /usr/local/bin/websocat https://github.com/vi/websocat/releases/latest/download/websocat.aarch64-unknown-linux-musl && \
    chmod a+x /usr/local/bin/websocat && \
    websocat --version'
```

```bash
# 5. 导出容器并恢复原始 ENTRYPOINT/CMD
docker stop temp-images
docker export temp-images | docker import \
    --change "ENTRYPOINT $ORIG_ENTRY" \
    --change "CMD $ORIG_CMD" \
    - ubuntu:22.04-custom
```

```bash
# 6. 清理临时容器
docker rm -f temp-images
```

**openEuler 示例**

```bash
# 1. 保存原镜像的 Entrypoint 和 Cmd
ORIG_ENTRY=$(docker inspect openeuler/openeuler:24.03 --format='{{json .Config.Entrypoint}}')
ORIG_CMD=$(docker inspect openeuler/openeuler:24.03 --format='{{json .Config.Cmd}}')
echo "原 ENTRYPOINT: $ORIG_ENTRY"
echo "原 CMD: $ORIG_CMD"
```

```bash
# 2. 启动临时容器
docker rm -f temp-images 2>/dev/null
docker run -d --name temp-images --privileged --entrypoint tail \
    openeuler/openeuler:24.03 -f /dev/null
```

```bash
# 3. 安装必要组件
docker exec temp-images bash -c " \
    yum install -y systemd systemd-sysv openssh-server sudo chrony \
    linuxptp socat curl wget iputils bind-utils iproute nc tcpdump passwd && \
    yum clean all && rm -rf /var/cache/yum /var/tmp/* /tmp/*"
```

```bash
# 4. 安装 websocat（用于 SSH 代理连接沙箱）
docker exec temp-images bash -c ' \
    wget -O /usr/local/bin/websocat https://github.com/vi/websocat/releases/latest/download/websocat.aarch64-unknown-linux-musl && \
    chmod a+x /usr/local/bin/websocat && \
    websocat --version'
```

```bash
# 5. 导出容器并恢复原始 ENTRYPOINT/CMD
docker stop temp-images
docker export temp-images | docker import \
    --change "ENTRYPOINT $ORIG_ENTRY" \
    --change "CMD $ORIG_CMD" \
    - openeuler:24.03-custom
```

```bash
# 6. 清理临时容器
docker rm -f temp-images
```

> **提示**：除安装命令外，两示例其余步骤完全一致，仅替换基础镜像名与最终镜像标签即可。

#### 6.1.3 方式三：直接使用测试镜像

如果仅用于测试，可直接使用基础镜像跳过制作步骤。`--download` 阶段已拉取 `ubuntu:22.04`，可直接上传到 Harbor 使用：

```bash
# 直接使用已有的基础镜像作为沙箱镜像
docker tag ubuntu:22.04 <SERVER_IP>:30443/e2b-orchestration/ubuntu:22.04
```

> **注意**：直接使用基础镜像缺少 systemd、sshd、websocat 等组件，沙箱功能受限（无法 SSH 连接、无法使用 websocat 代理）。仅推荐测试用途。

### 6.2 上传镜像到 Harbor

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
```

> Nomad 模式使用 HTTP 端口 2900：

```bash
docker tag ubuntu:22.04-custom <SERVER_IP>:2900/e2b-orchestration/ubuntu:22.04-custom
docker push <SERVER_IP>:2900/e2b-orchestration/ubuntu:22.04-custom
```

> `--make` 脚本已自动完成登录和推送，无需手动执行。

**验证镜像已上传**

```bash
# 查看 Harbor 仓库中的镜像列表
curl -sk https://<SERVER_IP>:30443/api/v2.0/projects/e2b-orchestration/repositories \
    -u admin:"${HARBOR_PASSWORD}" -H "accept: application/json" | jq '.[].name'
```

示例输出：

```
e2b-orchestration/ubuntu:22.04-custom
e2b-orchestration/ubuntu:22.04
```

```bash
# 或通过 docker pull 验证
docker pull <SERVER_IP>:30443/e2b-orchestration/ubuntu:22.04-custom
```

### 6.3 构建模板

#### 6.3.1 快速构建（create_template.py）

`create_template.py` 默认构建别名为 `openclaw` 的模板（固定从 `<HARBOR_IP>:30443/e2b-orchestration/ubuntu:22.04-custom` 镜像构建）：

```bash
# 使用默认 IP（10.10.10.10），适用于本地快速验证
python3 create_template.py

# 指定 SERVER_IP 和 HARBOR_IP
python3 create_template.py --server-ip <SERVER_IP> --harbor-ip <SERVER_IP>
```

脚本内部流程：

1. 从 `/root/.e2b/config.json` 读取 API Token
2. 设置环境变量 `E2B_API_URL`、`E2B_API_KEY` 等
3. 调用 `Template.build()` 从 Harbor 镜像构建模板
4. 模板别名为 `openclaw`

如需修改模板别名、镜像或资源配置，直接编辑 [create_template.py](create_template.py) 中的 `Template.build()` 调用。

#### 6.3.2 自定义模板构建

**基础示例**：

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

---

## 7. 创建沙箱

沙箱支持两种创建方式：

| 方式 | 入口 | 适用模式 | 说明 |
|------|------|----------|------|
| **SDK 创建** | E2B Python SDK / `create_sandbox.py` | Nomad / K8S | 通过 API 服务创建，返回沙箱 ID，可在脚本/程序中调用 |
| **K8S 沙箱 Pod** | `kubectl apply` 沙箱 Pod | 仅 K8S | 以 Pod 形式由 K8S 原生调度，webhook 自动注入沙箱配置 |

### 7.1 方式一：SDK 创建

#### 7.1.1 前提条件

- API 服务已启动（见 [4. 部署：Nomad 模式（单机）](#4-部署nomad-模式单机) / [5. 部署：K8S 模式（生产）](#5-部署k8s-模式生产)）
- 已安装 Python SDK：`pip install e2b==2.20.0 e2b_code_interpreter==2.4.1`
- 认证信息已写入 `/root/.e2b/config.json`（部署时自动生成，见 [7.4 认证信息](#74-认证信息)）

#### 7.1.2 一键脚本

```bash
python3 create_sandbox.py --server-ip <SERVER_IP>
```

脚本内部流程：

1. 读取 `/root/.e2b/config.json` 获取认证信息
2. 设置环境变量（`E2B_API_URL`、`E2B_HTTP_SSL`、`E2B_DOMAIN`、`E2B_ACCESS_TOKEN`、`E2B_API_KEY`）
3. 调用 `Sandbox.create("openclaw")` 创建沙箱
4. 输出沙箱 ID，并执行 `whoami` 验证

#### 7.1.3 Python SDK 完整示例

```python
import os
import json
from e2b import Sandbox

# 1. 设置环境变量
os.environ["E2B_API_URL"] = "http://<SERVER_IP>:3000"
os.environ["E2B_HTTP_SSL"] = "false"
os.environ["E2B_DOMAIN"] = "e2b.app"

# 2. 读取认证信息
with open("/root/.e2b/config.json") as f:
    data = json.load(f)
os.environ["E2B_ACCESS_TOKEN"] = data["accessToken"]
os.environ["E2B_API_KEY"] = data["teamApiKey"]

# 3. 创建沙箱（模板名 openclaw）
sbx = Sandbox.create("openclaw")
print(f"沙箱 ID: {sbx.sandbox_id}")

# 4. 关闭沙箱
sbx.kill()
```

#### 7.1.4 SSH 连接沙箱

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

### 7.2 方式二：K8S 沙箱 Pod 创建

通过 K8S 自定义资源或直接创建沙箱 Pod，沙箱以 Pod 形式由 K8S 原生调度。

#### 7.2.1 前提条件

- K8S 模式部署完成，`cri-multiplex` 已部署并创建 RuntimeClass `e2b`（见 [5.7 可选组件：cri-multiplex](#57-可选组件cri-multiplex)）
- `ENABLE_WEBHOOK=true` 且 e2b-webhook 已部署（见 [5.8 可选组件：e2b-webhook](#58-可选组件e2b-webhook)）
- 目标模板已存在（如 `openclaw`）

#### 7.2.2 创建沙箱 Pod

webhook 通过 `generateName: spotbox-*` 与 `batch-sandbox.sandbox.opensandbox.io/pod-index` 标签识别沙箱 Pod（BatchSandbox CR 为可选的高层封装，最终也以带同样标识的 Pod 形式落地）。以下为沙箱 Pod 示例：

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: spotbox-example-0
  namespace: test
  generateName: spotbox-abc123-          # spotbox Pod 标识
  labels:
    batch-sandbox.sandbox.opensandbox.io/pod-index: "0"   # 必填: webhook objectSelector 依赖此 label
spec:
  restartPolicy: Never
  containers:
    - name: sandbox
      image: e2b.dev/k9tscfp28i8tjmm97c9b:6858033e-db33-40df-87ca-7734c94031d9
      env:
        - name: TEMPLATE_NAME            # 必填: webhook 据此调用 transform API
          value: "openclaw"
```

```bash
kubectl apply -f sandbox-demo.yaml
```

创建流程：

1. `kubectl` 提交沙箱 Pod，e2b-webhook 拦截 CREATE 请求
2. webhook 从 `containers[0].env` 读取 `TEMPLATE_NAME`，调用 API `POST /sandboxes/transform` 获取沙箱配置
3. webhook 将配置注入为 `e2b.dev/*` 注解，并设置 `runtimeClassName: e2b`
4. cri-multiplex 将带 `runtimeClassName: e2b` 的 Pod 调度到 E2B orchestrator，以 Firecracker 微虚拟机启动沙箱

#### 7.2.3 验证

```bash
# 查看沙箱 Pod
kubectl get pods -l batch-sandbox.sandbox.opensandbox.io/pod-index

# 查看注入的沙箱配置注解
kubectl get pod -l batch-sandbox.sandbox.opensandbox.io/pod-index \
    -o jsonpath='{.items[0].metadata.annotations}'
```

> 说明：通过 BatchSandbox CR 创建时，可用 `kubectl get batchsandbox` 查看 CR 状态。

### 7.3 环境变量说明

| 变量 | 说明 | 示例 |
|------|------|------|
| `E2B_API_URL` | E2B API 地址 | `http://<IP>:3000` |
| `E2B_HTTP_SSL` | 是否启用 SSL | `false`（本地部署） |
| `E2B_DOMAIN` | E2B 域名 | `e2b.app` |
| `E2B_ACCESS_TOKEN` | 访问令牌 | 从 `/root/.e2b/config.json` 获取 |
| `E2B_API_KEY` | 团队 API Key | 从 `/root/.e2b/config.json` 获取 |

### 7.4 认证信息

认证信息存储在 `/root/.e2b/config.json`：

```json
{
    "accessToken": "<E2B_ACCESS_TOKEN>",
    "teamApiKey": "e2b_xxxxx"
}
```

---

## 8. E2B 插件部署

将 E2B 沙箱能力集成到 OpenClaw 容器中。

### 8.1 部署插件

```bash
# Nomad 模式（Docker 容器）
./build.sh --deploy-plugin

# K8S 模式
./build.sh --deploy-plugin <pod名> <模板名> <selector> <namespace>
```

### 8.2 插件部署流程

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

### 8.3 插件配置参数

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `target` | 容器/Pod 名 | 自动获取 |
| `template` | E2B 模板名 | `base`（Nomad）/ `openclaw`（K8S） |
| `selector` | K8S 标签选择器 | `app=openclaw-deploy-for-local-exec` |
| `namespace` | K8S 命名空间 | `default` |

---

## 9. 运维操作

### 9.1 服务管理

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

### 9.2 单独部署组件

```bash
./build.sh --deploy nomad       # 重新部署 Nomad
./build.sh --deploy consul      # 重新部署 Consul
./build.sh --deploy postgres    # 重新部署 PostgreSQL
./build.sh --deploy harbor      # 重新部署 Harbor
./build.sh --deploy services    # 构建镜像并部署 E2B 服务（Nomad/K8S 任务 + 生成 Token）
```

### 9.3 单独卸载组件

```bash
./build.sh --remove nomad       # 卸载 Nomad
./build.sh --remove consul      # 卸载 Consul
./build.sh --remove harbor      # 卸载 Harbor
./build.sh --remove postgres    # 卸载 PostgreSQL
```

### 9.4 Harbor 项目管理

单独创建 Harbor 项目，无需重新部署 Harbor：

```bash
# 创建默认项目 e2b-orchestration
./build.sh --create-harbor-project ''

# 创建指定项目
./build.sh --create-harbor-project my-project
```

脚本会自动等待 Harbor 就绪、登录、然后创建项目。

### 9.5 Nomad 任务管理

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

### 9.6 修改沙箱配置

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

### 9.7 下载离线包

```bash
# x86_64
ARCH=x86 ./build.sh --download

# arm64
ARCH=arm64 ./build.sh --download
```

### 9.8 全量卸载

```bash
./build.sh --uninstall
```

---

## 10. 常见问题

### 10.1 部署脚本失败（Nomad 403 错误）

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

### 10.2 模板构建失败（连接拒绝）

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

### 10.3 Consul 启动失败

**原因**：代理干扰。

**解决**：关闭系统代理后重启。

### 10.4 API 部署失败

**原因**：PostgreSQL 连接异常。

**解决**（Nomad 模式，详见 [4.3 启动服务](#43-启动服务)；K8S 模式，详见 [5.3 Master 节点部署](#53-master-节点部署)（步骤三））：

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

### 10.5 Template 启动失败

**原因**：`.env` 配置缺失。

**解决**：确保 `.env` 中包含：

```bash
export API_NODE_POOL=api
```

### 10.6 Harbor 镜像拉取失败

根据 `HARBOR_PROTOCOL` 配置，检查对应协议。

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

### 10.7 K8S 域名解析失败

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

## 11. 命令速查

| 操作 | Nomad 模式 | K8S 模式 |
|------|-----------|----------|
| 下载组件 | `./build.sh --download` | `./build.sh --download` |
| 安装 | `./build.sh --install` | `./build.sh --k8s <node> --install` |
| 启动 | `./build.sh --start` | `./build.sh --k8s <node> --start` |
| 停止 | `./build.sh --stop` | `./build.sh --stop` |
| 部署 Worker | - | `./deploy-worker.sh worker1` |
| 制作沙箱镜像 | `./build.sh --make <image>` | `./build.sh --make <image>` |
| 上传镜像到 Harbor | `docker push <IP>:2900/...` | `docker push <IP>:30443/...` |
| 构建模板 | `python3 create_template.py` | `python3 create_template.py` |
| 创建沙箱（SDK） | `python3 create_sandbox.py --server-ip <IP>` | `python3 create_sandbox.py --server-ip <IP>` |
| 创建沙箱（K8S Pod） | - | `kubectl apply -f sandbox-demo.yaml` |
| 部署 E2B 插件 | `./build.sh --deploy-plugin` | `./build.sh --deploy-plugin <pod> <模板> <selector> <ns>` |
| 重新部署服务 | `./build.sh --deploy` | `./build.sh --deploy` |
| 单独部署组件 | `./build.sh --deploy <comp>` | `./build.sh --deploy <comp>` |
| 单独卸载组件 | `./build.sh --remove <comp>` | `./build.sh --remove <comp>` |
| Nomad 任务管理 | `./build.sh --nomad-job <op> <job>` | - |
| 修改沙箱配置 | `docker exec postgres psql ...` | `kubectl exec ...` |
| 下载离线包 | `ARCH=arm64 ./build.sh --download` | `ARCH=arm64 ./build.sh --download` |
| 全量卸载 | `./build.sh --uninstall` | `./build.sh --uninstall` |

---

## 附录 A：环境变量全览

按用途分组汇总文档中出现的全部配置项，统一在 `.env` 中设置。

**部署基础**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `SERVER_IP` | 本机 IP，必须修改 | 无（必填） |
| `DEPLOY_MODE` | 部署模式 | `nomad`（`nomad` 或 `k8s`） |
| `ARCH` | 架构（下载离线包用） | 自动探测（`x86` / `arm64`） |

**Harbor**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `HARBOR_PROTOCOL` | Harbor 协议模式 | `both`（`http` / `https` / `both`） |
| `HARBOR_HTTP_PORT` | Harbor HTTP 端口 | `2900` |
| `HARBOR_HTTPS_PORT` | Harbor HTTPS 端口 | `30443` |
| `HARBOR_USERNAME` | Harbor 登录用户名 | `admin` |
| `HARBOR_PASSWORD` | Harbor admin 密码 | 无（必填） |
| `REGISTRY_PROJECT` | 镜像仓库项目名 | `e2b-orchestration` |

**PostgreSQL**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `POSTGRES_PORT` | PostgreSQL 端口 | `5432` |
| `POSTGRES_USER` | 数据库用户 | `postgres` |
| `POSTGRES_PASSWORD` | 数据库密码 | `local` |
| `POSTGRES_DOCKER_IMAGE` | 镜像名 | `postgres` |
| `POSTGRES_CONNECTION_STRING` | 连接串 | `postgresql://postgres:local@<IP>:5432/mydatabase?sslmode=disable` |

**存储后端**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `STORAGE_PROVIDER` | 存储后端（`MooncakeBucket` 时需配置 Mooncake，见 [3.5 Mooncake 配置（可选）](#35-mooncake-配置可选)） | `Local` |

**Mooncake（仅 `STORAGE_PROVIDER=MooncakeBucket` 时需要）**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `MOONCAKE_MASTER_ADDR` | 集群 Master 地址（固定节点） | 无（必填） |
| `MOONCAKE_METADATA_SERVER` | 元数据服务地址（固定节点） | 无（必填） |
| `MOONCAKE_LOCAL_HOSTNAME` | 本地节点 IP（自动获取，无需设置） | Nomad：`$${attr.unique.network.ip-address}`；K8S：`status.hostIP` |
| `MC_TCP_BIND_ADDRESS` | 本地绑定地址（自动获取，无需设置） | 同上 |
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

**K8S 集群（k8s-deploy.sh）**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `KUBEKEY_VERSION` | KubeKey 版本 | `v3.1.10` |
| `K8S_VERSION` | K8S 版本 | `v1.32.5` |
| `CNI_PLUGINS_VERSION` | CNI 插件版本 | `v1.6.2` |
| `CLUSTER_NAME` | 集群名 | `k8s` |
| `CONFIG_FILE` | 集群配置文件路径（create/all） | - |
| `HOST_IP` | 本机 IP | 自动探测 |
| `NODE_PASSWORD` | 节点 SSH 密码（必须提供） | - |
| `KUBELET_FLAGS_FILE` | kubelet 配置文件路径（cri-multiplex 用） | `/var/lib/kubelet/kubeadm-flags.env` |
| `ENABLE_WEBHOOK` | 是否启用 e2b-webhook | `false` |
| `API_NODE_POOL` | API 节点池名（Template 启动需配置） | - |

**E2B 客户端环境变量**（SDK / 脚本中设置，见 [7.3 环境变量说明](#73-环境变量说明)）

| 变量 | 说明 | 示例 |
|------|------|------|
| `E2B_API_URL` | E2B API 地址 | `http://<IP>:3000` |
| `E2B_HTTP_SSL` | 是否启用 SSL | `false` |
| `E2B_DOMAIN` | E2B 域名 | `e2b.app` |
| `E2B_ACCESS_TOKEN` | 访问令牌 | 取自 `/root/.e2b/config.json` |
| `E2B_API_KEY` | 团队 API Key | 取自 `/root/.e2b/config.json` |

---

## 附录 B：组件端口与地址

> 端口默认值均可在 `.env` 中调整，此处为默认配置。

| 组件 | 端口 / 地址 | 模式 | 说明 |
|------|-------------|------|------|
| API Service | `3000` | 通用 | 沙箱管理 API 入口（`API_PORT`） |
| Client Proxy（edge/session） | `3002` | 通用 | 沙箱连接代理，配合 `*.e2b.app` 域名（`EDGE_PROXY_PORT`） |
| Edge API | `3001` | 通用 | 内部集群端点（`EDGE_API_PORT`，K8S Service 名 `edge-api`） |
| Template Manager / Orchestrator | `5008` | 通用 | 模板构建与沙箱生命周期管理（`TEMPLATE_MANAGER_PORT` / `ORCHESTRATOR_PORT`） |
| API gRPC | `5009` | 通用 | API gRPC 端口（`API_GRPC_PORT`） |
| Nomad | `4646` | Nomad | 调度器，Web UI（`NOMAD_PORT`） |
| Consul HTTP | `8500` | Nomad | Consul 服务发现（`CONSUL_HTTP_PORT`） |
| Consul DNS | `8600` | Nomad | dnsmasq 通过 `server=/consul/127.0.0.1#8600` 解析 consul 域 |
| Harbor HTTP | `2900` | 通用 | `HARBOR_PROTOCOL=http` 时启用（`HARBOR_HTTP_PORT`） |
| Harbor HTTPS | `30443` | 通用 | `HARBOR_PROTOCOL=https` 时启用（`HARBOR_HTTPS_PORT`） |
| PostgreSQL | `5432` | 通用 | 元数据存储（`POSTGRES_PORT`） |
| Redis | `6379` | 通用 | 缓存 / 会话存储（`REDIS_PORT`） |
| docker-reverse-proxy | `5000` | 通用 | Harbor 镜像代理（`DOCKER_REVERSE_PROXY_PORT`） |
| Loki | `3100` | 通用 | 日志聚合（`LOKI_SERVICE_PORT`） |
| OTEL Collector gRPC | `4317` | 通用 | 遥测收集（`OTEL_COLLECTOR_GRPC_PORT`） |
| ClickHouse | `9010` | 可选 | 分析存储（`CLICKHOUSE_SERVER_PORT`） |
| cri-multiplex | `/run/cri-multiplex.sock` | K8S 可选 | CRI gRPC 多路复用器 socket |
| e2b-webhook Service | `443 → 8443` | K8S 可选 | 准入控制器 Service |
| envd | `49983`（沙箱内） | 通用 | 沙箱内守护进程，供 SDK 操作沙箱 |
| Mooncake Master | `50055` | 可选 | 分布式内存语义层 Master（`MOONCAKE_MASTER_ADDR`） |
| Mooncake Metadata | `8015` | 可选 | 元数据服务（`MOONCAKE_METADATA_SERVER`） |
| dnsmasq | `5353` | 通用 | 本地 DNS（`DNS_PORT`） |

---

## 附录 C：目录结构

### C.1 源码仓库 `deploy/`

```
deploy/
├── .env                      # 环境变量配置（必须修改 SERVER_IP）
├── build.sh                  # 主部署脚本（安装/启动/停止/卸载/部署）
├── k8s-deploy.sh             # K8S 集群部署脚本（KubeKey / cri-multiplex / 域名）
├── deploy-worker.sh          # K8S Worker 节点部署脚本
├── remote-worker-setup.sh    # Worker 远程初始化脚本（deploy-worker.sh 调用）
├── check-env.sh              # 部署前环境检查脚本
├── create_sandbox.py         # 沙箱创建脚本
├── create_template.py        # 模板创建脚本
├── patch_e2b.py              # E2B SDK 兼容性补丁
├── README.md                 # 项目说明
├── USAGE.md                  # 使用文档（本文件）
├── DEPLOY_DESIGN.md          # 部署设计文档
├── test-build.sh             # build.sh 单元测试
├── test-deploy.sh            # deploy.sh 测试
├── test-deploy-worker.sh     # deploy-worker.sh 测试
├── dockerfiles/              # 扩展 Dockerfile
│   └── orchestrator-mooncake.Dockerfile
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
    ├── edge.hcl              # Edge/Client Proxy 任务
    ├── redis.hcl             # Redis 任务
    └── template-manager.hcl  # Template Manager 任务
```

### C.2 部署目标目录 `/opt/e2b-infra/`

通过 RPM 包安装（`rpm -ivh KASandbox-*.aarch64.rpm`）后落地，结构与源码 `deploy/` 基本一致，并额外包含构建产物：

```
/opt/e2b-infra/
├── .env                      # 环境变量配置（必须修改 SERVER_IP）
├── build.sh                  # 主部署脚本
├── k8s-deploy.sh             # K8S 集群部署脚本
├── deploy.sh                 # K8S 部署执行脚本（dep/deploy.sh 副本）
├── deploy-worker.sh          # Worker 节点部署脚本
├── deploy-e2b-plugin.sh      # E2B 插件部署脚本（dep/ 副本）
├── remote-worker-setup.sh    # Worker 远程初始化脚本
├── check-env.sh              # 环境检查脚本
├── create_sandbox.py         # 沙箱创建脚本
├── create_template.py        # 模板创建脚本
├── patch_e2b.py              # E2B SDK 补丁
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
├── ingress-nginx.yaml        # Ingress Controller 清单（dep/ 副本）
├── wildcard-ingress.yaml     # Ingress 规则（dep/ 副本）
├── README.md / USAGE.md / DEPLOY_DESIGN.md
├── bin/                      # 二进制与 Dockerfile（根 build.sh 打包）
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
├── dep/                      # 部署依赖脚本（原始副本，build.sh 引用）
│   ├── deploy.sh / main.py / nginx.conf / init-client.sh
│   ├── install-nomad.sh / install-consul.sh
│   ├── uninstall-nomad.sh / uninstall-consul.sh
│   ├── start-server.sh / start-client.sh / run-nomad.sh / run-consul.sh
│   ├── deploy-e2b-plugin.sh / harbor.cnf / openclaw.yaml
│   └── ingress-nginx.yaml / wildcard-ingress.yaml
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
    ├── api.hcl / edge.hcl / redis.hcl / template-manager.hcl
```

> **说明**：`dep/` 保留原始副本供 `build.sh --install` 引用；`bin/` 与 `helm/` 由 RPM 打包进入 `/opt/e2b-infra/`，不属于源码 `deploy/`。