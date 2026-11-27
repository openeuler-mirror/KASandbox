# Cube 使用文档

> Cube 是基于 **RustVMM / KVM（Cloud Hypervisor 定制树 `cube-hypervisor`）** 的沙箱执行后端。在 KASandbox 体系中，它与 Firecracker（E2B）/ Android（StratoVirt）并列：通过 **cri-multiplex** 的 `RuntimeHandler=cube` 接入 CRI，由本机 **Cubelet** 创建 MicroVM 沙箱。  
> 本文档覆盖环境准备、CubeSandbox 部署、模板制作、本机验证、K8S / SandboxSched 接入、运维与排障。  
> E2B 编排见 [src-openeuler/e2b-infra](https://atomgit.com/src-openeuler/e2b-infra)。Android 见 [Android 沙箱用户指南](./e2b_android_user_guide.md)。

## 目录

- [1. 与 E2B 路径的关系](#1-与-e2b-路径的关系)
- [2. 快速开始](#2-快速开始)
  - [2.1 路径选择](#21-路径选择)
  - [2.2 本机验证快速上手](#22-本机验证快速上手)
  - [2.3 K8S 快速上手](#23-k8s-快速上手)
- [3. 概述](#3-概述)
  - [3.1 核心概念](#31-核心概念)
  - [3.2 架构与端口一览](#32-架构与端口一览)
  - [3.3 组件与 RPM 分工](#33-组件与-rpm-分工)
- [4. 环境准备](#4-环境准备)
  - [4.1 系统要求](#41-系统要求)
  - [4.2 设置前提（安装前必做）](#42-设置前提安装前必做)
  - [4.3 控制面依赖（MySQL / Redis / MinIO）](#43-控制面依赖mysql--redis--minio)
- [5. 部署 CubeSandbox](#5-部署-cubesandbox)
  - [5.1 上游 one-click（无本仓补丁）](#51-上游-one-click无本仓补丁)
  - [5.2 RPM 安装（src-openeuler/CubeSandbox）](#52-rpm-安装src-openeulercubesandbox)
  - [5.3 验证控制面](#53-验证控制面)
  - [5.4 openfuyao /internal 元数据](#54-openfuyao-internal-元数据)
- [6. 模板管理](#6-模板管理)
  - [6.1 与 E2B 镜像流程的差异](#61-与-e2b-镜像流程的差异)
  - [6.2 自构建 OCI 镜像](#62-自构建-oci-镜像)
  - [6.3 推送到 Harbor](#63-推送到-harbor)
  - [6.4 构建与查询模板](#64-构建与查询模板)
  - [6.5 （可选）官方预置镜像捷径](#65-可选官方预置镜像捷径)
- [7. 本机验证（cube-verify）](#7-本机验证cube-verify)
  - [7.1 编译 / 安装 cri-multiplex](#71-编译--安装-cri-multiplex)
  - [7.2 启动与生命周期冒烟](#72-启动与生命周期冒烟)
  - [7.3 压测脚本](#73-压测脚本)
- [8. 部署：K8S 模式](#8-部署k8s-模式)
  - [8.1 前置条件](#81-前置条件)
  - [8.2 部署 cri-multiplex（开 Cube）](#82-部署-cri-multiplex开-cube)
  - [8.3 RuntimeClass 与 Pod 注解](#83-runtimeclass-与-pod-注解)
  - [8.4 e2b-webhook（可选）](#84-e2b-webhook可选)
  - [8.5 验证](#85-验证)
- [9. 创建沙箱](#9-创建沙箱)
  - [9.1 CubeSandbox / E2B 兼容 SDK](#91-cubesandbox--e2b-兼容-sdk)
  - [9.2 K8S Pod（runtimeClassName: cube）](#92-k8s-podruntimeclassname-cube)
  - [9.3 SandboxSched agent（可选）](#93-sandboxsched-agent可选)
- [10. 运维操作](#10-运维操作)
  - [10.1 服务管理](#101-服务管理)
  - [10.2 卸载 cri-multiplex](#102-卸载-cri-multiplex)
  - [10.3 Cube 控制面启停](#103-cube-控制面启停)
  - [10.4 卸载 /internal 元数据](#104-卸载-internal-元数据)
- [11. 常见问题](#11-常见问题)
- [12. 命令速查](#12-命令速查)
- [附录 A：环境变量](#附录-a环境变量)
- [附录 B：端口与路径](#附录-b端口与路径)
- [附录 C：注解字段](#附录-c注解字段)

---

## 1. 与 E2B 路径的关系

| 维度 | E2B（Firecracker） | Cube（本文） |
|------|-------------------|--------------|
| Hypervisor | Firecracker | RustVMM + KVM（`cube-hypervisor`） |
| 控制面 | E2B API / orchestrator / template-manager | CubeAPI / CubeMaster / Cubelet |
| 镜像仓库 | **必须** Harbor `e2b-orchestration` | **推荐**与 E2B 同仓 Harbor；也可用任意可拉 OCI |
| 模板构建 | `create_template.py` / `e2b.Template.build(from_dockerfile…)` | 自构建 OCI → 推 Harbor → `cubemastercli tpl create-from-image`（**不支持** Dockerfile 直构） |
| 就绪条件 | provision + envd / `wait_for_port` | **强制** HTTP probe 2xx 后打快照 |
| CRI 入口 | cri-multiplex `handler=e2b` | cri-multiplex `handler=cube`（`-cube-enabled`） |
| 产物标识 | 模板 alias（如 `ubuntu-22-04-custom-1`） | `tpl-*` ID + 可选 alias |

要点：

- Cube CRI **不依赖** E2B orchestrator；直连本机 Cubelet `:9999`。
- 模板主路径与 E2B 对齐：自构建 OCI → Harbor → `tpl create-from-image`；无 Harbor 时可走官方预置镜像（§6.5）。
- 仅跑 Cube 时，**不必**装 [e2b-infra](https://atomgit.com/src-openeuler/e2b-infra) 编排栈；至少要：`cubesandbox` + 带 Cube 能力的 `KASandbox` cri-multiplex（源码 `./build.sh -c`，或确认二进制有 `-cube-enabled`）。
- 同一套 cri-multiplex 可同时注册 `e2b` / `android` / `cube` 等后端；Cube-only 节点也可以只开 `-cube-enabled`（不必起 e2b orchestrator）。

---

## 2. 快速开始

### 2.1 路径选择

| 场景 | 推荐路径 | 说明 |
|------|----------|------|
| 单机冒烟 / 压测 | [7. 本机验证](#7-本机验证cube-verify) | [5.2](#52-rpm-安装src-openeulercubesandbox) 的 RPM 落盘后跑 `install.sh`，再跑 `cube-verify/run_all.sh` |
| K8S 节点生产形态 | [8. 部署：K8S 模式](#8-部署k8s-模式) | 节点装 Cubelet + `k8s-deploy.sh cri-multiplex` |
| 无完整 K8S、只要 CRI 调度入口 | [9.3 SandboxSched](#93-sandboxsched-agent可选) | agent `RUNTIME_HANDLER=cube` |

### 2.2 本机验证快速上手

主路径与 E2B 模板流程对齐（Harbor 等见 [e2b-infra](https://atomgit.com/src-openeuler/e2b-infra)）：**自构建 OCI → 推 Harbor → `tpl create-from-image`**（详见第 6 章）。

```bash
# 0. 先做第 4 章前提：/dev/kvm、/data/cubelet=XFS、Docker ≥ 25
# 1. 安装 src-openeuler/CubeSandbox 的 RPM 后再部署（详见 5.2）
#    不要用 5.1 的上游在线脚本。
sudo /usr/local/services/cubetoolbox/one-click/install.sh

# 2. 确认控制面 + 计算节点已注册且 Healthy
ss -lntp | grep -E '9999|3000|3010'
curl -sS http://127.0.0.1:3000/health
cubeopscli node list --json   # 至少 1 个 Healthy=true

# 3. 自构建并推 Harbor（Harbor 已按 e2b-infra 文档部署；SERVER_IP / 密码自行替换）
SERVER_IP=<SERVER_IP>
HARBOR_PASSWORD=<HARBOR_PASSWORD>
mkdir -p /tmp/cube-selfbuild-img && cat > /tmp/cube-selfbuild-img/Dockerfile <<'EOF'
FROM cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
RUN echo 'cube-selfbuild' > /etc/cube-selfbuild-marker
EOF
docker build -t "${SERVER_IP}:30443/e2b-orchestration/cube-selfbuild:v1" /tmp/cube-selfbuild-img
docker login "${SERVER_IP}:30443" -u admin -p "${HARBOR_PASSWORD}"
docker push "${SERVER_IP}:30443/e2b-orchestration/cube-selfbuild:v1"

# 4. 自签 Harbor CA：CubeMaster / TemplateCenter 进程需信任证书（一次性）
#    将 harbor.crt 并入系统 CA 或设置 SSL_CERT_FILE=/etc/cube/ca-bundle-with-harbor.crt 后重启对应服务

# 5. 构建 READY 模板
cubemastercli tpl create-from-image \
  --image "${SERVER_IP}:30443/e2b-orchestration/cube-selfbuild:v1" \
  --writable-layer-size 1G \
  --expose-port 49999 --expose-port 49983 \
  --probe 49999 --probe-path /health \
  --registry-username admin --registry-password "${HARBOR_PASSWORD}" \
  --alias cube-selfbuild-v1
cubemastercli tpl watch --job-id <job_id>   # 等到 READY
cubemastercli tpl list

# 6. 编译并启动 cri-multiplex（Cube 开启）+ 生命周期冒烟
cd /path/to/KASandbox/cri-multiplex/cube-verify
./run_all.sh
```

### 2.3 K8S 快速上手

```bash
# 节点已装 CubeSandbox（控制面或 install-compute.sh）且模板 READY 后：

# 1. 安装**带 Cube 能力**的 KASandbox（含 cri-multiplex、k8s-deploy.sh）
#    新包（src-openeuler ≥1.0.25-2）平铺到 /opt/e2b-infra；旧包可能仍有 /opt/e2b-infra/standard
#    装完必须能：…/bin/cri-multiplex --help | grep cube-enabled
#    发行包若无该 flag，改用含 Cube 的包，或从源码/验证分支编译后设 CRI_MULTIPLEX_BIN
rpm -ivh KASandbox-*.rpm
cd /opt/e2b-infra
# 若只有旧布局：cd /opt/e2b-infra/standard

# 2. 部署 cri-multiplex 并切换 kubelet endpoint、创建 RuntimeClass cube
export CRI_MULTIPLEX_CUBE_ENABLED=true
export CRI_MULTIPLEX_CUBELET=127.0.0.1:9999
# export CRI_MULTIPLEX_BIN=/opt/e2b-infra/bin/cri-multiplex
sudo -E ./k8s-deploy.sh cri-multiplex

# 3. Pod：runtimeClassName: cube，并带齐附录 C 冷启动注解（见 §8.3；webhook 不能代替完整注解）
kubectl get runtimeclass cube
```

---

## 3. 概述

### 3.1 核心概念

| 概念 | 说明 |
|------|------|
| **OCI 镜像** | Docker/OCI 镜像是模板的**输入**，不能直接当 Cube 沙箱跑 |
| **模板（Template）** | CubeMaster 拉镜像 → 展开 rootfs → 起临时 MicroVM → HTTP probe 2xx → 打快照 → 分发；产物为 `tpl-*` |
| **Cubelet** | 节点侧运行时，CubeboxMgr gRPC `:9999`，真正创建/销毁 MicroVM |
| **CubeAPI / CubeMaster** | 控制面；模板 resolve、调度、元数据；API 默认 `:3000` |
| **cri-multiplex** | CRI 多路复用；`handler=cube` 时把 RunPodSandbox 等翻译成 Cubelet 调用 |
| **envd** | 沙箱内数据面（默认 `:49983`），供 SDK 执行命令 / 读写文件 / PTY |

### 3.2 架构与端口一览

```text
┌─────────────────────────────┐     CRI unix socket      ┌──────────────────────┐
│  cri-multiplex (KASandbox)  │◄────────────────────────│ kubelet / cube-e2e /  │
│  -cube-enabled              │                         │ SandboxSched agent    │
│  RuntimeClass: cube         │                         └──────────────────────┘
└─────────────┬───────────────┘
              │ CubeboxMgr gRPC :9999
              ▼
┌─────────────────────────────┐     HTTP :3000          ┌──────────────────────┐
│  Cubelet                    │◄── 模板 resolve ────────│ CubeAPI / CubeMaster │
│  /usr/local/services/       │                         │ (控制面，可同机)      │
│  cubetoolbox/               │                         └──────────────────────┘
└─────────────────────────────┘
```

| 端口 / Socket | 组件 | 说明 |
|---------------|------|------|
| `:9999` | Cubelet CubeboxMgr | cri-multiplex / 工具直连 |
| `:3000` | CubeAPI | 健康检查、模板 resolve、SDK |
| `:8089` | CubeMaster（常见） | 管理面（以实际部署为准） |
| `/run/cri-multiplex.sock` | cri-multiplex（K8S） | kubelet runtime endpoint |
| `/tmp/cube-verify/cri.sock` | cube-verify 默认 | 本机冒烟用 |
| 沙箱内 `:49983` | envd | SDK 数据面；Admin Extra 会带回 |

> Cube 沙箱网络由 Cubelet 侧 TAP 分配，**不经** cri-multiplex CNI / HostPort；IP 写回 CRI `PodSandboxStatus`。

### 3.3 组件与 RPM 分工

| RPM / 源仓 | 装什么 | 装到哪 | 是否含 Cubelet |
|------------|--------|--------|----------------|
| **`cubesandbox`**（[src-openeuler/CubeSandbox](https://atomgit.com/src-openeuler/CubeSandbox)） | Cubelet / CubeMaster / CubeAPI / shim / guest 等 | `/usr/local/services/cubetoolbox` | **是** |
| **`KASandbox`**（[src-openeuler/KASandbox](https://atomgit.com/src-openeuler/KASandbox)） | cri-multiplex、e2b-webhook、`k8s-deploy.sh` 等 | **`/opt/e2b-infra`**（平铺；`1.0.25-2` 起已去掉 `KASandbox-standard` / `…/standard`） | **否** |
| **`e2b-infra`**（[src-openeuler/e2b-infra](https://atomgit.com/src-openeuler/e2b-infra)） | E2B 编排栈（api / orchestrator / envd / …） | 同一 **`/opt/e2b-infra`**（先装 KASandbox，再装本包往里填编排二进制） | **否** |

运行时产品是上游 [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox) **v0.7.2**。`cubesandbox` RPM 来自 [src-openeuler/CubeSandbox](https://atomgit.com/src-openeuler/CubeSandbox)（本地目录 `CubeSandbox/`），在这份上游上加了 webhook resolve、CRI 归属护栏和 `/internal` 门面。第 5.1 节的官方 one-click 没有这三项。

`rpm -i cubesandbox-*.rpm` **只落盘**，不会自动起服务；装完还需跑 one-click `install.sh` / `install-compute.sh`。

---

## 4. 环境准备

装 RPM 或跑 `install.sh` **之前**先把下面三项就绪；缺任一项，`one-click/install.sh` 会在预检阶段直接退出。

### 4.1 系统要求

| 项 | 要求 |
|----|------|
| 架构 | x86_64 或 aarch64，`/dev/kvm` 可用 |
| 内存 / 磁盘 | ≥ 8 GB 内存；`/data/cubelet` ≥ 50 GB（多模板建议 ≥ 200 GB） |
| 文件系统 | **`/data/cubelet` 必须为 XFS**（reflink / CoW；ext4 会被 one-click 拒绝） |
| Docker | **≥ 25** + Compose v2；不要用 snap Docker；openEuler 自带过旧 Docker 不可用 |
| 权限 | root |

### 4.2 设置前提（安装前必做）

#### 4.2.1 KVM（`/dev/kvm`）

```bash
# 检查
ls -la /dev/kvm
# 期望类似：crw-rw---- 1 root kvm ... /dev/kvm
```

| 情况 | 做法 |
|------|------|
| 裸金属 / 已开嵌套虚拟化的云主机 | 一般已有 `/dev/kvm`；若无设备节点，确认内核模块已加载：`modprobe kvm`（x86 再按平台 `modprobe kvm_intel` 或 `kvm_amd`） |
| 普通云主机且无 `/dev/kvm`（x86_64） | 可走 CubeSandbox **PVM**（开发/评估用，非生产）：装机时 `CUBE_PVM_ENABLE=1 ./install.sh`；细节见官方 [PVM 部署](https://cubesandbox.com/guide/pvm-deploy.html) / [快速开始](https://github.com/TencentCloud/CubeSandbox) |
| aarch64 且无 KVM | 需要原生 KVM 裸金属 / 物理机，PVM 路径不适用 |

若 `lsmod` 里已有 `kvm_pvm`，安装时必须带 `CUBE_PVM_ENABLE=1`，否则后续建模板容易失败。

#### 4.2.2 `/data/cubelet` 必须是 XFS

`install.sh` 会向上查找承载 `/data/cubelet` 的文件系统；不是 `xfs` 会 `die` 退出。

```bash
# 检查（目录尚不存在时，看父路径所在 FS）
mkdir -p /data/cubelet
findmnt -no FSTYPE,TARGET /data/cubelet
# 或：df -T /data/cubelet
# 期望 FSTYPE = xfs
```

**推荐：单独一块盘格式化为 XFS 后挂到 `/data/cubelet`**（`<DISK>` 换成实际设备，如 `/dev/vdb` / `/dev/sdb1`；**会清空该盘数据**）：

```bash
# 确认盘符后再执行
lsblk
sudo mkfs.xfs -f <DISK>
sudo mkdir -p /data/cubelet
sudo mount <DISK> /data/cubelet
findmnt -no FSTYPE,SOURCE,TARGET /data/cubelet   # 应为 xfs

# 开机自动挂载（用实际 UUID）
UUID=$(blkid -s UUID -o value <DISK>)
echo "UUID=${UUID} /data/cubelet xfs defaults,nofail 0 2" | sudo tee -a /etc/fstab
sudo mount -a
```

若整机根分区已是 XFS，且空间够用，也可以直接 `mkdir -p /data/cubelet`（父路径是 XFS 即通过预检）；仍建议生产环境用独立数据盘。

#### 4.2.3 Docker ≥ 25 + Compose v2

```bash
# 检查
docker version          # Server Version 应 ≥ 25
docker compose version  # 或 docker-compose version
```

| 不要用 | 原因 |
|--------|------|
| snap Docker | 读不到 `/usr/local/services`；若已装：`sudo snap remove docker` |
| openEuler / 发行版自带过旧 Docker | 版本往往 &lt; 25，one-click 依赖会失败 |

**方式 A（与 E2B 同机、已装 e2b-infra / 带 deploy 脚本的包时）**：

```bash
cd /opt/e2b-infra              # 新包；旧布局再试 /opt/e2b-infra/standard
./build.sh --download          # 拉 docker-25.0.5.tgz 与 compose
sudo ./build.sh --deploy docker
docker version && docker compose version
```

仅跑 Cube、未装 e2b-infra 时请用方式 B，不必为装 Docker 去拉 E2B 编排包。

**方式 B（静态包，仅装 Docker；Cube-only 推荐）**：

```bash
# 架构二选一
ARCH=$(uname -m)   # x86_64 或 aarch64
curl -fsSL -o /tmp/docker.tgz \
  "https://download.docker.com/linux/static/stable/${ARCH}/docker-25.0.5.tgz"
sudo tar -xzf /tmp/docker.tgz -C /tmp
sudo cp /tmp/docker/* /usr/bin/

# Compose v2 插件或独立二进制均可；示意独立二进制：
COMPOSE_ARCH=$([ "$ARCH" = x86_64 ] && echo x86_64 || echo aarch64)
sudo curl -fsSL -o /usr/local/bin/docker-compose \
  "https://github.com/docker/compose/releases/download/v2.40.2/docker-compose-linux-${COMPOSE_ARCH}"
sudo chmod +x /usr/local/bin/docker-compose
# 兼容 `docker compose`：可选
sudo mkdir -p /usr/local/lib/docker/cli-plugins
sudo ln -sfn /usr/local/bin/docker-compose /usr/local/lib/docker/cli-plugins/docker-compose

# 若系统无 docker.service，写一份最小 unit 后启动
if [ ! -f /etc/systemd/system/docker.service ] && [ ! -f /usr/lib/systemd/system/docker.service ]; then
  sudo tee /etc/systemd/system/docker.service <<'EOF'
[Unit]
Description=Docker Application Container Engine
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/bin/dockerd
ExecReload=/bin/kill -s HUP $MAINPID
LimitNOFILE=infinity
Delegate=yes
KillMode=process
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
fi
sudo systemctl daemon-reload
sudo systemctl enable --now docker
docker info >/dev/null && echo "docker ok"
```

也可按 [Docker 官方文档](https://docs.docker.com/engine/install/) 安装 `docker-ce`（同样要求 ≥ 25，且不要用 snap）。

#### 4.2.4 一次验收

三项都通过后再装 RPM / 跑 `install.sh`：

```bash
test -e /dev/kvm && echo "kvm ok"
findmnt -no FSTYPE /data/cubelet | grep -qx xfs && echo "xfs ok"
docker version --format '{{.Server.Version}}'
docker compose version
```

### 4.3 控制面依赖（MySQL / Redis / MinIO）

| 场景 | 做法 |
|------|------|
| 单机 / 控制面 all-in-one | `install.sh` 默认用 Docker 起 MySQL、Redis、MinIO，一般无需另装 RPM |
| 关掉本机 MinIO | `CUBE_SANDBOX_MINIO_ENABLED=0` 并配置外部 `CUBE_S3_*` |
| 纯计算节点 | `install-compute.sh`，消费控制面已填好的 `CUBE_S3_*` |
| 仅 CRI 冒烟 | 仍建议完整 one-click 起控制面（建模板走 CubeMaster，解析走 CubeAPI `:3000`） |

---

## 5. 部署 CubeSandbox

### 5.1 上游 one-click（无本仓补丁）

这一节装的是上游发行包，版本不固定在 0.7.2，也不含 0001–0003。接 KASandbox webhook 或 openfuyao 时用 [5.2](#52-rpm-安装src-openeulercubesandbox)。

**x86_64（有网）：**

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
# 若走 PVM：附加 CUBE_PVM_ENABLE=1
```

**aarch64 / 离线：** 从 [Releases](https://github.com/TencentCloud/CubeSandbox/releases) 或 CNB 下载 `cube-sandbox-one-click-*-arm64.tar.gz`，解压后：

```bash
cd cube-sandbox-one-click-<version>-arm64
# 可选：cp env.example .env 后编辑外部依赖
sudo ./install.sh                 # 控制面 / 单机
# 计算节点：sudo ./install-compute.sh
```

### 5.2 RPM 安装（src-openeuler/CubeSandbox）

打包仓是 [src-openeuler/CubeSandbox](https://atomgit.com/src-openeuler/CubeSandbox)，本地目录 `CubeSandbox/`。基线是上游 **v0.7.2**，RPM 为 `cubesandbox-0.7.2`。补丁：`0001` webhook `POST /templates/resolve`、`0002` CRI 归属护栏、`0003` openfuyao 用的 9 个 `/internal` 接口。

正式包由 EulerMaker 按该仓 SPEC 构建。下面是在打包仓根目录本地调试 SPEC 的命令（需 Docker）。三个脚本必须和 spec 一起放进 SOURCES。

```bash
mkdir -p ~/rpmbuild/{SOURCES,SPECS}
cp CubeSandbox-0.7.2.tar.gz 0001-*.patch 0002-*.patch 0003-*.patch ~/rpmbuild/SOURCES/
cp scripts/install-internal-meta.sh scripts/uninstall-internal-meta.sh \
   scripts/selfverify-internal-meta.sh ~/rpmbuild/SOURCES/
cp cubesandbox.spec ~/rpmbuild/SPECS/
rpmbuild -ba ~/rpmbuild/SPECS/cubesandbox.spec

sudo rpm -ivh ~/rpmbuild/RPMS/$(uname -m)/cubesandbox-0.7.2-*.rpm

# 落盘后必须再部署
cd /usr/local/services/cubetoolbox/one-click
sudo ./install.sh
```

装完后脚本在 `/usr/local/services/cubetoolbox/scripts/`：

| 脚本 | 作用 |
|------|------|
| `install-internal-meta.sh` | 写入 `CUBE_ADMIN_TOKEN`、`CUBE_INTERNAL_META_DB` |
| `uninstall-internal-meta.sh` | 从 env 去掉上述两项；`--purge-db` 再删 catalog |
| `selfverify-internal-meta.sh` | 对已启动的 CubeAPI 做 9 接口 HTTP 冒烟 |

### 5.3 验证控制面

```bash
ss -lntp | grep -E '9999|3000|8089' || true
systemctl is-active cube-sandbox-cubelet || true
curl -sS http://127.0.0.1:3000/health || true
# one-click 自带冒烟（若存在）
# sudo ./smoke.sh
```

| 脚本 | 作用 |
|------|------|
| `install.sh` | 落盘 layout → MySQL/Redis/MinIO（或外部）→ systemd 起各组件 |
| `install-compute.sh` | 仅计算侧 |
| `smoke.sh` | 自带冒烟 |
| `down.sh` | 停服务 / 清理 |

### 5.4 openfuyao /internal 元数据

给 openfuyao 用的 9 个接口在 CubeAPI `:3000`，路径与 E2B `/internal` 同形，请求头 `X-Admin-Token`。webhook 创建仍走 `POST /templates/resolve`（补丁 0001），不在这 9 条里。

`rpm -i` 和 `install.sh` **不会**自动写 token。控制面起来之后：

```bash
# 不传 --token 时脚本会生成一个，并打印到终端。请记下。
sudo /usr/local/services/cubetoolbox/scripts/install-internal-meta.sh
sudo systemctl restart cube-sandbox-cube-api

# 确认（401 表示没带 token；带对 token 时从未 pause 的 id 应返回 paused=false）
set -a; source /etc/cube/cube-api.env; set +a
curl -sS -H "X-Admin-Token: ${CUBE_ADMIN_TOKEN}" \
  http://127.0.0.1:3000/internal/sandboxes/smoke/snapshot-state
```

| 项 | 默认 |
|----|------|
| env 文件 | `/etc/cube/cube-api.env` |
| `CUBE_ADMIN_TOKEN` | 未传入时由脚本生成 |
| `CUBE_INTERNAL_META_DB` | `/usr/local/services/cubetoolbox/CubeAPI/data/cubeapi-internal-meta.json`（权限 600） |

openfuyao 侧：`backend=cube` 时，internal API base 指本机 CubeAPI（`http://<控制面>:3000`），鉴权头与 `CUBE_ADMIN_TOKEN` 相同。Pause / Resume 用返回里的 `template_id`、`build_id`、`cube_snap_id`。接口字段见 `CubeSandbox/docs/internal-metadata-interface-design.md`。

冒烟（需要 cube-api 已加载上述 env）：

```bash
export CUBE_ADMIN_TOKEN=...    # 与 env 文件一致
/usr/local/services/cubetoolbox/scripts/selfverify-internal-meta.sh
```

---

## 6. 模板管理

推荐流程与 E2B 侧模板习惯一致（编排/Harbor 细节见 [e2b-infra](https://atomgit.com/src-openeuler/e2b-infra)）：

1. **自构建** OCI 镜像（`docker build`）
2. **推送到 Harbor**（`e2b-orchestration` 或其他项目）
3. **`cubemastercli tpl create-from-image`**（必须配置 `--expose-port` / `--probe` / `--probe-path`）
4. 等到 `READY` 后，CRI / SDK 用 `tpl-*` 或 alias 创建沙箱

CubeAPI **不接受** `from_dockerfile` 式 Template.build；须先有可拉的 OCI。

### 6.1 与 E2B 镜像流程的差异

| 步骤 | E2B（[e2b-infra](https://atomgit.com/src-openeuler/e2b-infra)） | Cube |
|------|-----|------|
| 制作镜像 | e2b-infra 的 `./build.sh --make` / 手改 Dockerfile | `docker build`（可基于 `sandbox-code` / `cubesandbox-base`） |
| 上传 | 推 Harbor `e2b-orchestration` | **同样推荐**推同一 Harbor |
| 建模板 | `create_template.py` 等 | `cubemastercli tpl create-from-image` |
| 就绪 | provision + envd / `wait_for_port` | MicroVM 内 HTTP probe **2xx** 后打快照 |

前置：CubeOps 中至少有一个 **Healthy** 计算节点（`cubeopscli node list`），否则会卡在 `DISTRIBUTING`（`no healthy nodes` / `context deadline exceeded`）。

### 6.2 自构建 OCI 镜像

最小可验证示例（基于官方 `sandbox-code`，含代码解释器 + envd，probe 口 `49999` / envd `49983`）：

```bash
mkdir -p /tmp/cube-selfbuild-img
cat > /tmp/cube-selfbuild-img/Dockerfile <<'EOF'
FROM cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
RUN echo 'cube-selfbuild' > /etc/cube-selfbuild-marker
EOF

docker build -t cube-selfbuild:v1 /tmp/cube-selfbuild-img
```

从更瘦的 base 起步（预装 envd）时：

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16
RUN apt-get update && apt-get install -y --no-install-recommends python3 \
    && rm -rf /var/lib/apt/lists/*
# CMD 为前台业务；envd 由入口脚本在后台拉起
```

仅提供自有 HTTP 服务、不用 SDK 命令/文件能力时，可不装 envd，但须把 `--probe` 指到应用自己的健康检查。更多说明见 CubeSandbox 文档《自定义模板镜像》。

### 6.3 推送到 Harbor

Harbor 按 [e2b-infra](https://atomgit.com/src-openeuler/e2b-infra) 文档部署（常见 HTTPS `:30443`）。推送命令与 E2B 相同；仅跑 Cube 时也可自建任意可拉的 OCI 仓库，不必装完整 e2b-infra。

**同机注意**：若 cube-proxy 已占用宿主 `:80`，Harbor 的 HTTP 映射不要再绑 80（可改为例如 `2900:8080`），HTTPS 仍用 `30443:8443`。compose 网络异常时（nginx/`jobservice` 报 `lookup core … no such host`），对 Harbor 目录执行 `docker compose down && docker compose up -d`，确认 `core` 等容器都在同一 `harbor_*` 网上后再推送。

```bash
SERVER_IP=<SERVER_IP>
HARBOR_PASSWORD=<HARBOR_PASSWORD>

# docker 对自签仓常需 insecure-registries（见 /etc/docker/daemon.json）后再 login
docker login "${SERVER_IP}:30443" -u admin -p "${HARBOR_PASSWORD}"
docker tag cube-selfbuild:v1 "${SERVER_IP}:30443/e2b-orchestration/cube-selfbuild:v1"
docker push "${SERVER_IP}:30443/e2b-orchestration/cube-selfbuild:v1"

# 可选：确认仓库
curl -sk -u "admin:${HARBOR_PASSWORD}" \
  "https://${SERVER_IP}:30443/api/v2.0/projects/e2b-orchestration/repositories" | jq .
```

**HTTPS 自签（必做，否则 §6.4 拉镜像会 x509）**：`docker push` 成功 ≠ CubeMaster 能拉。CubeMaster / TemplateCenter 走系统信任链，须把 Harbor 证书并入 bundle，再用 `SSL_CERT_FILE` 重启服务：

```bash
SERVER_IP=<SERVER_IP>
sudo mkdir -p /etc/cube
# 从正在服务的 Harbor 抽出证书（或使用安装目录里的 CA / server.crt）
echo | openssl s_client -connect "${SERVER_IP}:30443" -servername "${SERVER_IP}" 2>/dev/null \
  | openssl x509 -out /etc/cube/harbor.crt
sudo cp -a /etc/pki/tls/certs/ca-bundle.crt /etc/cube/ca-bundle-with-harbor.crt
echo | sudo tee -a /etc/cube/ca-bundle-with-harbor.crt >/dev/null
sudo cat /etc/cube/harbor.crt | sudo tee -a /etc/cube/ca-bundle-with-harbor.crt >/dev/null

sudo mkdir -p /etc/systemd/system/cube-sandbox-cubemaster.service.d
sudo mkdir -p /etc/systemd/system/cube-sandbox-cube-templatecenter.service.d
sudo tee /etc/systemd/system/cube-sandbox-cubemaster.service.d/harbor-ca.conf <<'EOF'
[Service]
Environment=SSL_CERT_FILE=/etc/cube/ca-bundle-with-harbor.crt
EOF
sudo cp /etc/systemd/system/cube-sandbox-cubemaster.service.d/harbor-ca.conf \
  /etc/systemd/system/cube-sandbox-cube-templatecenter.service.d/harbor-ca.conf
sudo systemctl daemon-reload
sudo systemctl restart cube-sandbox-cubemaster cube-sandbox-cube-templatecenter
```

> **不要依赖**在镜像引用前加 `http://` 前缀绕过自签。现行拉镜像路径仍可能先走 HTTPS，出现 `http: server gave HTTP response to HTTPS client` 或回退到 `:30443` 继续 x509。自签仓请按上面配置 `SSL_CERT_FILE`，§6.4 的 `--image` 使用 `https` 默认形式：`<SERVER_IP>:30443/e2b-orchestration/...`。

### 6.4 构建与查询模板

```bash
SERVER_IP=<SERVER_IP>
HARBOR_PASSWORD=<HARBOR_PASSWORD>

cubemastercli tpl create-from-image \
  --image "${SERVER_IP}:30443/e2b-orchestration/cube-selfbuild:v1" \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999 \
  --probe-path /health \
  --registry-username admin \
  --registry-password "${HARBOR_PASSWORD}" \
  --alias cube-selfbuild-v1

# 监控（create-from-image 默认会 watch；若只拿到 job_id 可单独 watch）
cubemastercli tpl watch --job-id <job_id>
cubemastercli tpl list
cubemastercli tpl info --template-id <tpl-id> --json

# HTTP 查询
curl -sS "http://127.0.0.1:3000/templates/<tpl-id>" | jq .
```

流水线：`PULLING` → `BUILDING`（OCI→ext4 + MicroVM + probe）→ `DISTRIBUTING` → `READY`。

本机实测（Harbor `cube-selfbuild:v1` → `tpl create-from-image`）可走到 `status=READY`、节点副本 `READY`。

也可从运行中沙箱固化模板：`cubemastercli tpl commit`（见官方《从沙箱制作模板》）。

### 6.5 （可选）官方预置镜像捷径

不想自构建时，可直接对公网预置镜像建模板（跳过 Harbor）：

```text
cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
```

境外可换 `cube-sandbox-int.tencentcloudcr.com/...`。probe 同样用 `49999` / `49983`。本机 e2e / cube-verify 默认 alias：`sandbox-code`（`CUBE_E2E_TEMPLATE` 可覆盖）。

---

## 7. 本机验证（cube-verify）

对应「Cube 运行时已起 → 模板 READY → cri-multiplex → CRI 生命周期」。

### 7.1 编译 / 安装 cri-multiplex

```bash
# 源码
cd /path/to/KASandbox
./build.sh -c
# 或
cd cri-multiplex && ./build.sh

# 或 RPM
rpm -ivh KASandbox-*.rpm
ls -l /opt/e2b-infra/bin/cri-multiplex
# 旧包：ls -l /opt/e2b-infra/standard/bin/cri-multiplex
```

### 7.2 启动与生命周期冒烟

```bash
cd /path/to/KASandbox/cri-multiplex/cube-verify

# 可选覆盖：
# export CUBELET_ADDR=127.0.0.1:9999
# export CUBE_API_URL=http://127.0.0.1:3000

# 默认模板 alias 是 sandbox-code。机器上没有时 02 会 404，须显式指定任一 READY 模板：
# curl -sS http://127.0.0.1:3000/templates | jq '[.[] | select(.status=="READY") | {id:.templateID // .template_id, alias:.aliases}]'
# export CUBE_E2E_TEMPLATE=cube-guide-s6-v1   # 或 tpl-…

./run_all.sh              # 冒烟：01 启动 + 02 生命周期
./run_all.sh --keep       # 结束后保留 multiplex
./run_all.sh --all        # 01–06（含压测脚本；见 §7.3）
```

> `run_all.sh` **不会**像 `run_stability.sh` 那样自动回退到「第一个 READY 模板」。缺 `sandbox-code` 时必须设 `CUBE_E2E_TEMPLATE`。

| 脚本 | 作用 |
|------|------|
| `01_start_multiplex.sh` | 编译并以 `-cube-enabled=true` 启动；默认 socket `/tmp/cube-verify/cri.sock` |
| `02_lifecycle.sh` | Create → Checkpoint → Pause → Resume → Remove |

| 动作 | 协议 |
|------|------|
| Create / Resume | CRI `RunPodSandbox`（同 uid 且本地 paused/parked → Resume，用引擎自己的 `snap-<24 hex>`，不用目录号 `csnap-`） |
| Pause / Checkpoint / GetRuntime | Admin v2 |
| Remove | 运行中：Cubelet Destroy，列表里留 NOTREADY。刚暂停：只把快照留在本机，供同 uid Resume；过几秒再 Remove 才 Destroy |

### 7.3 压测脚本

```bash
./run_all.sh --only 03    # 多轮耗时
./run_all.sh --only 04    # 并发冷启动
./run_all.sh --only 05    # 并发 pause/resume
./run_all.sh --only 06    # 密度
./run_all.sh --all        # 01–06 全量
```

CRI 与原生 CubeBench 的多轮对照不进 `run_all.sh`，单独跑 `run_stability.sh`。在 Cube 节点上以 root 执行。它会停掉已有的 `cri-multiplex` 和 `cube-e2e`，再在 `/tmp/cube-verify/cri.sock` 拉起自己的一份，所以不要和正在服务 kubelet 的那份 multiplex 同时跑。

脚本调用 `CRI_BIN` 里的 `cube-e2e`、`cri-multiplex`，以及上游源码树里的 `tests/perf/cubebench.sh`。`BENCH_DIR` 指向那棵上游树，不是 spec 打包仓。控制面（CubeAPI `:3000`、CubeMaster `:8089`、Cubelet）要已经在跑，并且至少有一个 READY 模板；优先用 alias `sandbox-code`，没有则取第一个 READY 模板。一轮 5 次大约一小时。跑完日志里有 `ALL_STABILITY_OK`。

```bash
cd /path/to/KASandbox/cri-multiplex/cube-verify
ROUNDS=5 CUBE_PROXY_NODE_IP=<节点IP> \
  BENCH_DIR=/path/to/TencentCloud/CubeSandbox \
  CRI_BIN=/path/to/cube-e2e-and-cri-multiplex \
  ROOT=/home/cube/bench/stability \
  bash run_stability.sh
```

| 变量 | 默认 | 作用 |
|------|------|------|
| `ROUNDS` | `5` | 轮数 |
| `CUBE_PROXY_NODE_IP` | `127.0.0.1` | 原生 CubeBench 用的节点 IP |
| `BENCH_DIR` | `/home/cube/bench` | 含 `tests/perf/cubebench.sh` 的上游树 |
| `CRI_BIN` | `/home/cube/cri/bin` | 放 `cube-e2e` 和 `cri-multiplex` 的目录 |
| `ROOT` | `/home/cube/bench/stability` | 输出目录；汇总在 `$ROOT/summary.tsv`，明细在 `$ROOT/out/` |
| `CUBELET_ADDR` | 有 sock 则 `unix:///data/cubelet/cubelet.sock`，否则 `127.0.0.1:9999` | cri-multiplex 连接的 Cubelet |
| `ENSURE_OE_XFS` | `0` | 设为 `1` 时按 33468 的 openEuler 盘挂上 XFS；普通机器不要开 |
| `CRI_CREATE_PAIRS` | `1 20\|10 20\|20 20\|50 50` | CRI Create 的 `c n` 档位（`\|` 分隔）；默认小规模，档位间清沙箱。旧大批量：`1 20\|10 200\|20 300\|50 500` |

每一轮依次做：CRI Create（上表默认小规模）、原生 §3.2、原生 §4.1–§4.6、原生 §3.3、CRI 密度（上限 500）。正式轮次之前会先热身一次原生 §3.2，热身不进 `summary.tsv`。

---

## 8. 部署：K8S 模式

### 8.1 前置条件

- 节点已有 containerd + kubelet（集群可用 KubeKey 等自建；**不必**为此安装 e2b-infra 编排栈）
- 本机或同机可达 Cubelet `:9999`；控制面 CubeAPI 可达（webhook resolve 时需要）
- 每台跑 Cube 沙箱的节点：已装 cubesandbox 并 `install.sh` 或 `install-compute.sh`
- 已装**带 Cube 能力**的 KASandbox cri-multiplex（`cri-multiplex --help | grep cube-enabled`）。部分仅含 E2B/Android 的发行包没有该 flag，此时 `k8s-deploy.sh` 写不出 `-cube-enabled`，须换包或自备二进制并设 `CRI_MULTIPLEX_BIN`
- 至少有一个 READY 模板（可在控制面构建后分发）

> 注意：部分 KubeKey 流程会替换宿主 `/usr/bin/containerd`；若节点原先用 Docker 静态包的 containerd，部署 K8S 前后请核对 `containerd --version` 与 docker/containerd 是否仍可用。

### 8.2 部署 cri-multiplex（开 Cube）

```bash
cd /opt/e2b-infra
# 旧包若只有 standard 树：cd /opt/e2b-infra/standard
# 确认二进制支持 Cube：
/opt/e2b-infra/bin/cri-multiplex --help | grep cube-enabled

export CRI_MULTIPLEX_CUBE_ENABLED=true
export CRI_MULTIPLEX_CUBELET=127.0.0.1:9999
# 二进制默认与脚本同树：./bin/cri-multiplex → 常为 /opt/e2b-infra/bin/cri-multiplex
# export CRI_MULTIPLEX_BIN=/path/to/cri-multiplex-with-cube

sudo -E ./k8s-deploy.sh cri-multiplex
```

脚本会：

1. 写入 `/etc/systemd/system/cri-multiplex.service`（含 `-cube-enabled` / `-cubelet-address`，以及 `-hide-sandbox-label flux-sandbox.io/direct=true`）
2. `systemctl enable --now cri-multiplex`，等待 `/run/cri-multiplex.sock`
3. 将 kubelet runtime endpoint 切到该 socket 并重启 kubelet
4. 创建 RuntimeClass：`android` / `e2b` / **`cube`**

部署后自检：

```bash
systemctl cat cri-multiplex | grep -E 'cube-enabled|cubelet-address'
kubectl get runtimeclass cube
# 期望日志含 cube-enabled: true
journalctl -u cri-multiplex -n 20 --no-pager | grep cube-enabled
```

> OpenFuyao agent 直连创建的 Cube 沙箱没有对应 K8s Pod。必须带上 `-hide-sandbox-label`，Cube 引擎才会把带 `flux-sandbox.io/direct=true` 的沙箱从 CRI List 里藏掉，否则 kubelet 会在约 1 秒内当孤儿清掉。

> 只跑 Cube 时不用起 e2b orchestrator（`:5008`）。e2b 引擎连不上时，只要别的引擎把列表成功返回（可以是空列表），`ListPodSandbox` 仍算成功。否则删除 cube Pod 会停在 Terminating。

### 8.3 RuntimeClass 与 Pod 注解

> **下面这段 YAML 不能直接冷启动到 Running。** 它只演示 `runtimeClassName` 与规格字段形状。真正创建前须从 `GET /templates/<id>` 的 `createRequest` 填齐附录 C（至少含 `cube.dev/image`、`command`/`args`、`disk-mb`、rootfs 相关键）。webhook **不会**展开整份 `createRequest`。

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: cube-sandbox-demo
  annotations:
    cube.dev/template-id: "tpl-xxxxxxxxxxxxxxxxxxxxxxxx"
    cube.dev/vcpu: "2"
    cube.dev/ram-mb: "2048"
    cube.dev/disk-mb: "1G"
    # 还须：cube.dev/image、image-storage-media、container-name、volume-name、
    # command、args、cube.master.rootfs.*、spec_fingerprint 等（附录 C）
spec:
  runtimeClassName: cube
  containers:
    - name: pause
      image: registry.k8s.io/pause:3.9
```

从模板生成注解的实用做法：

```bash
TID=tpl-xxxxxxxxxxxxxxxxxxxxxxxx
curl -sS "http://127.0.0.1:3000/templates/${TID}" | jq '.createRequest'
# 再按附录 C 映射进 Pod metadata.annotations 后 kubectl apply
```

### 8.4 e2b-webhook（可选）

e2b-webhook 随 KASandbox 提供。CubeProvider 行为：

- Pod 指定 `runtimeClassName: cube`（或策略选中 cube）时注入 `cube.dev/*`，并写入 `e2b.dev/team-id`（暂停接口要求 UUID）。不双写 `sandbox.cri/*`。
- 若缺少 `template-id` / `vcpu` / `ram-mb`，且配置了 `CUBE_API_URL`，则请求 CubeAPI `POST /templates/resolve` 补全。resolve 不返回磁盘。
- 若缺少 `disk-mb`，再 `GET /templates/{id}`，从 `createRequest.annotations` 的 `cube.master.rootfs.writable_layer_size` 取值；没有则用 volume 的 `size_limit`。须与模板可写层一致，否则冷启动会对快照 rootfs resize 失败。
- `e2b.dev/team-id`：请求里已有合法 UUID 优先，其次环境变量 `CUBE_TEAM_ID`（也须是 UUID），否则写零 UUID `00000000-0000-0000-0000-000000000000`。`team-1` 这类非 UUID 不会原样发出。

**部署**：用 Helm / `deploy.sh deploy-webhook`，Deployment 须带 `CUBE_API_URL`（指向本机或集群可达的 CubeAPI）。集群里若只有其它命名空间的临时 MWC（例如 agentenv e2e），**不会**处理 `default` 下的 cube Pod。

**冒烟口径（验注入，不验 Running）**：短 Pod 带 `runtimeClassName: cube` 与容器环境变量 `TEMPLATE_NAME=<alias 或 tpl-*>`，`kubectl apply --dry-run=server -o yaml` 后应出现 `cube.dev/template-id`、`vcpu`、`ram-mb`、`disk-mb`、`e2b.dev/team-id`。仍缺 image/command/rootfs 时 Pod 会停在 ContainerCreating，这是预期。`webhook-verify` 整套以 E2B 为主，不能代替上述 Cube 注入检查。

### 8.5 验证

```bash
systemctl status cri-multiplex
ls -l /run/cri-multiplex.sock
kubectl get runtimeclass cube
# 提交**已带齐附录 C 注解**的 cube Pod 后：
kubectl get pod -o wide
# 期望 Running；删掉后不应长期 Terminating
```

---

## 9. 创建沙箱

### 9.1 CubeSandbox / E2B 兼容 SDK

控制面 CubeAPI `:3000` 提供与 E2B 对齐的 REST 能力（以实际版本为准）。推荐官方 Python SDK：

```bash
pip install 'cubesandbox>=0.7.3'
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_API_KEY="${CUBE_API_KEY:-e2b_000000}"   # 与本地部署一致
export CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxxxxxxxxxx  # 或 READY alias
export CUBE_PROXY_NODE_IP=<节点可达 IP>               # 本机可填内网 IP，供 envd/proxy

python3 - <<'PY'
from cubesandbox import Sandbox
import os
with Sandbox.create(os.environ["CUBE_TEMPLATE_ID"], timeout=120) as sb:
    print(sb.commands.run("uname -a").stdout)
PY
```

模板需 READY，且镜像内 envd（或业务 probe 对应服务）可用。也可用 REST：`POST /sandboxes` + `X-API-Key`。详细 API 见 CubeSandbox 官方文档与 `sdk/`。

### 9.2 K8S Pod（runtimeClassName: cube）

见 [8.3](#83-runtimeclass-与-pod-注解)。适合把沙箱生命周期交给 kubelet + cri-multiplex。

### 9.3 SandboxSched agent（可选）

[SandboxSched](https://atomgit.com/openFuyao/SandboxSched) 的 `flux-sandbox-agent` 走 **E2B CRI 客户端**，设置 `RUNTIME_HANDLER=cube` 即可打到 Cube 后端（无需完整 K8S 控制面）。

```bash
# 1) cri-multiplex（cube-enabled）
cd KASandbox/cri-multiplex/cube-verify && ./01_start_multiplex.sh

# 2) 编译 agent
cd /path/to/SandboxSched
go build -o /tmp/flux-sandbox-agent ./cmd/flux-sandbox-agent

# 3) 一键 create → status → delete
bash /path/to/CubeSandbox/docs/sandboxsched-cube-smoke.sh
```

要点：

- `RUNTIME_TYPE=e2b` + `RUNTIME_HANDLER=cube` + `RUNTIME_SOCKET=unix:///tmp/cube-verify/cri.sock`
- 请求带齐 `cube.dev/*`；占位 `e2b.dev/{template,build,team}-id`（agent 校验用）
- Agent 的 `cpu` / `memory` 字段必填

---

## 10. 运维操作

### 10.1 服务管理

```bash
# cri-multiplex（K8S 节点）
systemctl status cri-multiplex
journalctl -u cri-multiplex -f

# Cube 组件（服务名以 one-click 实际 unit 为准）
systemctl status cube-sandbox-cubelet
systemctl status cube-sandbox-cubemaster
systemctl status cube-sandbox-cube-api
```

### 10.2 卸载 cri-multiplex

```bash
cd /opt/e2b-infra
# 旧包：cd /opt/e2b-infra/standard
sudo ./k8s-deploy.sh cri-multiplex-uninstall
```

会恢复 kubelet endpoint、删除 RuntimeClass（android/e2b/cube）、停止并清理 systemd 服务。

### 10.3 Cube 控制面启停

```bash
cd /usr/local/services/cubetoolbox/one-click
sudo ./down.sh          # 停止 / 清理（按脚本说明）
sudo ./install.sh       # 重新部署
```

### 10.4 卸载 /internal 元数据

只卸 openfuyao 用的门面配置，不卸 Cube 本身：

```bash
# 去掉 env 中的 token 与 catalog 路径
sudo /usr/local/services/cubetoolbox/scripts/uninstall-internal-meta.sh

# 同时删除 catalog 文件
sudo /usr/local/services/cubetoolbox/scripts/uninstall-internal-meta.sh --purge-db

sudo systemctl restart cube-sandbox-cube-api
```

`rpm -e cubesandbox` 会去掉 `/usr/bin` 下的 CLI 软链，**不会**删除 `/etc/cube/cube-api.env`、catalog，也不会清 `/data/cubelet`。要删 catalog 时先跑带 `--purge-db` 的卸载脚本。

---

## 11. 常见问题

| 现象 | 处理 |
|------|------|
| `rpm -i cubesandbox` 后无进程 | 正常；必须再跑 `one-click/install.sh` |
| Cubelet 起不来 / reflink 错误 | 确认 `/data/cubelet` 为 XFS（见 [4.2.2](#422-datacubelet-必须是-xfs)） |
| `install.sh` 预检失败（无 KVM / 非 XFS / Docker 过旧） | 按 [4.2](#42-设置前提安装前必做) 补齐后再跑 |
| 模板一直非 READY | `tpl watch`；检查镜像拉取、probe 端口是否监听并返回 2xx、CubeOps 是否有 Healthy 节点 |
| `DISTRIBUTING` / `no healthy nodes` | `cubeopscli node list`；确认 Cubelet 已向 CubeOps `:3010` 注册且 Ready |
| Harbor HTTPS x509 / 拉镜像失败 | docker push 成功不够；给 CubeMaster/TemplateCenter 配 `SSL_CERT_FILE`（含 Harbor 证书）并重启（§6.3）。**勿指望** `http://` 镜像前缀绕过 |
| Harbor 入口起不来 / `lookup core no such host` | compose 网络丢了 `core` 等别名；`docker compose down && up -d`，确认容器同网；`:80` 被 cube-proxy 占用时改 HTTP 映射（如 2900） |
| cri-multiplex 无 cube | `systemctl cat cri-multiplex` 须有 `-cube-enabled=true`；二进制 `--help` 须支持该 flag；并能连 `CUBELET_ADDR`。发行包无 Cube 时换包或自备二进制 |
| `k8s-deploy.sh: No such file` / `rc=127` | 新包在 `/opt/e2b-infra/`；仅旧包才有 `/opt/e2b-infra/standard/`。先 `ls /opt/e2b-infra/k8s-deploy.sh` |
| Create 缺字段失败 / Pod 一直 ContainerCreating | 补齐附录 C（含 image/command/rootfs）；webhook 只补 id/vcpu/ram/disk/team-id，**不能**单靠 webhook 冷启动；节点 Ready ≠ 注解已齐 |
| cube-verify 02：`template not found` / sandbox-code 404 | 设 `CUBE_E2E_TEMPLATE` 为任一 READY alias 或 `tpl-…`（`run_all` 不会自动回退） |
| pause 422，`team_id` 不是 UUID | 不要填 `team-1`。webhook 写 `e2b.dev/team-id`：已有 UUID 优先，否则 `CUBE_TEAM_ID`，否则零 UUID |
| cube Pod 删不掉 / `all engines failed`（e2b dial :5008） | Cube-only 下 e2b 不可达时的 List 聚合问题；升级含修复的 cri-multiplex，或保证过滤 List 时至少有一个引擎成功返回（可为空） |
| Resume 失败 | CRI Resume 仅本机 origin；跨节点恢复不在当前 CRI 范围 |
| Harbor 推了镜像但建模板失败 | 不用 E2B 的 `create_template.py`；用 `tpl create-from-image`；自签先配 §6.3 的 `SSL_CERT_FILE` |
| 只要 Cube、不要 E2B 全家桶 | **不必**装 [e2b-infra](https://atomgit.com/src-openeuler/e2b-infra)；`KASandbox` + `cubesandbox` 即可（无 Harbor 时走 §6.5 预置镜像） |
| Docker 过旧 / snap | 见 [4.2.3](#423-docker--25--compose-v2)：卸 snap，装 ≥ 25 |
| `/internal` 返回 401 | env 里要有 `CUBE_ADMIN_TOKEN`，请求头 `X-Admin-Token` 与之一致，并已重启 cube-api |
| `last-snapshot` 404 | 先 `pause-snapshot`，再 `POST /internal/builds/{build_id}/status` 且 `status=success` |
| 删模板 409 | catalog 里仍有 snapshot 引用；先删 snapshot / snapshot-template。此接口不删 CubeMaster 上的真实 `tpl-*` |
| `rpm -e` 后 token 还在 | 预期行为；用 `uninstall-internal-meta.sh --purge-db` 清 catalog |

已知引擎限制（摘要）：CRI `ExecSync` 已走 guest envd（与 SDK 直连同口径，返回 stdout/stderr/exit）；流式 `Exec`/`Attach` 仍未实现。Resume 未完整回放 network / expose ports。暂停、删除和注解见 [7.2](#72-启动与生命周期冒烟)、[8.3](#83-runtimeclass-与-pod-注解)。

---

## 12. 命令速查

```bash
# —— CubeSandbox ——
sudo /usr/local/services/cubetoolbox/one-click/install.sh
curl -sS http://127.0.0.1:3000/health
cubeopscli node list --json

# 自构建 → Harbor → 模板（主路径）
docker build -t <SERVER_IP>:30443/e2b-orchestration/cube-selfbuild:v1 /path/to/Dockerfile.dir
docker login <SERVER_IP>:30443 -u admin -p "${HARBOR_PASSWORD}"
docker push <SERVER_IP>:30443/e2b-orchestration/cube-selfbuild:v1
cubemastercli tpl create-from-image \
  --image <SERVER_IP>:30443/e2b-orchestration/cube-selfbuild:v1 \
  --writable-layer-size 1G \
  --expose-port 49999 --expose-port 49983 \
  --probe 49999 --probe-path /health \
  --registry-username admin --registry-password "${HARBOR_PASSWORD}" \
  --alias cube-selfbuild-v1
cubemastercli tpl watch --job-id <id>
cubemastercli tpl list
cubemastercli tpl info --template-id <tpl-id> --json

# —— 本机 CRI 冒烟 ——
cd KASandbox/cri-multiplex/cube-verify && ./run_all.sh

# —— CRI 与原生多轮对照（约 1 小时，不进 run_all.sh）——
cd KASandbox/cri-multiplex/cube-verify
ROUNDS=5 CUBE_PROXY_NODE_IP=<节点IP> \
  BENCH_DIR=/path/to/TencentCloud/CubeSandbox \
  bash run_stability.sh

# —— K8S ——
export CRI_MULTIPLEX_CUBE_ENABLED=true
export CRI_MULTIPLEX_CUBELET=127.0.0.1:9999
sudo -E /opt/e2b-infra/k8s-deploy.sh cri-multiplex
kubectl get runtimeclass cube
sudo /opt/e2b-infra/k8s-deploy.sh cri-multiplex-uninstall

# —— SandboxSched ——
bash CubeSandbox/docs/sandboxsched-cube-smoke.sh

# —— openfuyao /internal ——
sudo /usr/local/services/cubetoolbox/scripts/install-internal-meta.sh
sudo systemctl restart cube-sandbox-cube-api
export CUBE_ADMIN_TOKEN=...
/usr/local/services/cubetoolbox/scripts/selfverify-internal-meta.sh
sudo /usr/local/services/cubetoolbox/scripts/uninstall-internal-meta.sh --purge-db
```

---

## 附录 A：环境变量

| 变量 | 默认 / 示例 | 说明 |
|------|-------------|------|
| `CUBELET_ADDR` / `-cubelet-address` | `127.0.0.1:9999` | CubeboxMgr 地址 |
| `CRI_MULTIPLEX_CUBE_ENABLED` | `true`（K8S 部署时显式打开） | 写 systemd 时打开 Cube 后端 |
| `CRI_MULTIPLEX_CUBELET` | `127.0.0.1:9999` | 部署脚本传入的 Cubelet 地址 |
| `CRI_MULTIPLEX_BIN` | `/opt/e2b-infra/bin/cri-multiplex`（旧包可能是 `…/standard/bin/…`） | cri-multiplex 路径 |
| `CUBE_API_URL` | `http://127.0.0.1:3000` | 模板查询 / webhook resolve；openfuyao `backend=cube` 时 internal 基址也指这里 |
| `CUBE_TEAM_ID` | 未设置则用零 UUID | webhook 写入 `e2b.dev/team-id` 的备选；须为 UUID |
| `CUBE_ADMIN_TOKEN` | 安装脚本生成 | `/internal` 的 `X-Admin-Token`，写在 `/etc/cube/cube-api.env` |
| `CUBE_INTERNAL_META_DB` | `.../CubeAPI/data/cubeapi-internal-meta.json` | `/internal` catalog 文件 |
| `CUBE_E2E_TEMPLATE` | `sandbox-code` | cube-verify / e2e 默认模板 alias |
| `CUBE_TEMPLATE_ID` | `tpl-…` | SDK 创建沙箱 |
| `CUBE_S3_*` / `CUBE_EXTERNAL_*` | — | 外部对象存储 / DB（one-click `.env`） |
| `RUNTIME_HANDLER` | `cube` | SandboxSched → Cube |
| `RUNTIME_SOCKET` | `unix:///tmp/cube-verify/cri.sock` | agent CRI socket |

---

## 附录 B：端口与路径

| 路径 | 说明 |
|------|------|
| `/usr/local/services/cubetoolbox/` | Cube 安装根 |
| `/usr/local/services/cubetoolbox/one-click/install.sh` | RPM / 落盘后的部署入口 |
| `/usr/local/services/cubetoolbox/scripts/install-internal-meta.sh` | 写入 openfuyao `/internal` env |
| `/usr/local/services/cubetoolbox/scripts/uninstall-internal-meta.sh` | 清除 env；`--purge-db` 删 catalog |
| `/etc/cube/cube-api.env` | `CUBE_ADMIN_TOKEN` / `CUBE_INTERNAL_META_DB` |
| `/data/cubelet` | Cubelet 数据（须 XFS） |
| `/opt/e2b-infra/bin/cri-multiplex` | KASandbox 的 cri-multiplex（须支持 `-cube-enabled`；旧包在 `…/standard/bin/`） |
| `/opt/e2b-infra/k8s-deploy.sh` | K8S 节点部署（旧包在 `…/standard/`） |
| `KASandbox/cri-multiplex/cube-verify/` | 本机验证脚本 |
| `CubeSandbox/docs/cri-cube-deploy.md` | CRI+Cube 部署备忘 |
| `CubeSandbox/docs/sandboxsched-cube-smoke.sh` | SandboxSched→Cube 冒烟 |

---

## 附录 C：注解字段

冷启动常见注解（由 `GET /templates/<id>` 的 `createRequest` 派生；引擎 Create 固定带 `appsnapshot.version=v2`）：

| 注解 | 说明 |
|------|------|
| `cube.dev/template-id` | 模板 ID（`tpl-…`） |
| `cube.dev/vcpu` | vCPU |
| `cube.dev/ram-mb` | 内存 MB |
| `cube.dev/disk-mb` | 可写层大小。webhook 缺省时从模板 `writable_layer_size` 或 volume `size_limit` 补齐，须与模板一致 |
| `cube.dev/image` | 镜像引用 |
| `cube.dev/image-storage-media` | 存储介质字段 |
| `cube.dev/container-name` | 容器名 |
| `cube.dev/volume-name` | 卷名 |
| `cube.dev/command` / `cube.dev/args` | JSON 数组字符串 |
| `cube.master.rootfs.*` | rootfs / writable layer 等 |
| `cube.master.template.spec_fingerprint` | 规格指纹 |
| `cube.master.appsnapshot.version` | 常用 `v2` |
| `sandbox.cri/backend` | CubeProvider 不注入此注解 |

`e2b.dev/template-id`、`e2b.dev/build-id` 仍是 agent 占位。`e2b.dev/team-id` 由 webhook 写成 UUID，供 pause 使用，不是随意占位。路由仍靠 `RuntimeHandler=cube` 与 `cube.dev/*`。
