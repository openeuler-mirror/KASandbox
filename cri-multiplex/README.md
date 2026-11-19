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

业务命令统一通过 AgentENV SDK 执行；CRI `ExecSync`、`Exec`、`Attach` 返回 `Unimplemented`，不支持 `kubectl exec/attach` 和 exec 类型探针。CRI Container 仅为逻辑状态投影，不执行 Pod 的 command/args，不采集或转发容器日志。PortForward 返回 Unimplemented；业务访问使用原生 SDK/proxy。ContainerStats 仅返回身份信息，不报告共享 VM 的资源指标。Admin 仅提供 Pause、Checkpoint、GetRuntime；创建和恢复走 CRI，不注册 AdminLifecycle 或主动 Reconciler。

AgentENV 测试集中在 `cri-multiplex/agentenv-verify/`。离线契约检查使用 `python3 cri-multiplex/agentenv-verify/packaging/check_agentenv_contract.py --agentenv <Runtime源码>/src/api/openapi.yml`；Runtime 回归入口为 `runtime/run-release-regression.py`，通过 `--runtime-source` 指定已应用补丁的源码目录。节点集成测试应在独占测试环境执行。

Runtime 协议源在 `proto/agentenv/runtime/v1/runtime.proto`，与配套 Runtime 源码保持一致；Go 消息及 gRPC 文件由 protoc 生成。生成命令在 `cri-multiplex` 下运行：

```bash
protoc -I proto --go_out=. --go_opt=module=github.com/cri-multiplex --go_opt=Magentenv/runtime/v1/runtime.proto=github.com/cri-multiplex/pkg/agentenv/runtime/v1 --go-grpc_out=. --go-grpc_opt=module=github.com/cri-multiplex --go-grpc_opt=Magentenv/runtime/v1/runtime.proto=github.com/cri-multiplex/pkg/agentenv/runtime/v1 proto/agentenv/runtime/v1/runtime.proto
```

AgentENV 使用统一的逻辑 Sandbox ID：优先取 `sandbox.cri/sandbox-id`，未指定时取 Pod UID；CRI 返回此 ID，Pod metadata 保留原始 UID。Runtime 生成的原生 UUID 仅用于调用 Runtime 接口，与 Pod 状态一起保存，不再维护独立 ID 路由表。Image 接口无状态，不下载或缓存镜像；实际 rootfs 由模板提供。切换到此版前必须排空 AgentENV Sandbox，不迁移存活 Sandbox 的旧 ID 映射或 guest 进程状态。

Runtime 去掉自定义崩溃回收和全局维护屏障，ublk 恢复上游实现；保留正常请求幂等、Sandbox 操作互斥及生命周期处理。Runtime 协议版本为 `3.0.0`，移除私有 Remediate、进程控制和 GetStats RPC；mux 与 Runtime 应配套更新。

新增 Go 单元测试集中于 `agentenv-verify/testdata`。在 `cri-multiplex` 目录执行 `bash run-ut.sh all -count=1` 会自动装配临时副本，运行 mux 和 Webhook 测试，结束后清理副本；指定包时仅执行所选包。入口自身的回归检查使用 `python3 agentenv-verify/test_runner.py`。也可手动装配：在仓库根目录执行 `python3 cri-multiplex/agentenv-verify/prepare-tests.py --kind go --source . --output /tmp/agentenv-tests-<唯一名称>`，再分别进入输出目录中的 `cri-multiplex`、`e2b-webhook` 执行 `GOWORK=off go test -p 1 ./...`。测试只装配到临时副本；直接在生产源码目录运行 go test 不包含这些用例。

在 `cri-multiplex` 目录，安装/回退测试使用 `python3 agentenv-verify/packaging/test_install.py --script <Runtime源码>/scripts/release/agentenv-cri-runtime-patch/rpm-control.sh`，不访问系统服务。Runtime 单元测试使用 `CARGO_BUILD_JOBS=2 python3 agentenv-verify/runtime/run-release-regression.py --runtime-source <源码目录> --suite unit --suite runtime-unit --skip-fault-injection --output <新输出目录>`，入口自动在输出目录装配 `runtime/rust` 中的测试。原有上游测试保留，必要的结构体适配随源码提交；这些检查不能替代 VM 和真实 kubelet 验收。
