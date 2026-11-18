# 脚本与文档索引

脚本保留在原仓库，避免复制后产生版本漂移。路径均相对各自仓库根。

| 仓库 | 路径 | 用途 |
|---|---|---|
| AgentENV 源码 | scripts/release/agentenv-cri-runtime-patch/build-package.sh | 历史 tar 补丁工具，不用于当前正式 RPM |
| AgentENV 源码 | scripts/release/agentenv-cri-runtime-patch/build-rpm.sh | 历史 tar 打包工具，不替代当前源码 RPM spec |
| AgentENV | scripts/release/agentenv-cri-runtime-patch/install.sh | tar 补丁安装；有 dry-run，不具备 RPM 全流程维护屏障保证 |
| AgentENV | scripts/release/agentenv-cri-runtime-patch/rollback.sh | tar 安装回退；默认恢复备份配置 |
| AgentENV | scripts/release/agentenv-cri-runtime-patch/rpm-control.sh | 包内 agentenv-runtime-package 的来源；activate/deactivate/status/recover |
| AgentENV | scripts/release/agentenv-cri-runtime-patch/RPM.md | RPM 详细语义和维护屏障 |
| AgentENV | scripts/tests/run-release-regression.py | 按 suite 选择回归，输出 JSON/JUnit |
| AgentENV | scripts/tests/internal-api-regression.py | 九个内部接口 HTTP 用例 |
| AgentENV | scripts/tests/validate-internal-contract.py | 两仓 OpenAPI 与实际响应校验 |
| AgentENV | scripts/tests/test-maintenance-contract.py | 九接口维护响应契约负例 |
| AgentENV | scripts/tests/maintenance-cli-regression.py | 临时目录内维护 CLI 检查 |
| AgentENV | scripts/tests/rpm-maintenance-regression.py | 隔离服务桩的无故障维护测试 |
| AgentENV | scripts/tests/rpm-isolated-regression.py | 包含进程故障注入，不进入默认共享节点测试 |
| AgentENV | scripts/tests/RELEASE-REGRESSION.md | 九接口与 RPM 场景矩阵 |
| 打包仓 src-openeuler/AgentENV | agentenv.spec、docs/OFFLINE-BUILD.md | 当前 15.10 正式离线 RPM 构建入口 |
| KASandbox | deploy/tests/test_agentenv_webhook.py | Helm 离线渲染和部署命令桩回归 |
| KASandbox | scripts/check-agentenv-contract.py | 两仓九接口入参/必填及维护响应离线核对 |
| KASandbox | cri-multiplex/e2b-verify/42_agentenv_native_installed_e2e.py | 当前原生网络真实安装态测试 |
| KASandbox | cri-multiplex/e2b-verify/portforward-probe/main.go | 第 42 项所需 PortForward helper，已从历史证据正式归档 |
| KASandbox | cri-multiplex/pkg/engine/agentenv/*_test.go | CRI、网络所有权、HostPort 拒绝、状态恢复等单元测试 |
| KASandbox | e2b-webhook/providers/agentenv_test.go | 模板解析与注解校验 |
| KASandbox | cri-multiplex/pkg/engine/agentenv/README.md | 参数、能力与限制 |

34/35 为旧 CNI 故障用例，38 是故障套件，40/41 是历史安装态入口，均不是当前原生网络默认入口。36 soak、37 Runtime crash、39 remediation 仅保留历史用途，暂缓项目不执行。历史 rollout.py 含节点固定路径、哈希和基线，不能作为新节点通用安装器重跑。

本次 PR 不携带 34–41 历史 AgentENV 测试脚本；上文编号用于说明历史边界，当前安装态入口为 42，旧脚本仍保留在开发工作区。
