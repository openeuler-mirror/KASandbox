# cri-multiplex

CRI gRPC multiplexer. Routes pod/container operations to **containerd** or **E2B** based on `RuntimeHandler`.

## Architecture

```
Kubelet ──Unix socket──▶ cri-multiplex
                           ├── RuntimeHandler != "e2b" ──▶ ContainerEngine ──▶ containerd
                           └── RuntimeHandler == "e2b" ──▶ E2BEngine ──▶ orchestrator:5008 (SandboxService)
```

- `ContainerEngine` proxies all CRI calls directly to containerd (same as vanilla kubelet).
- `E2BEngine` always uses the orchestrator gRPC SandboxService.

## E2B Backend

| | gRPC |
|---|---|
| Target | orchestrator SandboxService |
| Address flag | `-orchestrator-address` (default `localhost:5008`) |
| Pod operations | RunPodSandbox (Create), StopPodSandbox (Update), RemovePodSandbox (Delete), PodSandboxStatus (List), ListPodSandbox (List) |

## CLI

```text
  -socket                 /run/cri-multiplex.sock
  -containerd-socket      /run/containerd/containerd.sock
  -orchestrator-address   host:port            (default "localhost:5008")
  -admin-socket           /run/cri-multiplex/admin.sock
  -node-name              $NODE_NAME
  -hide-sandbox-label     key=value            (default ""; hide matching E2B sandboxes from CRI list)
```

`-hide-sandbox-label`（如 `flux-sandbox.io/direct=true`)：携带该 label 的 E2B sandbox 会从 `ListPodSandbox`/`ListContainers` 响应中剔除。kubelet 的 PLEG/runtimeCache 因此看不到它们，`HandlePodCleanups` 的孤儿 sandbox 清杀不会命中——供 sandbox-agent 直连 `RunPodSandbox`（无 K8s Pod 对象）的场景使用。按 ID 的定向接口（`PodSandboxStatus`/`StopPodSandbox`/`RemovePodSandbox`/Admin API）不受影响。默认为空，即全部可见（原行为）。

### Admin API

Node-local gRPC service (`E2BSandboxAdminService`, proto `proto/admin.proto`) on `-admin-socket`, intended for the Node Agent: `PauseSandbox`, `CheckpointSandbox`, `GetSandboxRuntime`. Write operations are idempotent by `operation_id` and guarded by a per-sandbox operation lock.

## Pod annotations

Control sandbox parameters via `PodSandboxConfig.Annotations`:

| Annotation | Default |
|---|---|
| `e2b.dev/template-id` | `"default"` |
| `e2b.dev/build-id` | `"latest"` |
| `e2b.dev/team-id` | `"cri-multiplex"` |
| `e2b.dev/sandbox-id` | derived from Pod UID |
| `e2b.dev/vcpu` | `1` |
| `e2b.dev/ram-mb` | `2048` |
| `e2b.dev/allow-internet` | `true` |

## Build

```bash
go build ./cmd/cri-multiplex
```

Generated proto code is committed. To regenerate after upstream proto changes (use `proto/admin.proto` for the admin API):

```bash
protoc --experimental_allow_proto3_optional \
  --go_out=. --go_opt=module=github.com/cri-multiplex \
  --go-grpc_out=. --go-grpc_opt=module=github.com/cri-multiplex \
  -I proto -I /usr/include \
  proto/orchestrator.proto
```

Requires `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc`.

## Run

```bash
sudo cri-multiplex \
  -socket /run/cri-multiplex.sock \
  -containerd-socket /run/containerd/containerd.sock \
  -orchestrator-address localhost:5008
```

Requires root for Unix socket write access to `/run/`.

## AgentENV 原生网络接入

AgentENV backend 默认关闭，通过 `--agentenv-enabled` 和 `--agentenv-runtime-address` 接入 Runtime。AgentENV 使用 Runtime 原生网络，不执行 CNI ADD 或配置额外 HostPort；其他后端的网络行为保持不变。

AgentENV Pod 准入独立注册：先部署包含 AgentENV provider 的 Webhook 服务，再执行 `python3 deploy/agentenv/webhook.py install`（在仓库根目录运行，依赖 PyYAML、kubectl 和 Kubernetes 1.30+）。脚本复用 `e2b/e2b-webhook` 服务及 TLS Secret，不修改原有准入资源；卸载使用同一脚本的 `uninstall`。

AgentENV 测试集中在 `cri-multiplex/agentenv-verify/`。离线契约检查使用 `python3 cri-multiplex/agentenv-verify/packaging/check_agentenv_contract.py --agentenv <Runtime源码>/src/api/openapi.yml`；Runtime 回归入口为 `runtime/run-release-regression.py`，通过 `--runtime-source` 指定已应用补丁的源码目录。节点集成测试应在独占测试环境执行。

Runtime 协议源在 `proto/agentenv/runtime/v1/runtime.proto`，与配套 Runtime 源码保持一致；Go 消息及 gRPC 文件由 protoc 生成。生成命令在 `cri-multiplex` 下运行：

```bash
protoc -I proto --go_out=. --go_opt=module=github.com/cri-multiplex --go_opt=Magentenv/runtime/v1/runtime.proto=github.com/cri-multiplex/pkg/agentenv/runtime/v1 --go-grpc_out=. --go-grpc_opt=module=github.com/cri-multiplex --go-grpc_opt=Magentenv/runtime/v1/runtime.proto=github.com/cri-multiplex/pkg/agentenv/runtime/v1 proto/agentenv/runtime/v1/runtime.proto
```

AgentENV 的 CRI、产品和原生 Sandbox ID 对照保存在自身 `agentenv.id_map` 状态中，公共路由结构不扩展。镜像引用缓存不再单独持久化；容器记录仍保留镜像引用。升级到此版前必须停止投递并排空 AgentENV Sandbox，不迁移存活 Sandbox 的旧映射。

Runtime 去掉自定义崩溃回收和全局维护屏障，ublk 恢复上游实现；保留正常请求幂等、Sandbox 操作互斥及生命周期处理。Runtime 协议版本为 `2.0.0`，移除私有 Remediate RPC；mux 与 Runtime 应配套更新。

在 `cri-multiplex` 目录运行 `GOWORK=off go test -p 1 ./pkg/agentenv/... ./pkg/engine/... ./pkg/server/... ./cmd/...`。安装/回退测试使用 `python3 agentenv-verify/packaging/test_install.py --script <Runtime源码>/scripts/release/agentenv-cri-runtime-patch/rpm-control.sh`，通过临时目录和命令桩验证，不访问系统服务。Runtime 单元测试可运行 `CARGO_BUILD_JOBS=2 python3 agentenv-verify/runtime/run-release-regression.py --runtime-source <源码目录> --suite unit --suite runtime-unit --skip-fault-injection --output <新输出目录>`。这些测试不能替代 VM 和真实 kubelet 验收。
