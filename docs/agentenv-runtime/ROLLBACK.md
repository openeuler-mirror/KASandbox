# 升级、卸载与回退

## 先确定回退范围

| 目标 | Runtime | mux / Webhook | 原生数据与 Cube |
|---|---|---|---|
| 回退本次 mux 发布 | 保留 cri.15 | 恢复前一配套 mux，配置按发布前备份核对 | 全部保留 |
| Runtime 降级 | 切换到已验收版本对 | 按该 Runtime 能力使用对应 mux | 全部保留，不能任意跨 schema 降级 |
| 卸载 AgentENV CRI 接入 | 恢复原生 v0.2.4 并移除 Runtime RPM | 移除仅属于本接入的配置；共享 mux 保留其他后端 | 模板、缓存、业务数据、Cube 均保留 |

cri.14 不提供 cri.15 的原生网络事实，不能将当前原生网络 mux 搭配 cri.14。回退旧 CNI 版本也不是删除现有网络后直接切换，必须有单独配套方案与维护窗口。

## 1. 维护与快照

停止 AgentENV 调用方投递，等待 Sandbox（含 paused）和在途任务清空。若要重启共享 mux，同时确认其他后端已进入协调窗口。保存 unit/drop-in、二进制哈希、state-dir、Pod/service 基线和原生备份；不公开含凭据配置。

Runtime 包切换由管理工具负责进入/退出维护，不要先手工 enter 再 activate（管理工具会拒绝接管既有维护标记）。仅 mux 发布使用手工屏障：

```bash
export AENV_HOME_PATH=/var/lib/aenv
export AENV_CONFIG_PATH=/var/lib/aenv/config/config.toml
/usr/local/bin/server maintenance status
/usr/local/bin/server maintenance enter --timeout-seconds 60
```

超时保持阻止新建，不能直接删 blocked 文件或强行继续。维护屏障不阻止其他后端请求，也不自动迁移业务。

## 2. 仅回退 mux

校验旧二进制来源、SHA256、架构和与当前 Runtime/共享 state 的兼容性；复查无并发改配置。备份当前文件，用同目录暂存文件原子替换受管 mux 路径，再正常重启 cri-multiplex。除该受管二进制及本次确有改动的配置外，不覆盖其他文件。

```bash
systemctl restart cri-multiplex
crictl --runtime-endpoint=unix:///run/cri-multiplex.sock info
/usr/local/bin/server runtime-health --socket /run/aenv/agentenv-runtime.sock
# 确认运行进程 /proc/<MainPID>/exe 哈希等于预期，而不只校验磁盘文件。
/usr/local/bin/server maintenance exit
```

仅在恢复成功、无 pending、资源归属明确后退出维护并运行第 42 项。恢复失败保留维护，保留日志，由操作者修复；不在 finally 中无条件解锁。当前节点前一 mux 为部署审计目录中的 previous-mux，SHA256 aa59e107300c61d47eb1d7ccb1e48ff4f2ce6d01147a7f5c02225acc5b9bfdca；其他节点不能照抄此路径或哈希。

## 3. Runtime 升降级与中断恢复

```bash
# 升级：先暂存，再激活，包文件替换为经审核的配套制品。
rpm -Uvh /path/to/new-runtime.rpm
agentenv-runtime-package activate --drained
# 降级仅限已验证版本对；涉及原生/旧版加 --legacy-drained 并停止调用方。
rpm -Uvh --oldpackage /path/to/previous-runtime.rpm
agentenv-runtime-package activate --drained --legacy-drained
agentenv-runtime-package status
rpm -V agentenv-cri-runtime
```

active 与 staged 必须分别核对。激活失败会恢复前一运行版本，但 RPM 数据库可能仍为新暂存版本。存在 pending 时拒绝再次激活：

```bash
agentenv-runtime-package status
agentenv-runtime-package recover --drained --legacy-drained
```

仅在排空和备份完整时恢复；备份损坏则停止，不删除 pending 绕过检查。recover 后核对服务/版本，再决定重试或退回暂存包。真实断电/重启验证仍未执行。

## 4. 整套卸载 CRI 接入，保留原生 AgentENV

1. 停止 AgentENV 调用方和工作负载控制器的新建；等待全部 AgentENV Pod/Sandbox 清理。不要删 namespace 或其他业务 Pod 来“排空”。保存本次资源清单。
2. 只移除本次拥有、专用于 AgentENV 的准入注册和 RuntimeClass；共享 Webhook 配置仅移除 AgentENV 对应条目，保留其他后端。先备份，再按确切名称删除；不要批量删除所有 MutatingWebhookConfiguration。
3. Cube 共存节点：核对 30-cube.conf 与 Cube 二进制仍有效，移除本次拥有的 40-agentenv-cube-native.conf，使 Cube 配置重新生效；保留 shared state-dir、socket、其他 drop-in。正常 daemon-reload 和一次 mux 重启需要共享窗口。独立 AgentENV 节点：如果 kubelet 仍连接 mux，不能先停 mux；应先在该独立节点的授权窗口把 endpoint 恢复部署前值并核验，或保留 mux 的 containerd 转发功能。本项目共享测试节点不重启 kubelet。
4. 仅当 Webhook 服务专用于本接入且已移除对应准入注册时，停止/禁用该服务并归档本次二进制、unit、TLS 文件；共享服务不停止。不要删除他人创建的证书。
5. 恢复原生并卸载 Runtime RPM：

```bash
agentenv-runtime-package deactivate --drained --legacy-drained
# 先确认原生 HTTP、版本、模板和 server 哈希，确认 Runtime UDS 已撤销。
aenv --version
aenv list --output json
rpm -e agentenv-cri-runtime
```

6. 检查原生 HTTP 可用、Node/CRI 健康、Cube 与其他业务配置和状态未变；保留原生备份、审计日志、模板与数据。仅清理本次已确认无人引用的制品，不删除 /var/lib/aenv、共享 mux state、netns 或 iptables。

直接 rpm -e 在激活状态会拒绝。deactivate 保留当前 service、环境文件和操作者配置；历史 tar rollback.sh 默认恢复备份配置，共享节点若使用它须明确评估 --preserve-local-config。卸载 CRI 接入不等于卸载原生 AgentENV。

## 5. 验收与异常处理

成功标准：实际二进制/版本匹配目标，HTTP/Runtime（若保留）可用，maintenance blocked=false，pending=none，Node Ready，CRI Ready，测试资源收敛，Cube 配置及其他业务基线不变。恢复原生后 Runtime socket 不存在是预期，不能继续用 runtime-health 判断原生健康。

若 kernel corruption、磁盘满、未知活动 Sandbox、配置被并发修改或回退哈希不符，停止后续测试并保留证据，不自行重启节点、扩大权限或进行故障注入。

## Helm / Webhook 单独回退

在维护窗口恢复部署前同一管理方式的 Helm values/版本或独立清单，以及配套 Webhook 二进制、证书和 API key Secret；按原始清单恢复准入规则及 caBundle。不要删除共享 `e2b-webhook` 配置或覆盖 Cube Provider 配置。仅将 `webhook.agentenv.enabled=false` 并使用原管理方式更新清单，会移除新增 AgentENV 入口，保留既有两入口；不要把关开关当作停止 AgentENV CRI 业务。

如果要卸载整套 Webhook，先确认 E2B、Android、Cube 等调用方均不再依赖，再由资源所有者执行卸载。本 PR 不提供默认删除共享资源的自动脚本。保留 RuntimeClass、模板、Secret 和 state 的备份；不要随意删除命名空间。
