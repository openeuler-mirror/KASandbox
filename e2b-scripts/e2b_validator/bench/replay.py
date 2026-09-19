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
    ReplayStep,
    find_trajectories,
    generate_synthetic_trajectories,
    load_trajectory,
)

TRANSIENT_RETRY_ATTEMPTS = 3  # 瞬断错误最大重试次数（重试重新进限流队列）


def register(subparsers) -> None:
    parser = subparsers.add_parser(
        "replay",
        help="轨迹回放密度测试：创建→pause→按 delay_time 逐条 resume 执行→pause（paused 常驻 + 限量 RUNNING）",
    )
    parser.add_argument("--trajectory-dir", type=Path, help="轨迹目录（第一层 .json/.traj；不传则用合成轨迹）")
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
    parser.add_argument("--synthetic-steps", type=positive_int, help="合成轨迹的步数（默认 10）")
    parser.add_argument("--dry-run", action="store_true", help="只校验配置和轨迹、打印调度预览，不创建沙箱")
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
    trajectories: list[tuple[str, list[ReplayStep]]],
    target_count: int,
    concurrency: int,
    running_concurrency: int,
    launch_interval_sec: float,
    control_plane_qps: float,
    action_timeout: int,
    mem_threshold_pct: float,
) -> dict:
    params = {
        "target_count": target_count,
        "concurrency": concurrency,
        "running_concurrency": running_concurrency,
        "launch_interval_sec": launch_interval_sec,
        "control_plane_qps": control_plane_qps,
        "action_timeout": action_timeout,
        "mem_threshold_pct": mem_threshold_pct,
        "trajectory_count": len(trajectories),
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

    def _create(index: int) -> dict:
        def _op() -> dict:
            with limiter.slot(OperationType.CREATE):
                outcome = sdk_engine.create_one(ctx.template, index, timeout=ctx.sandbox_timeout)
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

    def _command(sandbox, action: str) -> tuple[float, int]:
        def _op() -> tuple[float, int]:
            # 同步 SDK 的 commands.run 建连与执行不可拆分，COMMAND 名额覆盖整个调用；
            # 实际并发由 running_concurrency 硬上限兜底，限流只平滑启动节奏
            with limiter.slot(OperationType.COMMAND):
                started = time.perf_counter()
                completed = sandbox.commands.run(action, timeout=action_timeout)
            return (time.perf_counter() - started) * 1000, completed.exit_code

        return _retry(_op, what=f"命令 {action[:40]!r}")

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
        nonlocal alive
        name, steps = trajectories[index % len(trajectories)]
        task_id = f"replay-{index}"
        record: dict = {
            "index": index,
            "trajectory": name,
            "ok": False,
            "create_ms": None,
            "server_ms": None,
            "steps": [],
            "error": None,
        }
        sandbox_id: str | None = None
        killed = False
        try:
            outcome = _create(index)
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
                    command_ms, exit_code = _command(sandbox, step.action)
                    step_record["command_ms"] = round(command_ms, 1)
                    step_record["exit_code"] = exit_code
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
            record["ok"] = True
        except Exception as exc:
            record["error"] = f"{type(exc).__name__}: {exc}"[:300]
        finally:
            if sandbox_id is not None and not killed:
                _kill(sandbox_id)
                with lock:
                    alive -= 1
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
    result["summary"] = {
        "target": target_count,
        "total": len(records),
        "succeeded": succeeded,
        "failed": failed,
        "elapsed_sec": round(time.monotonic() - started_monotonic, 1),
    }
    result["running_slots"] = scheduler.snapshot()
    result["control_plane"] = limiter.snapshot()
    result["latency"] = {
        "resume": sdk_engine.latency_stats(resume_ms_samples),
        "command": sdk_engine.latency_stats(command_ms_samples),
        "pause": sdk_engine.latency_stats(pause_ms_samples),
        "queue_wait": sdk_engine.latency_stats(queue_wait_ms_samples),
    }
    result["create"] = {
        **sdk_engine.latency_stats(create_ms_samples, "create"),
        "server": sdk_engine.latency_stats(server_ms_samples, "server") if server_ms_samples else None,
        "server_samples": len(server_ms_samples),
    }
    result["memory_curve"] = memory_curve
    result["trajectories"] = records

    leftovers = cleanup_created(ctx)
    if leftovers.get("failed"):
        ctx.note(f"收尾清理存在失败：{leftovers}")
    elif leftovers.get("deleted"):
        ctx.note(f"轨迹外残留沙箱已清理：{leftovers}")
    ensure_clean_slate(ctx, "replay-final")

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
    concurrency = int(_resolve_param(args.concurrency, section, "concurrency", 20))
    running_concurrency = int(_resolve_param(args.running_concurrency, section, "running_concurrency", 10))
    launch_interval_sec = float(_resolve_param(args.launch_interval_sec, section, "launch_interval_sec", 0.3))
    control_plane_qps = float(_resolve_param(args.control_plane_qps, section, "control_plane_qps", 100))
    action_timeout = int(_resolve_param(args.action_timeout, section, "action_timeout", 300))
    synthetic_steps = int(_resolve_param(args.synthetic_steps, section, "synthetic_steps", 10))
    mem_threshold_pct = (
        args.mem_threshold_pct
        if args.mem_threshold_pct is not None
        else float(bench_config.global_param(cfg, "mem_threshold_pct"))
    )

    # 轨迹准备先于 build_context：dry-run 不连接环境即可完成校验与预览
    if args.trajectory_dir:
        paths = find_trajectories(args.trajectory_dir)
        if not paths:
            raise ValueError(f"轨迹目录中没有 .json/.traj 文件：{args.trajectory_dir}")
        trajectories: list[tuple[str, list[ReplayStep]]] = [
            (path.name, load_trajectory(path)) for path in paths
        ]
        trajectory_source = str(Path(args.trajectory_dir).expanduser().resolve())
    else:
        synthetic_count = target_count if target_count > 0 else 60
        trajectories = [
            (f"synthetic-{index}", steps)
            for index, steps in enumerate(
                generate_synthetic_trajectories(synthetic_count, synthetic_steps, rng_seed=42)
            )
        ]
        trajectory_source = f"合成轨迹（{synthetic_count} 条 × {synthetic_steps} 步）"
    effective_target = target_count if target_count > 0 else len(trajectories)

    if args.dry_run:
        step_counts = [len(steps) for _name, steps in trajectories]
        delay_total = sum(
            step.delay_time_sec for _name, steps in trajectories for step in steps
        )
        preview = {
            "dry_run": True,
            "trajectory_source": trajectory_source,
            "trajectory_count": len(trajectories),
            "target_count": effective_target,
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
        }
        print(
            f"[replay dry-run] 轨迹来源：{trajectory_source}；{len(trajectories)} 条轨迹，"
            f"目标回放 {effective_target} 次；生命周期并发 {concurrency}，"
            f"RUNNING 名额 {running_concurrency}，控制面 {control_plane_qps} QPS"
        )
        print_json(preview)
        return 0

    template = bench_config.resolve_template(args.template, cfg)
    ctx = build_context(
        template=template,
        result_root=bench_config.resolve_result_root(cfg),
        netns_growth_threshold=int(bench_config.global_param(cfg, "netns_growth_threshold")),
        sandbox_timeout=args.sandbox_timeout or max(
            3600, int(bench_config.global_param(cfg, "sandbox_timeout"))
        ),
    )
    result = run(
        ctx,
        trajectories=trajectories,
        target_count=effective_target,
        concurrency=concurrency,
        running_concurrency=running_concurrency,
        launch_interval_sec=launch_interval_sec,
        control_plane_qps=control_plane_qps,
        action_timeout=action_timeout,
        mem_threshold_pct=mem_threshold_pct,
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
