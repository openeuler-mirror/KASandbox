# cube backend

cri-multiplex 的 Cube 后端：把 CRI 的 pod/container 操作翻译成本机 Cubelet `CubeboxMgr` 调用。模板不是镜像引用，Descriptor 不登记 `ImagePrefixes`；kubelet 通过 RuntimeHandler `cube` 进入。

## 配置

`New(Config)`：`Enabled=false` 时不拨号，CRI 入口返回 FailedPrecondition；开启时校验 `CubeletAddr` 必填。其余字段：

- `engine.Deps`：`Store`（状态持久化）、`NodeName`
- `Enabled`：总开关（`main` 的 `-cube-enabled`，默认 false）
- `CubeletAddr`：本机 CubeboxMgr 地址（`-cubelet-address`，默认 `127.0.0.1:9999`）
- `Timeout`：Cubelet RPC 超时（默认 120s）

## 网络入口

- 不经 cri-multiplex CNI / HostPort；沙箱网络由 Cubelet 侧 tap 分配，IP 写回 CRI `PodSandboxStatus` / Admin `pod_ip`。
- envd 默认端口 `49983`，放在 Admin GetRuntime 的 `Extra`。

## 清理责任

- `CleanupSandbox`：幂等 Destroy（Cubelet）并删本地 state；Paused 销毁带 `cube.pause.delete_tombstone`。
- 未实现 `Reconciler`；重启后不做主动孤儿对账（依赖 `RestoreState` + 后续 CRI/Admin 调用）。
- `RestoreState`：加载 `cube.pods`，用 Cubelet `List` 校正 paused，并派生 pod/container 路由。

## Admin 能力

实现 `AdminPauser` / `AdminCheckpointer` / `AdminRuntimeDescriber`（Admin v2）：Pause / Checkpoint / GetRuntime。

不实现 `AdminLifecycle`（Create/Update/List/Delete/ListCachedBuilds → Unimplemented）。Create / Resume 走 CRI `RunPodSandbox`（同 cri uid 且本地为 paused 时为 Resume）。

## 状态 section

`cube.pods`（嵌套在顶层 `cube` 下），经 `StateStore` 通用 KV 读写；进程内读改写出 `Engine.mu` / inflight 串行化。

## 模板构建

OCI 镜像不能直接当沙箱跑，需先建成 READY 模板（`tpl-…`）：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
  # 可选 --alias <name>；境外镜像换 cube-sandbox-int.…
```

流程：CubeMaster 建 job → TemplateCenter 拉镜像打 ext4（`rfs-…`）→ 临时 MicroVM 等到 HTTP probe 2xx → 打快照 → 分发到节点（需 XFS reflink，常见 `/data/cubelet`）。`cubemastercli tpl list` / `tpl watch` 查状态。

冷启动注解由调用方从 `GET /templates/<id>` 的 `createRequest` 填入（`cube.dev/template-id`、`vcpu`、`ram-mb`、`image`、`command`/`args`、`cube.master.rootfs.*`）；引擎 Create 固定带 `appsnapshot.version=v2`。

本机 e2e 默认模板 alias：`sandbox-code`（`tpl-19b42e93ee0b4cf8967c3c5f`；源镜像 `sandbox-code:latest`；可用 `CUBE_E2E_TEMPLATE` 覆盖）。

## 验证

```bash
cd cube-verify
./run_all.sh             # 冒烟：01 启动 + 02 生命周期（/tmp/cube-verify）
./run_all.sh --only 04   # 并发冷启动
./run_all.sh --only 05   # 并发热启动
./run_all.sh --all       # 01–06 全量
# E2B 请用 e2b-verify/
```

## ExecSync

`ExecSync` 对齐本机 SDK / 直连口径：向 guest **envd**（`http://{SandboxIP}:49983/process.Process/Start`，Connect+JSON）下发命令，返回 stdout / stderr / exit code。不再走 CubeboxMgr `Exec`（该 RPC 只有 `Ret`，无输出）。

流式 CRI `Exec` / `Attach` / `PortForward` 仍为 Unimplemented。

## 已知限制

- 冷启动依赖注解里的完整模板规格；Cubelet 不会只凭 `tpl-` 展开 `createRequest`。
- Resume 未完整回放 network / expose ports。
- Cubelet 不可达时仅本后端失败；默认关闭不影响 e2b/android。
