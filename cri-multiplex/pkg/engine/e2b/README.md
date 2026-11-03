# e2b backend

cri-multiplex 的 E2B 后端：把 CRI 的 pod/container 操作翻译成 E2B orchestrator 的 `SandboxService` 调用。

## 配置

`New(Config)` 校验 `OrchestratorAddr` 与 `NodeIP` 必填；其余字段：

- `engine.Deps`：`Store`（状态持久化）、`NodeName`、`NodeIP`
- `OrchestratorProxyAddr`：envd HTTP 代理地址
- `HideLabel`：`key=value`，命中的 sandbox 从 CRI List 中隐藏
- `CNI`：`util.CNIConfig`（含 netns/veth 预热池开关）

## 网络入口

- CNI：netns 前缀默认 `e2b-`，由 `util.CNIManager` 创建/删除；预热池缓存 netns+veth，命中时跳过串行 CNI ADD。
- HostPort：`util.HostPortManager`（20000–29999）+ iptables DNAT/MASQUERADE，规则带 `cri-multiplex:hostport:` 归属注释。

## 清理责任

- `CleanupSandbox`：幂等回收单个 sandbox（orchestrator Delete、CNI DEL、HostPort 规则、state 与路由）。
- `Reconcile`：运行时对账、路由对账（只动 `Engine==e2b` 的路由）、`e2b-` netns 孤儿、HostPort 孤儿规则。
- `RestoreState`：加载 `e2b.pods`/`e2b.images`，把重启前仍在运行的操作标记为失败，并派生 pod/container 路由。

## Admin 能力

实现根包 `AdminLifecycle`/`AdminEngine`（v1 admin socket 直接复用）：Create/Update/List/Delete/ListCachedBuilds + Pause/Checkpoint/GetRuntime。

## 状态 section

`e2b.pods`、`e2b.images`（嵌套在顶层 `e2b` 下）、`e2b_operations`（顶层键，保持旧布局），全部经 `StateStore` 的通用 KV 读写。

## 已知限制

- 进程内的 `e2b.*` 状态读改写由 `Engine.stateMu` 串行化；跨进程并发写同一 `state.json` 不受支持。
- 对账依赖 orchestrator `List`；orchestrator 不可达时本轮运行时对账跳过（只记日志）。
