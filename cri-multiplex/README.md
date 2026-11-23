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

AgentENV 与其他 Provider 共用 `e2b-webhook` 的 Deployment、Service 和 `MutatingWebhookConfiguration`。启用现有 Webhook，并在部署环境设置 `AGENTENV_API_URL`，使用从 Webhook Pod 可达的 AgentENV HTTP 地址。Helm 也可通过 `webhook.agentenvApiURL` 指定该地址。预先在 `e2b` 命名空间创建 `agentenv-api-key` Secret，键名为 `api-key`。AgentENV 配置使用自己的 URL 和密钥。启用后，既有 Pod 准入规则同时匹配普通 `runtimeClassName: agentenv` Pod，要求 Kubernetes 1.30+，沿用该条规则的 `failurePolicy: Fail`。若环境已使用旧版独立注册，应在统一规则生效并通过服务端 dry-run 后清理旧的 `agentenv-webhook` 注册。

业务命令统一通过 AgentENV SDK 执行；CRI `ExecSync`、`Exec`、`Attach` 返回 `Unimplemented`，不支持 `kubectl exec/attach` 和 exec 类型探针。CRI Container 仅为逻辑状态投影，不执行 Pod 的 command/args，不采集或转发容器日志。PortForward 返回 Unimplemented；业务访问使用原生 SDK/proxy。ContainerStats 仅返回身份信息，不报告共享 VM 的资源指标。Admin 仅提供 Pause、Checkpoint、GetRuntime。创建走 CRI，暂停后同 ID 恢复走 AgentENV 原生 HTTP `connect`。AgentENV 不注册 AdminLifecycle 或主动 Reconciler。

AgentENV 功能测试统一从 `bash cri-multiplex/agentenv-verify/run.sh` 进入，各套件可用 `--help` 查看参数。Kubernetes 用例使用已配置的测试环境。

```bash
bash cri-multiplex/agentenv-verify/run.sh contract --agentenv <Runtime源码>/src/api/openapi.yml
bash cri-multiplex/agentenv-verify/run.sh webhook --namespace <测试命名空间> --alias <别名> --template-id <模板ID> --build-id <构建ID>
bash cri-multiplex/agentenv-verify/run.sh cri --template <模板ID或别名> --output <新输出目录>
bash cri-multiplex/agentenv-verify/run.sh runtime --suite http --runtime-source <Runtime源码> --output <新输出目录>
```

扶摇调用打桩套件使用 Python 3.11+ 和 `grpcurl`。在测试环境准备 AgentENV 模板别名及对应 ID、测试凭证、统一 Webhook 的 CA 证书，以及 CRI、Admin v2 和 Runtime 的 Unix socket。用例直接提交 AdmissionReview 并使用返回的模板注解调用 CRI，随后执行查询、检查点、暂停、元数据回调和清理，无需启动扶摇或 Kubernetes 控制面。

```bash
bash cri-multiplex/agentenv-verify/run.sh openfuyao \
    --cri-socket "$CRI_SOCKET" --admin-socket "$ADMIN_SOCKET" --runtime-socket "$AGENTENV_SOCKET" \
    --cri-proto "$CRI_PROTO" --cri-proto-include "$PROTO_IMPORT_ROOT" \
    --webhook-url "$WEBHOOK_MUTATE_URL" --webhook-ca "$WEBHOOK_CA" \
    --template "$TEMPLATE_ALIAS" --template-id "$TEMPLATE_ID" \
    --credentials "$AENV_CREDENTIALS" --output "$RESULT_DIR"
```

Runtime 协议源在 `proto/agentenv/runtime/v1/runtime.proto`，与配套 Runtime 源码保持一致；Go 消息及 gRPC 文件由 protoc 生成。生成命令在 `cri-multiplex` 下运行：

```bash
protoc -I proto --go_out=. --go_opt=module=github.com/cri-multiplex --go_opt=Magentenv/runtime/v1/runtime.proto=github.com/cri-multiplex/pkg/agentenv/runtime/v1 --go-grpc_out=. --go-grpc_opt=module=github.com/cri-multiplex --go-grpc_opt=Magentenv/runtime/v1/runtime.proto=github.com/cri-multiplex/pkg/agentenv/runtime/v1 proto/agentenv/runtime/v1/runtime.proto
```

AgentENV Pod 设置 `spec.runtimeClassName: agentenv`，唯一必填的业务注解是 `metadata.annotations["sandbox.cri/template"]`。统一 Webhook 保留已有的模板 ID 或别名。Runtime 在创建时使用该值查找可运行快照，CRI 创建使用 300 秒操作超时和 24 小时生命周期上限。

AgentENV 使用 Pod UID 作为逻辑 Sandbox ID。Runtime 原生 UUID 用于调用 Runtime 接口，与 Pod 状态一起保存。Image 接口无状态，rootfs 由模板提供。切换前排空存量 AgentENV Sandbox，更新 Pod 模板并同步升级 cri-multiplex 和 Webhook。旧的自定义 Sandbox ID、Build、资源、网络、环境变量、安全模式和生命周期等注解不再参与 AgentENV 创建。

Runtime 去掉自定义崩溃回收和全局维护屏障，ublk 恢复上游实现；保留正常请求幂等、Sandbox 操作互斥及生命周期处理。Runtime 协议版本为 `3.0.0`，移除私有 Remediate、进程控制和 GetStats RPC；mux 与 Runtime 应配套更新。

Go UT 与源码同目录。在 `cri-multiplex` 下运行 `bash run-ut.sh all -count=1`，在 `e2b-webhook` 下运行 `bash run-tests.sh`。Rust UT 随 Runtime 补丁与被测模块一起提交。应用 RPM 源码补丁后，可直接运行：

```bash
cargo test --frozen --release -p agentenv --lib api::internal::tests -- --skip catalog_acknowledged_writes_survive_process_kill
cargo test --frozen --release -p agentenv --lib runtime_server::tests
```

安装和回退的模拟验证使用 `bash cri-multiplex/agentenv-verify/run.sh install-mock --script <Runtime源码>/scripts/release/agentenv-cri-runtime-patch/rpm-control.sh`。
