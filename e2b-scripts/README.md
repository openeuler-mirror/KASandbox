# E2B 自托管环境测试脚本

`e2b-scripts` 是面向自托管 E2B（KASandbox）的测试工具集，通过统一入口提供三类测试：

- **功能验收**：116 个真实 E2E 用例，验证控制面、数据面与 Python SDK 链路，用于版本上线验收与升级回归；
- **性能测试**：创建、规模、密度、快照、回滚、克隆、暂停恢复等基准测试，输出耗时分布、吞吐与成功率；
- **密度测试**：回放真实 agent 轨迹，模拟真实负载节奏，按并发档位加压寻找宿主机密度拐点。

每轮运行生成唯一 `run_id`，创建的 Sandbox、Template、Snapshot、后台进程等均以此隔离；清理只处理本轮资源，不影响运行前已存在的对象。

## 目录

- [测试方式一览](#测试方式一览)
- [目录结构](#目录结构)
- [1. 环境准备](#1-环境准备)
- [2. 功能验收（test-e2e）](#2-功能验收test-e2e)
- [3. 性能测试（bench）](#3-性能测试bench)
- [4. 密度测试（轨迹回放）](#4-密度测试轨迹回放)
- [5. 故障排查](#5-故障排查)
- [6. 退出码](#6-退出码)

## 测试方式一览

| 测试方式 | 入口命令 | 测什么 | 结果目录 |
| --- | --- | --- | --- |
| 功能验收 | `bash start.sh test-e2e --all` | 接口行为是否正确：参数、生命周期、文件、命令、快照、暂停恢复等 116 个用例 | `test-results/<run_id>/` |
| 性能测试 | `bash bench.sh <子命令>` | 单项操作的耗时分布、吞吐、成功率，以及空载单机密度 | `test-results/<run_id>-bench/` |
| 密度测试 | `bash bench.sh replay` / `replay-matrix` | 真实负载节奏下宿主机能稳定承载多少沙箱、何时出现性能拐点 | `test-results/<run_id>-bench/` |

三类测试互斥执行：运行前会原子获取 `test-results/.run.lock`，避免同时运行互相干扰。

## 目录结构

```text
e2b-scripts/
├── start.sh                     # 统一入口（Linux）
├── start.py                     # 环境初始化、SDK 依赖安装与子命令转发
├── bench.sh                     # 性能 / 密度测试入口，等价于 start.sh bench
├── bench.toml                   # bench 全部参数的单一配置来源
├── e2b-self-hosted.env.example  # .env 配置模板
├── prepare-replay-image.sh      # 构建 bench replay 默认任务镜像
└── e2b_validator/               # 实现代码，由入口统一加载
    ├── bench/                   # 性能测试与轨迹回放
    ├── e2e_*.py                 # E2E 用例、处理器、资源账本、诊断与报告
    └── create_*.py / run_command.py / ...  # 单项资源操作
```

## 1. 环境准备

### 1.1 运行要求

| 项目 | 要求 |
| --- | --- |
| 操作系统 | Linux，建议在 E2B API 节点运行 |
| Python | 3.10 或更高版本 |
| E2B SDK | `e2b>=2.19.0,<3`，首次启动自动安装 |
| 服务 | API、client-proxy、template-manager、Nomad、Consul、Registry（Harbor）可用 |
| 网络 | 可访问 API、client-proxy 与 Registry |
| 权限 | Template 构建与基础镜像自动发现建议使用 `root` |

脚本可放在任意目录。在 API 节点运行时，会自动读取 `/opt/e2b-infra/dep/.env` 与 `~/.e2b/config.json` 发现部署参数。

### 1.2 获取代码

```bash
git clone <仓库地址>/KASandbox.git
cd KASandbox/e2b-scripts
```

### 1.3 配置 Team API Key

API 节点上优先从 `~/.e2b/config.json` 的 `teamApiKey` 字段读取：

```bash
python3 -c 'import json,pathlib;print(json.load(open(pathlib.Path.home()/".e2b/config.json"))["teamApiKey"])'
```

Key 由 Kubernetes Secret 提供时（Secret 名称与字段以实际部署为准）：

```bash
kubectl -n e2b get secret e2b-api-key -o jsonpath='{.data.api-key}' | base64 -d; echo
```

以上命令会输出完整凭据，仅在受控终端执行。

### 1.4 配置基础镜像

`E2B_E2E_BASE_IMAGE` 是功能验收构建 fixture Template 所用的基础镜像，不是 api、orchestrator 等服务镜像。要求：

- 已推送到部署使用的 Registry，且 `template-manager` 所在节点可以拉取；
- 与部署架构及 `envd` 运行要求兼容；
- 具备基础 Linux 用户态环境。

```bash
# 在部署节点查看候选镜像
docker images --format '{{.Repository}}:{{.Tag}}' | grep -Ei 'ubuntu|debian|e2b.*base'
```

未显式配置时按以下顺序解析：`--base-image` 参数 → `E2B_E2E_BASE_IMAGE` → 可见 Template 的构建元数据 → API 节点部署配置与本地 Docker 镜像。建议显式填写，避免测试对象随环境变化。

### 1.5 创建 .env

```bash
cp e2b-self-hosted.env.example .env
chmod 600 .env
vi .env
```

```dotenv
E2B_API_KEY=<team-api-key>
E2B_E2E_BASE_IMAGE=<registry>/<project>/<image>:<tag>
# 脚本不在 API 节点运行时补充
E2B_API_URL=http://<api-host>:3000
```

| 变量 | 必填 | 说明 |
| --- | --- | --- |
| `E2B_API_KEY` | 功能验收必填 | Team API Key；API 节点可自动读取 |
| `E2B_E2E_BASE_IMAGE` | 功能验收必填 | fixture Template 基础镜像 |
| `E2B_API_URL` | 非 API 节点必填 | 默认 `http://127.0.0.1:3000` |
| `E2B_DOMAIN` | 否 | Sandbox 数据面域名或 IP |
| `E2B_HTTP_SSL` | 否 | 数据面是否使用 TLS |
| `E2B_PROXY_PORT` | 否 | client-proxy 端口，默认 `3002` |
| `E2B_SANDBOX_URL` | 否 | 固定 Sandbox 数据面入口 |

也可使用项目目录外的配置文件：`bash start.sh --env-file /path/to/e2b.env <子命令>`。

### 1.6 自检

```bash
bash start.sh    # 只读检查：API 连通、Template 查询、本地运行环境
```

输出 `E2B_STARTUP_OK` 即环境就绪。

## 2. 功能验收（test-e2e）

### 2.1 执行方式

```bash
# 全量验收：自动构建本轮隔离的 e2e-<run_id>-fixture Template，执行 116 个用例
bash start.sh test-e2e --all

# 使用已有的 ready Template
bash start.sh test-e2e --all --template <template>

# 仅对本次运行覆盖基础镜像
bash start.sh test-e2e --all --base-image <registry>/<project>/<image>:<tag>

# 执行指定用例；存在前置依赖时自动补齐依赖链（如 CPR-002 会先执行 CPR-001）
bash start.sh test-e2e --case SB-004 --case UP-007
```

全量验收会创建真实 Sandbox 与 Template，需预留 Nomad、Firecracker、Registry 与构建节点资源。

### 2.2 测试范围

| 业务 | 用例编号 | 数量 | 核心检查 |
| --- | --- | ---: | --- |
| 创建 Sandbox | `SB-001` ~ `SB-010` | 10 | 参数组合、生命周期、安全属性、错误边界 |
| 创建 Template | `TP-001` ~ `TP-008` | 8 | 镜像构建、Dockerfile、资源参数、失败归因 |
| 执行命令 | `CMD-001` ~ `CMD-014` | 14 | 退出码、输出、用户、目录、超时和异常目标 |
| 上传文件 | `UP-001` ~ `UP-008` | 8 | 文本、二进制、空文件、路径、覆盖和摘要 |
| 下载文件 | `DL-001` ~ `DL-008` | 8 | 内容一致性、目录创建、覆盖保护和异常路径 |
| 查询 Sandbox | `LSB-001` ~ `LSB-006` | 6 | 可见性、metadata 和分页边界 |
| 查询 Template | `LTP-001` ~ `LTP-006` | 6 | 可见性、构建状态、稳定性和分页边界 |
| 后台命令与流式输出 | `BG-001` ~ `BG-006`、`STR-001` ~ `STR-002` | 8 | 后台执行、stdin、重连、终止和输出回调 |
| Filesystem | `FS-001` ~ `FS-008` | 8 | 读取格式、批量写入、目录、重命名、删除和参数边界 |
| 文件事件与直连 URL | `WAT-001` ~ `WAT-002`、`URL-001` | 3 | 文件监控、递归监控、上传下载签名 URL |
| Sandbox SDK | `SI-001` ~ `SI-003`、`LC-001` | 4 | 状态、重连和生命周期 |
| Network | `NET-001` | 1 | Host 路由 |
| Snapshot | `SNP-001` ~ `SNP-004` | 4 | 创建、查询、恢复、删除和重复删除 |
| Checkpoint / Restore | `CPR-001` ~ `CPR-008` | 8 | 精确回滚、状态隔离、系统路径和进程恢复 |
| Pause / Resume / Connect | `PRC-001` ~ `PRC-011`、`PRC-014` ~ `PRC-015` | 13 | full-memory、memory 参数兼容、autoPause、autoResume |
| PTY | `PTY-001` ~ `PTY-003` | 3 | 创建、stdin、尺寸调整、重连和终止 |
| Template SDK | `TSDK-001` ~ `TSDK-004` | 4 | 序列化、后台构建、状态、存在性和 tag |

负向用例收到预期错误时记为 `PASS`。不包含 CLI、MCP gateway 与 Volumes，这三项由部署平台单独验收。用例定义见 `e2b_validator/e2e_test_cases.py` 与 `e2b_validator/e2e_extended_cases.py`。

### 2.3 结果与判定

```text
test-results/<run_id>/
├── report.md       # 用例步骤、判定和证据
├── result.json     # 结构化结果
├── resources.json  # 本轮资源与清理记录
└── commands.log    # 脱敏后的命令证据
```

| 状态 | 含义 |
| --- | --- |
| `PASS` | 实际行为符合预期；非法请求被正确拒绝也属于通过 |
| `FAIL` | 接口行为或独立校验结果与预期不一致 |
| `BLOCKED` | 部署未提供所需能力，或调度、镜像、网络、资源问题阻断验证；不表示断言失败 |
| `SKIPPED` | 前置依赖未通过或可选能力缺失，本轮不执行有效断言 |

- 终端逐用例显示目标、参数、预期、实测结果与耗时；`FAIL` / `BLOCKED` 额外输出失败阶段、关键证据、可能原因和只读排查命令。
- 输出重定向到文件、管道或由 Agent 调用时，每个用例额外输出单行 `AGENT_DIAGNOSTIC=<json>`，可用 `E2B_AGENT_OUTPUT=1/0` 强制开关。
- 所有输出均经过凭据脱敏；对外提供前仍需检查业务数据与内部地址。
- 资源回收顺序：后台进程 / PTY / Watcher / tag → Sandbox → Snapshot → Template，均按资源账本中的 ID 删除，不按名称批量删除。

### 2.4 单项资源操作

```bash
bash start.sh list-sandboxes
bash start.sh list-templates
bash start.sh create-sandbox --template <template> --timeout 300 \
  --metadata '{"env":"test"}' --envs '{"APP_ENV":"test"}'
bash start.sh create-template --name <name> --base-image <image> --cpu-count 1 --memory-mb 1024
bash start.sh create-template --name <name> --dockerfile ./Dockerfile --cpu-count 2 --memory-mb 4096
bash start.sh run-command   --sandbox-id <id> --command "whoami && pwd" [--user root]
bash start.sh upload-file   --sandbox-id <id> --local-path ./demo.txt --remote-path /tmp/demo.txt
bash start.sh download-file --sandbox-id <id> --remote-path /tmp/demo.txt --local-path ./demo.txt
```

## 3. 性能测试（bench）

### 3.1 测试项

| 子命令 | 测量内容 |
| --- | --- |
| `create` | 单沙箱创建耗时分布与吞吐；`create-kill` 测完即删，`create-only` 保留存活 |
| `scale` | 同一模板一次性拉起 N 个沙箱的整批耗时（首个请求发出 → 全部 running） |
| `density` | 分批累积存活，按 cgroup / PSS / free 三种口径计量单沙箱内存与磁盘开销 |
| `snapshot-concurrency` | 并发创建快照耗时 |
| `snapshot-dirty` | 写入指定大小脏页后的快照耗时与产物大小 |
| `create-from-snapshot` | 从快照并发恢复沙箱耗时 |
| `rollback` / `clone` / `pause-resume` | 回滚、克隆、暂停恢复耗时 |
| `all` | 串行执行全部测试项并生成汇总报告（`--profile quick` 自检 / `full` 完整档位） |
| `kill-all` | 清理 bench 创建的沙箱（`--all` 清理全部） |
| `clean-host` | 宿主级清理 netns、veth 与 iptables 残留（有运行中的沙箱时拒绝执行） |

### 3.2 配置

`bench.toml` 是全部 bench 参数的单一来源：`[global]` 定义模板、沙箱超时、内存安全阈值、结果目录等公共参数；各测试项一节，档位用 `[[<节>.tiers]]` 描述；`[profiles.quick]` / `[profiles.full]` 覆盖档位规模。修改数字即可调整档位，无需改代码。

参数优先级：命令行 > `--config` 指定文件 > `bench.toml` > 代码内置默认值。

被测模板优先级：`-t/--template` > `bench.toml [global].template` > 环境变量 `BENCH_TEMPLATE_ID` > 自动基准模板。均未指定时自动复用或构建 `bench-standard-2c2g`（2 vCPU / 2 GiB）。

### 3.3 执行步骤

```bash
# 1. 查看合并后的生效配置（不执行测试）
bash bench.sh all --print-config --profile full

# 2. 冒烟：单并发创建 3 次，确认链路可用
bash bench.sh create -c 1 -n 3

# 3. 按需执行单项测试
bash bench.sh create -c 50 -n 500 -m create-only
bash bench.sh scale --sizes 1,100,500 --rounds 3
bash bench.sh density -c 50 --batch-size 50 --max-sandboxes 500
bash bench.sh snapshot-concurrency -c 10 -n 5
bash bench.sh clone -n 100 -c 20 --rounds 2

# 4. 一键全量编排
bash bench.sh all --profile full

# 5. 清理
bash bench.sh kill-all
```

### 3.4 指标

| 指标 | 含义 |
| --- | --- |
| `avg / p50 / p90 / p95 / max`（ms） | 单操作耗时分布，客户端口径（含 SDK / HTTP 开销） |
| `server_*`（ms） | 服务端口径：沙箱 `startedAt` 减去请求发出时刻，仅创建类操作提供；要求与 API 同机运行 |
| `wall_ms` | 整批耗时：首个请求发出 → 全部完成 |
| `throughput_per_s` | 成功操作数 ÷ wall |
| `success_rate` | 成功率（%） |

结果写入 `test-results/<run_id>-bench/`：每个测试项一个 `bench_<名称>.json`，`bench all` 额外生成 `bench_all.json` 与中文汇总 `report.md`。

### 3.5 执行机制

- **并发起跑**：创建请求在 barrier 处统一放行，整批计时从放行瞬间起算，消除线程池启动抖动。
- **档间清理**：每档结束后清理残留沙箱，等待 firecracker / jailer / nbd 回到基线后再进入下一档，过程记录在 `cleanup.log`。
- **内存安全闸**：`scale`、`density` 等在 MemAvailable 低于总内存的 `mem_threshold_pct`（默认 15%）时停止加压。
- **池恢复等待**：档位可配置 `pre_wait`（秒），用于高并发档前等待网络池恢复，不计入测量。

## 4. 密度测试（轨迹回放）

### 4.1 测试模型

密度测试回放真实 agent 轨迹：每条轨迹是一串命令及其间隔（`delay`，对应 agent 思考时间），在任务镜像中按原始节奏执行，从而复现真实负载的资源占用与时间分布。提供两种模型：

| 子命令 | 生命周期 | 测什么 |
| --- | --- | --- |
| `replay` | 创建后立即 pause；每步 resume → 执行命令 → pause；RUNNING 名额受 `--running-concurrency` 限制 | paused 与 running 混合形态下的承载能力 |
| `replay-nolifecycle` | 创建 → 按 delay 逐条执行命令 → 删除，全程保持运行 | 沙箱常驻运行时单档位的命令延迟与资源占用 |
| `replay-matrix` | 按档位逐档驱动 `replay-nolifecycle` | 并发逐档升高时的延迟变化，自动定位拐点 |

### 4.2 准备工作

| 项目 | 说明 |
| --- | --- |
| 轨迹 | 每条轨迹一个 JSON 文件，包含命令序列与 delay |
| 任务镜像 | 轨迹对应的运行环境。应预先内置轨迹所需的依赖（语言模块缓存、构建工具等），避免运行时下载外网依赖失败或超时，扭曲测试结果 |
| Catalog | `replay-nolifecycle` / `replay-matrix` 使用的 workload 清单，通过 `--catalog` 指定，格式见 4.4 |
| 网络池 | `NETWORK_POOL_REUSED_SLOTS_SIZE` 不小于目标档位，并在测试前预热；冷池直接跑大档位会导致创建排队超时 |

### 4.3 replay：带 pause / resume 的回放

```bash
# 合成轨迹自检（不传 --trajectory-dir 时用通用只读命令合成）
bash bench.sh replay --target-count 5 -c 5 --running-concurrency 2

# 真实轨迹回放
bash bench.sh replay --trajectory-dir <轨迹目录> --target-count 60 -c 20 --running-concurrency 10

# 只校验配置与轨迹、打印调度预览，不创建沙箱
bash bench.sh replay --dry-run --target-count 5

# 多模板混合回放
bash bench.sh replay --mix-config ./mix-config.json --running-concurrency 30
```

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `--target-count` | 60 | 总回放次数；超过轨迹数时循环复用 |
| `-c/--concurrency` | 20 | 同时进行的轨迹数（paused 常驻规模） |
| `--running-concurrency` | 10 | 同一时刻处于 running 的沙箱上限 |
| `--control-plane-qps` | 100 | 控制面请求（create / pause / resume / command）统一限速 |
| `--action-timeout` | 300 | 单条命令超时秒数 |
| `--mix-config` | 无 | 多模板混合配置（JSON），各负载按 `vm_count` 交错发射 |
| `--snapshot-mode` | `none` | `same-sandbox` / `chain`：测连续快照或链式恢复开销 |

未指定模板时，真实轨迹模式使用 `bench.toml [replay]` 中配置的任务模板，不存在则按 `task_template_image` 自动构建；该镜像可用 `prepare-replay-image.sh <registry前缀>` 构建推送。

### 4.4 replay-nolifecycle / replay-matrix：常驻运行的梯度加压

Catalog 为 JSON 数组，每项描述一个 workload：

```json
[
  {
    "name": "prettier-plugin-pug-448",
    "image": "<registry>/swerebench-arm64-prettier-plugin-pug:448-2d9f897",
    "workdir": "/plugin-pug",
    "instance_id": "prettier__plugin-pug-448",
    "replay_file": "prettier__plugin-pug-448__<uuid>.replay.json",
    "env": {"CI": "true"}
  }
]
```

| 字段 | 说明 |
| --- | --- |
| `name` | workload 名称，派生模板名 `swr60-<name>`（规格 2 vCPU / 4 GiB） |
| `image` | 任务镜像 |
| `workdir` | 命令执行目录 |
| `instance_id` / `replay_file` | 轨迹标识与轨迹文件名（位于 `--trajectory-root` 下） |
| `env` | 可选，为该 workload 的每条命令注入环境变量，如 `CI=true` 让 vitest 以单次模式运行 |

```bash
CATALOG=<catalog.json>
TRAJ=<轨迹目录>

# 1. 按 catalog 检查 / 构建全部任务模板
bash bench.sh replay-provision --catalog $CATALOG

# 2. 单档位：预览与执行
bash bench.sh replay-nolifecycle --catalog $CATALOG --trajectory-root $TRAJ --dry-run
bash bench.sh replay-nolifecycle --catalog $CATALOG --trajectory-root $TRAJ \
  --target-count 124 --concurrency 124 --command-timeout-continue

# 3. 梯度矩阵：逐档加压，到达拐点自动停止
bash bench.sh replay-matrix --catalog $CATALOG --trajectory-root $TRAJ \
  --tiers 124,372,496,744 --baseline-p95-ms <基线p95> --command-timeout-continue
```

执行规则：

- 每档 `并发数 = 任务数 = 档位值`，各 workload 按平滑加权轮询交错发射。
- 命令以 `root` 在 workdir 下执行，超时可选 10 / 30 / 300 秒（默认 300）。非零退出码是负载本身的结果，只记录不中断；超时默认终止该任务，加 `--command-timeout-continue` 则记录后继续。
- 任务结束后删除沙箱，并逐个查询确认已删除（清理核验）。

| `replay-matrix` 参数 | 默认 | 说明 |
| --- | --- | --- |
| `--tiers` 或 `--start` / `--step` | 120 / 60 | 显式档位列表，或按起点和步长递推 |
| `--max-tier` | 600 | 档位上限保护 |
| `--cooldown` | 180 | 档间冷却秒数 |
| `--baseline-p95-ms` | 第一档实测 | 注入基线 command p95，注入后第一档也参与拐点判定 |
| `--degradation-multiplier` | 3.0 | 某档 command p95 超过基线的倍数即判定为拐点（0 为禁用） |
| `--min-success-rate` | 1.0 | 档位有效所需的最低任务成功率 |
| `--sandbox-timeout` | 3600 | 沙箱生命周期，须大于最长任务耗时 |

执行失败、成功率不足、清理核验失败或到达拐点，任一发生即终止矩阵，原因记录在 `matrix.json`。

### 4.5 结果解读

输出目录 `test-results/<run_id>-bench/`：`report.md`（中文汇总）、`matrix.json`（档位汇总与终止原因）、`tier-<N>/replay-result.json`（逐任务、逐命令明细）。

- **以 command p95 和单任务耗时分布为主要指标**。墙钟时间等于最慢任务的耗时，易受个别长尾 workload 影响，不适合跨档比较。
- **基线**应在负载较低、没有超时伪影的档位上实测，并在环境或镜像变化后重新测定。
- **拐点先看尾部**：密度升高时 p95 通常先于 p50 劣化，是更早的预警指标。
- SWE 轨迹中存在一定比例的非零退出（测试失败、编译报错），属于负载固有特征，只要各档占比稳定即不影响判断。

## 5. 故障排查

| 现象 | 排查方向 | 参考命令 |
| --- | --- | --- |
| `401 Invalid API key` | `.env` 中残留旧 Key，覆盖了自动发现结果 | `grep '^E2B_API_KEY=' .env` |
| `500 Failed to place sandbox` | Nomad allocation、template-manager、Firecracker / cgroup 资源 | `nomad job status`；`nomad alloc status <id>` |
| `template builder not found` | template-manager 服务注册、Registry 镜像是否可拉取 | `consul catalog services \| grep template` |
| 命令退出码 124 / 126、文件摘要不一致 | Template 默认用户、Shell、目录权限、client-proxy 数据面 | `run-command --command 'id; command -v sh; stat -c "%U:%G %a %n" /tmp'` |
| 回放中某类命令稳定卡满超时 | 沙箱内在线下载依赖被网络阻断，或测试进入 watch 等交互模式 | 在沙箱内对比联网与离线执行耗时；检查是否需要 `CI=true` |

**SDK 兼容处理**：安装的 SDK 缺少同步 `pause()` / Snapshot API，或 `Sandbox.connect()` 引用了未定义的 `envd_version` 时，脚本启用内置兼容实现（`beta_pause`、`AsyncSandbox` 桥接），并在 stderr 输出 `E2B SDK compatibility fallback active`。不修改已安装的 SDK，也不吞掉鉴权、网络与服务端异常。

## 6. 退出码

| 退出码 | 含义 |
| ---: | --- |
| `0` | 操作成功，或全部 E2E 用例符合预期 |
| `1` | SDK、网络、服务端错误，或存在 `FAIL` / `BLOCKED` |
| `2` | 参数或配置错误 |
| `130` | 用户中断 |
| 其他 | `run-command` 透传的远端命令退出码 |
