# 沙箱快照回滚（checkpoint / restore）

给运行中的沙箱**原地**打快照并回滚：内存、vCPU、设备状态、根文件系统一次存下，
之后把沙箱放回那一刻。沙箱进程不重建、不重启——快照前就在跑的进程连同 PID 和
启动时刻一起复活，回滚耗时是几十毫秒量级，而不是一次冷启动。

> 本文只讲**部署与运维**。SDK 用法见 `py-sdk/e2b/checkpointd/README.md`。

## 1. 前提

| 项 | 要求 | 不满足会怎样 |
|---|---|---|
| CPU / 内核 | aarch64，内核支持 **HDBSS**（KVM 能力号 `502`，`KVM_CAP_ARM_HW_DIRTY_STATE_TRACK`） | 脏页跟踪不启用，功能照常但每次 checkpoint 全量拷贝 guest 内存 |
| Firecracker | 必须是本仓库 `firecracker/` 构建出的二进制 | 上游发行版没有 `PUT /snapshot/rollback`，restore 直接失败 |
| Python SDK | 必须包含 `e2b/checkpointd/`（本仓库 `py-sdk/`） | `sb.checkpoint` 属性不存在 |
| 文件系统 | **无特殊要求**，根盘 ext4 即可 | —— |

## 2. 配置：默认零配置

orchestrator 启动时自己查 `KVM_CHECK_EXTENSION(502)`：

* 查到 → 脏页跟踪**默认开启**，因为在硬件标脏的机器上开启它几乎不要钱；
* 查不到 → **默认关闭**。这里关闭的是跟踪本身，不是"退回软件方案"：真要在这种机器上
  开启，内核只能靠给每个干净页加写保护、首次写时触发一次 VM exit 来实现，对那些
  从不做快照的沙箱是纯亏，所以默认不开。

所以 950 这类带 HDBSS 的机器**什么都不用配**就能做增量快照，也不会被部署成
"能用但悄悄退化"的状态。

`deploy/.env` 里的四个开关全部是可选覆盖，默认值就是对的：

| 变量 | 默认 | 什么时候需要动 |
|---|---|---|
| `FC_TRACK_DIRTY_PAGES` | 跟随硬件自动判定 | 想绕过自动判定、强制开或关时 |
| `FC_HDBSS_ORDER` | `1`（2 页 / 8 KiB per vCPU） | 写密集负载缓冲区溢出时调大 |
| `FC_HDBSS_REQUIRED` | `false` | 见下节，只在一种窄情况下有意义 |
| `CHECKPOINT_FULL_ROOT` | 开 | 想省掉首次全量那一份时关掉 |

### 脏页跟踪的三种落点

Firecracker 先看 orchestrator 有没有要求脏页跟踪（`armed`），没要求就直接是
`Off`，**不会去碰 HDBSS，也不会启用软件写保护**。所以实际只有三种落点：

| 机器 | `FC_TRACK_DIRTY_PAGES` | 结果 |
|---|---|---|
| 有 HDBSS | 不设 | **`hdbss`** —— 硬件标脏，增量快照。**这是目标部署形态** |
| 无 HDBSS | 不设 | `off` —— 不启用任何跟踪，checkpoint 每次全量拷贝内存 |
| 无 HDBSS | 强制 `true` | `kvm-wp` —— 软件写保护，每个干净页首次写触发 VM exit |

第三种是唯一会走到软件写保护的路径，通常不是你想要的。

### `FC_HDBSS_REQUIRED` 什么时候有用

只有一种情况：机器**报告了**能力号 502（所以跟踪被 arm 了），但 `enable_hdbss()`
实际调用失败，这时 Firecracker 会静默退回 `kvm-wp`。设成 `true` 能让它启动就失败，
而不是带着几十倍的代价上线。

机器压根没有 HDBSS 时这个变量不起作用——那种情况下跟踪根本没 arm，走的是上表第二行。

## 3. 确认真的生效了

orchestrator 启动时会打一行能力自检：

```
checkpoint capabilities  store=/orchestrator/build/checkpoints
                         track_dirty_pages=true
                         track_dirty_pages_reason="hardware dirty state tracking present (KVM capability 502)"
```

`track_dirty_pages_reason` 的三种取值：

| 取值 | 含义 |
|---|---|
| `hardware dirty state tracking present (KVM capability 502)` | ✅ 硬件标脏，增量快照 |
| `no hardware dirty state tracking; software tracking costs a VM exit per clean page` | ⚠️ 没有 HDBSS，已自动关闭跟踪，**每次 checkpoint 全量拷贝内存** |
| `FC_TRACK_DIRTY_PAGES="..."` | 被环境变量强制指定 |

跟踪关闭时还会额外打一条 WARN。**排查"为什么每次快照都很慢"，先看这一行。**

另外每次 checkpoint 的结果里带 `mem_mode` 字段（`full` / `incremental`），
调用方可以据此发现静默退化成全量拷贝的情况。

## 4. 容量规划

产物落在 `${ORCHESTRATOR_BASE_PATH}/build/checkpoints`，默认
`/orchestrator/build/checkpoints`。

`CHECKPOINT_FULL_ROOT` 默认开，意味着**每个沙箱的第一次 checkpoint 会写一份完整的
guest 内存**，之后的都是增量（只占实际脏页）。按

```
沙箱数 × 每沙箱内存  +  各代增量之和
```

预留。举例：20 个 2 GiB 沙箱各做一条 5 代的链、每代脏 128 MiB，约
`20 × 2 GiB + 20 × 4 × 128 MiB ≈ 50 GiB`。

关掉 `CHECKPOINT_FULL_ROOT` 能省掉全量那一份，代价是恢复时早于所有 checkpoint 的
内存页要回模板内存文件去取——集群部署下这意味着回滚过程中访问对象存储。

## 5. 验收

`deploy/checkpoint_verify.py` 做正确性验收，三代现场、逐级回退、跨代前滚、
交替回滚，外加"`kill -9` 掉快照前就在跑的心跳进程后回滚，进程连同 PID 一起复活"
这条最硬的证据。全过约 60 项断言。

```bash
python3 deploy/checkpoint_verify.py --server-ip <你的 SERVER_IP>
```


脚本只打屏、自己不落盘，要留记录用重定向：

```bash
python3 deploy/checkpoint_verify.py --server-ip 10.10.10.10 2>&1 | tee checkpoint-verify.log
```

末尾会按 `create 全量` / `create 增量` / `restore` 三类给出次数与 p50 / min / max。
那是客户端墙钟，样本少、每代现场又大，**只当量级看，不是性能基准**。

## 6. 与原生生命周期操作的边界

checkpoint / restore 和 e2b 原有的 create / connect / pause / kill 都在动同一个
沙箱的磁盘层栈与内存，会互相影响。下面是实测结论：

### 可以做

| 组合 | 说明 |
|---|---|
| checkpoint 之后继续用沙箱 | 照常执行命令、读写文件 |
| **checkpoint 之后 pause / connect** | 磁盘数据完整回来，**包括 checkpoint 之前写的**。多次 checkpoint（多层封存）也一样 |
| restore 之后 pause / connect | 同上 |
| **pause / connect 之后新建 checkpoint 并 restore** | 完整可用 |
| restore 被拒绝之后继续用沙箱 | 拒绝是干净的，不留半吊子状态 |

### 不能做（边界）

| 组合 | 行为 | 为什么 |
|---|---|---|
| **pause / connect 之后 restore 到 pause 之前的 checkpoint** | 拒绝，报 `checkpoint ... not found` | pause 会把当时的层栈整体压进沙箱快照，resume 起来的是一个新的沙箱实例，**旧 checkpoint 账本不跨 pause**。`checkpoint.list()` 在 resume 之后返回空 |
| kill 之后 connect | 拒绝，报 `Paused sandbox ... not found` | e2b 原本就是这样，与 checkpoint 无关 |

**checkpoint 不跨 pause/resume** 是目前唯一需要使用者知道的限制。要在 pause 之后
还能回到某个状态，先 resume、再重新做一次 checkpoint。

### 没有被破坏的

矩阵里 **BROKEN 项为 0** —— 没有任何一个 e2b 原生能力因为引入 checkpoint/restore
而不可用或结果错误。判定一律用 `O_DIRECT` 读回 8 MiB 随机数据比 sha256，
绕开 guest page cache（走缓存会把磁盘层的问题完全盖住）。

## 7. 排查

| 现象 | 多半是 |
|---|---|
| `sb.checkpoint` 属性不存在 | SDK 里没有 `e2b/checkpointd/`，装的是上游 PyPI 包 |
| restore 报 404 / 不支持 | Firecracker 不是本仓库构建的，没有 `PUT /snapshot/rollback` |
| 每次 checkpoint 都很慢、`mem_mode` 恒为 `full` | 脏页跟踪没开——看第 3 节那行日志 |
| Firecracker 启动即失败，日志提到 HDBSS | 设了 `FC_HDBSS_REQUIRED=true`，且机器报告了能力 502 但实际启用失败。这是预期行为 |
| 写密集负载下增量收益不明显 | HDBSS 缓冲区溢出，调大 `FC_HDBSS_ORDER` |
| checkpoints 目录撑爆磁盘 | 见第 4 节容量规划；确认 `CHECKPOINT_FULL_ROOT` 是否需要关掉 |
