"""bench replay: 轨迹回放密度测试（借鉴 replay-aenv 的密度模型）。

每条轨迹的生命周期：创建沙箱 → 立即 pause → 每条 action 循环
{等待 delay_time（保持 paused）→ 抢 RUNNING 名额 → resume → 执行命令 → pause → 释放名额}
→ 结束后删除沙箱。大量沙箱以 paused 形态存活（占内存/槽位），
同时只有 running_concurrency 个名额在 RUNNING——测的是真实 agent 负载节奏下
宿主机可稳定承载的 paused+running 混合密度（与 density 的空沙箱堆积上限互补）。
"""

from __future__ import annotations

import argparse
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from ..e2b_common import positive_int, print_json
from e2b import CommandExitException
from . import config as bench_config
from . import report as bench_report
from . import sdk_engine
from .common import (
    BenchContext,
    add_common_arguments,
    add_template_argument,
    base_result,
    build_context,
    cleanup_created,
    clean_host_orphans,
    collect_environment,
    connect_sdk,
    delete_snapshot,
    ensure_clean_slate,
    finish_result,
    guarded,
    read_meminfo,
)
from .scheduler import (
    OperationType,
    RunningSlotScheduler,
    SmoothRateLimiter,
    is_transient_sandbox_error,
)
from .trajectory import (
    WRITE_MODES,
    MixTask,
    ReplayStep,
    build_mix_schedule,
    find_trajectories,
    generate_synthetic_trajectories,
    load_mix_config,
    load_trajectory,
    rewrite_write_txt_action,
    wrap_action,
)

TRANSIENT_RETRY_ATTEMPTS = 3  # 瞬断错误最大重试次数（重试重新进限流队列）
SNAPSHOT_MODES = ("none", "same-sandbox", "chain")


def _register_summary(subparsers) -> None:
    parser = subparsers.add_parser(
        "replay-summary",
        help="汇总 test-results 下所有 replay 运行，输出跨档位密度对比表（配 density 爬坡使用）",
    )
    parser.add_argument("--last", type=positive_int, help="只取最近 N 场（默认全部）")
    parser.add_argument("--config", type=Path, help="bench 配置文件路径（用于定位 result_root）")
    parser.add_argument("-o", "--output", type=Path, help="汇总表另存为 Markdown 文件")
    parser.set_defaults(handler=execute_summary)


def execute_summary(args: argparse.Namespace) -> int:
    import json as _json
    import os as _os

    cfg = bench_config.load(args.config)
    root = bench_config.resolve_result_root(cfg)
    rows: list[dict] = []
    for path in sorted(root.glob("*-bench/bench_replay.json"), key=lambda p: _os.path.getmtime(p)):
        try:
            data = _json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        summary = data.get("summary") or {}
        latency = data.get("latency") or {}
        slots = data.get("running_slots") or {}
        curve = data.get("memory_curve") or []
        params = data.get("params") or {}

        def _p95(key: str) -> float | None:
            stats = latency.get(key) or {}
            return stats.get("p95_ms")

        rows.append({
            "run": path.parent.name.replace("-bench", ""),
            "档位": params.get("target_count"),
            "名额": params.get("running_concurrency"),
            "成功率": f"{summary.get('succeeded', 0)}/{summary.get('total', 0)}",
            "耗时s": summary.get("elapsed_sec"),
            "命令失败": summary.get("command_failures", 0),
            "名额peak": slots.get("peak_active"),
            "平均排队s": round(slots.get("average_queue_wait_sec") or 0, 1),
            "resume_p95": _p95("resume"),
            "command_p95": _p95("command"),
            "pause_p95": _p95("pause"),
            "alive峰值": max((p.get("alive", 0) for p in curve), default=None),
            "可用内存最低MiB": min((p.get("mem_available_mb", 0) for p in curve), default=None),
            "status": data.get("status"),
        })
    if args.last:
        rows = rows[-args.last:]
    if not rows:
        print(f"没有找到 replay 结果（{root}/*-bench/bench_replay.json）")
        return 1

    headers = ["run", "档位", "名额", "成功率", "耗时s", "命令失败", "名额peak",
               "平均排队s", "resume_p95", "command_p95", "pause_p95",
               "alive峰值", "可用内存最低MiB", "status"]
    lines = ["| " + " | ".join(headers) + " |", "|" + "---|" * len(headers)]
    for row in rows:
        lines.append("| " + " | ".join(str(row.get(h, "")) for h in headers) + " |")
    table = "\n".join(lines)
    print(table)
    if args.output:
        args.output.write_text(table + "\n", encoding="utf-8")
        print(f"已写入 {args.output}")
    return 0


def register(subparsers) -> None:
    parser = subparsers.add_parser(
        "replay",
        help="轨迹回放密度测试：创建→pause→按 delay_time 逐条 resume 执行→pause（paused 常驻 + 限量 RUNNING）",
    )
    parser.add_argument("--trajectory-dir", type=Path, help="轨迹目录（第一层 .json/.traj；不传则用合成轨迹）")
    parser.add_argument(
        "--mix-config",
        type=Path,
        help="多模板混合回放配置（JSON：{concurrency?, workloads: [{name?, template, trajectory_dir, vm_count}]}；"
        "与 --trajectory-dir/-t 互斥，各负载共享全局并发/RUNNING 名额/控制面 QPS）",
    )
    parser.add_argument(
        "--target-count",
        type=int,
        help="总回放次数（默认 60；0 = 每条轨迹一次；超过文件数循环复用）",
    )
    parser.add_argument("-c", "--concurrency", type=positive_int, help="生命周期并发：同时进行回放的轨迹数（默认 20）")
    parser.add_argument("--running-concurrency", type=positive_int, help="RUNNING 名额硬上限（默认 10）")
    parser.add_argument("--launch-interval-sec", type=float, help="相邻轨迹启动最小间隔秒（默认 0.3）")
    parser.add_argument("--control-plane-qps", type=float, help="全局控制面 QPS（默认 100）")
    parser.add_argument("--action-timeout", type=positive_int, help="单条 action 超时秒（默认 300）")
    parser.add_argument(
        "--workdir",
        help="action 执行前 cd 的工作目录并做 SWE 包装（str_replace_editor 归一化 + bash -lc）；"
        "默认：--trajectory-dir 模式为 /testbed，合成轨迹不包装",
    )
    parser.add_argument(
        "--cmd-user",
        help="沙箱内执行命令的用户；默认：--trajectory-dir 模式为 root（对齐 replay-aenv task.toml），"
        "合成轨迹为模板默认用户",
    )
    parser.add_argument("--synthetic-steps", type=positive_int, help="合成轨迹的步数（默认 10）")
    parser.add_argument("--dry-run", action="store_true", help="只校验配置和轨迹、打印调度预览，不创建沙箱")
    parser.add_argument(
        "--snapshot-mode",
        choices=SNAPSHOT_MODES,
        help="快照变体：none（默认，paused 常驻）；same-sandbox（同一沙箱每步执行后原地打快照，"
        "测连续快照开销，对齐 replay_agent_snapshot_same_sandbox）；chain（每步从上一快照重建沙箱、"
        "执行、打快照、删除，测快照链式恢复，对齐 replay_agent_snapshot）",
    )
    parser.add_argument(
        "--write-mode",
        choices=WRITE_MODES,
        help="writeTxt N 动作的写入模式改写：buffered（默认不改写）/tmpfs（dd 写 /dev/shm）/"
        "directio（dd oflag=direct 直写 workdir），用于测量不同写入路径对快照增长的影响",
    )
    parser.add_argument(
        "--mem-threshold-pct",
        type=float,
        help="内存安全闸：MemAvailable 低于总内存该百分比时停止发射新轨迹（默认取 global.mem_threshold_pct）",
    )
    add_template_argument(parser)
    add_common_arguments(parser)
    parser.add_argument("-o", "--output", help="JSON 报告输出路径")
    parser.add_argument("--sandbox-timeout", type=positive_int, help="沙箱生命周期秒数")
    parser.set_defaults(handler=guarded(execute))
    _register_summary(subparsers)


def _retry(operation, *, what: str, attempts: int = TRANSIENT_RETRY_ATTEMPTS):
    """瞬断错误重试：每次重试都重新走 operation 内部的限流队列；非瞬断直接抛。"""
    for attempt in range(1, attempts + 1):
        try:
            return operation()
        except Exception as exc:
            if attempt >= attempts or not is_transient_sandbox_error(exc):
                raise
            print(f"  [重试] {what} 第 {attempt} 次瞬断失败：{str(exc)[:80]}")
    raise AssertionError("unreachable")


def run(
    ctx: BenchContext,
    *,
    tasks: list[MixTask],
    concurrency: int,
    running_concurrency: int,
    launch_interval_sec: float,
    control_plane_qps: float,
    action_timeout: int,
    mem_threshold_pct: float,
    workdir: str | None,
    cmd_user: str | None,
    snapshot_mode: str = "none",
    write_mode: str = "buffered",
) -> dict:
    target_count = len(tasks)
    templates = sorted({task.template for task in tasks})
    workloads = sorted({task.workload for task in tasks if task.workload != "-"})
    params = {
        "target_count": target_count,
        "concurrency": concurrency,
        "running_concurrency": running_concurrency,
        "launch_interval_sec": launch_interval_sec,
        "control_plane_qps": control_plane_qps,
        "action_timeout": action_timeout,
        "mem_threshold_pct": mem_threshold_pct,
        "workdir": workdir,
        "cmd_user": cmd_user,
        "snapshot_mode": snapshot_mode,
        "write_mode": write_mode,
        "trajectory_count": len({(task.workload, task.trajectory) for task in tasks}),
        "templates": templates,
        "workloads": workloads,
    }
    result = base_result("replay", ctx, params)
    clean_host_orphans(ctx, "replay-pre")
    sdk_engine._ensure_shared_api_client()

    baseline = read_meminfo()
    total_kb = baseline.get("MemTotal", 0)
    threshold_kb = total_kb * mem_threshold_pct / 100
    result["baseline"] = {
        "mem_total_mb": round(total_kb / 1024),
        "mem_available_mb": round(baseline.get("MemAvailable", 0) / 1024),
        "threshold_mb": round(threshold_kb / 1024),
    }

    scheduler = RunningSlotScheduler(running_concurrency)
    limiter = SmoothRateLimiter(qps=control_plane_qps, inflight_cap=concurrency)

    lock = threading.Lock()
    abort_event = threading.Event()
    sampler_stop = threading.Event()
    alive = 0
    records: list[dict] = []
    memory_curve: list[dict] = []
    create_ms_samples: list[float] = []
    server_ms_samples: list[float] = []
    resume_ms_samples: list[float] = []
    command_ms_samples: list[float] = []
    pause_ms_samples: list[float] = []
    queue_wait_ms_samples: list[float] = []
    reload_ms_samples: list[float] = []
    snapshot_ms_samples: list[float] = []
    started_monotonic = time.monotonic()

    def _memory_sampler() -> None:
        while not sampler_stop.wait(5.0):
            meminfo = read_meminfo()
            available_kb = meminfo.get("MemAvailable", 0)
            with lock:
                current_alive = alive
            memory_curve.append({
                "elapsed_s": round(time.monotonic() - started_monotonic, 1),
                "alive": current_alive,
                "mem_available_mb": round(available_kb / 1024),
            })
            if available_kb < threshold_kb and not abort_event.is_set():
                abort_event.set()
                ctx.note(
                    f"内存安全闸触发：MemAvailable {round(available_kb / 1024)} MiB "
                    f"低于阈值 {round(threshold_kb / 1024)} MiB（总内存 {mem_threshold_pct}%），"
                    "停止发射新轨迹，在途轨迹跑完"
                )

    def _create(task: MixTask, index: int) -> dict:
        def _op() -> dict:
            with limiter.slot(OperationType.CREATE):
                outcome = sdk_engine.create_one(task.template, index, timeout=ctx.sandbox_timeout)
            if not outcome["ok"]:
                raise RuntimeError(outcome["error"])
            return outcome

        return _retry(_op, what=f"轨迹 {index} create")

    def _pause(sandbox_id: str) -> float:
        def _op() -> float:
            with limiter.slot(OperationType.PAUSE):
                timed = ctx.client.pause_timed(sandbox_id)
            if not timed.ok:
                raise RuntimeError(timed.error or "pause failed")
            return timed.latency_ms

        return _retry(_op, what=f"沙箱 {sandbox_id[:8]} pause")

    def _resume(sandbox_id: str) -> float:
        def _op() -> float:
            with limiter.slot(OperationType.RESUME):
                timed = ctx.client.resume_timed(sandbox_id, timeout=ctx.sandbox_timeout)
            if not timed.ok:
                raise RuntimeError(timed.error or "resume failed")
            return timed.latency_ms

        return _retry(_op, what=f"沙箱 {sandbox_id[:8]} resume")

    def _command(sandbox, action: str) -> tuple[float, int, str]:
        def _op() -> tuple[float, int, str]:
            # 同步 SDK 的 commands.run 建连与执行不可拆分，COMMAND 名额覆盖整个调用；
            # 实际并发由 running_concurrency 硬上限兜底，限流只平滑启动节奏
            with limiter.slot(OperationType.COMMAND):
                started = time.perf_counter()
                try:
                    completed = sandbox.commands.run(action, timeout=action_timeout, user=cmd_user)
                except CommandExitException as exc:
                    # 命令非零退出是负载内容本身（agent 探索失败、编辑不匹配等），
                    # 不是基础设施错误：记录 exit_code 继续回放（对齐 replay-aenv
                    # stop_on_error=False 语义），不进瞬断重试
                    return (time.perf_counter() - started) * 1000, exc.exit_code, (exc.stderr or "")[-300:]
            return (time.perf_counter() - started) * 1000, completed.exit_code, ""

        return _retry(_op, what=f"命令 {action[:40]!r}")

    def _prepare_action(raw_action: str) -> str:
        action = raw_action
        if write_mode != "buffered":
            action = rewrite_write_txt_action(action, write_mode, workdir or "/testbed")
        return wrap_action(action, workdir) if workdir else action

    def _snapshot(sandbox) -> tuple[float, str | None]:
        """原地打一次快照（SDK 路径，SNAPSHOT 名额），返回 (耗时ms, snapshotID)。"""

        def _op():
            with limiter.slot(OperationType.SNAPSHOT):
                timed = sdk_engine.snapshot_one(sandbox)
            if not timed.ok:
                raise RuntimeError(timed.error or "snapshot failed")
            return timed

        timed = _retry(_op, what="sandbox snapshot")
        snapshot_id = timed.data.get("snapshotID") if isinstance(timed.data, dict) else None
        return timed.latency_ms, snapshot_id

    def _reload(base: str, index: int) -> tuple[float, str]:
        """从模板或快照 ID 重建沙箱（RELOAD 名额），返回 (耗时ms, sandbox_id)。"""

        def _op():
            with limiter.slot(OperationType.RELOAD):
                timed = ctx.client.create_timed(
                    base, timeout=ctx.sandbox_timeout, metadata=ctx.metadata
                )
            if not timed.ok:
                raise RuntimeError(timed.error or "reload failed")
            return timed

        timed = _retry(_op, what=f"轨迹 {index} reload")
        return timed.latency_ms, timed.sandbox_id

    def _untrack(sandbox_id: str) -> None:
        with ctx._lock:
            if sandbox_id in ctx.created_ids:
                ctx.created_ids.remove(sandbox_id)

    def _delete_snapshots(snapshot_ids: list[str], label: str) -> None:
        for snapshot_id in snapshot_ids:
            if not delete_snapshot(ctx.client, snapshot_id):
                ctx.note(f"轨迹 {label} 的快照 {snapshot_id} 删除失败（需手工清理）")

    def _kill(sandbox_id: str) -> None:
        def _op() -> None:
            with limiter.slot(OperationType.CLEANUP):
                response = ctx.client.kill(sandbox_id)
            if response.status not in (200, 204, 404):
                raise RuntimeError(f"kill status={response.status}")

        try:
            _retry(_op, what=f"沙箱 {sandbox_id[:8]} kill")
        except Exception as exc:
            ctx.note(f"轨迹沙箱 {sandbox_id} 销毁失败：{str(exc)[:120]}（残留由收尾清理兜底）")

    def _replay_one(index: int) -> None:
        if snapshot_mode == "chain":
            _replay_chain(index)
            return
        nonlocal alive
        task = tasks[index]
        name = task.trajectory
        steps = task.steps
        task_id = f"replay-{index}"
        record: dict = {
            "index": index,
            "workload": task.workload,
            "template": task.template,
            "trajectory": name,
            "ok": False,
            "create_ms": None,
            "server_ms": None,
            "steps": [],
            "failed_commands": 0,
            "snapshot_count": 0,
            "error": None,
        }
        sandbox_id: str | None = None
        killed = False
        snapshot_ids: list[str] = []
        try:
            outcome = _create(task, index)
            sandbox_id = outcome["sandbox_id"]
            ctx.track(sandbox_id)
            with lock:
                alive += 1
            record["create_ms"] = round(outcome["create_time_s"] * 1000, 1)
            with lock:
                create_ms_samples.append(record["create_ms"])
            # 计时窗外回读服务端真值（server_ms = startedAt - 请求发出时刻）
            started_at = ctx.client.sandbox_started_at(sandbox_id)
            if started_at is not None:
                server_ms = round((started_at - outcome["request_wall"]) * 1000, 1)
                record["server_ms"] = server_ms
                with lock:
                    server_ms_samples.append(server_ms)

            # create 的 instance 不带 sandbox_url 代理路由，命令执行走 connect_sdk 重连句柄
            #（与 snapshot_dirty 的既有路径一致）
            sandbox = connect_sdk(sandbox_id, timeout=ctx.sandbox_timeout)

            record["initial_pause_ms"] = round(_pause(sandbox_id), 1)

            for step_index, step in enumerate(steps):
                lease = scheduler.acquire(
                    task_id, ready_at=time.monotonic() + step.delay_time_sec
                )
                step_record: dict = {
                    "index": step_index,
                    "delay_time_sec": step.delay_time_sec,
                    "queue_wait_ms": round(lease.queue_wait_sec * 1000, 1),
                    "resume_ms": None,
                    "command_ms": None,
                    "pause_ms": None,
                    "exit_code": None,
                }
                try:
                    step_record["resume_ms"] = round(_resume(sandbox_id), 1)
                    action = _prepare_action(step.action)
                    command_ms, exit_code, stderr_tail = _command(sandbox, action)
                    step_record["command_ms"] = round(command_ms, 1)
                    step_record["exit_code"] = exit_code
                    if exit_code != 0:
                        step_record["stderr_tail"] = stderr_tail
                        record["failed_commands"] += 1
                    if snapshot_mode == "same-sandbox":
                        # 每步执行后原地打快照（不删不重建），测连续快照开销
                        snapshot_ms, snapshot_id = _snapshot(sandbox)
                        step_record["snapshot_ms"] = round(snapshot_ms, 1)
                        if snapshot_id:
                            snapshot_ids.append(snapshot_id)
                    step_record["pause_ms"] = round(_pause(sandbox_id), 1)
                except BaseException:
                    # lease 必须持有到沙箱确认 paused 或 deleted 为止
                    try:
                        _pause(sandbox_id)
                    except Exception:
                        _kill(sandbox_id)
                        killed = True
                        with lock:
                            alive -= 1
                    raise
                finally:
                    lease.release()
                record["steps"].append(step_record)
                with lock:
                    queue_wait_ms_samples.append(step_record["queue_wait_ms"])
                    resume_ms_samples.append(step_record["resume_ms"])
                    command_ms_samples.append(command_ms)
                    pause_ms_samples.append(step_record["pause_ms"])
                    if "snapshot_ms" in step_record:
                        snapshot_ms_samples.append(step_record["snapshot_ms"])
            record["ok"] = True
        except Exception as exc:
            record["error"] = f"{type(exc).__name__}: {exc}"[:300]
        finally:
            if sandbox_id is not None and not killed:
                _kill(sandbox_id)
                with lock:
                    alive -= 1
            record["snapshot_count"] = len(snapshot_ids)
            _delete_snapshots(snapshot_ids, f"replay-{index}")
            with lock:
                records.append(record)

    def _replay_chain(index: int) -> None:
        """chain 快照变体：每步从上一快照重建沙箱 → 执行 → 打快照 → 删除。

        对齐 replay-aenv replay_agent_snapshot.py：沙箱不常驻，快照作为下一棒的
        templateID（持久 checkpoint），测快照链式恢复性能。与 paused 常驻模型对照。
        """
        nonlocal alive
        task = tasks[index]
        task_id = f"replay-{index}"
        record: dict = {
            "index": index,
            "workload": task.workload,
            "template": task.template,
            "trajectory": task.trajectory,
            "ok": False,
            "create_ms": None,
            "server_ms": None,
            "steps": [],
            "failed_commands": 0,
            "snapshot_count": 0,
            "error": None,
        }
        current_snapshot_id: str | None = None
        snapshot_ids: list[str] = []
        sandbox_id: str | None = None
        try:
            for step_index, step in enumerate(task.steps):
                lease = scheduler.acquire(
                    task_id, ready_at=time.monotonic() + step.delay_time_sec
                )
                step_record: dict = {
                    "index": step_index,
                    "delay_time_sec": step.delay_time_sec,
                    "queue_wait_ms": round(lease.queue_wait_sec * 1000, 1),
                    "reload_ms": None,
                    "command_ms": None,
                    "snapshot_ms": None,
                    "exit_code": None,
                }
                try:
                    # 首步从模板创建，后续从上一快照重建
                    reload_ms, sandbox_id = _reload(current_snapshot_id or task.template, index)
                    step_record["reload_ms"] = round(reload_ms, 1)
                    ctx.track(sandbox_id)
                    with lock:
                        alive += 1
                        reload_ms_samples.append(step_record["reload_ms"])
                    sandbox = connect_sdk(sandbox_id, timeout=ctx.sandbox_timeout)
                    action = _prepare_action(step.action)
                    command_ms, exit_code, stderr_tail = _command(sandbox, action)
                    step_record["command_ms"] = round(command_ms, 1)
                    step_record["exit_code"] = exit_code
                    if exit_code != 0:
                        step_record["stderr_tail"] = stderr_tail
                        record["failed_commands"] += 1
                    snapshot_ms, snapshot_id = _snapshot(sandbox)
                    step_record["snapshot_ms"] = round(snapshot_ms, 1)
                    if not snapshot_id:
                        raise RuntimeError("快照响应缺少 snapshotID，无法链式继续")
                    snapshot_ids.append(snapshot_id)
                    current_snapshot_id = snapshot_id
                    _kill(sandbox_id)
                    _untrack(sandbox_id)
                    with lock:
                        alive -= 1
                    sandbox_id = None
                except BaseException:
                    if sandbox_id is not None:
                        _kill(sandbox_id)
                        _untrack(sandbox_id)
                        with lock:
                            alive -= 1
                        sandbox_id = None
                    raise
                finally:
                    lease.release()
                record["steps"].append(step_record)
                with lock:
                    queue_wait_ms_samples.append(step_record["queue_wait_ms"])
                    command_ms_samples.append(command_ms)
                    snapshot_ms_samples.append(step_record["snapshot_ms"])
            record["ok"] = True
        except Exception as exc:
            record["error"] = f"{type(exc).__name__}: {exc}"[:300]
        finally:
            if sandbox_id is not None:
                _kill(sandbox_id)
                _untrack(sandbox_id)
                with lock:
                    alive -= 1
            record["snapshot_count"] = len(snapshot_ids)
            _delete_snapshots(snapshot_ids, f"replay-{index}")
            with lock:
                records.append(record)

    sampler = threading.Thread(target=_memory_sampler, name="bench-replay-meminfo", daemon=True)
    sampler.start()
    launched = 0
    try:
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = []
            for index in range(target_count):
                if abort_event.is_set():
                    break
                futures.append(pool.submit(_replay_one, index))
                launched += 1
                if launch_interval_sec > 0 and index + 1 < target_count:
                    # 发射间隔分段睡眠，内存闸触发时能及时停发
                    deadline = time.monotonic() + launch_interval_sec
                    while not abort_event.is_set() and time.monotonic() < deadline:
                        time.sleep(min(0.1, deadline - time.monotonic()))
            for future in futures:
                future.result()
    finally:
        sampler_stop.set()
        sampler.join(timeout=10)
        scheduler.close()

    succeeded = sum(1 for record in records if record["ok"])
    failed = len(records) - succeeded
    command_failures = sum(record["failed_commands"] for record in records)
    result["summary"] = {
        "target": target_count,
        "total": len(records),
        "succeeded": succeeded,
        "failed": failed,
        "command_failures": command_failures,
        "snapshots_created": sum(record["snapshot_count"] for record in records),
        "elapsed_sec": round(time.monotonic() - started_monotonic, 1),
    }
    # 按 workload 汇总（mix 模式定位哪个负载拖后腿；单轨迹模式 workload 恒为 "-"）
    by_workload: dict[str, dict] = {}
    for record in records:
        bucket = by_workload.setdefault(
            record["workload"],
            {"template": record["template"], "total": 0, "succeeded": 0, "failed": 0, "command_failures": 0},
        )
        bucket["total"] += 1
        bucket["succeeded" if record["ok"] else "failed"] += 1
        bucket["command_failures"] += record["failed_commands"]
    result["workload_summaries"] = by_workload
    if command_failures:
        ctx.note(
            f"共 {command_failures} 条 action 非零退出（agent 负载内容本身，"
            "非基础设施错误；明细见 trajectories[].steps[].stderr_tail）"
        )
    result["running_slots"] = scheduler.snapshot()
    result["control_plane"] = limiter.snapshot()
    result["latency"] = {
        "resume": sdk_engine.latency_stats(resume_ms_samples),
        "command": sdk_engine.latency_stats(command_ms_samples),
        "pause": sdk_engine.latency_stats(pause_ms_samples),
        "queue_wait": sdk_engine.latency_stats(queue_wait_ms_samples),
    }
    if reload_ms_samples:
        result["latency"]["reload"] = sdk_engine.latency_stats(reload_ms_samples)
    if snapshot_ms_samples:
        result["latency"]["snapshot"] = sdk_engine.latency_stats(snapshot_ms_samples)
    result["create"] = {
        **sdk_engine.latency_stats(create_ms_samples, "create"),
        "server": sdk_engine.latency_stats(server_ms_samples, "server") if server_ms_samples else None,
        "server_samples": len(server_ms_samples),
    }
    result["memory_curve"] = memory_curve
    result["trajectories"] = records

    # 收尾清理失败（如 result_dir 被外部清理）不能毁掉整场数据，先出报告再清理
    try:
        leftovers = cleanup_created(ctx)
        if leftovers.get("failed"):
            ctx.note(f"收尾清理存在失败：{leftovers}")
        elif leftovers.get("deleted"):
            ctx.note(f"轨迹外残留沙箱已清理：{leftovers}")
        ensure_clean_slate(ctx, "replay-final")
    except Exception as exc:
        ctx.note(f"收尾清理异常（数据已保留）：{type(exc).__name__}: {exc}")

    if abort_event.is_set():
        result["status"] = "aborted"
        result["error"] = "内存安全闸触发，提前停止发射新轨迹（已记录中止前数据）"
    elif succeeded == 0:
        result["status"] = "failed"
        result["error"] = "没有轨迹回放成功"
    return finish_result(result, ctx)


def _resolve_param(cli_value, section: dict, key: str, default):
    return cli_value if cli_value is not None else section.get(key, default)


def execute(args: argparse.Namespace) -> int:
    cfg = bench_config.load(args.config)
    section = cfg.get("replay", {})
    target_count = int(_resolve_param(args.target_count, section, "target_count", 60))
    if target_count < 0:
        raise ValueError("--target-count 不能为负数（0 = 每条轨迹一次）")
    running_concurrency = int(_resolve_param(args.running_concurrency, section, "running_concurrency", 10))
    launch_interval_sec = float(_resolve_param(args.launch_interval_sec, section, "launch_interval_sec", 0.3))
    control_plane_qps = float(_resolve_param(args.control_plane_qps, section, "control_plane_qps", 100))
    action_timeout = int(_resolve_param(args.action_timeout, section, "action_timeout", 300))
    synthetic_steps = int(_resolve_param(args.synthetic_steps, section, "synthetic_steps", 10))
    snapshot_mode = _resolve_param(args.snapshot_mode, section, "snapshot_mode", "none")
    write_mode = _resolve_param(args.write_mode, section, "write_mode", "buffered")
    mem_threshold_pct = (
        args.mem_threshold_pct
        if args.mem_threshold_pct is not None
        else float(bench_config.global_param(cfg, "mem_threshold_pct"))
    )

    if args.mix_config and (args.trajectory_dir or args.template):
        raise ValueError("--mix-config 与 --trajectory-dir/-t 互斥（模板由配置中各 workload 指定）")

    # 轨迹准备先于 build_context：dry-run 不连接环境即可完成校验与预览
    mix_config_concurrency: int | None = None
    if args.mix_config:
        mix_config_concurrency, workloads = load_mix_config(args.mix_config)
        tasks = build_mix_schedule(workloads)
        trajectory_source = str(Path(args.mix_config).expanduser().resolve())
        # mix 与真实轨迹同语义：SWE 包装 + root 执行（可用 --workdir/--cmd-user 覆盖）
        workdir = args.workdir if args.workdir is not None else "/testbed"
        cmd_user = args.cmd_user if args.cmd_user is not None else "root"
        template = "mix:" + "+".join(workload.name for workload in workloads)
    else:
        template = bench_config.resolve_template(args.template, cfg)
        if args.trajectory_dir:
            paths = find_trajectories(args.trajectory_dir)
            if not paths:
                raise ValueError(f"轨迹目录中没有 .json/.traj 文件：{args.trajectory_dir}")
            trajectories: list[tuple[str, list[ReplayStep]]] = [
                (path.name, load_trajectory(path)) for path in paths
            ]
            trajectory_source = str(Path(args.trajectory_dir).expanduser().resolve())
            # 真实轨迹来自 SWE 任务模板，默认在 /testbed 下执行并做 SWE 包装
            workdir = args.workdir if args.workdir is not None else "/testbed"
            # 对齐 replay-aenv task.toml（user = "root"）：SWE 工具的 registry
            # 状态文件在 /root/.swe-agent-env，非 root 执行 str_replace_editor 会 PermissionError
            cmd_user = args.cmd_user if args.cmd_user is not None else "root"
        else:
            synthetic_count = target_count if target_count > 0 else 60
            trajectories = [
                (f"synthetic-{index}", steps)
                for index, steps in enumerate(
                    generate_synthetic_trajectories(synthetic_count, synthetic_steps, rng_seed=42)
                )
            ]
            trajectory_source = f"合成轨迹（{synthetic_count} 条 × {synthetic_steps} 步）"
            workdir = args.workdir or None
            cmd_user = args.cmd_user
        effective_target = target_count if target_count > 0 else len(trajectories)
        tasks = [
            MixTask(
                workload="-",
                template=template,
                trajectory=trajectories[index % len(trajectories)][0],
                steps=tuple(trajectories[index % len(trajectories)][1]),
            )
            for index in range(effective_target)
        ]

    # 并发优先级：命令行 > mix 配置 > bench.toml > 内置默认
    concurrency = int(
        args.concurrency
        if args.concurrency is not None
        else (mix_config_concurrency or section.get("concurrency", 20))
    )

    if args.dry_run:
        step_counts = [len(task.steps) for task in tasks]
        delay_total = sum(step.delay_time_sec for task in tasks for step in task.steps)
        preview = {
            "dry_run": True,
            "trajectory_source": trajectory_source,
            "target_count": len(tasks),
            "steps_per_trajectory": {
                "min": min(step_counts),
                "max": max(step_counts),
                "avg": round(sum(step_counts) / len(step_counts), 1),
            },
            "delay_time_total_sec": round(delay_total, 1),
            "concurrency": concurrency,
            "running_concurrency": running_concurrency,
            "launch_interval_sec": launch_interval_sec,
            "control_plane_qps": control_plane_qps,
            "action_timeout": action_timeout,
            "mem_threshold_pct": mem_threshold_pct,
            "workdir": workdir,
            "cmd_user": cmd_user,
            "snapshot_mode": snapshot_mode,
            "write_mode": write_mode,
        }
        if args.mix_config:
            preview["workloads"] = [
                {
                    "name": workload.name,
                    "template": workload.template,
                    "vm_count": workload.vm_count,
                    "trajectory_dir": str(workload.trajectory_dir),
                }
                for workload in workloads
            ]
            preview["schedule_head"] = [
                f"{task.workload}:{task.trajectory}" for task in tasks[:10]
            ]
        print(
            f"[replay dry-run] 轨迹来源：{trajectory_source}；目标回放 {len(tasks)} 次；"
            f"生命周期并发 {concurrency}，RUNNING 名额 {running_concurrency}，"
            f"控制面 {control_plane_qps} QPS"
        )
        print_json(preview)
        return 0

    if not args.mix_config and args.trajectory_dir and not template:
        # 真实轨迹未显式指定模板：自动就位任务模板（查 ready 复用 / 从 registry 镜像构建），
        # 镜像不存在时先用 e2b-scripts/prepare-replay-image.sh 构建推送
        from .client import BenchClient
        from .replay_template import ensure_replay_task_template

        template, _task_name, _task_source = ensure_replay_task_template(
            BenchClient(timeout=600),
            name=str(section.get("task_template_name", "django-money-task-2c2g")),
            image=str(
                section.get(
                    "task_template_image",
                    "193.30.8.2:30443/e2b-orchestration/django-money:poc_v2",
                )
            ),
            cpu=int(section.get("task_template_cpu", 2)),
            memory_mb=int(section.get("task_template_memory_mb", 2048)),
        )

    ctx = build_context(
        template=template,
        result_root=bench_config.resolve_result_root(cfg),
        netns_growth_threshold=int(bench_config.global_param(cfg, "netns_growth_threshold")),
        sandbox_timeout=args.sandbox_timeout or max(
            3600, int(bench_config.global_param(cfg, "sandbox_timeout"))
        ),
    )
    if not args.mix_config and not template:
        # 未显式指定模板时 build_context 自动解析/构建基准模板，
        # 任务的模板以解析结果为准（否则空模板会导致 400 Invalid template reference）
        tasks = [MixTask(t.workload, ctx.template, t.trajectory, t.steps) for t in tasks]
    result = run(
        ctx,
        tasks=tasks,
        concurrency=concurrency,
        running_concurrency=running_concurrency,
        launch_interval_sec=launch_interval_sec,
        control_plane_qps=control_plane_qps,
        action_timeout=action_timeout,
        mem_threshold_pct=mem_threshold_pct,
        workdir=workdir,
        cmd_user=cmd_user,
        snapshot_mode=snapshot_mode,
        write_mode=write_mode,
    )
    environment = collect_environment(ctx.client, ctx.template, template_source=ctx.template_source)
    json_path = bench_report.write_bench_json(ctx.result_dir, "replay", result)
    if args.output:
        import shutil

        shutil.copyfile(json_path, args.output)
    bench_report.write_report(ctx.result_dir, [result], environment, ctx.run_id)
    print_json({
        "result_dir": str(ctx.result_dir),
        "summary": result["summary"],
        "running_slots": result["running_slots"],
        "control_plane": result["control_plane"],
    })
    failed = result["summary"]["failed"]
    return 0 if result["status"] in ("ok", "aborted") and failed == 0 else 1
