# Cube 使用文档

> Cube 是基于 **RustVMM / KVM（Cloud Hypervisor 定制树 `cube-hypervisor`）** 的沙箱执行后端。在 KASandbox 体系中，它与 Firecracker（E2B）/ Android（StratoVirt）并列：通过 **cri-multiplex** 的 `RuntimeHandler=cube` 接入 CRI，由本机 **Cubelet** 创建 MicroVM 沙箱。  
> 本文档覆盖环境准备、CubeSandbox 部署、模板制作、本机验证、K8S 接入、运维与排障。  
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
  - [5.2.1 安装失败排查与恢复](#521-安装失败排查与恢复)
  - [5.3 验证控制面](#53-验证控制面)
- [6. 模板管理](#6-模板管理)
  - [6.1 与 E2B 的差异](#61-与-e2b-的差异摘要)
  - [6.2 自构建 OCI 镜像](#62-自构建-oci-镜像)
  - [6.3 推送到 Harbor](#63-推送到-harbor)
    - [6.3.1 角色与端口](#631-角色与端口)
    - [6.3.2 与 Cube 同机时的注意点](#632-与-cube-同机时的注意点)
    - [6.3.3 下载并部署](#633-下载并部署仅-harbor)
    - [6.3.4 推送镜像](#634-推送镜像)
    - [6.3.5 自签证书](#635-自签证书cubemastertemplatecenter-信任)
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
    - [8.4.1 能力边界](#841-能力边界)
    - [8.4.2 前置](#842-前置harbor-ca--containerd)
    - [8.4.3 安装](#843-安装cube-only)
    - [8.4.4 验收](#844-验收注入不验-running)
    - [8.4.5 回退](#845-回退)
  - [8.5 验证](#85-验证)
- [9. 创建沙箱](#9-创建沙箱)
  - [9.1 CubeSandbox / E2B 兼容 SDK](#91-cubesandbox--e2b-兼容-sdk)
  - [9.2 K8S Pod（runtimeClassName: cube）](#92-k8s-podruntimeclassname-cube)
- [10. 运维操作](#10-运维操作)
  - [10.1 服务管理](#101-服务管理)
  - [10.2 卸载 cri-multiplex](#102-卸载-cri-multiplex)
  - [10.3 Cube 控制面启停](#103-cube-控制面启停)
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
| 镜像仓库 | 常用 Harbor | **推荐** Harbor 或任意可拉 OCI；**不强制**装 E2B 编排 |
| 模板构建 | `create_template.py` / `e2b.Template.build(from_dockerfile…)` | 自构建 OCI → 推 Harbor → `cubemastercli tpl create-from-image`（**不支持** Dockerfile 直构） |
| 就绪条件 | provision + envd / `wait_for_port` | **强制** HTTP probe 2xx 后打快照 |
| CRI 入口 | cri-multiplex `handler=e2b` | cri-multiplex `handler=cube`（`-cube-enabled`） |
| 产物标识 | 模板 alias（如 `ubuntu-22-04-custom-1`） | `tpl-*` ID + 可选 alias |

Cube-only 最少：`cubesandbox` + 带 `-cube-enabled` 的 cri-multiplex；CRI 直连本机 Cubelet `:9999`，**不必**装 e2b-infra 编排。模板：OCI →（可选 Harbor）→ `tpl create-from-image`；无仓见 §6.5。

---

## 2. 快速开始

### 2.1 路径选择

| 场景 | 推荐路径 | 说明 |
|------|----------|------|
| 单机冒烟 / 压测 | [7. 本机验证](#7-本机验证cube-verify) | [5.2](#52-rpm-安装src-openeulercubesandbox) 的 RPM 落盘后跑 `install.sh`，再跑 `cube-verify/run_all.sh` |
| K8S 节点生产形态 | [8. 部署：K8S 模式](#8-部署k8s-模式) | 节点装 Cubelet + `k8s-deploy.sh cri-multiplex` |
| SDK / REST 建沙箱 | [9.1](#91-cubesandbox--e2b-兼容-sdk) | CubeAPI `:3000`，模板需 READY |

### 2.2 本机验证快速上手

主路径：OCI → Harbor（§6.3）→ `tpl create-from-image`（§6）。前提见第 4 章；装机用 [5.2](#52-rpm-安装src-openeulercubesandbox)，不要用 §5.1 上游脚本。

```bash
sudo /usr/local/services/cubetoolbox/one-click/install.sh
# 验收口径同 §5.3（勿加 || true）
ss -lntp | grep -E ':(9999|3000|3010|8089)\b' || { echo "FAIL: 控制面端口未齐"; exit 1; }
curl -sfS http://127.0.0.1:3000/health | grep -q . || { echo "FAIL: /health"; exit 1; }
systemctl is-active --quiet cube-sandbox-cubelet || { echo "FAIL: cubelet inactive"; exit 1; }
cubeopscli node list --json | grep -q '"Healthy"[[:space:]]*:[[:space:]]*true' \
  || { echo "FAIL: 无 Healthy 节点"; exit 1; }

SERVER_IP=<SERVER_IP>
HARBOR_PASSWORD=<HARBOR_PASSWORD>
# Harbor 起仓与 CA：§6.3；下面假设仓已可用
mkdir -p /tmp/cube-selfbuild-img && cat > /tmp/cube-selfbuild-img/Dockerfile <<'EOF'
FROM cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
RUN echo 'cube-selfbuild' > /etc/cube-selfbuild-marker
EOF
docker build -t "${SERVER_IP}:30443/e2b-orchestration/cube-selfbuild:v1" /tmp/cube-selfbuild-img
docker login "${SERVER_IP}:30443" -u admin -p "${HARBOR_PASSWORD}"
docker push "${SERVER_IP}:30443/e2b-orchestration/cube-selfbuild:v1"

cubemastercli tpl create-from-image \
  --image "${SERVER_IP}:30443/e2b-orchestration/cube-selfbuild:v1" \
  --writable-layer-size 1G \
  --expose-port 49999 --expose-port 49983 \
  --probe 49999 --probe-path /health \
  --registry-username admin --registry-password "${HARBOR_PASSWORD}" \
  --alias cube-selfbuild-v1
cubemastercli tpl watch --job-id <job_id>   # 等到 READY

cd /path/to/KASandbox/cri-multiplex/cube-verify && ./run_all.sh
```

### 2.3 K8S 快速上手

节点已装 CubeSandbox（或 `install-compute.sh`）且模板 READY 后：

```bash
rpm -ivh KASandbox-*.rpm   # 须带 -cube-enabled；布局见 §3.3
cd /opt/e2b-infra
export CRI_MULTIPLEX_CUBE_ENABLED=true
export CRI_MULTIPLEX_CUBELET=127.0.0.1:9999
sudo -E ./k8s-deploy.sh cri-multiplex
# kubelet 走 multiplex 时再：sudo ./k8s-deploy.sh kubelet-endpoint
kubectl get runtimeclass cube
# Pod 须 runtimeClassName: cube + 附录 C（§8.3）
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
│  cri-multiplex (KASandbox)  │◄────────────────────────│ kubelet / cube-e2e    │
│  -cube-enabled              │                         │                       │
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
| **`KASandbox`**（[src-openeuler/KASandbox](https://atomgit.com/src-openeuler/KASandbox)） | cri-multiplex、e2b-webhook、`k8s-deploy.sh` 等 | **`/opt/e2b-infra`** | **否** |
| **`e2b-infra`**（[src-openeuler/e2b-infra](https://atomgit.com/src-openeuler/e2b-infra)） | E2B 编排（可选） | 同一 `/opt/e2b-infra` | **否** |

基线上游 [CubeSandbox](https://github.com/TencentCloud/CubeSandbox) **v0.7.2**；src-openeuler RPM 另含 resolve / CRI 护栏等补丁（§5.1 在线脚本没有）。`rpm -i` 只落盘，须再跑 `install.sh` / `install-compute.sh`。

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

**方式 A**（已有 `/opt/e2b-infra` 的 `build.sh`）：

```bash
cd /opt/e2b-infra
./build.sh --download docker   # 或按包内说明拉 docker/compose
sudo ./build.sh --deploy docker
docker version && docker compose version
```

**方式 B**（静态包，Cube-only 常用）：

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

这一节装的是上游发行包，版本不固定在 0.7.2，也不含 src-openeuler 打包补丁。接 KASandbox webhook / CRI 时用 [5.2](#52-rpm-安装src-openeulercubesandbox)。

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

推荐装 [src-openeuler/CubeSandbox](https://atomgit.com/src-openeuler/CubeSandbox) 的 `cubesandbox-0.7.2`（含 resolve / CRI 护栏补丁）。EulerMaker 出正式包；本地编包把 Source/patch 放入 `~/rpmbuild/SOURCES` 后 `rpmbuild -ba cubesandbox.spec`。

```bash
sudo rpm -ivh cubesandbox-0.7.2-*.rpm
cd /usr/local/services/cubetoolbox/one-click
sudo ./install.sh
```

首次 `install.sh` 会拉 MySQL 等镜像并初始化，可能要数分钟：`journalctl -u cube-sandbox-mysql -f`。

> **同版本号（Version）升级注意**  
> 若只升高 `Release`（例如 `0.7.2-3` → `0.7.2-4`），`install.sh` 仍按版本键 `v0.7.2` 管理 guest inventory：目标目录已存在时会跳过刷新，RPM 新带入的 guest / agent **不会**自动覆盖 `/data/cubelet/root/component_versions` 下的旧树。此后建沙箱可能失败（日志中可见 `reset guest time`、`MappingPastEof` 等），易被误判为安装未成功。  
> 请勿仅执行 `rpm -U` 后直接复用旧 inventory。下列三种方式**任选其一**：  
>
> 1. 删除对应版本键目录后重跑安装（路径以本机为准）：  
>    `rm -rf /data/cubelet/root/component_versions/*/v0.7.2 && cd /usr/local/services/cubetoolbox/one-click && sudo ./install.sh`  
> 2. 卸载 RPM 后重新安装（`/data/cubelet` 业务数据是否保留见 [§10](#10-运维操作)）。  
> 3. 改用变更了 Version（会生成新版本键）的安装包。  
>
> 无论采用哪种方式，升级后验收须包含「新模板达到 READY 并可成功创建沙箱」，不能只依赖 quickcheck。另见 [§11 常见问题](#11-常见问题)。

### 5.2.1 安装失败排查与恢复

`install.sh` 卡在依赖容器、`cube-sandbox-mysql` 反复重启、或 §5.3 验收全挂时，先区分「还在拉镜像/初始化」与「已被健康检查打断留下半初始化卷」。直接重跑 **不会** 自愈半初始化卷。

| 现象 | 自查 | 处置 | 清卷 |
|------|------|------|------|
| 长时间无新日志，但容器 `health: starting` | `docker ps` / `docker logs -f <mysql容器>` | 继续等；首次拉镜像 + InnoDB 初始化可超过 1 分钟 | 否 |
| `cube-sandbox-mysql` 每几秒重启；journal 有 `waited=` / `last_status=starting` | `journalctl -u cube-sandbox-mysql \| grep wait_for_container_health` | 健康预算内未就绪被 stop，卷常已半初始化 | **是** |
| 重跑 `install.sh` 仍失败，日志停在 InnoDB / 初始化 | `docker volume ls \| grep mysql` | 必须先删容器与数据卷再装 | **是** |
| Redis / MinIO 同类卡死 | 对应 `cube-sandbox-*` unit 的 journal + `docker ps` | 同 MySQL：半初始化则清对应 volume 再 `install.sh` | 视情况 |

清卷后重装（会丢掉该依赖库数据，仅用于装机失败恢复）：

```bash
sudo systemctl stop cube-sandbox-control.target || true
# 容器名 / volume 名以本机 docker ps / volume ls 为准，常见前缀 support_cube-sandbox-
sudo docker rm -f $(sudo docker ps -aq --filter name=cube-sandbox-mysql) 2>/dev/null || true
sudo docker volume rm support_cube-sandbox-mysql-data 2>/dev/null || true
cd /usr/local/services/cubetoolbox/one-click
sudo ./install.sh
```

健康等待在 RPM 的 `mysql-postcheck.sh`（较新包约 **300s**，旧包可能仍是 80s）；改文件后重装 RPM 会被覆盖。

### 5.3 验证控制面

判据与 §2.2 相同。**不要**给验收命令加 `|| true`。

```bash
# 期望：下列端口均有监听（缺任一即失败）
ss -lntp | grep -E ':(9999|3000|3010|8089)\b' || { echo "FAIL: 控制面端口未齐"; exit 1; }

# 期望：active
systemctl is-active --quiet cube-sandbox-cubelet || { echo "FAIL: cubelet inactive"; exit 1; }

# 期望：HTTP 200 且有响应体（常见含 sandboxes 等字段；sandboxes:0 在未建沙箱时正常）
curl -sfS http://127.0.0.1:3000/health | grep -q . || { echo "FAIL: /health"; exit 1; }

# 期望：至少 1 个 Healthy=true
cubeopscli node list --json | grep -q '"Healthy"[[:space:]]*:[[:space:]]*true' \
  || { echo "FAIL: 无 Healthy 节点"; exit 1; }

# 推荐：one-click 自带冒烟（覆盖多组件，存在则应跑）
cd /usr/local/services/cubetoolbox/one-click
sudo ./smoke.sh
```

| 脚本 | 作用 |
|------|------|
| `install.sh` | 落盘 layout → MySQL/Redis/MinIO（或外部）→ systemd 起各组件 |
| `install-compute.sh` | 仅计算侧 |
| `smoke.sh` | 自带冒烟（安装后验收优先用它） |
| `down.sh` | 停服务 / 清理 |

---

## 6. 模板管理

1. `docker build` 出 OCI  
2. 推到可拉 Registry（Harbor 见 [6.3](#63-推送到-harbor)；或 §6.5 公网预置）  
3. `cubemastercli tpl create-from-image`（须 `--expose-port` / `--probe` / `--probe-path`）  
4. `READY` 后用 `tpl-*` / alias 建沙箱  

不接受 E2B 式 `from_dockerfile`。对比总表见 §1。CubeOps 须至少 1 个 **Healthy** 节点，否则卡在 `DISTRIBUTING`。

### 6.1 与 E2B 的差异（摘要）

建模板只用 `tpl create-from-image`（不是 `create_template.py`）；就绪靠 MicroVM 内 HTTP probe **2xx** 后打快照。

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

可选私有仓；无仓用 [6.5](#65-可选官方预置镜像捷径)。与 CubeSandbox 装在同一台机器时，按下面约定配端口和数据目录即可。

#### 6.3.1 角色与端口

| 用途 | 宿主口 | 说明 |
|------|--------|------|
| login / push / 建模板拉镜像 | **30443** HTTPS | 日常只用这个 |
| Harbor HTTP（可选） | **2900**（默认；旧 compose 常误绑 80） | 避开 cube-proxy `:80` |
| cube-proxy | **80** | 勿让 Harbor 再占 |

本仓 `build.sh --deploy harbor` 会把 `HARBOR_HTTP_PORT` / `HTTPS` 写入 compose；官方 installer 须手改。

#### 6.3.2 与 Cube 同机时的注意点

| 注意点 | 做法 |
|------|------|
| Harbor 数据目录 | 设 `HARBOR_DATA_DIR=/data/harbor`（**不要**写成裸 `/data`）。卸载 Harbor 会 `rm -rf` 该目录；若指向 `/data`，会连同 Cube 的 `/data/cubelet` 一并删掉 |
| `:80` | `build.sh --deploy harbor`，或把 compose 里 `80:8080` 改成 `2900:8080`；HTTPS 保持 `30443` |
| `lookup core … no such host` | 到 Harbor 安装目录执行 `docker compose down && up -d` |
| 同机再跑 KubeKey | `systemctl restart docker`（§8.1）；更稳的顺序是先装 K8S 再装 Harbor |

#### 6.3.3 下载并部署

需要推自构建 OCI 时再装。前置：`/opt/e2b-infra` 的 `build.sh` + Docker ≥ 25。

| 变量 | 推荐 |
|------|------|
| `SERVER_IP` | 本机可达 IP |
| `HARBOR_DATA_DIR` | `/data/harbor` |
| `HARBOR_HTTP_PORT` / `HARBOR_HTTPS_PORT` | `2900` / `30443` |
| `HARBOR_PASSWORD` | 强密码（admin） |
| `REGISTRY_PROJECT` | `e2b-orchestration`（可改） |

离线包放 `dep/`：x86_64 用 [harbor-offline-installer-v2.13.0.tgz](https://github.com/goharbor/harbor/releases/download/v2.13.0/harbor-offline-installer-v2.13.0.tgz)；aarch64 用 [wise2c … aarch64-v2.13.0.tgz](https://github.com/wise2c-devops/build-harbor-aarch64/releases/download/v2.13.0/harbor-offline-installer-aarch64-v2.13.0.tgz)。

```bash
cd /opt/e2b-infra
sed -i 's|^export HARBOR_DATA_DIR=.*|export HARBOR_DATA_DIR=/data/harbor|' .env
mkdir -p /data/harbor dep
./build.sh --download harbor    # 只拉 Harbor，不拉编排全家桶
./build.sh --deploy harbor

curl -sk "https://${SERVER_IP}:30443/api/v2.0/health"
ss -lntp | grep -E ':(2900|30443)\b'
docker login "${SERVER_IP}:30443" -u admin -p "${HARBOR_PASSWORD}"
```

卸载：`./build.sh --remove harbor`（只删校验通过的 `HARBOR_DATA_DIR`）。

#### 6.3.4 推送镜像

```bash
SERVER_IP=<SERVER_IP>
HARBOR_PASSWORD=<HARBOR_PASSWORD>   # 与 .env / harbor.yml 一致

# 自签仓：docker 侧常需 insecure-registries（/etc/docker/daemon.json）后再 login
docker login "${SERVER_IP}:30443" -u admin -p "${HARBOR_PASSWORD}"
docker tag cube-selfbuild:v1 "${SERVER_IP}:30443/e2b-orchestration/cube-selfbuild:v1"
docker push "${SERVER_IP}:30443/e2b-orchestration/cube-selfbuild:v1"

curl -sk -u "admin:${HARBOR_PASSWORD}" \
  "https://${SERVER_IP}:30443/api/v2.0/projects/e2b-orchestration/repositories" | jq .
```

login / push / 建模板始终走 **HTTPS `:30443`**（与 HTTP 2900 无关）。

#### 6.3.5 自签证书（CubeMaster/TemplateCenter 信任）

`docker push` 成功 ≠ CubeMaster 能拉。自签仓须把 Harbor 证并入 bundle，设 `SSL_CERT_FILE` 并重启：

```bash
SERVER_IP=<SERVER_IP>
sudo mkdir -p /etc/cube
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

> **不要**用镜像引用前加 `http://` 绕过自签。拉镜像路径仍可能先走 HTTPS，出现 `http: server gave HTTP response to HTTPS client` 或回到 `:30443` 继续 x509。§6.4 的 `--image` 用：`<SERVER_IP>:30443/<project>/...`。

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

流水线：`PULLING` → `BUILDING` → `DISTRIBUTING` → `READY`。也可 `cubemastercli tpl commit` 从运行中沙箱固化（见官方文档）。

### 6.5 （可选）官方预置镜像捷径

不想自构建时，可直接对公网预置镜像建模板（跳过 Harbor）：

```text
cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
```

境外可换 `cube-sandbox-int.tencentcloudcr.com/...`。probe 同样用 `49999` / `49983`。本机 e2e / cube-verify 默认 alias：`sandbox-code`（`CUBE_E2E_TEMPLATE` 可覆盖）。

---

## 7. 本机验证（cube-verify）

前提：控制面已起、模板 READY。

### 7.1 编译 / 安装 cri-multiplex

```bash
cd /path/to/KASandbox && ./build.sh -c    # 或 cd cri-multiplex && ./build.sh
# 或：rpm -ivh KASandbox-*.rpm && ls /opt/e2b-infra/bin/cri-multiplex
```

### 7.2 启动与生命周期冒烟

```bash
cd /path/to/KASandbox/cri-multiplex/cube-verify
# 默认模板 alias=sandbox-code；没有则：export CUBE_E2E_TEMPLATE=<READY alias|tpl-…>
./run_all.sh              # 01 启动 + 02 生命周期
./run_all.sh --keep       # 保留 multiplex
./run_all.sh --all        # 01–06（含 §7.3）
```

`run_all.sh` **不会**自动挑第一个 READY 模板（与 `run_stability.sh` 不同）。

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

CRI 与原生 CubeBench 多轮对照用 `run_stability.sh`（不进 `run_all.sh`）。会停掉现有 `cri-multiplex`/`cube-e2e` 并自拉一份，**勿与服务 kubelet 的 multiplex 同机并行**。`BENCH_DIR` 指上游 CubeSandbox 树（含 `tests/perf/cubebench.sh`）。约 1 小时/5 轮；结束看 `ALL_STABILITY_OK`。

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
| `CUBE_PROXY_NODE_IP` | `127.0.0.1` | 原生 CubeBench 节点 IP |
| `BENCH_DIR` | `/home/cube/bench` | 上游树（含 cubebench） |
| `CRI_BIN` | `/home/cube/cri/bin` | `cube-e2e` + `cri-multiplex` |
| `ROOT` | `/home/cube/bench/stability` | 输出（`summary.tsv`） |
| `CUBELET_ADDR` | sock 或 `127.0.0.1:9999` | Cubelet |
| `ENSURE_OE_XFS` | `0` | 仅特殊挂盘场景；普通机勿开 |
| `CRI_CREATE_PAIRS` | `1 20\|10 20\|20 20\|50 50` | Create 档位 |

---

## 8. 部署：K8S 模式

### 8.1 前置条件

- containerd + kubelet 可用  
- 跑沙箱的节点：cubesandbox 已 `install.sh` / `install-compute.sh`，Cubelet `:9999` 可达  
- KASandbox cri-multiplex 带 `-cube-enabled`（否则设 `CRI_MULTIPLEX_BIN`）  
- 至少一个 READY 模板；用 webhook resolve 时 CubeAPI 须对 Pod 网络可达  

KubeKey 可能替换宿主 `containerd`，装完核对版本。同机已有 Harbor/Docker 再跑 kk：`stop firewalld` 可能清掉 Docker NAT（push/30443 挂而 K8S 无报错）→ `systemctl restart docker`；更稳：**先 K8S 再 Harbor**。

### 8.2 部署 cri-multiplex（开 Cube）

```bash
cd /opt/e2b-infra
/opt/e2b-infra/bin/cri-multiplex --help | grep cube-enabled
export CRI_MULTIPLEX_CUBE_ENABLED=true
export CRI_MULTIPLEX_CUBELET=127.0.0.1:9999
sudo -E ./k8s-deploy.sh cri-multiplex
# 需要时再切 kubelet：sudo ./k8s-deploy.sh kubelet-endpoint
```

会写 systemd（含 `-cube-enabled`、`-cubelet-address`、`-hide-sandbox-label flux-sandbox.io/direct=true`）、起 sock、建 RuntimeClass `cube`（及 e2b/android）；**默认不改** kubelet endpoint。

```bash
systemctl cat cri-multiplex | grep -E 'cube-enabled|cubelet-address'
kubectl get runtimeclass cube
journalctl -u cri-multiplex -n 20 --no-pager | grep cube-enabled
```

无 K8s Pod、带 `flux-sandbox.io/direct=true` 的直连沙箱依赖 `-hide-sandbox-label`，否则会被 kubelet 当孤儿清掉。Cube-only 不必起 e2b orchestrator；e2b 不可达时须保证其它引擎 List 能成功返回（可为空），否则删 Pod 可能卡 Terminating。

### 8.3 RuntimeClass 与 Pod 注解

下面 YAML **不能**直接 Running：须从 `GET /templates/<id>` 的 `createRequest` **填齐附录 C**（含 `cube.dev/image`、command/args、rootfs 等）。[§8.4](#84-e2b-webhook可选) webhook **只**补 template-id/vcpu/ram（及有限 disk），不补 image/command/rootfs。

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
    # 另须附录 C：image、command/args、rootfs、spec_fingerprint …
spec:
  runtimeClassName: cube   # 勿省略，否则 webhook 可能默认选 e2b
  containers:
    - name: pause
      image: registry.k8s.io/pause:3.9
```

```bash
curl -sS "http://127.0.0.1:3000/templates/${TID}" | jq '.createRequest'
# 按附录 C 映射到 annotations 后 kubectl apply
```

### 8.4 e2b-webhook（可选）

Cube-only 可只装 webhook（不必起 E2B api/orchestrator）。E2B 全栈见 [e2b_user_guide §6.8](./e2b_user_guide.md#68-可选组件e2b-webhook)。

#### 8.4.1 能力边界

| 会做 | 不会做 |
|------|--------|
| 选中 cube 时注入部分 `cube.dev/*`、`e2b.dev/team-id` | 展开完整附录 C |
| 缺 template-id/vcpu/ram 且有 `CUBE_API_URL` → resolve | 返回 image/command/rootfs |
| 缺 disk-mb 时旁路 `GET /templates/{id}` | 替代手写 createRequest |
| team-id：已有 UUID > `CUBE_TEAM_ID` > 零 UUID | 转发 `team-1` 等非 UUID |

#### 8.4.2 前置（Harbor CA / containerd / CNI）

1. 集群可用：`kubectl get nodes` Ready；已有 RuntimeClass `cube`（§8.2）。
2. CubeAPI 对 **webhook Pod 网段可达**（同机常见宿主 `:3000`）。`CUBE_API_URL` 填 **Pod 内能 curl 通** 的地址（优先节点 `InternalIP`，不要想当然用仅挂在业务 veth 上的 `SERVER_IP`）。
3. Pod 网络自检（任挂一个 `e2b`/`kube-system` Pod）：
   - 节点 → PodIP:端口（kubelet http 探针依赖这条；`cni0` 丢失时 CoreDNS/webhook 会一起挂）。
   - Pod → CubeAPI（CNI `isDefaultGateway` 须为 `true`，否则 Pod 只有网段路由、`dial … network is unreachable`）。
4. 若 webhook / 业务镜像来自自签 Harbor `:30443`：
   - Harbor 自签证须带 **`CA:TRUE`**（`dep/harbor.cnf` 已含；旧证 `CA:FALSE` 时 `build.sh` 会重签）。containerd 只把 CA 证当信任根，否则即使用 `hosts.toml` 的 `ca=` 仍会 `unknown authority`。
   - 节点只走 **certs.d**：在 `/etc/containerd/certs.d/<推拉用的主机名或 IP>:30443/` 下放 `hosts.toml`（并在 containerd 配置里设 `config_path = "/etc/containerd/certs.d"`）。`SERVER_IP` 与节点 InternalIP 不一致时，两个名字各建一份目录。
   - **不要**再在 containerd 的 `config.toml` 里额外写自签仓的 TLS 段或旧的 `[registry.mirrors]`。这两套和 `config_path`/certs.d **不能并用**，叠在一起常导致拉镜像失败，严重时 containerd/CRI 起不来。改完后 `systemctl restart containerd`。
   - `build.sh` 在 k8s 模式会写 certs.d、清掉旧 mirrors，并把证同步到 nginx 实际挂载的 cert 目录。
   - 换架构重打镜像后：`crictl rmi` 旧 digest 或临时 `imagePullPolicy=Always`，避免 `IfNotPresent` 继续用坏层。
5. arm64 节点上的 webhook 镜像须为 **linux/arm64**，且基础镜像与动态链接二进制匹配（openEuler 二进制勿塞进 debian-slim，否则 `exec … no such file`）。
6. webhook 默认跑在集群网络、监听 `:8443`。若就绪探针失败，先查 CNI/`cni0`（上条），**不要**因此改成 `hostNetwork: true` 仍占用 `:8443`：同机 Harbor/openresty 常已占用宿主 `8443`，会报 `bind: address already in use`。
7. CubeMaster 拉业务镜像的信任仍走 [6.3.5](#635-自签证书cubemastertemplatecenter-信任)，与 containerd 是两条线。

#### 8.4.3 安装（Cube-only）

```bash
cd /opt/e2b-infra
# .env：ENABLE_WEBHOOK=true
# CUBE_API_URL=http://$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}'):3000
# WEBHOOK_REQUIRE_API_POOL=false（单节点默认）；可选 CUBE_TEAM_ID=<UUID>
kubectl create namespace e2b --dry-run=client -o yaml | kubectl apply -f -
./deploy.sh deploy-webhook
kubectl -n e2b rollout status deploy/e2b-webhook --timeout=180s
kubectl -n e2b get deploy e2b-webhook -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CUBE_API_URL")].value}{"\n"}'
```

Webhook 只会拦住：`runtimeClassName: cube` 的 Pod，以及带 E2B `pod-index` 标签的沙箱 Pod（普通 Pod 不进）。对 Cube：若需要向 CubeAPI 补 `cube.dev/*` 却失败，**会直接拒绝创建**（不会静默放行成缺注解、一直 ContainerCreating 的 Pod）。E2B 路径失败时仍会放行，并尽量只写入 `runtimeClassName`（与 Cube 不同）。

#### 8.4.4 验收（注入，不验 Running）

```bash
# 用已有 READY 的 alias 或 tpl-*
cat <<'EOF' | kubectl apply --dry-run=server -o yaml -f - | tee /tmp/cube-wh-dry.yaml
apiVersion: v1
kind: Pod
metadata:
  name: cube-wh-smoke
  namespace: default
spec:
  runtimeClassName: cube
  containers:
    - name: pause
      image: registry.k8s.io/pause:3.9
      env:
        - name: TEMPLATE_NAME
          value: "sandbox-code"   # 换成你的 READY alias / tpl-*
EOF

# 期望出现（有 CUBE_API_URL 且 resolve 成功时）：
grep -E 'cube.dev/(template-id|vcpu|ram-mb|disk-mb)|e2b.dev/team-id' /tmp/cube-wh-dry.yaml
```

缺 image/command/rootfs 时真 apply 仍会 ContainerCreating（§8.3）。`webhook-verify` 以 E2B 为主，不能代替本检查。

#### 8.4.5 回退

```bash
kubectl delete mutatingwebhookconfiguration e2b-webhook --ignore-not-found
kubectl -n e2b delete deploy,svc,sa e2b-webhook --ignore-not-found
kubectl -n e2b delete secret e2b-webhook-tls e2b-api-key --ignore-not-found
# 可选：sed -i 's/^export ENABLE_WEBHOOK=.*/export ENABLE_WEBHOOK=false/' .env
```

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

见 [8.3](#83-runtimeclass-与-pod-注解)。

---

## 10. 运维操作

### 10.1 服务管理

```bash
systemctl status cri-multiplex
journalctl -u cri-multiplex -f
systemctl status cube-sandbox-cubelet cube-sandbox-cubemaster cube-sandbox-cube-api
```

### 10.2 卸载 cri-multiplex

```bash
cd /opt/e2b-infra && sudo ./k8s-deploy.sh cri-multiplex-uninstall
```

恢复 kubelet endpoint、删 RuntimeClass、停 systemd。

### 10.3 Cube 控制面启停

```bash
cd /usr/local/services/cubetoolbox/one-click
sudo ./down.sh && sudo ./install.sh
```

`rpm -e cubesandbox` 不清 `/data/cubelet`。仅升高 Release、Version 不变时的升级限制见 [§5.2](#52-rpm-安装src-openeulercubesandbox)。

---

## 11. 常见问题

| 现象 | 处理 |
|------|------|
| `rpm -i cubesandbox` 后无进程 | 正常；必须再跑 `one-click/install.sh` |
| Cubelet 起不来 / reflink 错误 | 确认 `/data/cubelet` 为 XFS（见 [4.2.2](#422-datacubelet-必须是-xfs)） |
| `install.sh` 预检失败（无 KVM / 非 XFS / Docker 过旧） | 按 [4.2](#42-设置前提安装前必做) 补齐后再跑 |
| `install.sh` 卡依赖容器 / mysql 反复重启 / 重跑仍失败 | 见 [5.2.1](#521-安装失败排查与恢复)；半初始化须删 `support_cube-sandbox-mysql-data` 再装 |
| 模板一直非 READY | `tpl watch`；检查镜像拉取、probe 端口是否监听并返回 2xx、CubeOps 是否有 Healthy 节点 |
| `DISTRIBUTING` / `no healthy nodes` | `cubeopscli node list`；确认 Cubelet 已向 CubeOps `:3010` 注册且 Ready |
| Harbor HTTPS x509 / 拉镜像失败 | docker push 成功不够；给 CubeMaster/TemplateCenter 配 `SSL_CERT_FILE`（§6.3.5）。**勿指望** `http://` 镜像前缀绕过 |
| Harbor `:80` bind / HTTP 口不对 | 与 cube-proxy 抢口。确认 `.env` 的 `HARBOR_HTTP_PORT` 并用本仓 `build.sh --deploy harbor`；或手工改 compose（§6.3.1–6.3.3）。login/push 仍用 `:30443` |
| Harbor `lookup core no such host` | Harbor 目录 `docker compose down && up -d`；确认 `core` 在同一 `harbor_*` 网（§6.3.2） |
| 卸载 Harbor 误删 Cube 数据 | `HARBOR_DATA_DIR` 必须是独立目录（推荐 `/data/harbor`），不要写成裸 `/data`（§6.3.2） |
| 同机 KubeKey 后 docker/Harbor 挂 | `systemctl restart docker`（§8.1） |
| cri-multiplex 无 cube | unit 须有 `-cube-enabled`；二进制 `--help` 有该 flag；能连 Cubelet |
| `k8s-deploy.sh` 找不到 | 确认已装 KASandbox，路径为 `/opt/e2b-infra/k8s-deploy.sh` |
| Pod ContainerCreating / Create 缺字段 | 补齐附录 C；webhook 不能单独冷启动（§8.3） |
| webhook Pending / 无 resolve | `WEBHOOK_REQUIRE_API_POOL=false` + `CUBE_API_URL` 后 `deploy-webhook`；镜像 x509 见 §8.4.2 |
| webhook Running 但 Ready=0；探针超时 | 查 `ip link show cni0`；无桥则重建 `cni0`（`10.233.64.1/24`）并删 Pod 重建；见 §8.4.2 |
| webhook Ready 但 deny：`network is unreachable` | Pod 无默认路由：CNI `isDefaultGateway` 改 `true` 后重建 Pod；`CUBE_API_URL` 用 Pod 内可 curl 的地址 |
| webhook `exec … no such file` / 错架构 | arm64 重打 linux/arm64 镜像；基座与二进制同发行版；`crictl rmi` 清旧 digest |
| hostNetwork webhook `bind:8443` | 与 Harbor nginx 冲突；保持默认集群网络或换端口，勿盲开 hostNetwork |
| cube Pod 被 webhook deny | CubeAPI/模板/`CUBE_API_URL`；或手写齐 template-id+vcpu+ram |
| `crictl pull` `unknown authority` | 旧证 `CA:FALSE`：删证后 `--deploy harbor` 重签，刷新 `certs.d`（含 `SERVER_IP` 与 InternalIP） |
| cube-verify `sandbox-code` 404 | `export CUBE_E2E_TEMPLATE=<READY>` |
| pause 422（team_id 非 UUID） | 勿填 `team-1`；用 UUID / `CUBE_TEAM_ID` / 零 UUID |
| Pod 删不掉（e2b dial :5008） | Cube-only：升级含 List 空成功修复的 multiplex，或保证其它引擎 List 成功 |
| Resume 失败 | 仅本机 origin；无跨节点 CRI Resume |
| 推了 Harbor 建模板失败 | 用 `tpl create-from-image`；自签配 §6.3.5 |
| Docker 过旧 / snap | §4.2.3 |
| 同 Version 仅升 Release 后建沙箱失败 | guest inventory 按版本键跳过刷新；处理见 [§5.2](#52-rpm-安装src-openeulercubesandbox) |

引擎限制：`ExecSync` 已走 guest envd；流式 Exec/Attach 未实现；Resume 未完整回放 network/expose ports。

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
sudo /opt/e2b-infra/k8s-deploy.sh kubelet-endpoint
kubectl get runtimeclass cube
sudo /opt/e2b-infra/k8s-deploy.sh cri-multiplex-uninstall

# —— e2b-webhook（Cube-only，详见 §8.4）——
# .env: ENABLE_WEBHOOK=true WEBHOOK_REQUIRE_API_POOL=false CUBE_API_URL=http://<NODE_IP>:3000
# cd /opt/e2b-infra && ./deploy.sh deploy-webhook
```

---

## 附录 A：环境变量

| 变量 | 默认 / 示例 | 说明 |
|------|-------------|------|
| `CUBELET_ADDR` / `-cubelet-address` | `127.0.0.1:9999` | CubeboxMgr 地址 |
| `CRI_MULTIPLEX_CUBE_ENABLED` | `true`（K8S 部署时显式打开） | 写 systemd 时打开 Cube 后端 |
| `CRI_MULTIPLEX_CUBELET` | `127.0.0.1:9999` | 部署脚本传入的 Cubelet 地址 |
| `CRI_MULTIPLEX_BIN` | `/opt/e2b-infra/bin/cri-multiplex` | cri-multiplex 路径 |
| `CUBE_API_URL` | `http://127.0.0.1:3000` | 模板查询 / webhook resolve |
| `CUBE_TEAM_ID` | 未设置则用零 UUID | webhook 写入 `e2b.dev/team-id` 的备选；须为 UUID |
| `CUBE_E2E_TEMPLATE` | `sandbox-code` | cube-verify / e2e 默认模板 alias |
| `CUBE_TEMPLATE_ID` | `tpl-…` | SDK 创建沙箱 |
| `CUBE_S3_*` / `CUBE_EXTERNAL_*` | — | 外部对象存储 / DB（one-click `.env`） |

---

## 附录 B：端口与路径

| 路径 | 说明 |
|------|------|
| `/usr/local/services/cubetoolbox/` | Cube 安装根 |
| `/usr/local/services/cubetoolbox/one-click/install.sh` | RPM / 落盘后的部署入口 |
| `/data/cubelet` | Cubelet 数据（须 XFS） |
| `/data/harbor` | 推荐的 Harbor 数据目录（勿用裸 `/data` 当 `HARBOR_DATA_DIR`） |
| `:30443` | Harbor HTTPS（login / push / 建模板拉镜像） |
| `:2900` | Harbor HTTP 宿主映射（`HARBOR_HTTP_PORT`，`build.sh` 会写入 compose） |
| `/opt/e2b-infra/bin/cri-multiplex` | KASandbox 的 cri-multiplex（须支持 `-cube-enabled`） |
| `/opt/e2b-infra/k8s-deploy.sh` | K8S 节点部署 |
| `KASandbox/cri-multiplex/cube-verify/` | 本机验证脚本 |
| `CubeSandbox/docs/cri-cube-deploy.md` | CRI+Cube 部署备忘 |

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
