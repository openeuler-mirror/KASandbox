# E2B Self-Hosted Test Suite

面向自托管 E2B（KASandbox）的测试工具，统一入口提供三类能力：

| 能力 | 命令 | 说明 |
| --- | --- | --- |
| 环境验收 | `bash start.sh test-e2e --all` | 116 个 E2E 用例，覆盖控制面、数据面与 Python SDK |
| 性能测试 | `bash bench.sh <子命令>` | 创建、规模、密度、快照、回滚、克隆、暂停恢复基准 |
| 密度测试 | `bash bench.sh replay-matrix ...` | 回放真实 agent 轨迹，按并发档位加压寻找宿主机密度拐点 |

每次运行以 `run_id` 标记所创建的资源，清理只作用于本轮资源。

## 快速开始

要求：Linux（建议在 API 节点运行）、Python ≥ 3.10；E2B SDK（`e2b>=2.19.0,<3`）首次运行时自动安装。

```bash
cd e2b-scripts
cp e2b-self-hosted.env.example .env && chmod 600 .env   # 填写下表必填项
bash start.sh                                            # 只读自检：API 连通、Template 查询、本地依赖
```

| 变量 | 必填 | 说明 |
| --- | --- | --- |
| `E2B_API_KEY` | 是 | Team API Key；API 节点上可从 `~/.e2b/config.json` 的 `teamApiKey` 自动读取 |
| `E2B_E2E_BASE_IMAGE` | E2E 必填 | `template-manager` 可拉取的 Template 基础镜像 |
| `E2B_API_URL` | 非 API 节点必填 | 默认 `http://127.0.0.1:3000` |
| `E2B_DOMAIN` / `E2B_HTTP_SSL` / `E2B_PROXY_PORT` / `E2B_SANDBOX_URL` | 否 | 数据面地址、TLS、client-proxy 端口（默认 `3002`）、固定数据面入口 |

也可通过 `bash start.sh --env-file <path> <子命令>` 使用项目外的配置文件。

## 环境验收

```bash
bash start.sh test-e2e --all                             # 构建本轮 fixture Template 后执行全部用例
bash start.sh test-e2e --all --template <template>       # 使用已有 Template
bash start.sh test-e2e --case SB-004 --case UP-007       # 执行指定用例，自动补齐前置依赖
```

覆盖范围：Sandbox 与 Template 的创建和查询、命令执行、文件上传下载、后台命令与流式输出、Filesystem 与 Watcher、Snapshot、Checkpoint / Restore、Pause / Resume / Connect、PTY、Template SDK。用例定义见 `e2b_validator/e2e_test_cases.py` 与 `e2b_validator/e2e_extended_cases.py`。

| 状态 | 含义 |
| --- | --- |
| `PASS` | 符合预期（负向用例被正确拒绝也记为通过） |
| `FAIL` | 行为与预期不一致 |
| `BLOCKED` | 部署缺少所需能力，或资源、网络、镜像问题阻断验证 |
| `SKIPPED` | 前置用例未通过或可选能力缺失 |

失败用例会输出失败阶段、关键证据和只读排查命令；输出重定向或由 Agent 调用时，额外输出单行 `AGENT_DIAGNOSTIC=<json>`（`E2B_AGENT_OUTPUT=0/1` 可强制关闭或开启）。

资源操作也可以单独调用：`list-sandboxes`、`list-templates`、`create-sandbox`、`create-template`、`run-command`、`upload-file`、`download-file`，参数见 `bash start.sh <子命令> -h`。

## 性能测试

| 子命令 | 测量内容 |
| --- | --- |
| `create` | 并发创建耗时分布与吞吐 |
| `scale` | 同一模板一次拉起 N 个沙箱的整批耗时 |
| `density` | 分批累积存活沙箱，按 cgroup / PSS / free 三种口径计量内存 |
| `snapshot-concurrency` / `snapshot-dirty` / `create-from-snapshot` | 并发快照、脏页快照、从快照恢复 |
| `rollback` / `clone` / `pause-resume` | 回滚、克隆、暂停恢复 |
| `all` | 串行执行全部测试项并生成汇总报告（`--profile quick` / `full`） |
| `kill-all` / `clean-host` | 清理 bench 创建的沙箱 / 清理宿主残留的 netns、veth 与 iptables 规则 |

```bash
bash bench.sh all --print-config --profile full   # 查看合并后的生效配置
bash bench.sh create -c 50 -n 500
bash bench.sh all --profile full
bash bench.sh kill-all
```

- 统计口径：avg / p50 / p90 / p95 / max（ms）、整批 wall、吞吐、成功率；创建类操作另外输出服务端口径 `server_*`（沙箱 `startedAt` 减去请求发出时刻，需与 API 同机运行）。
- 参数优先级：命令行 > `--config` 指定文件 > `bench.toml` > 代码默认值。未指定模板时自动复用或构建 `bench-standard-2c2g`。
- 各档位串行执行，档间等待残留清理收敛；`test-e2e` 与 `bench` 通过 `test-results/.run.lock` 互斥。

## 密度测试

在各自的任务镜像中按原始时间间隔回放真实 SWE agent 轨迹，观察并发升高时命令延迟的变化。提供两种回放模型：

| 子命令 | 模型 | 适用场景 |
| --- | --- | --- |
| `replay-nolifecycle` / `replay-matrix` | 创建 → 按轨迹间隔逐条执行命令 → 删除，全程保持运行 | 沙箱常驻运行时的宿主机密度与拐点 |
| `replay` | 创建后立即 pause，每步 resume → 执行 → pause，RUNNING 名额受限 | paused + running 混合形态的承载能力 |

### Catalog

`catalogs/*.json` 描述参与回放的 workload，每项对应一条轨迹和一个任务镜像；模板名为 `swr60-<name>`，规格 2 vCPU / 4 GiB。

```json
{
  "name": "prettier-plugin-pug-448",
  "image": "<registry>/swerebench-arm64-prettier-plugin-pug:448-2d9f897",
  "workdir": "/plugin-pug",
  "instance_id": "prettier__plugin-pug-448",
  "replay_file": "prettier__plugin-pug-448__<uuid>.replay.json",
  "env": {"CI": "true"}
}
```

`env` 为可选字段，为该 workload 的每条命令注入环境变量，例如 `CI=true` 让 vitest 以单次运行模式执行而不进入 watch 模式。

### 执行

```bash
CATALOG=catalogs/swerebench-arm64-62.json
TRAJ=<轨迹目录>

bash bench.sh replay-provision --catalog $CATALOG                  # 检查 / 构建全部任务模板
bash bench.sh replay-nolifecycle --catalog $CATALOG --trajectory-root $TRAJ --dry-run
bash bench.sh replay-matrix --catalog $CATALOG --trajectory-root $TRAJ \
  --tiers 124,372,496,744 --baseline-p95-ms <基线p95> --command-timeout-continue
```

- 每档 `并发数 = 任务数 = 档位值`，各 workload 按平滑加权轮询交错发射。
- 命令以 `root` 在 workdir 下执行，超时可选 10 / 30 / 300 秒（默认 300）。非零退出码属于负载本身的结果，只记录不中断；超时默认终止该任务，加 `--command-timeout-continue` 则记录后继续。
- 某档 command p95 超过基线的 `--degradation-multiplier` 倍（默认 3）即判定为拐点；执行失败、成功率低于 `--min-success-rate` 或清理核验失败同样终止矩阵。档间默认冷却 180 秒。
- `--sandbox-timeout`（默认 3600 秒）必须大于最长任务耗时，否则长任务会被平台提前回收。

### 结果解读

结果写入 `test-results/<run_id>-bench/`，包括 `report.md`、`matrix.json` 和 `tier-<N>/replay-result.json`（逐任务、逐步明细）。

- 以 command p95 和单任务耗时分布为主要指标。墙钟时间等于最慢任务的耗时，容易被个别长尾 workload 主导。
- 基线应在无网络异常、无超时伪影的低档位上实测得到。
- 运行前确保网络池容量（`NETWORK_POOL_REUSED_SLOTS_SIZE`）不小于档位值，并先完成预热；任务镜像应内置轨迹所需的依赖，避免运行时下载外网依赖失败或超时扭曲结果。

## 结果与退出码

| 场景 | 输出目录 | 主要文件 |
| --- | --- | --- |
| 环境验收 | `test-results/<run_id>/` | `report.md`、`result.json`、`resources.json`、`commands.log`（已脱敏） |
| 性能测试 / 密度测试 | `test-results/<run_id>-bench/` | `bench_*.json`、`report.md`、`matrix.json`、`cleanup.log` |

| 退出码 | 含义 |
| ---: | --- |
| `0` | 成功，或全部用例符合预期 |
| `1` | SDK / 网络 / 服务端错误，或存在 `FAIL` / `BLOCKED` |
| `2` | 参数或配置错误 |
| `130` | 用户中断 |
| 其他 | `run-command` 透传的远端命令退出码 |

## 故障排查

| 现象 | 排查方向 |
| --- | --- |
| `401 Invalid API key` | `.env` 中残留旧 Key，覆盖了自动发现结果 |
| `500 Failed to place sandbox` | Nomad allocation、template-manager、Firecracker / cgroup 资源 |
| `template builder not found` | template-manager 服务注册、Registry 镜像是否可拉取 |
| 命令退出码 124 / 126、文件摘要不一致 | Template 默认用户、Shell、目录权限、client-proxy 数据面 |
| 回放中某类命令稳定卡满超时 | 沙箱内在线下载依赖被网络阻断；在沙箱内对比联网与离线执行耗时 |

当安装的 SDK 缺少同步 `pause()` / Snapshot API，或 `Sandbox.connect()` 引用了未定义的 `envd_version` 时，脚本会启用内置的兼容实现，并在 stderr 输出 `E2B SDK compatibility fallback active`。不会修改已安装的 SDK。
