# E2B 自托管验收与性能测试脚本

面向自托管 E2B（KASandbox）的统一测试工具，覆盖三类场景：

| 场景 | 入口 | 用途 |
| --- | --- | --- |
| 功能验收 | `bash start.sh test-e2e --all` | 116 个真实 E2E 用例，覆盖控制面、数据面与 Python SDK，用于上线验收与升级回归 |
| 性能测试 | `bash bench.sh <子命令>` | 创建、规模、密度、快照、回滚、克隆、暂停恢复等基准测试 |
| 真实轨迹密度测试 | `bash bench.sh replay-matrix ...` | 用真实 SWE agent 轨迹回放，按档位加压寻找宿主机密度拐点 |

每轮运行以 `run_id` 隔离资源，清理只处理本轮创建的对象，不影响运行前已存在的 Sandbox / Template。

## 目录

- [1. 快速开始](#1-快速开始)
- [2. 配置](#2-配置)
- [3. 功能验收（test-e2e）](#3-功能验收test-e2e)
- [4. 性能测试（bench）](#4-性能测试bench)
- [5. 真实轨迹密度测试（replay-nolifecycle / replay-matrix）](#5-真实轨迹密度测试replay-nolifecycle--replay-matrix)
- [6. 结果与判定](#6-结果与判定)
- [7. 故障定位](#7-故障定位)
- [8. 退出码](#8-退出码)

## 目录结构

```text
e2b-scripts/
├── start.sh / start.py          # 统一入口：环境初始化、依赖安装、子命令转发
├── bench.sh                     # 性能测试入口（等价于 start.sh bench）
├── bench.toml                   # bench 全部参数的单一配置来源
├── e2b-self-hosted.env.example  # .env 配置模板
├── prepare-replay-image.sh      # 构建 bench replay 任务镜像（django-money）
├── catalogs/                    # 真实轨迹密度测试的镜像目录（workload 清单）
├── templates/replay62/          # replay62 中需修复的模板 Dockerfile 及说明
├── docs/
│   ├── E2E_CASES.md             # 116 个 E2E 用例清单
│   └── BENCH.md                 # bench 指标口径、计时方式、宿主清理等详解
├── e2b_validator/               # 实现代码（由入口加载，无需单独执行）
│   ├── bench/                   # 性能测试与轨迹回放
│   └── e2e_*.py                 # E2E 用例、处理器、诊断与报告
└── test-results/                # 运行结果（已加入 .gitignore）
```

## 1. 快速开始

**运行要求**

| 项目 | 要求 |
| --- | --- |
| 系统 | Linux，建议在 E2B API 节点运行 |
| Python | ≥ 3.10 |
| E2B SDK | `e2b>=2.19.0,<3`，首次启动自动安装 |
| 服务 | API、client-proxy、template-manager、Nomad、Consul、Harbor 可用 |
| 权限 | 构建 Template 与自动发现基础镜像建议使用 `root` |

**三步上手**

```bash
git clone https://gitcode.com/fqy_Sandbox/KASandbox.git && cd KASandbox/e2b-scripts

cp e2b-self-hosted.env.example .env && chmod 600 .env
vi .env                          # 填写 E2B_API_KEY 与 E2B_E2E_BASE_IMAGE（见第 2 节）

bash start.sh                    # 只读自检：API、Template 查询与本地运行环境
```

## 2. 配置

### 2.1 配置项

| 变量 | 是否必填 | 说明 |
| --- | --- | --- |
| `E2B_API_KEY` | 完整 E2E 必填 | Team API Key；API 节点可从 `/root/.e2b/config.json` 的 `teamApiKey` 自动读取 |
| `E2B_E2E_BASE_IMAGE` | 完整 E2E 必填 | `template-manager` 可拉取的 Template 基础镜像（不是 api / orchestrator 服务镜像） |
| `E2B_API_URL` | 非 API 节点必填 | 默认 `http://127.0.0.1:3000` |
| `E2B_DOMAIN` | 否 | Sandbox 数据面域名或 IP |
| `E2B_HTTP_SSL` | 否 | 数据面是否使用 TLS |
| `E2B_PROXY_PORT` | 否 | client-proxy 端口，默认 `3002` |
| `E2B_SANDBOX_URL` | 否 | 固定 Sandbox 数据面入口 |

脚本默认只在 API 节点读取 `/opt/e2b-infra/dep/.env` 与 `/root/.e2b/config.json` 自动发现部署参数；也可用 `--env-file` 指定受控目录中的配置：

```bash
bash start.sh --env-file /root/secure/e2b.env list-sandboxes
```

### 2.2 获取 Team API Key

```bash
# 方式一：用户配置
python3 -c 'import json,pathlib;print(json.load(open(pathlib.Path.home()/".e2b/config.json"))["teamApiKey"])'

# 方式二：Kubernetes Secret（名称与字段以实际部署为准）
kubectl -n e2b get secret e2b-api-key -o jsonpath='{.data.api-key}' | base64 -d; echo
```

上述命令会输出完整凭据，仅在受控终端执行。

### 2.3 基础镜像

基础镜像须已推送到部署使用的 Registry、可被 `template-manager` 拉取、与部署架构及 `envd` 兼容。示例：

```dotenv
E2B_E2E_BASE_IMAGE=193.30.8.2:30443/e2b-orchestration/ubuntu:22.04-custom
```

解析优先级：`--base-image` > `E2B_E2E_BASE_IMAGE` > 可见 Template 的构建元数据 > API 节点部署配置与本地 Docker 镜像。建议显式填写，避免测试对象随环境漂移。

## 3. 功能验收（test-e2e）

### 3.1 执行

```bash
bash start.sh test-e2e --all                                   # 构建本轮 fixture 并执行全部 116 个用例
bash start.sh test-e2e --all --template <ready-template>       # 使用已有 Template
bash start.sh test-e2e --all --base-image <registry>/<image>  # 临时覆盖基础镜像
bash start.sh test-e2e --case SB-004 --case UP-007            # 指定用例，自动补齐前置依赖
```

全量验收会创建真实 Sandbox 与 Template，需预留 Nomad、Firecracker、Harbor 与构建节点资源。

### 3.2 覆盖范围

| 业务 | 数量 | 核心检查 |
| --- | ---: | --- |
| 创建 Sandbox / Template | 10 / 8 | 参数组合、生命周期、镜像构建、错误边界 |
| 执行命令 | 14 | 退出码、输出、用户、目录、超时 |
| 上传 / 下载文件 | 8 / 8 | 内容一致性、路径、覆盖保护 |
| 查询 Sandbox / Template | 6 / 6 | 可见性、metadata、分页边界 |
| 后台命令与流式输出 | 8 | 后台执行、stdin、重连、终止、回调 |
| Filesystem、Watcher、Signed URL | 11 | 读写、目录、监控、签名 URL |
| Sandbox SDK、Network | 4 / 1 | 状态、重连、生命周期、Host 路由 |
| Snapshot、Checkpoint / Restore | 4 / 8 | 创建、恢复、隔离、进程恢复 |
| Pause / Resume / Connect | 13 | full-memory、autoPause、autoResume |
| PTY、Template SDK | 3 / 4 | 终端交互、序列化、后台构建、tag |

完整用例表见 [docs/E2E_CASES.md](docs/E2E_CASES.md)。不包含 CLI、MCP gateway 与 Volumes（由部署平台单独验收）。

### 3.3 常用单项命令

```bash
bash start.sh list-sandboxes
bash start.sh list-templates
bash start.sh create-sandbox --template <tpl> --timeout 300 --metadata '{"env":"test"}' --envs '{"APP_ENV":"test"}'
bash start.sh create-template --name <name> --base-image <image> --cpu-count 1 --memory-mb 1024
bash start.sh create-template --name <name> --dockerfile ./Dockerfile --cpu-count 2 --memory-mb 4096
bash start.sh run-command   --sandbox-id <id> --command "whoami && pwd" [--user root]
bash start.sh upload-file   --sandbox-id <id> --local-path ./a.txt --remote-path /tmp/a.txt
bash start.sh download-file --sandbox-id <id> --remote-path /tmp/a.txt --local-path ./a.txt
```

## 4. 性能测试（bench）

统计口径：avg / p50 / p90 / p95 / max（ms）、整批 wall、吞吐、成功率；创建类操作额外输出服务端真值 `server_*`（沙箱 `startedAt` − 请求发出时刻）。各档串行执行、档间清空沙箱，不做预热。

| 子命令 | 测量内容 |
| --- | --- |
| `create` | 并发创建耗时与吞吐（`create-kill` / `create-only`） |
| `scale` | 同一模板一次性拉起 N 个沙箱的整批 wall |
| `density` | 分批累积存活，三层内存计量（cgroup / PSS / free） |
| `snapshot-concurrency` / `snapshot-dirty` / `create-from-snapshot` | 并发快照、脏页快照、快照恢复 |
| `rollback` / `clone` / `pause-resume` | 回滚、克隆、暂停恢复 |
| `replay` | 带 pause/resume 的轨迹回放（paused + running 混合密度） |
| `all` | 串行执行全部测试项并生成汇总报告（`--profile quick` / `full`） |
| `kill-all` / `clean-host` | 清理 bench 沙箱 / 宿主级清理 netns、veth、iptables |

参数优先级：命令行 > `--config` 指定文件 > `bench.toml` > 代码默认值。

```bash
bash bench.sh all --print-config --profile full   # 查看生效配置（不执行）
bash bench.sh create -c 1 -n 3                     # 冒烟自检
bash bench.sh scale --sizes 1,100,500 --rounds 3
bash bench.sh density -c 50 --batch-size 50 --max-sandboxes 500
bash bench.sh all --profile full                   # 全量编排
bash bench.sh kill-all                             # 清理 bench 沙箱
```

未指定模板时自动复用或构建 `bench-standard-2c2g`（2 vCPU / 2 GiB）。结果写入 `test-results/<run_id>-bench/`。`test-e2e` 与 `bench` 通过 `test-results/.run.lock` 互斥，避免互相污染。

指标定义、计时口径、宿主清理规则与 `replay` 参数详见 [docs/BENCH.md](docs/BENCH.md)。

## 5. 真实轨迹密度测试（replay-nolifecycle / replay-matrix）

用 [cubesandbox-replay-suite](https://gitcode.com/yuncongyue/cubesandbox-replay-suite) 的真实 SWE agent 轨迹，在各自的 swerebench 镜像中按原始节奏回放命令，测量不同并发档位下的命令延迟，寻找宿主机密度拐点。输出口径与该仓库对齐。

### 5.1 回放模型

每个任务：**create → 按轨迹 delay 逐条执行命令 → delete**，全程无 pause/resume。

- 每档 `concurrency = target_count = 档位值`，各 workload 平滑加权交错发射；
- 命令以 `root` 在 workdir 下执行，超时仅允许 10 / 30 / 300 秒（默认 300）；
- 非零退出码是负载本身的结果，只记录不中断；超时默认中断该任务，`--command-timeout-continue` 时记录后继续；
- 结束后逐个 `GET` 核对沙箱已删除（清理核验）。

### 5.2 Catalog

`catalogs/*.json` 描述 workload 清单，每条对应一条轨迹与一个模板（名称 `swr60-<name>`，规格 2U4G）：

```json
{
  "name": "prettier-plugin-pug-448",
  "image": "swr.cn-north-4.myhuaweicloud.com/kunpeng-ai/swerebench-arm64-prettier-plugin-pug:448-2d9f897",
  "workdir": "/plugin-pug",
  "instance_id": "prettier__plugin-pug-448",
  "replay_file": "prettier__plugin-pug-448__<uuid>.replay.json",
  "env": {"CI": "true"}
}
```

| 字段 | 说明 |
| --- | --- |
| `name` / `image` / `workdir` / `instance_id` / `replay_file` | 必填 |
| `env` | 可选。该 workload 每条命令额外注入的环境变量，写入结果记录。示例中 `CI=true` 让 vitest 跳过 watch 模式 |

当前使用 `swerebench-arm64-62.json`（62 条：Java 22 / TS 18 / Go 22）。

### 5.3 执行流程

```bash
CATALOG=catalogs/swerebench-arm64-62.json
TRAJ=/opt/fqy/cubesandbox-replay-suite/datasets/replay62/replays
export LOCAL_TEMPLATE_STORAGE_BASE_PATH=/data/templates

# 1. 构建 / 检查 62 个模板（默认 4 路并行）
bash bench.sh replay-provision --catalog $CATALOG

# 2. 重建需修复的 4 个模板（见 templates/replay62/README.md）

# 3. 单档预览与执行
bash bench.sh replay-nolifecycle --catalog $CATALOG --trajectory-root $TRAJ --dry-run
bash bench.sh replay-nolifecycle --catalog $CATALOG --trajectory-root $TRAJ \
  --target-count 124 --concurrency 124 --command-timeout-continue

# 4. 梯度矩阵：逐档加压，拐点自动停止
bash bench.sh replay-matrix --catalog $CATALOG --trajectory-root $TRAJ \
  --tiers 124,372,496,744 --max-tier 744 \
  --baseline-p95-ms 8187 --command-timeout-continue
```

`replay-matrix` 主要参数：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `--tiers` 或 `--start` / `--step` | 120 / 60 | 显式档位列表，或按起点与步长递推 |
| `--max-tier` | 600 | 档位上限保护 |
| `--cooldown` | 180 | 档间冷却秒数 |
| `--baseline-p95-ms` | 第一档实测 | 注入基准 command p95，第一档也参与拐点判定 |
| `--degradation-multiplier` | 3.0 | command p95 超过基准的倍数即判定拐点（0 为禁用） |
| `--min-success-rate` | 1.0 | 档位有效所需的最低任务成功率 |
| `--sandbox-timeout` | 3600 | 沙箱生命周期；须大于最长任务耗时，否则长任务会被回收 |

执行失败、清理核验失败、成功率不足或到达拐点，任一发生即终止矩阵，原因写入 `matrix.json`。

### 5.4 结果解读

输出 `test-results/<run_id>-bench/`：`report.md`（中文汇总）、`matrix.json`、`tier-<N>/replay-result.json`（逐任务、逐步明细）。

- **以 command p95 与单任务耗时分布为主要指标**。墙钟等于最慢任务的耗时，受个别长尾 workload 主导，不宜跨档比较。
- **基线**：2026-09-29 修复模板后 124 档实测 command p95 = **8187 ms**，任务 p50 / p95 = 991 / 1506 s，墙钟 2450 s。
- 约 27% 的命令非零退出是 SWE 轨迹固有特征（测试失败、编译报错），各档占比稳定，与负载无关。

### 5.5 环境准备要点

- **网络池容量**：大档位前将 `NETWORK_POOL_REUSED_SLOTS_SIZE` 调至不小于档位值。复用池只能由真实沙箱删除回收来补充，可先持有 N 个沙箱再一次性删除完成预热；冷池直接跑大档位会导致 create 排队超时。
- **模板可用性**：内网对部分公网源静默丢包时，依赖在线下载的命令会卡满超时，扭曲结果。当前已知的 4 个模板修复见 [templates/replay62/README.md](templates/replay62/README.md)。
- **每轮前后**：确认无其他测试在跑（`pgrep -c firecracker`），跑完用 `bash bench.sh kill-all` 清理。

## 6. 结果与判定

E2E 每轮生成独立目录：

```text
test-results/<run-id>/
├── report.md       # 用例步骤、判定与证据
├── result.json     # 结构化结果
├── resources.json  # 本轮资源与清理记录
└── commands.log    # 脱敏命令证据
```

| 状态 | 含义 |
| --- | --- |
| `PASS` | 行为符合预期；非法请求被正确拒绝也记为通过 |
| `FAIL` | 接口行为或独立校验与预期不一致 |
| `BLOCKED` | 部署未提供所需能力，或调度、镜像、网络、资源问题阻断验证 |
| `SKIPPED` | 前置依赖未通过或可选能力缺失，本轮不做有效断言 |

`FAIL` / `BLOCKED` 会输出失败阶段、关键证据、可能原因与只读定位命令。输出重定向到文件、管道或 Agent 时，每个用例额外输出单行 `AGENT_DIAGNOSTIC={...}`（`E2B_AGENT_OUTPUT=1/0` 可强制开关）。所有输出均已做凭据脱敏；外发前仍需检查业务数据与内部地址。

资源回收顺序：后台进程 / PTY / Watcher / tag → Sandbox → Snapshot → Template（按资源账本 ID 删除，不按名称批量删除）。Template 删除失败不影响判定，记录在 `resources.json`。

## 7. 故障定位

| 现象 | 优先排查 | 参考命令 |
| --- | --- | --- |
| `401 Invalid API key` | `.env` 中残留旧 Key，覆盖了自动发现结果 | `grep '^E2B_API_KEY=' .env` |
| `500 Failed to place sandbox` | Nomad allocation、template-manager、Firecracker / cgroup、资源限制 | `nomad job status`；`nomad alloc status <id>`；`ss -tlnp \| grep -E ':3000\|:3002\|:5008'` |
| `template builder not found` | template-manager 注册、builder allocation、Harbor 镜像可达性 | `consul catalog services \| grep template`；`docker manifest inspect <image>` |
| 命令退出码 `124/126`、文件摘要不一致 | Template 默认用户、Shell、目录权限、client-proxy 数据面 | `run-command --command 'id; command -v sh; stat -c "%U:%G %a %n" /tmp'` |
| 回放中某类命令稳定卡满超时 | 沙箱内依赖在线下载（Go / Gradle / Maven / npm）被静默丢包 | 在沙箱内对比联网与离线执行耗时；参考 `templates/replay62/README.md` |

**SDK 兼容处理**：`Sandbox.connect()` 抛出 `envd_version` 未定义的 `NameError`，或 e2b 2.20.0 缺少同步 `pause()` / Snapshot API 时，脚本自动启用项目内兼容桥接（`beta_pause`、`AsyncSandbox`），并在 stderr 提示 `E2B SDK compatibility fallback active`。不修改已安装的 SDK，也不吞掉鉴权、网络与服务端异常。

## 8. 退出码

| 退出码 | 含义 |
| ---: | --- |
| `0` | 操作成功，或全部 E2E 用例符合预期 |
| `1` | SDK、网络、服务端错误，或存在 `FAIL` / `BLOCKED` |
| `2` | 参数或配置错误 |
| `130` | 用户中断 |
| 其他 | `run-command` 返回的远端命令退出码 |
