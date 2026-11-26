# KASandbox

KASandbox 是鲲鹏沙箱场景下围绕 e2b-infra 的自有运行时组件仓库。e2b 编排栈
（api / orchestrator / envd / client-proxy）由 e2b-infra 包提供，本仓库只维护
KASandbox 特有的部分：

- **cri-multiplex**：CRI 多路复用器（本仓库核心）
- **e2b-webhook**：Kubernetes 准入 Webhook
- **deploy / helm**：部署脚本与 Helm Chart

## cri-multiplex

cri-multiplex 是一个 CRI shim，把 CRI 请求多路复用到多种沙箱运行时：
containerd（默认容器）、e2b（Firecracker / StratoVirt 微虚拟机）、AgentENV
等，并为 e2b / AgentENV 沙箱提供节点级的 Pause / Checkpoint / Admin 能力。

目录结构：

- `cri-multiplex/proto/`：接口定义（`orchestrator.proto` 拷贝自 e2b-infra 上游，须保持同步）
- `cri-multiplex/pkg/`：生成的 pb.go 与各后端实现
- `cri-multiplex/cmd/cri-multiplex`：主程序

常用命令：

```bash
./build.sh                       # 构建 e2b-webhook
./build.sh -c                    # 构建 cri-multiplex
./run-ut.sh                      # cri-multiplex 单元测试
./check_interface.sh [BASE] [TARGET]   # cri-multiplex 接口门禁（默认 HEAD^/HEAD）
```

`check_interface.sh` 会拦截破坏性的 proto 变更（字段号/名改绑、删除未 reserved
的字段、删除 RPC/文件）以及生成代码与 proto 的不同步，修改
`cri-multiplex/proto/` 后必须通过该门禁。

## 部署

出包时，KASandbox rpm 安装 cri-multiplex、e2b-webhook 与部署资产到
`/opt/e2b-infra`；编排栈由 e2b-infra rpm 包补充。安装顺序：先装 KASandbox，
再装 e2b-infra，然后执行 `/opt/e2b-infra` 下的部署脚本（详见 `deploy/`）。
