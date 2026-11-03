# android backend

cri-multiplex 的 Android (Cuttlefish) 后端：以 CRI pod sandbox 的形式拉起 cvd，暴露 ADB/WebRTC 端口。

## 配置

`New(Config)` 只做默认值填充与初始化，主机前置校验仍在 `RunPodSandbox`（`ensureEnabled`/`validateHostPrerequisites`）。

- `engine.Deps`：`Store`、`NodeName`、`NodeIP`
- `Enabled`：关闭时 CRI 调用返回明确错误
- `ArtifactsDir`：cvd 制品目录（默认 `/home/fjq/cf17`）
- `ADBPortStart` / `BaseInstanceNumStart` / `WebRTCPortStart` / `LaunchTimeout`
- `StateDir`：每 sandbox 的 workdir 根目录
- `CNI`：`util.CNIConfig`（netns 前缀默认 `android-`）

## 网络入口

- CNI：`util.CNIManager`，netns 名 `android-<shortID>`，guest 网络由 `network.go` 配置。
- ADB/WebRTC 端口由 `portOwners` 分配，sandbox 释放时回收。

## 清理责任

- `CleanupSandbox`：停止进程组、CNI DEL、删除带 owner 标记的 workdir/artifacts、删 state 与路由。
- `Reconcile`：运行时对账、路由对账（只动 `Engine==android`）、state/artifacts 孤儿、`CRI_MULTIPLEX_SANDBOX_ID` 进程孤儿、`android-` netns 孤儿。
- `RestoreState`：加载 `android.pods`，校验进程存活并派生 pod/container 路由。

## 状态 section

`android.pods`（嵌套在顶层 `android` 下），经 `StateStore` 通用 KV 读写；读改写出 `Engine.stateMu` 串行化。

## 已知限制

- 需要宿主机具备 cvd 制品与 KVM；无制品时仅在 `RunPodSandbox` 报错。
- 进程孤儿扫描遍历 `/proc`，在超大进程数节点上有开销。
