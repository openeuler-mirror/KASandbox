# AgentENV engine

本包是 KASandbox `cri-multiplex` 到 AgentENV Runtime 的内部 adapter。它只依赖
`github.com/cri-multiplex/pkg/agentenv/runtime/v1`，不依赖 AgentENV 的 Rust 类型。

## 当前能力

- 显式注册：`-agentenv-enabled=true` 且 `-agentenv-runtime-address` 非空时注册 `agentenv` backend。
  仅支持原生网络；`-agentenv-network-mode` 可省略，显式设置时仅接受 `runtime-managed`。
- Admin Create/List/Update/Delete/GetRuntime。
- CRI `RunPodSandbox`：只接受 `sandbox.cri/template`，禁止 image、`TEMPLATE_NAME`、Pod 名和
  RuntimeClass 回退。
- CRI `PodSandboxStatus/ListPodSandbox/StopPodSandbox/RemovePodSandbox`：
  - Runtime `RUNNING` 且原生网络事实有效、活实例地址归属一致时投影为 `SANDBOX_READY`；
  - 其他阶段和已 Stop 的本地记录投影为 `SANDBOX_NOTREADY`；
  - Stop 按 Runtime Delete 实现，Remove 幂等清理本地状态和 RouteTable。
- 三套 ID 映射统一保存在共享 `engine.RouteTable`：
  - `cri_sandbox_id`
  - `product_sandbox_id`
  - `native_sandbox_id`
- 三套 ID 映射只在共享 `engine.RouteTable` 中持久化，启动时直接恢复，不再维护第二份 mapping 状态。
- CRI 本地投影保存在 `agentenv.pods`，用于重启恢复、Status/List 和 Stop 后查询。

## ID 规则

- 外部 Admin v2 的 `sandbox_id` 在本 adapter 中作为 `product_sandbox_id`。
- CRI PodSandbox ID 使用 Pod UID。
- 产品 ID 优先取 `sandbox.cri/sandbox-id`，缺失时使用 Pod UID。
- native UUID 只使用 AgentENV Runtime 返回值，不用产品 ID 或 CRI ID 替代。
- 创建前先写 RouteTable reservation；先持久化 `creating` 记录，再调用 Runtime。
- Runtime 返回 `Unknown/DeadlineExceeded` 时保留原生网络投影、operation ID 和 fingerprint，重试使用同一
  operation ID；确定失败或恢复失败则进入 `cleanup-pending` 由 reconciler 重试。
- 相同 Pod UID 的重复 RunPodSandbox 会先检查 Runtime，存在时直接返回且不再次创建。
- `CreateContainer` 同时接受原始 `agentenv.dev/*` 引用和 `PullImage/ImageStatus` 返回的
  `agentenv-image:<hash>` ID；后者会反查并保存原始虚拟镜像引用。

## 配置与契约

`Config.RuntimeAddress` 必填；`New` 还必须获得共享 `engine.RouteTable`。连接在首次调用时建立，
`Close` 关闭连接；`New` 不访问网络、不写状态。

支持的 CRI template annotation：

- `sandbox.cri/template`：必填，唯一模板入口。
- `aenv.dev/template-id`：可选 canonical template ID。
- `sandbox.cri/build`：可选 build ID。
- `sandbox.cri/sandbox-id`：可选产品 ID。
- `sandbox.cri/vcpu`、`sandbox.cri/memory-mb`、`sandbox.cri/disk-mb`。
- `sandbox.cri/metadata`：JSON string map。
- `aenv.dev/timeout-seconds`、`aenv.dev/auto-resume`、`aenv.dev/secure`、
  `aenv.dev/allow-internet`、`aenv.dev/network`、`aenv.dev/env-vars`、
  `aenv.dev/custom-extension-params`。
- CRI 路径强制 `auto_pause=false`；`aenv.dev/auto-pause=true` 返回 `InvalidArgument`。

## 限制

- 容器和镜像仍为 CRI 逻辑投影；本 backend 不执行 OCI 镜像，也不创建 Pod 之外的 microVM。
- `ExecSync` 已接入 Runtime 内部 `SandboxExecService`，通过 AgentENV Node Proxy 调用 guest envd；
  容器不存在或未运行、命令为空时不会触发 Runtime 副作用。
- CRI `Exec`、`Attach`、`PortForward` 通过 SPDY 或 WebSocket v5 提供 streaming，支持
  stdout/stderr、stdin、PTY、resize 和真实退出码。
- MuxServer 通过 `SubscribeContainerEvents` 聚合 `GetContainerEvents`；AgentENV engine 自身不直接
  提供 CRI event stream。
- 网络分配和回收全部由 AgentENV Runtime 完成，mux 只核验并保存原生网络事实。
- 非零 HostPort 返回 `InvalidArgument`，HostPort=0 的 containerPort 声明允许；保留代理和 PortForward。
- `agentenv.pods` 保存原生网络事实、容器投影和虚拟镜像引用；恢复时核验网络所有权。
- reconciler 收养 pending Runtime 并回收过期投影和 RouteRecord，不扫描或删除宿主 netns、iptables 规则。
- Pause 已接入内部 Runtime RPC；DEGRADED remediation 通过内部 `AdminRemediator` 提供，不进入 Admin v2；Checkpoint、CachedBuilds 尚未实现。ContainerStats/ListContainerStats 仍使用 sandbox 级真实 metrics。
- `UpdateContainerResources` 对 Firecracker 没有安全的在线 resize 语义，当前显式返回 `Unimplemented`。
- `ExecSync` 仅提供同步执行，不提供 TTY、stdin、resize、连接续传或交互式输出。
- `aenv.dev/*` schema 冻结前不得增加隐式 annotation 解析。
## 网络模式（2026-11-16 扩样验收）

`-agentenv-network-mode=runtime-managed` 为默认模式：网络分配和回收归 AgentENV Runtime，mux 只保存当前实例的网络事实，不执行 AgentENV CNI ADD/DEL，不创建额外 HostPort。需要 Runtime GetRuntime 返回 Running 实例的 `network.mode`、`network.pod_ip` 和 `netns_path`；正式 cri.14 二进制不具备此返回，需配套基于 cri.14 增加只读网络信息的 cri.15 补丁。

地址使用宿主交互 IP，而非 VM 内重复的 link-local 地址。访问沿用 AgentENV 节点代理；CRI Pod IP 和 PortForward 也使用活实例地址。带非零 HostPort 的 `port_mappings` 请求返回 InvalidArgument；仅声明 containerPort（HostPort=0）不创建端口绑定。保留 AgentENV 原有 Sandbox 隔离和节点本地路由语义，不声明跨节点 Pod 网络、Kubernetes DNS 或 NetworkPolicy 已完整支持。

AgentENV 的 CNI pool、直接 CNI ADD/DEL、HostPort 规则管理和旧池自动清理实现均已移除。`Config` 不再包含 CNI/HostPort 或网络切换字段；`-agentenv-require-cni`、`-agentenv-hostport-mode`、`-agentenv-cni-netns-prefix` 及旧 pool 参数不再接受。E2B/Android 的 CNI 能力不变。

升级前须用旧版正常排空 CNI Sandbox 并清理遗留资源。新版本识别到持久化 `cni_record` 或非空 `host_ports` 时拒绝恢复，不自动迁移、丢弃状态或清理网络；这些旧 JSON 字段只用于兼容性检查。历史性能对照和回退制品继续归档。

独立原生网络测试仍需独立 socket/state-dir，并隔离 E2B 前缀、关闭测试实例 orphan reconcile。原生安装态入口为 `42_agentenv_native_installed_e2e.py`。34/35 的 CNI 故障脚本及调用它们的历史套件不适用于本版本；本轮未执行故障、soak 或其他暂缓项目。

三路扩样各 120 个正式样本，四项既定门槛全部通过：原生网络启动均值 855.1 ms、P95 3035.8 ms、回收均值 399.9 ms；pool 分别为 1252.8、3908.9、2280.3 ms。全部 34 个超过 3 秒的启动均关联到 ublk 设备不可读后的长等待，原 cri.14 HTTP 控制组也复现。完整证据见工作区 `test-results/native-expanded-20261116`；源码默认值不代表节点已部署，最终安装状态以交接文档和上线审计为准。

用户协调维护窗口后已完成 cri.15＋原生网络安装态验收和上线。共享 mux 同时启用 AgentENV 与 Cube，原 30-cube.conf 和 Cube 二进制保留，新增 40-agentenv-cube-native.conf。组合构建源码、差异、280项校验证据及回退说明见工作区 test-results/native-expanded-20261116/coexist-evidence 和《原生网络安装态验收报告》。既有 CoreDNS 异常及其他暂缓项未处理；未宣称完成 Cube 独立实机回归。

2026-11-16 裁减版已完成安装态、回退后安装态和正式上线验收；共享 mux 保留 Cube 配置。在线裁减 mux SHA256 为 `61c807bdebffd7f5b0b34330a4a7a9424462d3363cd3f0e83457f355454bf009`。后续原生 v0.2.4 性能复测因节点内核异常中断，已恢复 cri.15，不代表完整性能复测通过。

整套部署、卸载回退、脚本索引与测试步骤见 [统一使用指导](../../../../docs/agentenv-runtime/README.md)。
