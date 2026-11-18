# KASandbox PR 准备与验证记录

更新：2026-11-18。本记录描述提交候选，不代表安装态验收或已上线。

## 分支与范围

- fork：`m0_73757964/KASandbox`，分支 `feat/agentenv-cri-native`。
- 上游目标：`openeuler/KASandbox:master`，当前基线 `24c24fda0357851e3af815597cb66ec2e2dc8e03`。
- 首个提交 `8d4d9e8`：AgentENV Runtime gRPC client、CRI 原生网络 engine、路由/状态适配及测试。
- 本轮追加：Webhook Provider、模板解析与准入测试、OpenAPI 维护响应及生成代码、部署/Helm 适配、离线配置测试和用户指导。
- 不包含 Runtime RPM/vendor、本地 .env、二进制、34–41 历史故障/安装脚本。早期共享 CNI/HostPort util 改动以及损坏 state 文件启动策略改动未纳入提交。
- 正常追加提交并推送，不改写历史、不强推；PR 由用户创建。

## 行为与兼容性

AgentENV CRI 默认关闭，网络归 Runtime；Webhook Provider 只按 AgentENV RuntimeClass 和 `sandbox.cri/template` 解析模板。模板请求不跟随重定向，限制响应大小，整个解析共用超时预算。沿用共享 Webhook 的 Provider 失败 fail-open 行为；解析失败不注入模板结果，最终由 engine/Runtime 校验。

Helm 的 AgentENV 准入规则单独开关、默认关闭，只匹配 AgentENV 普通 Pod；BatchSandbox 标签 Pod 使用已有入口。启用要求 Kubernetes >=1.30；旧部署未启用时仍渲染原两入口，API key Secret 仍可选。启用时预先校验 API key，Secret 原位更新、caBundle 动态一次更新全部入口，关闭开关不删除已有共享资源。

Webhook 容器使用 UID/GID 65532、只读根文件系统和禁用提权，不挂载 ServiceAccount token；新增 startupProbe 和 `minAvailable: 1` PDB。后者会影响自愿驱逐，使用方需保留可用副本。部署脚本有真实写操作和 Webhook Pod 重启；本轮只执行离线测试桩。

OpenAPI 仅增加三个 POST 的 503 / Retry-After 契约及生成客户端解析，不增加请求参数或必填项，不声称 KASandbox 已实现 AgentENV 的维护屏障。九个重名接口的请求契约逐项核对。

## 验证方式

Linux aarch64，Go 1.25.4；非 root、nice 15、ionice idle、GOMAXPROCS=2，独立目录。Webhook 使用全模块 mock 测试；API 只测试受影响的生成契约包，不启动 DB、API server、Runtime 或 VM。

```bash
cd cri-multiplex
GOWORK=off go test -count=1 -p 2 ./...
GOWORK=off go vet -p 2 ./...
GOWORK=off go build -p 2 ./...
cd ../e2b-webhook
GOWORK=off go test -count=1 -p 2 ./...
GOWORK=off go vet -p 2 ./...
GOWORK=off go build -p 2 ./...
cd ../packages/api
GOWORK=off go generate ./internal/api
GOWORK=off go test -count=1 -p 2 ./internal/api
cd ../..
python3 deploy/tests/test_agentenv_webhook.py
python3 scripts/check-agentenv-contract.py --agentenv /path/to/AgentENV/src/api/openapi.yml
```

首个 CRI 提交已全量通过。其余最终命令与结果见本目录 `VALIDATION.md`。Windows 本地执行 Go 测试和生成器被 Application Control 阻止，改在 Linux 验证；Linux 初始依赖缓存不全，所需依赖补到专用目录，不安装宿主软件。部署桩初次运行因 Windows 检出 CRLF 失败，测试源码恢复 Git 中的 LF 后复跑。

## Cube 与暂缓项

本次上游基线尚未包含同事 Cube 分支。历史组合验证对应 `607735b3ebca491c5c3824ce350ad658d0c60dae`；本轮未重新整合该分支。后续合并须保留 mux main、EngineType、Webhook main 的双方注册入口，不能用独立候选覆盖共享 Cube 制品或资源。

测试前后 aenv、cri-multiplex、e2b-webhook-agentenv 保持 inactive，Cube `30-cube.conf` 哈希保持 `1814061bfd16d6a8dfef1dc897baf55360031471f2d9b33ace3fdf2cd23c79e9`。

未执行线上切换、VM/真实调用方联调、真实 Helm/kubectl 部署、故障注入、节点重启/断电、x86_64 实机、Cube 独立回归或其他已暂缓项目。平台 CI 需用户创建 PR 后运行。
