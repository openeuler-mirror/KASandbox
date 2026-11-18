# 配套交付验证结果

日期：2026-11-18。目标分支：`feat/agentenv-cri-native`。本轮完成源码验证与推送准备，不创建 PR，不进行线上部署。

| 检查 | 结果 |
|---|---|
| CRI 全模块 mock / vet / build | 通过，首个提交 `8d4d9e8` 的验证，本轮只补充其文档 |
| Webhook 全模块 `go test -count=1 -p 2 ./...` | 通过，Linux aarch64；live API opt-in 未启用 |
| Webhook `go vet -p 2 ./...`、`go build -p 2 ./...` | 通过，Linux aarch64 |
| Webhook Linux amd64 交叉 build / vet | 通过，Go 1.25.4；不等于 x86 实机回归 |
| `go generate ./internal/api` | 通过，oapi-codegen v2.5.1；二次离线生成 SHA256 一致 |
| API `go test -count=1 -p 2 ./internal/api`、`go vet -p 2 ./internal/api` | 通过，包含完整内嵌 OpenAPI 校验与三个 503 客户端解析用例 |
| 两仓九接口 | 请求参数、requestBody、必填要求及三个 POST 维护响应一致 |
| Helm / 部署桩 | 9 个测试通过；2/3/4 入口子场景通过，Helm v3.14.3 |
| Shell 语法、gofmt、git diff --check、文档链接与敏感信息检查 | 通过 |
| 本地与 Linux 测试输入 | 16 个代码/配置输入 SHA256 一致 |

生成的 `packages/api/internal/api/api.gen.go` SHA256：`5d36381fd47a306b1d8bdfc237feb49931778be34a9dad98cd6ac3e291f61a5c`。变动包括 503 类型/解析与压缩的内嵌 Swagger，不含新增请求必填字段。

## 回归覆盖

Webhook：alias 与直接 ID、指定 ready build、最新 build 未 ready、缺失模板、canonical 冲突、不回退到镜像/TEMPLATE_NAME、鉴权只读请求、拒绝重定向、异常响应不泄露响应体/key、响应大小上限、超时/取消及多请求共用超时预算。通过真实 AgentENV Provider 与 httptest 模板 API 验证 AdmissionReview，未知 Cube RuntimeClass 放行且不改写；不声称已完成 Cube Provider 整合。

Helm/部署：AgentENV 默认关闭兼容旧两入口，启用时只针对 AgentENV；非 root 与 Secret 要求；旧 Kubernetes 仅在启用新规则时拒绝渲染；禁用时不删资源；缺 key 在变更前失败；动态 caBundle 原子 patch；API key 文件 0600、不出现在命令参数；失败清理临时文件且不重启。kubectl/openssl 全部替换为桩，无真实集群写操作。

## 环境与限制

Linux 验证使用非 root、nice 15、ionice idle、GOMAXPROCS=2、独立目录。首次缓存不足导致 API 生成失败，网络下载停滞后停止本轮下载进程，复用本地模块缓存完成离线生成和测试，未修改依赖版本。Windows Application Control 阻止本地测试/生成器，故使用 Linux 结果。部署桩初次有 CRLF 和日志文件权限问题，修正测试副本/日志路径后最终复跑通过；不是部署验收。

测试前后 aenv、cri-multiplex、e2b-webhook-agentenv 均保持 inactive；Cube `30-cube.conf` SHA256 一致：`1814061bfd16d6a8dfef1dc897baf55360031471f2d9b33ace3fdf2cd23c79e9`。

未执行镜像运行/真实 Helm 安装、服务重启、线上切换、VM、真实 OpenFuyao 联调、故障注入或其他已暂缓项目。未创建 PR，平台 CI 尚未运行。历史安装态结论不替代当前提交的线上验收。

验证日志归档 SHA256：`825de7bad5c762f737ceab9ba4db8abd67a58da56e527c0a2228f1dda4035753`。归档保存在交付工作区，不把编译日志或二进制混入源码仓。
