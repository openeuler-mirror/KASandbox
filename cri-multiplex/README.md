# cri-multiplex

CRI gRPC multiplexer. Routes pod/container operations to **containerd** or **E2B** based on `RuntimeHandler`.

## Architecture

```
Kubelet ──Unix socket──▶ cri-multiplex
                           ├── RuntimeHandler != "e2b" ──▶ ContainerEngine ──▶ containerd
                           └── RuntimeHandler == "e2b" ──▶ E2BEngine
                                                            ├── [grpc]  ──▶ orchestrator:5008 (SandboxService)
                                                            └── [rest]  ──▶ api.e2b.dev:443 (POST /sandboxes)
```

- `ContainerEngine` proxies all CRI calls directly to containerd (same as vanilla kubelet).
- `E2BEngine` has two interchangeable backends selected via `-e2b-backend`.

## Backends

| | gRPC (default) | REST |
|---|---|---|
| Target | orchestrator SandboxService (gRPC) | E2B REST API (HTTP) |
| Address flag | `-orchestrator-address` (default `localhost:5008`) | `-e2b-api-url` + `-e2b-api-key` |
| Pod operations | RunPodSandbox (Create), StopPodSandbox (Update), RemovePodSandbox (Delete), PodSandboxStatus (List), ListPodSandbox (List) | RunPodSandbox (POST /sandboxes), StopPodSandbox (POST /pause), RemovePodSandbox (DELETE), ListPodSandbox (GET /sandboxes) |
| Container ops | stub | stub |
| `e2b.dev/template-id` | optional (default `"default"`) | **required** |

## CLI

```text
  -socket                 /run/cri-multiplex.sock
  -containerd-socket      /run/containerd/containerd.sock
  -e2b-backend            grpc | rest          (default "grpc")
  -orchestrator-address   host:port            (default "localhost:5008")
  -e2b-api-url            https://api.e2b.app
  -e2b-api-key            sk-xxx
```

## Pod annotations

Control sandbox parameters via `PodSandboxConfig.Annotations`:

| Annotation | Backend | Default |
|---|---|---|
| `e2b.dev/template-id` | gRPC + REST | `"default"` |
| `e2b.dev/build-id` | gRPC | `"latest"` |
| `e2b.dev/team-id` | gRPC | `"cri-multiplex"` |
| `e2b.dev/vcpu` | gRPC | `1` |
| `e2b.dev/ram-mb` | gRPC | `2048` |
| `e2b.dev/allow-internet` | gRPC | `false` |

REST backend requires `e2b.dev/template-id` and returns an error if missing.

## Build

```bash
go build ./cmd/cri-multiplex
```

Generated proto code is committed. To regenerate after upstream proto changes:

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
# gRPC backend (default) — run alongside orchestrator on localhost:5008
sudo cri-multiplex \
  -socket /run/cri-multiplex.sock \
  -containerd-socket /run/containerd/containerd.sock

# REST backend — point at E2B API
sudo cri-multiplex \
  -e2b-backend rest \
  -e2b-api-url https://api.e2b.app \
  -e2b-api-key sk-xxx \
  -socket /run/cri-multiplex.sock \
  -containerd-socket /run/containerd/containerd.sock
```

Requires root for Unix socket write access to `/run/`.
