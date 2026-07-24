# AGENTS.md — cri-multiplex

## Project

Go 1.23+ module (`github.com/cri-multiplex`). CRI gRPC multiplexer that routes pod/container operations between **containerd** (real) and **E2B** (mock/stub) based on `RuntimeHandler`.

## Commands

```
go build ./cmd/cri-multiplex     # build binary
go run ./cmd/cri-multiplex       # run directly
go vet ./...                     # static check
```

No Makefile, no CI, no tests exist yet.

## CLI

```
cri-multiplex \
  -socket /run/cri-multiplex.sock \
  -containerd-socket /run/containerd/containerd.sock \
  -orchestrator-address localhost:5008
```

- `-socket` — Unix socket this server listens on (default `/run/cri-multiplex.sock`)
- `-containerd-socket` — upstream containerd socket (default `/run/containerd/containerd.sock`)
- `-orchestrator-address` — E2B orchestrator gRPC target (default `localhost:5008`)

Requires root or write access to `/run/` for the socket.

## Architecture

```
cmd/cri-multiplex/main.go   — entrypoint, wires engines + server
pkg/engine/engine.go        — RuntimeEngine interface (all CRI methods)
pkg/engine/container.go     — ContainerEngine: real gRPC client to containerd
pkg/engine/e2b.go           — E2BEngine interface + factory
pkg/engine/grpc_e2b.go      — gRPC backend: orchestrator SandboxService client
pkg/orchestrator/           — generated proto types + gRPC client for SandboxService
proto/orchestrator.proto    — proto source copied from infra/packages/orchestrator/
pkg/server/mux.go           — MuxServer: gRPC server, routes by RuntimeHandler
test/test_pod_default.json  — sample pod sandbox config for manual testing
```

### Routing logic (mux.go)

- **RunPodSandbox**: if `RuntimeHandler == "e2b"` → E2BEngine, else → ContainerEngine. Stores pod→engine mapping in `podRoutes`.
- **CreateContainer**: looks up parent pod via `podRoutes`, stores container→engine in `containerRoutes`.
- **All other container/pod ops**: resolve engine via the route maps.
- **List\*** ops (pods, containers, images, stats): fan-out to both engines concurrently, merge results.
- **Image ops** (Status, Pull, Remove, FsInfo): try containerd first, fall back to E2B on error.
- **Status/Version/UpdateRuntimeConfig**: always delegate to containerd only.
- **GetContainerEvents**: returns error (not supported by multiplexer).

### E2BEngine

`E2BEngine` uses the E2B orchestrator's `SandboxService` gRPC API.

Dials the E2B orchestrator's `SandboxService` gRPC (port 5008).

| CRI method | Orchestrator RPC | Notes |
|---|---|---|
| `RunPodSandbox` | `Create` | sandboxID = `e2b-{UID}`; E2B config extracted from CRI annotations |
| `StopPodSandbox` | `Update` (set end_time=now) | Soft-stop via deadline |
| `RemovePodSandbox` | `Delete` | Hard destroy |
| `PodSandboxStatus` | `List` + client-side filter | No Get-by-ID RPC exists |
| `ListPodSandbox` | `List` | Maps `RunningSandbox` → CRI `PodSandbox` |

### Shared CRI annotations (`e2b.dev/*`)

Set on `PodSandboxConfig.Annotations`:

| Annotation | Default |
|---|---|
| `e2b.dev/template-id` | none |
| `e2b.dev/build-id` | `"latest"` |
| `e2b.dev/team-id` | `"cri-multiplex"` |
| `e2b.dev/vcpu` | `1` |
| `e2b.dev/ram-mb` | `2048` |
| `e2b.dev/allow-internet` | `false` |

CRI `Labels` → SandboxConfig `Metadata` (gRPC backend). CRI `Metadata.Uid` → `SandboxId`, `Metadata.Name` → `Alias`.

### Proto sync

`proto/orchestrator.proto` is copied from `infra/packages/orchestrator/orchestrator.proto`. To regenerate:

```bash
protoc \
  --go_out=. --go_opt=module=github.com/cri-multiplex \
  --go-grpc_out=. --go-grpc_opt=module=github.com/cri-multiplex \
  --experimental_allow_proto3_optional \
  -I proto -I /usr/include \
  proto/orchestrator.proto
```

Keep the proto in sync when the upstream orchestrator proto changes.

## Key constraints

- ContainerEngine uses lazy-init gRPC connection (`sync.Once` on first call). `Close()` must be called via defer in main.
- gRPC backend uses lazy-init connection (`sync.Mutex` guarding one-shot dial).
- Both `RuntimeService` and `ImageService` are registered on the same gRPC server.
- Embeds `UnimplementedRuntimeServiceServer` and `UnimplementedImageServiceServer` — adding new CRI methods requires implementing them or they will panic.
- `annTemplateID` (`e2b.dev/template-id`) is defined in `e2b.go`.
