# bench 性能测试详解

> 从 README 拆出的完整说明。快速上手见 [README](../README.md#4-性能测试bench)；真实轨迹密度测试（replay-nolifecycle / replay-matrix）见 README 对应章节。

`bench` 子命令的统计口径：avg / min / p95 / max（毫秒）+ wall + per（wall ÷ 操作数）+ 吞吐 + 成功率，各档位串行执行、档间清空沙箱。预热已禁用（`warmup = 0`），每轮都计入正式测量。

## 测试项一览

| 测试项 | 命令 | 测量内容 | 说明 |
| --- | --- | --- | --- |
| 并发创建 | `bench create` | 单沙箱创建耗时分布 + 吞吐 | `create-kill` 测完即删；`create-only` 保留存活 |
| 规模测试 | `bench scale` | 同一模板一次性拉起 N 个沙箱的整批 wall（首个请求发出 → 全部 running） | 每档执行前检查内存安全闸 |
| 单机密度 | `bench density` | 分批累积存活，测单沙箱内存/磁盘开销 | 三层内存计量，见「宿主观测与清理」 |
| 快照并发 | `bench snapshot-concurrency` | 并发打快照耗时 | 逐档串行 |
| 脏页快照 | `bench snapshot-dirty` | 写入指定 MB 脏页后的快照耗时与产物大小 | 逐档串行 |
| 快照恢复 | `bench create-from-snapshot` | 从快照并发创建沙箱耗时 | 逐档串行 |
| 回滚 | `bench rollback` | 沙箱回滚耗时 | 逐档串行 |
| 克隆 | `bench clone` | 沙箱克隆耗时与吞吐 | 逐档串行 |
| 暂停恢复 | `bench pause-resume` | pause / resume 耗时 | 逐档串行 |
| 轨迹回放密度 | `bench replay` | 真实 agent 负载节奏下的 paused+running 混合密度（见「轨迹回放密度测试（bench replay）」） | 内存安全闸兜底 |
| 一键编排 | `bench all` | 串行执行全部测试项并生成汇总报告 | `--profile quick` 自检 / `full` 完整档位 |

辅助命令：`bench kill-all` 清理 bench 创建的沙箱（默认仅清理带 bench 标记的；`--all` 删除全部）；`bench clean-host` 宿主级清理（见「宿主观测与清理」）。

## 配置文件 bench.toml

所有 bench 参数的单一来源（TOML，stdlib `tomllib` 解析，无新增依赖）。优先级：**命令行参数 > `--config` 指定文件 > 默认 `bench.toml` > 代码内置兜底**。

`[global]` 字段：

| 字段 | 含义 | 默认值 |
| --- | --- | --- |
| `template` | 模板 ID；命令行 `-t/--template` 与环境变量 `BENCH_TEMPLATE_ID` 优先 | `""`（必须提供） |
| `warmup` | 每轮正式测量前的热身轮数，结果丢弃（已禁用） | `0` |
| `sandbox_timeout` | 沙箱生命周期秒数，到期服务端自动回收防泄漏 | `600` |
| `mem_threshold_pct` | 内存安全闸：MemAvailable 低于总内存该百分比即中止 density/scale | `15.0` |
| `netns_growth_threshold` | 单个测试项结束后 netns 净增长超过该值时报告告警 | `100` |
| `result_root` | 结果输出目录（相对路径基于 e2b-scripts/ 解析） | `test-results` |

各测试项一节（`[create]` / `[scale]` / `[density]` / `[snapshot_concurrency]` / `[snapshot_dirty]` / `[create_from_snapshot]` / `[rollback]` / `[clone]` / `[pause_resume]`），档位用 `[[<节>.tiers]]` 数组表描述；`[profiles.quick]` / `[profiles.full]` 只覆盖各节档位规模，quick 为小规模自检，full 为完整档位。修改数字即可调整档位，无需改代码。

## 执行方式

两个入口等价：`bash bench.sh <子命令>` 是 `bash start.sh bench <子命令>` 的简写。下面按"先配置、再单项、后全量、最后清理"的顺序组织。

**第一步：跑前确认生效配置（只回显，不执行测试）**

```bash
bash start.sh bench all --print-config                      # 合并后的默认配置
bash start.sh bench all --print-config --profile full       # 完整档位的生效配置
bash start.sh bench all --print-config --config /path/to/bench-mini.toml   # 自定义配置文件
```

**第二步：指定被测模板**

模板解析优先级：`-t/--template` > `bench.toml [global].template` > 环境变量 `BENCH_TEMPLATE_ID` > 自动基准模板。三者都未指定时自动使用 `bench-standard-2c2g`（2 vCPU / 2048 MiB）：先按名称查找 ready 且本机 `/tmp/templates` 产物齐全的既有模板复用（日志打印"复用基准模板 <id>"），没有则自动构建一次，构建完成后长期复用。

```bash
bash bench.sh scale --sizes 100 -t <ready-template-id>   # 方式一：命令行指定
export BENCH_TEMPLATE_ID=<ready-template-id>             # 方式二：环境变量
# 不指定也可以，脚本会自动复用或构建 bench-standard-2c2g
```

**第三步：冒烟自检（可选但建议）**

```bash
bash bench.sh create -c 1 -n 3 -w 1   # 单并发创建 3 次，验证链路可用
```

**第四步：按需执行单项压测**

```bash
# 并发创建：50 并发共 500 个（create-only 模式保留存活，用于观察稳态）
bash bench.sh create -c 50 -n 500 -w 3 -m create-only

# 规模测试：同一模板一次性并发拉起 N 个沙箱，测整批 wall（首个请求发出 → 全部 running）
# 每档执行前检查内存安全闸，低于阈值即中止该档并记录
bash bench.sh scale --sizes 1,100,500 --rounds 3

# 单机密度：分批累积存活 + 三层内存计量（见「宿主观测与清理」）
# 内存安全闸：每批前检查 MemAvailable 低于总内存 15%（--mem-threshold-pct 可调）立即中止
bash bench.sh density -c 50 --batch-size 50 --max-sandboxes 500
```

```bash
# 快照系列（逐档串行）
bash bench.sh snapshot-concurrency -c 10 -n 5      # 并发打快照
bash bench.sh snapshot-dirty -d 100 -n 3           # 写入 100MB 脏页后打快照
bash bench.sh create-from-snapshot -c 20 -n 3      # 从快照并发恢复
```

```bash
# 回滚 / 克隆 / 暂停恢复
bash bench.sh rollback -c 10 -n 5
bash bench.sh clone -n 100 -c 20 --rounds 2
bash bench.sh pause-resume -c 10 -n 5
```

**第五步：一键全量编排**

串行执行全部测试项并生成汇总报告：

```bash
bash bench.sh all --profile quick   # 小规模自检
bash bench.sh all --profile full    # 完整档位
```

**第六步：跑完清理**

```bash
bash bench.sh kill-all          # 清理 bench 创建的沙箱（默认仅清理带 bench 标记的）
bash bench.sh kill-all --all    # 删除全部沙箱（慎用）
```

## 指标与输出

| 指标 | 含义 |
| --- | --- |
| `wall_ms` | 整批 wall：首个请求发出（barrier 放行）→ 全部完成 |
| `avg / p50 / p90 / p95 / max（_ms）` | 单操作耗时分布（线性插值百分位），客户端口径，含 Python SDK/HTTP 开销 |
| `server_avg / server_p50 / server_p90 / server_p95 / server_max（_ms）` | 服务端真值口径：沙箱 `startedAt` − 请求发出时刻，剔除全部客户端 Python 开销（见「并发计时口径」） |
| `server_batch_span_ms` | 整批服务端跨度：最早请求发出 → 最晚 `startedAt`（多轮取均值）；与 `wall_ms` 并列对照，差值即客户端整批开销（见「并发计时口径」） |
| `server_samples` | server_* 有效样本数（startedAt 回读成功的沙箱数） |
| `per_unit_avg_ms` | wall ÷ 操作数，等效串行成本 |
| `throughput_per_s` | 吞吐：成功操作数 ÷ wall |
| `success_rate` | 成功率（%） |
| `destroy_*` | 销毁耗时分布（统一并发销毁，默认并发 32） |

输出写入 `test-results/<run_id>-bench/`：每个测试项一个 `bench_<名称>.json`，`bench all` 额外生成 `bench_all.json` 和中文汇总 `report.md`（含环境信息、各测试项数据表、结论）。每档/每项之间执行残留清理并等待运行时收敛（标记沙箱归零、firecracker/jailer/nbd 回基线、连续 3 个采样稳定），收敛过程记录在同目录 `cleanup.log`。

沙箱回收规则：除 `create -m create-only` 和 `density --keep-sandboxes` 外，所有测试项结束后自动清理创建的沙箱。注意：create/scale/density 走 SDK `Sandbox.create` 的沙箱**不带 metadata 标记**（与用户脚本一致），`bench kill-all` 的 metadata 匹配扫不到它们——正常路径由进程内显式 ID 列表（`ctx.created_ids`）逐个销毁，残留由服务端沙箱 timeout（SDK 创建固定 3600s）兜底回收；`create-only` / `--keep-sandboxes` 保留的沙箱同理，只能靠 timeout 回收。

## 并发计时口径

所有「基于模板创建沙箱」的路径与 max_test 脚本完全一致——SDK `Sandbox.create(template, timeout=3600)`（create / scale / density 三个创建类子命令走 `ThreadPoolExecutor` 分批并发，每批 ≤600，批内失败数下一批自动补充，最多 10 轮，测完统一并发销毁；snapshot 系列 / rollback / clone / pause-resume 的源沙箱准备也走同一 SDK 创建），不带 metadata。

SDK 创建路径默认固化两个客户端优化（均可用环境变量回退）：

| 环境变量 | 默认 | 作用 |
| --- | --- | --- |
| `E2B_SDK_CLIENT_POC_MODE=shared-api-client` | 开 | 全部线程复用预建的 API client，消除每线程构建 ConnectionConfig/transport 的开销与竞争；置空该变量关闭 |
| `E2B_CREATE_BARRIER=1` | 开 | 批内线程在 barrier 统一等待，主线程放行后同一瞬间发出全部请求，批 wall 从放行瞬间起算，消除线程池 ramp-up 抖动；设 `0` 关闭 |

**快照制作也走 SDK**：`e2b_sdk_compat.create_snapshot`（同步 `Sandbox.create_snapshot` 缺失时自动桥接 `AsyncSandbox`，e2b 2.20.0 即此路径）。唯一仍走 REST 的是「从快照/checkpoint 创建沙箱」（SDK 无同步 snapshot 恢复创建入口）以及 pause/resume/删除等生命周期操作；REST 并发用 `threading.Barrier` 同步起跑：burst 型（任务数 == 并发数）全部请求同一瞬间发出，流水线型（总请求数 > 并发数）只同步首波起跑、后续自然流动；wall 计时从 barrier 放行瞬间起算。

**服务端真值口径（server_*）**：客户端口径的 `avg/p50/...` 即使做了共享 client 与 barrier，仍包含 Python 自身开销（httpx 收发、JSON 解析、SDK 等待 envd、线程调度，实测高并发下可达数百毫秒）。为此所有「创建沙箱」路径额外输出 `server_*` 指标：

- 定义：`server_ms = 沙箱 startedAt − 请求发出时刻（本地墙钟）`。`startedAt` 由 API 在 orchestrator 放置并创建完成后写入（`packages/api/internal/orchestrator/create_instance.go` 中 placement 后取 `time.Now()`），即「API 受理 → 沙箱 running」的真实服务端耗时，**不含任何客户端 Python 开销**；纳秒精度（RFC3339Nano），报告取毫秒。
- 整批口径：`server_batch_span_ms = max(startedAt) − min(请求发出时刻)`，即「最早请求进入服务端 → 最晚沙箱创建完成」的服务端视角整批端到端，与客户端视角的 `wall_ms` 并列；两者差值 ≈ 客户端整批开销（barrier 放行前准备 + 结果收集/调度）。barrier 同步起跑时它与 `server_max_ms` 数值接近，流水线模式（请求分波发出）下更能体现真实批量跨度。
- 回读方式（均在计时窗外，不污染测量）：SDK 创建路径（create / scale / density）每批结束后一次 `GET /sandboxes`（v1，running 全量不分页）批量取数，缺失的逐个 `GET /sandboxes/{id}` 兜底；REST 创建路径（create-from-snapshot / rollback / clone / snapshot-dirty 的恢复创建）在 `create_timed` 返回后立即回读详情，代价是每次创建多一次 loopback GET，不计入 `latency_ms`。
- 适用范围：仅「创建类」操作（模板创建与快照/checkpoint 恢复创建）；pause/resume、快照制作、销毁无对应服务端时间戳，不提供 server_*。
- 前提与告警：要求脚本与 API **同机部署**（默认 `127.0.0.1:3000`，时钟同源）。远程运行时两机时钟偏差会直接体现为 server_ms 整体偏移；出现负数 server_ms 时结果 notes 会告警「时钟不同步，server_* 不可信」。

## 与 test-e2e 互斥隔离

test-e2e 是 116 个功能验收用例，bench 是性能压测，两者同时跑会互相污染。`test-e2e` 与所有 `bench` 子命令执行前都会原子抢锁 `test-results/.run.lock`（O_CREAT|O_EXCL，内容为 holder/run_id/started_at/pid 的 JSON）；抢不到即退出并提示持锁方与开始时间，确认对方结束后重试，或加 `--force` 删除旧锁强制继续。正常结束与异常退出（含 Ctrl+C）都会通过 finally/atexit 释放锁。

## 宿主观测与清理

**density 内存计量口径（三层指标，采集器始终开启，无需配置）**：每批沙箱创建后静置 2.5s（UFFD 懒加载落定）再采集——① 独立 cgroup v1（`/sys/fs/cgroup/e2b/sbx-<id>/`，兼容 memory 子树布局）usage/stat：Σ 总量准确，但共享页记在首个 touch 的 cgroup，单沙箱均摊有偏差；② 对应 firecracker 进程（`--api-sock /tmp/fc-<沙箱ID>-<buildID>.sock` 映射）的 `smaps_rollup`：PSS 均摊/min/p95/max、私有脏页（含 Private_Hugetlb——VM 内存大页是懒加载主体，本机大页池不占 MemAvailable，故 free 口径会低估真实占用）、共享页均摊与共享比例；③ 原 free available 差值保留为参考列。报告表格为「存活沙箱数 | free 可用（参考） | Σ cgroup usage | PSS 均摊 | 私有脏页均摊 | 共享页均摊 | 共享比例 | 单沙箱开销（free 口径，参考）」；JSON 含每沙箱明细数组（per_sandbox）便于后处理。

**宿主网络残留观测**：v35 架构的 orchestrator 为每个沙箱预建 netns（`ns-N`）+ veth（`veth-N`）+ iptables 规则，槽位 N 严格 1:1 对应；网络池槽位（new 池 640 + reused 池 1000）的双活对子是常驻设计。正常 kill 后槽位回复用池，但创建失败/超时回收的沙箱会留下「半对子」孤儿（有 veth-N 无 ns-N，或有 ns-N 无 veth-N）及悬空 iptables 规则，积累到一定程度会拖垮吞吐。`ensure_clean_slate` 的每次收敛采样会把 netns/veth/iptables 计数写入 `cleanup.log`；若某测试项结束时 netns 数相比该项开始增长超过 `netns_growth_threshold`（默认 100），报告 notes 会告警。

**每小轮前的孤儿清理（`clean_host_orphans`）**：每个档位第一轮 warmup 之前、density 开始前、`ensure_clean_slate` 收敛后、bench all pre-flight 都会执行一次。判定规则：**只有半对子才删**（`veth-N` 存在而 `ns-N` 不存在 → 删 veth；`ns-N` 存在而 `veth-N` 不存在 → 删 netns）；iptables 过滤两类：引用「当前不存在 veth」的规则，以及 `nat POSTROUTING -s 10.11.x.y/32 ... MASQUERADE` 中 HostIP 映射槽位（N=(b<<8)|c，基址 10.11.0.0/16）已不在存活集合的孤儿规则（docker 的 172.x 等其他 MASQUERADE 不动）；**双活（ns-N 与 veth-N 都在）一律不碰**——那是池槽位或活动沙箱。安全前置：仅当 firecracker=0 且 jailer=0 时执行；iptables-restore 失败会报错并把原规则留在 `/tmp/bench-iptables-orphans-*.rules` 供排查。统计（orphan_netns/orphan_veth/orphan_rules/orphan_masq_rules）写入 `cleanup.log`。

```bash
# 两次 full 之间做宿主级清理（会拒绝在有沙箱或 firecracker/jailer 进程时执行）
# 注意：clean-host 会连同网络池预建槽位一并清空（比孤儿清理激进）；
# 建议执行后重启 template-manager（本命令不做 nomad 操作）
bash bench.sh clean-host --dry-run   # 先只看数量
bash bench.sh clean-host             # 实际清理：删 ns-* netns、veth-* 接口、引用 veth- 的 iptables 规则
```

**scale 档位上限**：`[scale]` 基础节为 tiers 形式（size=1 / 100 / 200，从 500 降档——当前环境网络池容量与孤儿残留风险的实测平衡点）；旧的 `sizes = [...]` 写法仍然兼容；需要更大档位前先 `bench clean-host` 清理宿主。

**档位前池恢复等待（pre_wait）**：所有 tiers 类型的档位支持可选字段 `pre_wait`（秒，默认 0）。高并发档（create c=20/c=50、scale 200）失败多与系统池子来不及恢复有关，bench.toml 已给这些档加 `pre_wait = 180`。档的 pre_wait > 0 时：先静置等待（每 30s 打印剩余时间到 stderr，期间不创建任何沙箱），随后执行一次 `clean_host_orphans` + `ensure_clean_slate` 快速确认，再开始 warmup/正式测量；等待时间不计入任何测量指标；pre_wait 值记录在每档 JSON 的 params 与 report.md 参数行中，保证数据可溯源。

## 轨迹回放密度测试（bench replay）

借鉴 replay-aenv 的轨迹回放模型，模拟真实 agent 负载节奏：每条轨迹**创建沙箱 → 立即 pause → 每条 action 循环{等待 delay_time（保持 paused，模拟 LLM 推理间隔）→ 抢 RUNNING 名额 → resume → 执行命令 → pause → 释放名额} → 结束后删除沙箱**。大量沙箱以 paused 形态存活（占内存/槽位），同一时刻只有 `--running-concurrency` 个名额处于 RUNNING——与 `density`（空沙箱堆积、测内存/槽位上限）不同，replay 测的是**真实负载节奏下宿主机可稳定承载的 paused+running 混合密度**。

```bash
# 最小示例：合成轨迹自检（不传 --trajectory-dir 时用通用只读命令合成，
# delay_time 从 replay-aenv 真实轨迹分布采样，目录缺失时兜底 0.5~8s 均匀分布）
bash bench.sh replay --target-count 5 -c 5 --running-concurrency 2

# 真实轨迹回放：目录第一层 .json/.traj 文件，target_count 超过文件数时循环复用
bash bench.sh replay --trajectory-dir /path/to/delay_time_trajectories --target-count 60

# 只校验配置与轨迹、打印调度预览，不创建沙箱（不连接环境）
bash bench.sh replay --dry-run --target-count 5
```

真实轨迹模式不传 `-t` 时自动就位任务模板（默认 `django-money-task-2c2g`，2 vCPU / 2048 MiB）：先按名称找 ready 且本地产物齐全的模板复用，找不到则从 `[replay].task_template_image` 配置的 registry 镜像自动构建。镜像本身用下面的脚本一次性构建推送（全新机器）：

```bash
# 需要 replay-aenv 源码（Dockerfile 在其 dockerfiles/ 下）；TARGET_REPO 默认走 gitcode 镜像，
# 可访问 GitHub 时可覆盖为 https://github.com/django-money/django-money.git
bash prepare-replay-image.sh <registry前缀，如 193.30.8.2:30443/e2b-orchestration> [replay-aenv源码目录]
# 然后把 bench.toml [replay].task_template_image 改为 <registry前缀>/django-money:poc_v2
```

| 参数 | 默认 | 含义 |
| --- | --- | --- |
| `--trajectory-dir` | 无（合成轨迹） | 轨迹目录（第一层 .json/.traj；缺省/非法 delay_time 按 0 秒处理并告警） |
| `--target-count` | 60 | 总回放次数；`0` = 每条轨迹一次；超过文件数循环复用 |
| `-c/--concurrency` | 20 | 生命周期并发：同时进行回放的轨迹数（paused 常驻规模上限） |
| `--running-concurrency` | 10 | RUNNING 名额硬上限（RunningSlotScheduler，FIFO + delay_time 延时堆， lease 从预约起算，客户端准入永不超限） |
| `--launch-interval-sec` | 0.3 | 相邻轨迹启动最小间隔 |
| `--control-plane-qps` | 100 | 全局控制面 QPS（SmoothRateLimiter：create/pause/resume/command/cleanup 统一 FIFO 排队，按 1/qps 平滑分发、不补发追突发；瞬断错误 502/503/504/429/timeout/connection reset 等自动重试最多 3 次，重试重新排队） |
| `--action-timeout` | 300 | 单条 action 超时秒 |
| `--synthetic-steps` | 10 | 合成轨迹的步数 |
| `--workdir` | 真实轨迹/mix 为 `/testbed`，合成轨迹不包装 | action 执行前 cd 的工作目录并做 SWE 包装（str_replace_editor 边界换行归一化 + bash -lc） |
| `--cmd-user` | 真实轨迹/mix 为 `root`，合成轨迹为模板默认用户 | 沙箱内执行命令的用户（SWE 工具 registry 状态文件在 /root/.swe-agent-env，非 root 会 PermissionError） |
| `--mix-config` | 无 | 多模板混合回放配置（JSON），与 `--trajectory-dir`/`-t` 互斥 |
| `--snapshot-mode` | `none` | 快照变体：`same-sandbox`（同一沙箱每步执行后原地打快照，测连续快照开销）；`chain`（每步从上一快照重建沙箱→执行→打快照→删除，测快照链式恢复，沙箱不常驻）；报告分别增加 `latency.snapshot` / `latency.reload` 口径 |
| `--write-mode` | `buffered` | `writeTxt N` 动作写入改写：`tmpfs`（dd 写 /dev/shm）/ `directio`（dd oflag=direct 直写 workdir），用于对比不同写入路径对快照耗时的影响 |
| `--dry-run` | 关 | 只校验配置和轨迹、打印调度预览，不创建沙箱 |
| `--mem-threshold-pct` | `global.mem_threshold_pct` | 内存安全闸：MemAvailable 低于阈值时停止发射新轨迹（在途跑完），结果标 `aborted` |

### 多模板混合回放（--mix-config）

对齐 replay-aenv 的 mix 模式：一个批次内混合多个模板/轨迹目录的负载，各负载共享全局并发、RUNNING 名额与控制面 QPS；发射顺序按各负载 `vm_count` 做平滑加权轮询（SWRR）交错，单负载内轨迹循环复用。

```json
{
  "concurrency": 60,
  "workloads": [
    {"name": "django-money", "template": "django-money-task-2c2g", "trajectory_dir": "/data/traces/django-money", "vm_count": 40},
    {"name": "std-2c2g", "template": "e2b/bench-standard-2c2g", "trajectory_dir": "/data/traces/std", "vm_count": 20}
  ]
}
```

```bash
bash bench.sh replay --mix-config ./mix-config.json --dry-run   # 先看调度预览（schedule_head）
bash bench.sh replay --mix-config ./mix-config.json --running-concurrency 30
```

配置字段：`concurrency`（可选，生命周期并发；优先级：命令行 `-c` > 配置 > bench.toml）；`workloads[].name`（缺省取 template，slug 化后必须唯一）；`workloads[].template`（各负载独立模板）；`workloads[].trajectory_dir`（相对路径以配置文件所在目录为基准）；`workloads[].vm_count`（该负载的**总回放次数**，不是并发数）。报告中 `workload_summaries` 按负载分别汇总 total/succeeded/failed/command_failures。

报告字段（`bench_replay.json`）：`summary`（total/succeeded/failed/command_failures/elapsed_sec）；`workload_summaries`（按负载分别汇总 total/succeeded/failed/command_failures，单轨迹模式 workload 恒为 `-`）；`running_slots`（maximum/active/peak_active/waiting/granted/average_queue_wait_sec）；`control_plane`（qps/in_flight/waiting/dispatched/average_wait_sec/max_wait_sec/按操作类型分布）；`latency`（resume/command/pause/queue_wait 各自的 avg/p50/p90/p95/max，毫秒）；`create`（客户端 create_* + 服务端真值 server_* 口径，同 4.7.5）；`memory_curve`（每 5s 采样的 {elapsed_s, alive, mem_available_mb} 曲线）；`trajectories`（每条的 workload/template/create_ms 与逐步 queue_wait/resume/command/pause/exit_code/stderr_tail 明细）。pause/resume 走 REST 计时路径（同 pause-resume），命令执行走 SDK `commands.run`。
