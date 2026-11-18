# AGENTS.md — cri-multiplex

## Project

Go 1.23+ module (`github.com/cri-multiplex`). CRI gRPC multiplexer that routes pod/container operations between **containerd** (real) and **E2B** (mock/stub) based on `RuntimeHandler`.

## Commands

This module is not part of the parent repo's `go.work`; run Go commands with `GOWORK=off`
(or from outside the workspace).

```
GOWORK=off go build ./...              # build all packages
GOWORK=off go build ./cmd/cri-multiplex # build binary
GOWORK=off go vet ./...                 # static check
GOWORK=off go test ./...                # unit tests (fake orchestrator / mock envd; no root needed)
```

No Makefile, no CI.

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
- `-admin-socket` — node-local admin gRPC socket；v1（E2B 专用，deprecated）与 v2（后端中性）共用（默认 `/run/cri-multiplex/admin.sock`）
- `-unknown-handler-policy` — 未注册 RuntimeHandler 的策略：`passthrough`（默认，交给默认后端 containerd）或 `reject`（返回错误）
- `-runtime-name` — 报告给 CRI 的 runtime 名称（默认 `e2b-cri-shim`）
- `-node-name` — Kubernetes node name recorded in runtime facts (defaults to `NODE_NAME` env)
- `-hide-sandbox-label` — hide E2B sandboxes carrying this label (`key=value`, e.g. `flux-sandbox.io/direct=true`) from `ListPodSandbox`/`ListContainers`, so kubelet's orphan-sandbox GC never sees them (agent-direct `RunPodSandbox` without a K8s Pod object). Empty = visible (default, legacy behavior)
- `-cni-pool-enabled` — E2B CNI netns/veth 预热池总开关（默认 false = 关闭）。需与 `-cni-pool-size > 0` 同时设置才生效。开启后后台协程提前执行完整 CNI ADD（netns 创建 + veth 配对 + host-local IPAM 分配），`RunPodSandbox` 直接从池中取用，规避并发创建时 CNI 插件链的串行瓶颈；有 RunPodSandbox 在途时预热自动暂停，创建优先；池空时回退为实时 CNI ADD。进程启动时自动清理上一轮遗留的预热池 entry（新命名可完整 CNI DEL 释放 IPAM，旧命名仅删 netns）。详见《CNI 并发创建优化与池化改造.md》
- `-cni-pool-size` — E2B CNI netns/veth 预热池容量（默认 0 = 关闭），仅在 `-cni-pool-enabled` 同时开启时生效

Requires root or write access to `/run/` for the socket.

## Architecture

每个后端是一个独立子包，实现 `RuntimeEngine`，并通过 `Module()` 声明 `BackendDescriptor`（Type/Handler/Aliases/ImagePrefixes）
与可选能力（`Cleaner` / `Reconciler` / `StateRestorer` / `HealthChecker`）。`engine.Registry` 是唯一的装配点，
`MuxServer`、`CleanupManager`、admin v2 都只通过注册表解析后端，不按名字分支。

```
cmd/cri-multiplex/main.go        — 装配顺序：StateStore → containerd/e2b/android.New → Registry.Register(Module()) → Freeze
                                   → CleanupManager → MuxServer → mux.RestoreState → Reconcile → Start → admin v1+v2
pkg/engine/engine.go             — RuntimeEngine 接口 + EngineType 常量
pkg/engine/registry.go           — Registry / BackendDescriptor / BackendModule / Deps
pkg/engine/capability.go         — Cleaner / Reconciler / StateRestorer / HealthChecker 契约
pkg/engine/route_store.go        — RouteTable（持久化 pod/container → backend，含 sandbox_id）
pkg/engine/state_store.go        — JSON StateStore：routes/cleanup_tasks 固定字段 + 后端 section KV（布局兼容旧 state.json）
pkg/engine/cleanup.go            — CleanupManager：Schedule（按 registry 找 Cleaner）+ 启动对账
pkg/engine/admin.go              — 中性 Admin DTO 与 AdminLifecycle/Pauser/Checkpointer/RuntimeDescriber 能力接口
pkg/engine/admin_v1_compat.go    — v1 专用 E2B 视图类型（deprecated，仅为 v1 协议兼容保留）
pkg/engine/containerd/           — containerd 后端（engine.go + module.go，最小样板）
pkg/engine/e2b/                  — E2B 全部实现：engine/pod/create/state/admin_*/cleanup/reconcile/cni_pool/streaming + module.go
pkg/engine/android/              — Android 后端：engine/network/state/reconcile/cleanup/owner + module.go
pkg/engine/util/                 — 后端无关主机能力：CNI、HostPort、iptables/nftables 规则、资源归属
pkg/server/mux.go                — MuxServer：注册表驱动的 CRI 路由与 fan-out
pkg/admin/                       — v1 admin server（deprecated）+ backend Registry + v2 分发器
pkg/admin/v2/                    — 生成的 v2 proto 代码（adminv2）
pkg/orchestrator/                — 生成的 orchestrator proto 类型 + SandboxService 客户端
proto/admin.proto                — v1 admin proto（E2B 专用；imports orchestrator.proto）
proto/admin_v2.proto             — v2 admin proto（后端中性；不 import orchestrator.proto）
proto/orchestrator.proto         — 从 infra/packages/orchestrator 复制的 proto
test/test_pod_default.json       — 手工测试用的 PodSandbox 样例
```

### Routing logic (mux.go)

- **RunPodSandbox**：`Registry.ResolveHandler(RuntimeHandler)` 命中 → 该后端；未命中按 `-unknown-handler-policy`
  （`passthrough` 用默认后端即 containerd，`reject` 返回错误）处理，并写入 `RouteTable`（pod → backend）。
- **CreateContainer**：按父 pod 的路由确定后端，再把 container → backend（含 `sandbox_id`）写入路由表。
- **其余 pod/container 操作**：先查 `RouteTable`（container 路由缺失时回退到其 sandbox 路由），miss 再按注册表默认策略处理。
- **List\***：向所有后端并发 fan-out 并按后端合并；单后端失败保留其它后端结果（全部失败才报错，`Health` 只记日志）。
- **Image 操作**（Status/Pull/Remove）：先按 `Descriptor.ImagePrefixes` 最长前缀匹配，未命中再按注册表顺序回退；`ImageFsInfo` 同理。
- **Status/Version/UpdateRuntimeConfig**：始终委托给 containerd 后端。
- **GetContainerEvents**：返回不支持（多路复用器不接管 events）。

旧构造函数 `server.NewMuxServerLegacy` / `engine.NewCleanupManagerLegacy` 只为过渡期测试保留（名字含 `Legacy`）。

### 新增后端（设计 §5.6）

1. 新建 `pkg/engine/<name>/`：`Engine` 实现 `RuntimeEngine` + `New(cfg)`，`module.go` 提供 `Descriptor` 与 `Module()`。
2. `cmd/cri-multiplex/main.go` 构造并在 `Registry.Register` 中登记。
3. 需要清理/对账/状态恢复就实现对应能力；需要 admin v2 就实现 `engine.Admin*` 接口——`mux.go`/`cleanup.go`/`registry.go`/`admin/server.go` 零修改。

### E2BEngine

`pkg/engine/e2b.Engine` 使用 E2B orchestrator 的 `SandboxService` gRPC（默认 `localhost:5008`）。

| CRI method | Orchestrator RPC | Notes |
|---|---|---|
| `RunPodSandbox` | `Create` | sandboxID = `e2b-{UID}`；E2B 配置从 CRI 注解提取 |
| `StopPodSandbox` | `Delete` | 停止时销毁 VM（幂等、orchestrator 侧异步）；CNI DEL / 端口 / 路由清理由 `RemovePodSandbox` 完成 |
| `RemovePodSandbox` | `Delete` | 硬删除（已删除则幂等 no-op）+ CNI DEL + 路由清理，经 `CleanupManager` 重试 |
| `PodSandboxStatus` | `List` + 客户端过滤 | orchestrator 没有 Get-by-ID RPC |
| `ListPodSandbox` | `List` | `RunningSandbox` → CRI `PodSandbox` |

### Shared CRI annotations (`e2b.dev/*`)

Set on `PodSandboxConfig.Annotations`:

| Annotation | Default |
|---|---|
| `e2b.dev/template-id` | none |
| `e2b.dev/build-id` | `"latest"` |
| `e2b.dev/team-id` | `"cri-multiplex"` |
| `e2b.dev/sandbox-id` | none (derived from Pod UID); stable logical sandbox ID, lowercase letters/digits/`-`, 1-64 chars |
| `e2b.dev/vcpu` | `1` |
| `e2b.dev/ram-mb` | `2048` |
| `e2b.dev/allow-internet` | `true` |

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

`proto/admin.proto` is cri-multiplex's own node-local admin API. It imports `orchestrator.proto`
（E2BSandboxService 直接复用 orchestrator SandboxService 的消息类型），so both files must be
compiled together:

```bash
protoc \
  --go_out=. --go_opt=module=github.com/cri-multiplex \
  --go-grpc_out=. --go-grpc_opt=module=github.com/cri-multiplex \
  --experimental_allow_proto3_optional \
  -I proto -I /usr/include \
  proto/orchestrator.proto proto/admin.proto
```

`proto/admin_v2.proto` 是后端中性的 admin API（v2）。它**不 import** `orchestrator.proto`，
因此单独生成到 `pkg/admin/v2`（package `adminv2`）：

```bash
protoc \
  --go_out=. --go_opt=module=github.com/cri-multiplex \
  --go-grpc_out=. --go-grpc_opt=module=github.com/cri-multiplex \
  --experimental_allow_proto3_optional \
  -I proto -I /usr/include \
  proto/admin_v2.proto
```

### Admin socket services

Two generations of services share one admin unix socket:

**v1（E2B 专用，deprecated，至少保留两个版本）**

- **E2BSandboxAdminService** — Pause/Checkpoint with operation-id idempotency, plus GetSandboxRuntime.
- **E2BSandboxService** — orchestrator-shaped lifecycle API (message types reused from
  `orchestrator.proto`), **not a pass-through**: `Create` runs the full cri-multiplex lifecycle via
  `createE2BSandbox` (CNI incl. prewarm pool, HostPort, tracker, stateStore; `config.sandbox_id`
  required, used as both cri id and e2b id; expose-ports taken from `config.metadata` under the
  same `e2b.dev/expose-ports` key; envd token taken from `config.envd_access_token`); `Delete` is
  Stop+Remove in one call
  (blocking sandbox operation lock + `cleanupSandboxResources`, idempotent OK when missing);
  `Update`/`List`/`ListCachedBuilds` are thin forwards. Always enabled.

**v2（后端中性，新调用方用它）**

- **SandboxService** — Create/Update/List/Delete/ListCachedBuilds。
- **SandboxAdminService** — PauseSandbox/CheckpointSandbox/GetSandboxRuntime。
- 请求里的 `backend` 为空默认 `e2b`；`admin.Registry` 按 Type/Handler/Aliases 解析，
  未注册返回 `NotFound`。后端能力缺失返回 `Unimplemented`（e2b 8 个方法全实现，
  android/containerd 本期不实现 v2）。target 用 `cri_sandbox_id` 或 `sandbox_id` 二选一；
  给了 CRI ID 且路由归属不符返回 `FailedPrecondition`，路由缺失时交给引擎判断。
- Pause/Checkpoint 的 `timeout_seconds`：0 → 默认 60s，>300 → 300s（1–299s 原样透传），审计日志用 klog 输出
  `backend/action/operation_id/target/result/duration_ms`，不记录 metadata。
- 新后端只需实现 `engine.Admin*` 能力接口并在 `main` 注册，无需改 `pkg/admin/server.go`。

## Key constraints

- ContainerEngine uses lazy-init gRPC connection (`sync.Once` on first call). `Close()` must be called via defer in main.
- gRPC backend uses lazy-init connection (`sync.Mutex` guarding one-shot dial).
- Both `RuntimeService` and `ImageService` are registered on the same gRPC server.
- Embeds `UnimplementedRuntimeServiceServer` and `UnimplementedImageServiceServer` — adding new CRI methods requires implementing them or they will panic.
- `annTemplateID` (`e2b.dev/template-id`) is defined in `pkg/engine/e2b`；共享注解键集中在 e2b/config.go。

## PerfTrace 打点（性能采集）

`createE2BSandbox` 成功路径输出单行 `[PerfTrace]` 日志（通用 `k=v`，新增字段直接追加）：

```
[PerfTrace] sandbox=<uid> cni_ms=<n> cni_source=<pool|direct|disabled> cni_netns_ms=<n> cni_loup_ms=<n> \
  cni_plugin_ms=<n> cni_parse_ms=<n> orch_create_ms=<n> hostport_ms=<n> persist_ms=<n> total_ms=<n>
[PerfTrace] sandbox=<uid> route_persist_ms=<n>   # mux.go saveRoute（SaveRoute 全量刷盘），单独一行
```

- `cni_*` 子字段仅 direct ADD 路径填写，池化命中为 0；`persist_ms` 是 `SaveBackendState("e2b.pods", …)`（全量 MarshalIndent + fsync + rename，store 互斥锁 + `Engine.stateMu`）的墙钟耗时（含锁排队）。
- 采集脚本：`e2b-verify/28_bulk_concurrency_standalone.sh`（单机）、`29_bulk_concurrency_multinode.sh`（多节点），按 `[PerfTrace]` 解析统计 avg/p50/p90/p99。
- 端到端基准：`e2b-verify/32_bulk_runp_concurrent.py`（并发栅栏同时发起 N 个 RunPodSandbox，输出分位数与 `[BulkResult]` 机读行，可选 `--cleanup`）；`e2b-verify/33_multi_round_benchmark.sh`（连续多轮跑 32 号脚本并附带环境指纹：负载/内存/大页/netns 泄漏数等，用于跨轮对比稳定性）。

