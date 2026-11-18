# AgentENV 低级运行时交付与使用指导

更新日期：2026-11-18。适用：原生 AgentENV v0.2.4 → Runtime 0.2.4-15.10（cri.15）+ 裁减后的 CRI 原生网络 mux。

## 从哪里开始

1. [安装部署与日常使用](DEPLOY.md)：组件、构建、独立节点、Cube 共存、Webhook、Pod 示例和健康验收。
2. [升级、卸载与回退](ROLLBACK.md)：维护窗口、三种回退、资源保留、故障恢复。
3. [测试执行指导](TESTING.md)：脚本入口、依赖、参数、结果解释和暂缓范围。
4. [脚本清单](SCRIPTS.md)：两仓文件位置，避免复制出多套安装器。
5. [本轮验证结果](VALIDATION.md)：命令、通过项与未执行范围。

本文档中的 AENV_REPO 指已应用 Runtime 补丁的 AgentENV 源码根（可使用源码 RPM 完成 %prep 后的目录），不能直接使用未打补丁的原生 v0.2.4；KA_REPO 指 KASandbox 仓库根，由使用者设置为各自源码目录。实际命令在 Linux 目标环境执行，先替换示例路径、节点、模板和证书。不可把测试节点凭据放入交付包。

## 当前交付状态与边界

当前提交在 `feat/agentenv-cri-native` 分支，包含 CRI、Webhook、OpenAPI、Helm 和使用指导。Runtime RPM 在独立 [打包仓](https://atomgit.com/src-openeuler/AgentENV) 交付。下列安装态和性能数据仅是历史记录，本轮不重新上线或运行 VM。

- 2026-11-16 的 cri.15 + 原生网络裁减 mux 安装态、回退后安装态与上线验收通过。这是历史验证记录，不代表当前节点服务状态或本次 PR 候选已经上线。
- Runtime server SHA256：`ccbf0ac35b89e213bbac56a81309a87424dd8e8cc24999d681546410c3f5d8b7`。
- Cube 共存 mux SHA256：`61c807bdebffd7f5b0b34330a4a7a9424462d3363cd3f0e83457f355454bf009`。
- AgentENV 不再执行 CNI ADD/DEL、维护 CNI pool 或额外 HostPort；E2B/Android 的 CNI 功能不在本轮裁减范围。
- Runtime RPM 仅管理 Runtime。mux、Webhook、RuntimeClass、准入配置由部署人员分别管理；本文提供全流程，不宣称存在通用的一键整栈安装器。
- 本地 mux 主干未包含 Cube 装配入口。共存节点必须使用含 Cube 后端的已验收组合制品，不能用本地普通构建替代。
- 原生网络保持 AgentENV 节点本地语义；不承诺跨节点 Pod 网络、集群 DNS、NetworkPolicy 等价于标准 Kubernetes CNI。非零 HostPort 拒绝；容器是逻辑投影，不运行任意 OCI 镜像。
- 最新原生 v0.2.4 对照复测因启动 HTTP 500 和节点既有内核 list_add corruption 告警中断，已恢复 cri.15；不能把该轮写成完整性能验收通过。

历史制品和报告保留在工作区 test-results/native-expanded-20261116、test-results/native-final-perf-20261116。运行安装前须单独确认当前节点内核、磁盘及业务状态。

提交准备和 Cube 共存版本关系见 [PR 准备记录](PR-PREPARATION.md)。
