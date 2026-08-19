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
| `e2b.dev/allow-internet` | `false` |

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
