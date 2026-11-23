# AgentENV 使用与部署指南

AgentENV 可以作为 KASandbox 的 CRI 后端。kubelet 通过 cri-multiplex 的 `agentenv` handler 创建和删除 Sandbox，AgentENV Runtime 负责启动虚拟机及管理 Sandbox 网络。业务命令和文件操作通过 AgentENV 的 E2B 兼容 SDK 执行。

## 1. 快速开始

1. 在已安装原生 AgentENV v0.2.4 的节点上，[安装并验证 CRI Runtime 补丁](#3-安装-agentenv-cri-runtime)。
2. [准备一个可启动的 AgentENV 模板](#4-准备模板)，记下模板 ID 或别名。
3. [启用 cri-multiplex 的 AgentENV handler](#5-接入-kubernetes)，并确保 kubelet 指向 cri-multiplex。
4. [创建测试 Pod](#6-创建和删除-sandbox)，确认 Ready 后删除。已有统一 Webhook 的集群按第 5.4 节启用 AgentENV provider。

## 2. 组件与环境准备

| 组件 | 作用 | 默认入口 |
| --- | --- | --- |
| AgentENV Runtime | 创建、查询和删除 Sandbox，管理原生网络 | `aenv.service`，gRPC socket `/run/aenv/agentenv-runtime.sock` |
| AgentENV HTTP API | 查询模板，提供 SDK API 和 Sandbox proxy | 节点本机 `http://127.0.0.1:8000` |
| cri-multiplex | 将 kubelet 的 CRI 调用分发到对应后端 | `/run/cri-multiplex.sock` |
| 统一 e2b-webhook | 处理 Pod 准入，AgentENV provider 可查询模板 | Kubernetes `e2b` 命名空间中的 `e2b-webhook` |

AgentENV 与 CubeSandbox、E2B 可以共享 cri-multiplex。AgentENV 的网络由自己的 Runtime 管理，无需为该后端配置 CNI 或 HostPort。其他后端的部署见 [Cube 使用文档](./cube_user_guide.md)及 [E2B 使用文档](./e2b_user_guide.md)。

安装前确认以下条件。

- 节点已部署原生 AgentENV v0.2.4，能够启动一个原生 Sandbox。新节点先按 [AgentENV 上游文档](https://kvcache-ai.github.io/AgentENV/)安装原生服务。
- 补丁包或 Runtime RPM 与节点架构和原生版本匹配。KVM 和 ublk 设备可用，`aenv.service` 能访问设备，数据盘有足够空间保存模板和 Sandbox。
- 准备 `kubectl`、`crictl` 和已有 Kubernetes 集群。用统一 Webhook 为普通 AgentENV Pod 做准入时，集群需为 Kubernetes 1.30 或更新版本。
- 在维护窗口暂停向这些节点提交新的 AgentENV 请求，等待在途操作结束并清理现有 AgentENV Sandbox。记录原有 systemd、kubelet 和 Webhook 配置。重启共享 cri-multiplex 或切换 kubelet endpoint 前，协调同节点上的其他后端与工作负载。

```bash
uname -m
uname -r
sudo systemctl status aenv.service --no-pager
ls -l /dev/kvm /dev/ublk-control
df -h /var/lib/aenv
```

## 3. 安装 AgentENV CRI Runtime

### 3.1 使用补丁包

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

### 3.2 使用 Runtime RPM

也可以在原生节点安装由 [src-openeuler/AgentENV 打包仓](https://atomgit.com/src-openeuler/AgentENV)构建的 `agentenv-cri-runtime` RPM。在补丁包和 RPM 中选一种方式。RPM 安装后需显式激活，`--drained` 表示已完成第 2 节的流量停投和 Sandbox 清理。

```bash
# 将路径替换为本机对应架构的单个 RPM 文件。
sudo dnf install /path/to/agentenv-cri-runtime.rpm
sudo agentenv-runtime-package status
sudo agentenv-runtime-package activate --drained
```

### 3.3 验证服务

```bash
sudo systemctl is-active aenv.service
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8000/health
sudo test -S /run/aenv/agentenv-runtime.sock
```

预期服务为 `active`，HTTP 健康接口返回 `204`，且 socket 存在。自定义 HTTP 监听地址或 socket 路径的节点使用实际配置。

## 4. 准备模板

如已有可用模板，直接使用 `aenv template list` 查到的模板 ID 或别名。以下演示从 OCI 镜像创建名为 `agentenv-demo` 的模板。多节点部署请确认每个承载 AgentENV Pod 的节点都能解析并加载该模板。

```bash
aenv pull docker.io/library/ubuntu:22.04 --name agentenv-demo
aenv template list
```

## 5. 接入 Kubernetes

### 5.1 准备 KASandbox RPM

使用含 AgentENV handler 和统一 Webhook provider 的 KASandbox 版本。已有对应 RPM 可直接安装。在构建机自行打包时，于 KASandbox 源码根目录运行以下命令。脚本取 `HEAD` 的已提交内容作为构建源码，产物默认在 `/tmp/rpmbuild/RPMS/` 下。

```bash
./scripts/build-rpm.sh
```

在目标节点安装对应架构的 RPM。包内的 cri-multiplex 位于 `/opt/e2b-infra/bin/cri-multiplex`。

```bash
sudo dnf install /path/to/KASandbox.rpm
/opt/e2b-infra/bin/cri-multiplex --help 2>&1 | grep agentenv
```

### 5.2 在节点启用 AgentENV handler

首次部署 cri-multiplex 的节点可以用 `sudo /opt/e2b-infra/k8s-deploy.sh cri-multiplex` 创建基础服务。已有共享 mux 的节点直接更新现有服务配置，该脚本会重写服务单元。

先用 `sudo systemctl cat cri-multiplex` 查看生效配置，再用 `sudo systemctl edit cri-multiplex` 添加 drop-in。在 `[Service]` 中先用空的 `ExecStart=` 清除原值，再写完整的原 `ExecStart=` 并追加下面两个参数。执行 `ExecStart` 的二进制需为第 5.1 节确认的版本。保留原有 socket、state-dir、CubeSandbox 和其他后端参数。若服务以非 root 身份运行，确认该用户有权连接 Runtime socket。

```text
--agentenv-enabled=true --agentenv-runtime-address=unix:///run/aenv/agentenv-runtime.sock
```

```bash
sudo systemctl daemon-reload
sudo systemctl restart cri-multiplex
sudo systemctl is-active cri-multiplex
sudo crictl --runtime-endpoint unix:///run/cri-multiplex.sock info
```

查看实际生效的 kubelet 启动配置，确认 `--container-runtime-endpoint` 指向 `unix:///run/cri-multiplex.sock`。已经指向该 socket 时无需重启 kubelet。若要从 containerd 切换，按集群管理方式更新 endpoint。通过 kubeadm 管理且 endpoint 已写在 `/var/lib/kubelet/kubeadm-flags.env` 时，可以在维护窗口执行 `sudo /opt/e2b-infra/k8s-deploy.sh kubelet-endpoint`，此命令会重启 kubelet。

### 5.3 创建 RuntimeClass

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

### 5.4 配置统一 Webhook

沿用集群现有的 `e2b-webhook` Deployment、Service 和 `MutatingWebhookConfiguration`。先在 `e2b` 命名空间提供名为 `agentenv-api-key` 的 Secret，键名为 `api-key`。下面的 `$AGENTENV_KEY_FILE` 指向本机受限文件，内容为 API Key 本身且无尾部换行。

```bash
kubectl -n e2b create secret generic agentenv-api-key \
  --from-file=api-key="$AGENTENV_KEY_FILE" --dry-run=client -o yaml | kubectl apply -f -
```

通过 KASandbox 部署脚本管理时，在部署目录的 `.env` 中配置以下参数，并使用含 AgentENV provider 的 Webhook 镜像。按现有部署流程更新统一 Webhook。如果原先就是通过 `./deploy.sh deploy-webhook` 管理，则继续使用该命令。Helm 管理的集群在现有 release 中设置 `webhook.enabled: true` 和 `webhook.agentenvApiURL`，保留已有 provider 配置和 TLS 证书注入流程。若仍注册了旧版独立 `agentenv-webhook`，先确认统一规则能通过第 6 节的服务端 dry-run，再由集群管理员移除旧注册。

```dotenv
ENABLE_WEBHOOK=true
AGENTENV_API_URL=https://agentenv-api.example.com
```

`AGENTENV_API_URL` 必须从 Webhook Pod 可达。更新后检查 Deployment Ready、注册项的 `caBundle` 已配置。统一规则会处理普通 `runtimeClassName: agentenv` Pod，默认排除 `e2b` 及 Kubernetes 系统命名空间。Webhook 保留 Pod 已填写的模板 ID 或别名，Runtime 在创建时解析该值。准入 dry-run 通过后，仍需创建一次 Pod 验证模板能启动。

```bash
kubectl -n e2b rollout status deployment/e2b-webhook
kubectl get mutatingwebhookconfiguration e2b-webhook
```

## 6. 创建和删除 Sandbox

在可调度到 AgentENV 节点的测试命名空间中保存以下内容为 `agentenv-demo.yaml`。将 `sandbox.cri/template` 改为第 4 节查到的模板 ID 或别名。这是 AgentENV Pod 唯一必填的业务注解。`containers[].image` 是 Kubernetes Pod 的必填字段，CRI 中的 Container 记录是逻辑投影，Sandbox 内的 rootfs 和程序由模板提供。

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
kubectl -n "$namespace" create --dry-run=server -f agentenv-demo.yaml -o yaml
kubectl -n "$namespace" create -f agentenv-demo.yaml
kubectl -n "$namespace" wait --for=condition=Ready pod/agentenv-demo --timeout=300s
kubectl -n "$namespace" get pod agentenv-demo -o wide
```

首次验收建议用别名和对应模板 ID 各测一次，每轮结束后删除测试 Pod。要验证 SDK 执行，先完成下面的测试，再删除 Pod。

### 6.1 使用 SDK 执行业务命令

新建实例的 CRI Sandbox ID 为 Pod UID。SDK 连接需要 AgentENV 原生 Sandbox ID。可在 Pod 所在节点用 Admin v2 `GetSandboxRuntime` 查询 `runtime.extra.native_sandbox_id`。以下命令从 KASandbox 源码根目录执行，需要 `grpcurl` 和 `jq`。

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

### 6.2 删除测试 Sandbox

通过 Kubernetes 删除 Pod，由 kubelet 清理对应 Sandbox。

```bash
kubectl -n "$namespace" delete pod agentenv-demo --wait=true
# 在原 Pod 所在节点查看 CRI Sandbox 已清理。
sudo crictl --runtime-endpoint unix:///run/cri-multiplex.sock pods --name agentenv-demo
```

自动化验收入口为 [agentenv-verify/run.sh](../../cri-multiplex/agentenv-verify/run.sh)。在专用测试环境从源码根目录运行 `bash cri-multiplex/agentenv-verify/run.sh --help` 查看用例，`webhook --help` 和 `cri --help` 分别显示准入和安装态测试参数。

## 7. 回退与排障

回退前停止新建 AgentENV Pod，删除已有的 AgentENV Pod 并确认 Runtime 上的对应 Sandbox 已清理。移除本次为 AgentENV 添加的 mux 参数并在维护窗口重启 mux。仅为 AgentENV 接入的集群配置可以同步移除，包括本次创建的 RuntimeClass、统一 Webhook 中的 AgentENV URL 和 AgentENV Secret。若为本次部署切换了 kubelet endpoint，按节点原配置恢复。仍承载 CubeSandbox、E2B 等后端的 mux 和统一 Webhook 应保留。

补丁包部署时在原解包目录执行 `sudo ./rollback.sh`。需要选择备份时使用 `sudo ./rollback.sh --backup-dir /path/to/native-backup`。RPM 部署时执行 `sudo agentenv-runtime-package deactivate --drained`，确认原生服务恢复后，若需要卸载再执行 `sudo dnf remove agentenv-cri-runtime`。最后用 `systemctl is-active aenv.service`、HTTP `/health` 和已有原生模板验证回退。模板、Sandbox 数据及凭据目录按原节点的数据管理策略保留。

| 现象 | 检查方法 |
| --- | --- |
| `aenv.service` 启动失败或找不到 Runtime socket | `sudo journalctl -u aenv.service -n 80`。确认 ublk 驱动与运行内核匹配，并检查 `/dev/kvm`、`/dev/ublk-control` 的访问权限。 |
| Pod 一直 `Pending` 或 `FailedCreatePodSandBox` | `kubectl describe pod` 查看调度和创建事件。确认 RuntimeClass、节点 endpoint、mux 启动参数、socket 路径与访问权限。 |
| Pod 准入失败 | 检查 `e2b-webhook` 的 Deployment、Service、`caBundle` 与日志，确认 AgentENV HTTP 地址可达、Secret 键名正确。 |
| 模板无法启动 | 到 Pod 所在节点用 `aenv template list` 确认模板 ID 或别名，检查 Runtime 日志和对应模板是否可运行。 |
| SDK 无法连接 | 核对原生 Sandbox ID、API Key 和客户端到 AgentENV HTTP API 及 `/proxy` 的连通性。 |
