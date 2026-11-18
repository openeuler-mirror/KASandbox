# AgentENV CRI 接入

## 范围

在 registry 中新增默认关闭的 AgentENV backend，通过 Runtime gRPC 管理 Sandbox，并提供 CRI 容器、镜像、Exec/Attach/PortForward、统计及事件适配。容器和镜像是 Runtime Sandbox 的逻辑投影，不是 OCI 容器运行时。

共享 RouteTable 保存 CRI、产品和原生 Sandbox ID；可选组合持久化将 route 与 backend state 合并为一次 snapshot。旧字段兼容，旧后端构造方式保留，损坏状态文件处理保持上游行为。

AgentENV 复用 Runtime 原生网络，不使用 CNI pool/direct 或额外 HostPort。本提交不改变共享 CNI/HostPort util，不包含 Webhook、OpenAPI、部署脚本、Runtime RPM、预编译二进制或故障注入脚本。

## 启用与回退

准备 AgentENV Runtime RPM 0.2.4-15.10 配套服务、有效模板及 Runtime Unix socket。在现有 mux 启动参数后追加：

```text
-agentenv-enabled=true
-agentenv-runtime-address=unix:///run/agentenv/agentenv-runtime.sock
-agentenv-network-mode=runtime-managed
```

请以实际 Runtime socket 配置为准。CRI 请求使用 RuntimeHandler `agentenv` 和必填 annotation `sandbox.cri/template`。详细注解与限制见 [engine README](pkg/engine/agentenv/README.md)。不要对有运行中 Sandbox 的共享 mux 直接替换二进制或重启。

回退前停止新 AgentENV 请求并通过当前版本清理其 Sandbox，再在维护窗口恢复旧 mux 二进制及原参数；保留 state 备份，不直接删除共享 state。仅关闭 AgentENV 参数不能替代 Sandbox 清理。

## 验证（2026-11-18）

基线 `24c24fda0357851e3af815597cb66ec2e2dc8e03`，Go 1.25.4，GOWORK=off。

- Linux aarch64：非 root、nice 15、ionice idle、GOMAXPROCS=2；依赖缓存离线使用。
- `go test -count=1 -p 2 ./...`：通过，涵盖 AgentENV、共享 route/state、mux 及现有后端 mock 回归。
- `go vet -p 2 ./...`、`go build -p 2 ./...`：通过。
- Windows 上以 GOOS=linux、GOARCH=amd64 执行 `go build -p 2 ./...` 和 `go vet -p 2 ./...`：通过；不是 x86 实机测试。
- 未启动 Runtime/VM，未运行安装态脚本或 opt-in 网络/故障测试；共享服务保持原状态。

安装态测试脚本为 `e2b-verify/42_agentenv_native_installed_e2e.py`，仅在已获准的专用测试窗口使用；本轮未执行。

## Cube 共存与后续 PR

本分支未包含同事尚未合入当前上游基线的 Cube backend。双方合并时须同时保留 `cmd/cri-multiplex/main.go` 的注册/参数及 `pkg/engine/engine.go` 的 EngineType。不得使用本分支二进制覆盖正在运行 Cube 的组合制品。现场 Cube 配置未修改。

本次只推送 CRI 分支，Webhook、OpenAPI 及统一部署交付留在原准备目录，后续单独审查提交。平台 CI 结果以创建 PR 后实际运行结果为准。
