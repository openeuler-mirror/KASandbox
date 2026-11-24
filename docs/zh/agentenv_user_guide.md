# AgentENV 使用与部署指南

AgentENV 通过 KASandbox 的 cri-multiplex 接入 CRI。AgentENV Runtime 负责创建虚拟机和管理 Sandbox 网络，业务命令和文件操作通过 AgentENV 的 E2B 兼容 SDK 执行。本文覆盖环境准备、Runtime 部署、模板管理、本机验证、Kubernetes 接入和回退。

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
  - [4.3 部署与验证工具](#43-部署与验证工具)
- [5. 部署 AgentENV](#5-部署-agentenv)
  - [5.1 补丁包安装](#51-补丁包安装)
  - [5.2 RPM 安装（src-openeuler/AgentENV）](#52-rpm-安装src-openeuleragentenv)
  - [5.3 验证 Runtime](#53-验证-runtime)
  - [5.4 OpenFuyao /internal 元数据（可选）](#54-openfuyao-internal-元数据可选)
- [6. 模板管理](#6-模板管理)
  - [6.1 与 E2B 模板流程的差异](#61-与-e2b-模板流程的差异)
  - [6.2 从 OCI 镜像构建与查询](#62-从-oci-镜像构建与查询)
  - [6.3 使用已有模板](#63-使用已有模板)
- [7. 本机验证（agentenv-verify）](#7-本机验证agentenv-verify)
  - [7.1 编译或安装 cri-multiplex](#71-编译或安装-cri-multiplex)
  - [7.2 启动与生命周期冒烟](#72-启动与生命周期冒烟)
  - [7.3 自动化验收](#73-自动化验收)
- [8. 部署：K8S 模式](#8-部署k8s-模式)
  - [8.1 前置条件](#81-前置条件)
  - [8.2 部署 cri-multiplex（启用 AgentENV）](#82-部署-cri-multiplex启用-agentenv)
  - [8.3 RuntimeClass 与 Pod 注解](#83-runtimeclass-与-pod-注解)
  - [8.4 统一 e2b-webhook（可选）](#84-统一-e2b-webhook可选)
  - [8.5 验证](#85-验证)
- [9. 创建沙箱](#9-创建沙箱)
  - [9.1 E2B 兼容 SDK](#91-e2b-兼容-sdk)
  - [9.2 通过 K8S Pod 创建和删除](#92-通过-k8s-pod-创建和删除)
  - [9.3 OpenFuyao 接入验证（可选）](#93-openfuyao-接入验证可选)
- [10. 运维操作](#10-运维操作)
  - [10.1 服务管理](#101-服务管理)
  - [10.2 下线 AgentENV CRI 接入](#102-下线-agentenv-cri-接入)
  - [10.3 回退或卸载 AgentENV Runtime](#103-回退或卸载-agentenv-runtime)
  - [10.4 停用 Webhook 和内部 API 接入](#104-停用-webhook-和内部-api-接入)
- [11. 常见问题](#11-常见问题)
- [12. 命令速查](#12-命令速查)
- [附录 A：环境变量](#附录-a环境变量)
- [附录 B：端口与路径](#附录-b端口与路径)
- [附录 C：注解字段](#附录-c注解字段)

---

## 1. 与 E2B 路径的关系

| 维度 | E2B | AgentENV |
| --- | --- | --- |
| CRI 入口 | cri-multiplex，handler 为 `e2b` | 共用 cri-multiplex，handler 为 `agentenv` |
| Sandbox 生命周期 | E2B orchestrator | 节点上的 AgentENV Runtime |
| 模板 | 由 E2B 模板构建流程生成 | 由 AgentENV 构建，使用模板 ID 或别名 |
| 网络 | 由 E2B 后端和对应 CNI 配置管理 | 由 AgentENV Runtime 管理 |
| 业务访问 | E2B SDK | E2B 兼容 SDK，通过 AgentENV HTTP API 和 proxy |

AgentENV 与 E2B、CubeSandbox 可共享 cri-multiplex。各后端有自己的 Runtime 和模板。其他部署流程见 [E2B 使用文档](./e2b_user_guide.md)和 [Cube 使用文档](./cube_user_guide.md)。

---

## 2. 快速开始

### 2.1 路径选择

| 场景 | 操作 |
| --- | --- |
| 单节点创建和删除 Sandbox | [部署 Runtime](#5-部署-agentenv)、[准备模板](#6-模板管理)，然后做[本机验证](#7-本机验证agentenv-verify) |
| 使用 kubelet 调度 AgentENV Pod | 完成节点 Runtime 和模板准备后，[部署 Kubernetes 接入](#8-部署k8s-模式)并[创建测试 Pod](#92-通过-k8s-pod-创建和删除) |
| 验证 OpenFuyao 接口 | [运行调用方打桩测试](#93-openfuyao-接入验证可选) |

### 2.2 本机验证快速上手

1. 完成[环境检查](#4-环境准备)，安装配套的 [AgentENV CRI 补丁包](#51-补丁包安装)或 [Runtime RPM](#52-rpm-安装src-openeuleragentenv)。
2. 用 `aenv template list` 选择可用模板，或按第 6 章创建模板。
3. 启动 cri-multiplex，通过 `crictl` 直连创建、查询和删除 Sandbox。命令见[第 7 章](#7-本机验证agentenv-verify)。

### 2.3 K8S 快速上手

1. 在 AgentENV 节点启用 `agentenv` handler，确认 kubelet 指向 cri-multiplex。
2. 创建 `agentenv` RuntimeClass。需要 Webhook 准入时，启用统一 `e2b-webhook` 的 AgentENV provider。配置见[第 8 章](#8-部署k8s-模式)。
3. 使用 `runtimeClassName: agentenv`，在 `sandbox.cri/template` 填写模板 ID 或别名。[创建测试 Pod](#92-通过-k8s-pod-创建和删除)，等待 Ready 后删除。

---

## 3. 概述

### 3.1 核心概念

| 概念 | 说明 |
| --- | --- |
| Template | 创建 Sandbox 所用的 AgentENV 模板，提供根文件系统及运行环境 |
| CRI Sandbox ID | kubelet 和 cri-multiplex 使用的标识，新建实例取 Pod UID |
| AgentENV 原生 Sandbox ID | Runtime 分配的 UUID，用于连接 AgentENV SDK |
| CRI Container | Pod 容器在 CRI 中的状态记录，业务程序由模板和 SDK 管理 |

### 3.2 架构与端口一览

```text
Kubernetes API Server → 统一 e2b-webhook → AgentENV HTTP API（准入和模板查询）
kubelet / crictl      → cri-multiplex   → AgentENV Runtime → Sandbox
AgentENV SDK          → HTTP API / proxy                 → Sandbox
```

| 入口 | 用途 |
| --- | --- |
| `/run/cri-multiplex.sock` | kubelet 和 crictl 的 CRI socket |
| `/run/cri-multiplex/admin.sock` | 节点本机 Admin v2 接口，用于查询运行状态、暂停及检查点 |
| `/run/aenv/agentenv-runtime.sock` | cri-multiplex 调用 AgentENV Runtime |
| `http://127.0.0.1:8000` | AgentENV HTTP 健康检查、模板接口与 SDK proxy |

AgentENV 使用 Runtime 原生网络，AgentENV Pod 无需配置 CNI 或 HostPort。

### 3.3 组件与 RPM 分工

| 制品 | 安装内容 |
| --- | --- |
| AgentENV CRI 补丁包或 `agentenv-cri-runtime` RPM | 在已有 AgentENV v0.2.4 安装上启用 Runtime 接口，沿用 `aenv.service` |
| `KASandbox` RPM | cri-multiplex、统一 `e2b-webhook` 的程序和部署配置 |

---

## 4. 环境准备

### 4.1 系统要求

- Linux 节点具备可用的 KVM 和 ublk 设备，`aenv.service` 可访问设备，数据盘有足够空间存放模板和 Sandbox。
- 节点已运行原生 AgentENV v0.2.4，能启动原生 Sandbox。新节点先按 [AgentENV 上游文档](https://kvcache-ai.github.io/AgentENV/)部署原生服务。
- 补丁包或 Runtime RPM 与节点架构和原生版本匹配。

### 4.2 设置前提（安装前必做）

```bash
uname -m
uname -r
sudo systemctl status aenv.service --no-pager
ls -l /dev/kvm /dev/ublk-control
df -h /var/lib/aenv
```

在维护窗口暂停向目标节点提交新的 AgentENV 请求，等待在途操作结束并清理已有 AgentENV Sandbox。备份原有 AgentENV 程序和配置，记录 systemd、kubelet 和 Webhook 配置。重启共享 cri-multiplex 或切换 kubelet endpoint 前，协调同节点上的其他后端与工作负载。

### 4.3 部署与验证工具

本机 CRI 冒烟需要 `crictl` 和 `jq`。Kubernetes 部署另需 `kubectl` 和可用的集群。启用普通 AgentENV Pod 的统一 Webhook 准入规则时，集群需为 Kubernetes 1.30 或更新版本。

---

## 5. 部署 AgentENV

以下两种方式任选其一。装完后按第 5.3 节检查服务。

### 5.1 补丁包安装

从可信制品源取得补丁包及对应的 SHA-256 校验文件。以下以 `0.2.4-cri.17` 的 aarch64 制品为例，执行时按实际文件名调整。校验和预检均成功后再运行安装。

```bash
package=agentenv-cri-runtime-patch-0.2.4-cri.17-linux-aarch64
sha256sum -c "$package.tar.gz.sha256" &&
  tar -xzf "$package.tar.gz" &&
  cd "$package" &&
  sudo ./install.sh --dry-run &&
  sudo ./install.sh
```

安装脚本会备份原生程序和配置并重启 `aenv.service`。记下输出中的备份目录，保留解包目录供回退使用。制品操作详见 [AgentENV 低级运行时安装指南](https://atomgit.com/m0_73757964/Agentenv-test/blob/main/agentenv-cri-selftest/%E5%AE%89%E8%A3%85%E6%8C%87%E5%8D%97.md)。

### 5.2 RPM 安装（src-openeuler/AgentENV）

也可以在原生节点安装由 [src-openeuler/AgentENV 打包仓](https://atomgit.com/src-openeuler/AgentENV)构建的 `agentenv-cri-runtime` RPM。RPM 安装后需显式激活，`--drained` 表示已完成第 4.2 节的流量停投和 Sandbox 清理。

```bash
# 将路径替换为本机对应架构的单个 RPM 文件。
sudo dnf install /path/to/agentenv-cri-runtime.rpm
sudo agentenv-runtime-package status
sudo agentenv-runtime-package activate --drained
```

### 5.3 验证 Runtime

```bash
sudo systemctl is-active aenv.service
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8000/health
sudo test -S /run/aenv/agentenv-runtime.sock
```

预期服务为 `active`，HTTP 健康接口返回 `204`，且 socket 存在。自定义 HTTP 监听地址或 socket 路径的节点使用实际配置。

### 5.4 OpenFuyao /internal 元数据（可选）

AgentENV HTTP API 提供 OpenFuyao 使用的 `/internal/*` 快照及模板元数据接口。调用方按 [OpenAPI 定义](../../spec/openapi.yml)适配。由部署方配置 `AENV_INTERNAL_ADMIN_TOKEN`，调用方通过 `X-Admin-Token` 请求头鉴权。打桩验证见[第 9.3 节](#93-openfuyao-接入验证可选)。

---

## 6. 模板管理

### 6.1 与 E2B 模板流程的差异

`sandbox.cri/template` 引用 AgentENV 模板。创建 Pod 之前，先在 AgentENV 侧准备模板。多节点部署时，所有承载 AgentENV Pod 的节点均需能解析并加载该模板。

### 6.2 从 OCI 镜像构建与查询

下例从公开 OCI 镜像创建模板。使用私有镜像时，先按 AgentENV 的镜像仓库配置准备访问权限。

```bash
aenv pull docker.io/library/ubuntu:22.04 --name agentenv-demo
aenv template list
```

### 6.3 使用已有模板

通过 `aenv template list` 确认已有模板的 ID 或别名，将其中一个值填入 Pod 的 `sandbox.cri/template`。同一个模板可分别用 ID 和别名验证。

---

## 7. 本机验证（agentenv-verify）

### 7.1 编译或安装 cri-multiplex

使用含 AgentENV handler 和统一 Webhook provider 的 KASandbox 版本。已有对应 RPM 可直接安装。在构建机自行打包时，于 KASandbox 源码根目录运行以下命令。脚本取 `HEAD` 的已提交内容作为构建源码，产物默认在 `/tmp/rpmbuild/RPMS/` 下。

```bash
./scripts/build-rpm.sh
```

在目标节点安装对应架构的 RPM。包内的 cri-multiplex 位于 `/opt/e2b-infra/bin/cri-multiplex`。

```bash
sudo dnf install /path/to/KASandbox.rpm
/opt/e2b-infra/bin/cri-multiplex --help 2>&1 | grep agentenv
```

开发环境也可以从 KASandbox 源码根目录执行 `./build.sh -c`，按构建输出选择节点架构对应的 cri-multiplex 二进制。

### 7.2 启动与生命周期冒烟

按[第 8.2 节](#82-部署-cri-multiplex启用-agentenv)启用 AgentENV handler 并启动 mux。在专用测试节点用以下命令直连 CRI socket。将 `template` 替换为可用模板 ID 或别名。该步骤无需 Kubernetes 集群。命令参数见 [crictl 文档](https://github.com/kubernetes-sigs/cri-tools/blob/master/docs/crictl.md)。

```bash
(
  set -eo pipefail
  template=agentenv-demo
  uid=$(cat /proc/sys/kernel/random/uuid)
  work_dir=$(mktemp -d)
  pod_config="$work_dir/pod.json"
  cri=(sudo crictl --runtime-endpoint unix:///run/cri-multiplex.sock)
  cleanup() {
    if [ "$?" -ne 0 ]; then
      "${cri[@]}" stopp "$uid" >/dev/null 2>&1 || true
      "${cri[@]}" rmp "$uid" >/dev/null 2>&1 || true
    fi
    rm -f -- "$pod_config"
    rmdir -- "$work_dir"
  }
  trap cleanup EXIT

  jq -n --arg uid "$uid" --arg template "$template" '{
    metadata: {name: ("agentenv-smoke-" + $uid), namespace: "agentenv-test", uid: $uid, attempt: 0},
    annotations: {"sandbox.cri/template": $template}
  }' > "$pod_config"
  id=$("${cri[@]}" runp -r agentenv -T 300s "$pod_config")
  "${cri[@]}" inspectp -o json "$id" | jq -e '.status.state == "SANDBOX_READY"'
  "${cri[@]}" stopp "$id"
  "${cri[@]}" rmp "$id"
  remaining=$("${cri[@]}" pods -q --id "$id")
  test -z "$remaining"
)
```

### 7.3 自动化验收

在 KASandbox 源码根目录使用 [agentenv-verify/run.sh](../../cri-multiplex/agentenv-verify/run.sh)。`cri` 安装态用例需要已接入的 Kubernetes、统一 Webhook、专用 `agentenv-e2e` 命名空间和 SDK，以及没有存量 AgentENV Sandbox 的测试节点。`webhook` 用例通过服务端 dry-run 验证模板 ID 和别名保持原值。

```bash
bash cri-multiplex/agentenv-verify/run.sh cri --help
bash cri-multiplex/agentenv-verify/run.sh webhook --help
```

---

## 8. 部署：K8S 模式

### 8.1 前置条件

目标节点已通过第 5.3 节的 Runtime 服务检查，能加载第 6 章准备的模板，且已安装第 7.1 节准备的 cri-multiplex。

### 8.2 部署 cri-multiplex（启用 AgentENV）

首次部署 cri-multiplex 的节点可以用 `sudo /opt/e2b-infra/k8s-deploy.sh cri-multiplex` 创建基础服务。已有共享 mux 的节点直接更新现有服务配置，该脚本会重写服务单元。

先用 `sudo systemctl cat cri-multiplex` 查看生效配置，再用 `sudo systemctl edit cri-multiplex` 添加 drop-in。在 `[Service]` 中先用空的 `ExecStart=` 清除原值，再写完整的原 `ExecStart=` 并追加下面两个参数。执行 `ExecStart` 的二进制需为第 7.1 节确认的版本。保留原有 socket、state-dir、CubeSandbox 和其他后端参数。若服务以非 root 身份运行，确认该用户有权连接 Runtime socket。

```text
--agentenv-enabled=true --agentenv-runtime-address=unix:///run/aenv/agentenv-runtime.sock
```

```bash
sudo systemctl daemon-reload
sudo systemctl restart cri-multiplex
sudo systemctl is-active cri-multiplex
sudo crictl --runtime-endpoint unix:///run/cri-multiplex.sock info
```

### 8.3 RuntimeClass 与 Pod 注解

确认 kubelet 生效的 `--container-runtime-endpoint` 指向 `unix:///run/cri-multiplex.sock`。已有相同 endpoint 时沿用当前配置。使用 kubeadm 且 endpoint 已写在 `/var/lib/kubelet/kubeadm-flags.env` 时，可在维护窗口执行 `sudo /opt/e2b-infra/k8s-deploy.sh kubelet-endpoint`，此命令会重启 kubelet。其他环境按集群管理方式更新 endpoint。

已有同名 RuntimeClass 时确认其 handler 为 `agentenv`。如果只有部分节点部署了 AgentENV，请在 RuntimeClass 的 `scheduling.nodeSelector` 中指定这些节点的标签。

```bash
kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: agentenv
handler: agentenv
EOF
kubectl get runtimeclass agentenv
```

Pod 设置 `spec.runtimeClassName: agentenv`，并将模板 ID 或别名填入 `metadata.annotations["sandbox.cri/template"]`。完整示例见[第 9.2 节](#92-通过-k8s-pod-创建和删除)。

### 8.4 统一 e2b-webhook（可选）

沿用集群现有的 `e2b-webhook` Deployment、Service 和 `MutatingWebhookConfiguration`。先在 `e2b` 命名空间提供名为 `agentenv-api-key` 的 Secret，键名为 `api-key`。下面的 `$AGENTENV_KEY_FILE` 指向本机受限文件，内容为 API Key 本身且无尾部换行。

```bash
kubectl -n e2b create secret generic agentenv-api-key \
  --from-file=api-key="$AGENTENV_KEY_FILE" --dry-run=client -o yaml | kubectl apply -f -
```

通过 KASandbox 部署脚本管理时，在部署目录的 `.env` 中配置以下参数，并使用含 AgentENV provider 的 Webhook 镜像。按现有部署流程更新统一 Webhook。如果原先就是通过 `./deploy.sh deploy-webhook` 管理，则继续使用该命令。Helm 管理的集群在现有 release 中设置 `webhook.enabled: true` 和 `webhook.agentenvApiURL`，保留已有 provider 配置和 TLS 证书注入流程。若仍注册了旧版独立 `agentenv-webhook`，先确认统一规则生效且通过第 8.5 节的服务端 dry-run，再由集群管理员移除旧注册。

```dotenv
ENABLE_WEBHOOK=true
AGENTENV_API_URL=https://agentenv-api.example.com
```

`AGENTENV_API_URL` 必须从 Webhook Pod 可达。更新后检查 Deployment Ready、注册项的 `caBundle` 已配置。统一规则会处理普通 `runtimeClassName: agentenv` Pod，默认排除 `e2b` 及 Kubernetes 系统命名空间。Webhook 保留 Pod 已填写的模板 ID 或别名，Runtime 在创建时解析该值。准入 dry-run 通过后，仍需创建一次 Pod 验证模板能启动。

```bash
kubectl -n e2b rollout status deployment/e2b-webhook
kubectl get mutatingwebhookconfiguration e2b-webhook
```

### 8.5 验证

根据第 9.2 节保存 `agentenv-demo.yaml` 后，先执行服务端 dry-run，确认 API Server 能处理请求，且模板注解保持原值。启用 Webhook 的集群可同时检查统一 Webhook 日志，确认该 Pod 命中准入规则。随后实际创建 Pod 并等待 Ready，完成第 9.2 节的删除检查。

```bash
# 将 namespace 改为现有测试命名空间。
namespace=agentenv-test
kubectl -n "$namespace" create --dry-run=server -f agentenv-demo.yaml -o yaml
```

---

## 9. 创建沙箱

### 9.1 E2B 兼容 SDK

使用 CRI 创建实例时，先按第 9.2 节创建 Pod，再用 SDK 连接和执行业务命令。

新建实例的 CRI Sandbox ID 为 Pod UID。SDK 连接需要 AgentENV 原生 Sandbox ID。可在 Pod 所在节点用 Admin v2 `GetSandboxRuntime` 查询 `runtime.extra.native_sandbox_id`。复用第 9.2 节的 `namespace`。以下命令从 KASandbox 源码根目录执行，需要 `grpcurl` 和 `jq`。

```bash
pod_uid=$(kubectl -n "$namespace" get pod agentenv-demo -o jsonpath='{.metadata.uid}')
export AGENTENV_SANDBOX_ID=$(
  jq -nc --arg id "$pod_uid" '{backend:"agentenv",criSandboxId:$id}' |
    sudo grpcurl -plaintext -import-path cri-multiplex/proto -proto admin_v2.proto \
      -d @ unix:///run/cri-multiplex/admin.sock \
      crimultiplex.admin.v2.SandboxAdminService/GetSandboxRuntime |
    jq -er '.runtime.extra.native_sandbox_id'
)
```

在 Python 虚拟环境中用 `python -m pip install e2b` 安装 E2B SDK。将 `E2B_API_KEY` 设置为 AgentENV API Key，将 `E2B_API_URL` 设置为客户端可达的 AgentENV HTTP 地址。在 Runtime 节点上可用 `http://127.0.0.1:8000`。若 SDK 在另一台机器运行，同时传入前一步查到的 `AGENTENV_SANDBOX_ID`。

```python
import os
from e2b import Sandbox

api_url = os.environ["E2B_API_URL"].rstrip("/")
sandbox = Sandbox.connect(
    os.environ["AGENTENV_SANDBOX_ID"],
    api_key=os.environ["E2B_API_KEY"],
    api_url=api_url,
    sandbox_url=f"{api_url}/proxy",
)
print(sandbox.commands.run("echo agentenv-ok").stdout)
```

AgentENV 的 CRI 不提供 `kubectl exec`、`attach`、`port-forward`、容器日志或 exec 类型探针。通过 SDK 和 AgentENV proxy 访问业务服务。

### 9.2 通过 K8S Pod 创建和删除

在可调度到 AgentENV 节点的测试命名空间中保存以下内容为 `agentenv-demo.yaml`。将 `sandbox.cri/template` 改为第 6 章查到的模板 ID 或别名。这是 AgentENV Pod 唯一必填的业务注解。`containers[].image` 是 Kubernetes 的必填字段。AgentENV 的 CRI Container 是逻辑状态记录，根文件系统和程序由模板提供。

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: agentenv-demo
  annotations:
    sandbox.cri/template: agentenv-demo
spec:
  runtimeClassName: agentenv
  restartPolicy: Never
  containers:
    - name: app
      image: agentenv.dev/app:v1
      imagePullPolicy: IfNotPresent
```

```bash
# 将 namespace 改为现有的测试命名空间。
namespace=agentenv-test
kubectl -n "$namespace" create -f agentenv-demo.yaml
kubectl -n "$namespace" wait --for=condition=Ready pod/agentenv-demo --timeout=300s
kubectl -n "$namespace" get pod agentenv-demo -o wide
```

首次验收可用别名和对应模板 ID 各测一次。如需验证 SDK，在 Pod Ready 后按第 9.1 节操作。每轮结束后删除测试 Pod，由 kubelet 清理对应 Sandbox。

```bash
kubectl -n "$namespace" delete pod agentenv-demo --wait=true
# 在原 Pod 所在节点查看 CRI Sandbox 已清理。
sudo crictl --runtime-endpoint unix:///run/cri-multiplex.sock pods --name agentenv-demo
```

### 9.3 OpenFuyao 接入验证（可选）

在专用测试环境中，`openfuyao` 套件直接向统一 Webhook 提交 AdmissionReview，然后调用 CRI、Admin v2 和 AgentENV `/internal/*`，验证创建、查询、检查点、暂停及清理。该套件使用打桩调用方。按 [cri-multiplex README](../../cri-multiplex/README.md#agentenv-原生网络接入)准备 Python 3.11+、`grpcurl`、测试凭据、模板、Webhook CA 和各 Unix socket。

```bash
bash cri-multiplex/agentenv-verify/run.sh openfuyao --help
```

---

## 10. 运维操作

### 10.1 服务管理

```bash
sudo systemctl status aenv.service cri-multiplex --no-pager
sudo journalctl -u aenv.service -u cri-multiplex -n 80
kubectl -n e2b rollout status deployment/e2b-webhook
```

最后一条命令用于部署了统一 Webhook 的集群。

### 10.2 下线 AgentENV CRI 接入

先停止新建 AgentENV Pod，删除已有 AgentENV Pod 并确认对应 Sandbox 已清理。在维护窗口移除 AgentENV mux 参数并重启 mux，保留 CubeSandbox、E2B 等其他后端的配置。仅为 AgentENV 新建的 RuntimeClass 可在停用后移除。共享 mux 继续承载其他后端时，保留 kubelet 到 mux 的 endpoint。若此次部署独占 mux 并切换过 endpoint，按节点原配置恢复。

### 10.3 回退或卸载 AgentENV Runtime

补丁包部署时，在原解包目录运行 `sudo ./rollback.sh`。需要选择备份时使用 `sudo ./rollback.sh --backup-dir /path/to/native-backup`。RPM 部署时运行 `sudo agentenv-runtime-package deactivate --drained`，确认已恢复后，如需卸载再运行 `sudo dnf remove agentenv-cri-runtime`。

用 `systemctl is-active aenv.service`、HTTP `/health` 和已有的原生模板验证回退。模板、Sandbox 数据及凭据目录按节点原有的数据管理策略保留。

### 10.4 停用 Webhook 和内部 API 接入

确认相应调用已停用后，从统一 Webhook 的部署配置移除 AgentENV URL，应用原发布流程，并按集群策略清理 AgentENV 专用 Secret。保留统一 Webhook 及其他 provider 配置。OpenFuyao 接入停用时，按环境策略撤销其 Admin Token 并收回到内部 API 的访问授权。

---

## 11. 常见问题

| 现象 | 检查方法 |
| --- | --- |
| `aenv.service` 启动失败或找不到 Runtime socket | `sudo journalctl -u aenv.service -n 80`。确认 ublk 驱动与运行内核匹配，并检查 `/dev/kvm`、`/dev/ublk-control` 的访问权限。 |
| Pod 一直 `Pending` 或 `FailedCreatePodSandBox` | `kubectl describe pod` 查看调度和创建事件。确认 RuntimeClass、节点 endpoint、mux 启动参数、socket 路径与访问权限。 |
| Pod 准入失败 | 检查 `e2b-webhook` 的 Deployment、Service、`caBundle` 与日志，确认 AgentENV HTTP 地址可达、Secret 键名正确。 |
| 模板无法启动 | 到 Pod 所在节点用 `aenv template list` 确认模板 ID 或别名，检查 Runtime 日志和对应模板是否可运行。 |
| SDK 无法连接 | 核对原生 Sandbox ID、API Key 和客户端到 AgentENV HTTP API 及 `/proxy` 的连通性。 |

---

## 12. 命令速查

```bash
# Runtime 与模板
sudo systemctl is-active aenv.service
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8000/health
aenv template list

# CRI 和 Kubernetes
sudo crictl --runtime-endpoint unix:///run/cri-multiplex.sock info
kubectl get runtimeclass agentenv

# 验证入口（从 KASandbox 源码根目录运行）
bash cri-multiplex/agentenv-verify/run.sh --help
```

---

## 附录 A：环境变量

| 变量 | 使用位置 | 用途 |
| --- | --- | --- |
| `ENABLE_WEBHOOK` | KASandbox 部署 `.env` | 设为 `true` 启用统一 Webhook |
| `AGENTENV_API_URL` | 统一 Webhook | 从 Webhook Pod 可达的 AgentENV HTTP 地址 |
| `AGENTENV_API_KEY` | 统一 Webhook | 来自 `e2b/agentenv-api-key` Secret 的 `api-key` |
| `E2B_API_URL`、`E2B_API_KEY` | SDK 客户端 | 客户端可达的 HTTP 地址和 AgentENV API Key |
| `AENV_INTERNAL_ADMIN_TOKEN` | AgentENV Runtime | OpenFuyao `/internal/*` 请求的 Admin Token |

---

## 附录 B：端口与路径

| 端口或路径 | 用途 |
| --- | --- |
| `127.0.0.1:8000` | AgentENV HTTP API 示例地址，实际监听地址以节点配置为准 |
| `/run/aenv/agentenv-runtime.sock` | AgentENV Runtime gRPC socket |
| `/run/cri-multiplex.sock` | cri-multiplex CRI socket |
| `/run/cri-multiplex/admin.sock` | cri-multiplex Admin v2 socket |
| `/var/lib/aenv` | AgentENV 默认数据目录 |
| `/opt/e2b-infra/bin/cri-multiplex` | KASandbox RPM 安装的 mux |
| `/opt/e2b-infra/k8s-deploy.sh` | Kubernetes 节点侧部署脚本 |
| `cri-multiplex/agentenv-verify/` | KASandbox 源码仓中的验证入口及脚本 |

---

## 附录 C：注解字段

| 字段 | 要求 | 说明 |
| --- | --- | --- |
| `spec.runtimeClassName` | 必填，值为 `agentenv` | Kubernetes 根据 RuntimeClass 选择 AgentENV handler |
| `metadata.annotations["sandbox.cri/template"]` | 唯一必填的 AgentENV 业务注解 | 填写模板 ID 或别名，统一 Webhook 保留原值 |

旧版的 Build、资源、网络和安全配置等 AgentENV 注解已退出创建参数。升级前先更新 Pod 模板。
