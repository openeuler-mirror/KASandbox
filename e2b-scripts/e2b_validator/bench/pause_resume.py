"""bench pause-resume: concurrent pause then resume of N sandboxes, measured separately."""

from __future__ import annotations

import argparse

from ..e2b_common import positive_int, print_json
from . import config as bench_config
from . import report as bench_report
from .common import (
    BenchContext,
    add_common_arguments,
    add_template_argument,
    base_result,
    build_context,
    cleanup_created,
    collect_environment,
    drain_notes,
    finish_result,
    guarded,
    kill_ids,
    pre_tier_settle,
    ensure_clean_slate,
    merge_tier_results,
    run_concurrent,
    timed_create,
    wait_state,
)
from .stats import wall_stats


def register(subparsers) -> None:
    parser = subparsers.add_parser("pause-resume", help="N 沙箱并发 pause 后并发 resume，分别统计")
    parser.add_argument("-c", "--concurrency", type=positive_int, help="并发沙箱数（覆盖配置，单档）")
    parser.add_argument("-n", "--rounds", type=positive_int, help="正式测量轮数（覆盖配置，单档）")
    parser.add_argument("-w", "--warmup", type=int, help="热身轮数，结果丢弃（默认取 global.warmup）")
    add_template_argument(parser)
    add_common_arguments(parser)
    parser.add_argument("-o", "--output", help="JSON 报告输出路径")
    parser.add_argument("--sandbox-timeout", type=positive_int, help="沙箱生命周期秒数")
    parser.set_defaults(handler=guarded(execute))


def run(
    ctx: BenchContext,
    *,
    concurrency: int,
    rounds: int,
    warmup: int,
    pre_wait: float = 0,
) -> dict:
    params = {"concurrency": concurrency, "rounds": rounds, "warmup": warmup, "pre_wait": pre_wait}
    result = base_result("pause-resume", ctx, params)
    pre_tier_settle(ctx, f"pause-resume-c{concurrency}", pre_wait)
    sandbox_ids: list[str] = []
    try:
        for _ in range(concurrency):
            created = timed_create(ctx)
            if not created.ok or not created.sandbox_id:
                raise RuntimeError(f"无法创建测试沙箱: {created.error}")
            sandbox_ids.append(created.sandbox_id)

        pause_walls: list[float] = []
        resume_walls: list[float] = []
        pause_total = pause_failed = resume_total = resume_failed = 0

        for round_index in range(max(0, warmup) + rounds):
            measured = round_index >= max(0, warmup)

            def _pause(index: int):
                return ctx.client.pause_timed(sandbox_ids[index])

            pause_results, pause_wall = run_concurrent(_pause, len(sandbox_ids), concurrency)
            paused_ok = [item for item in pause_results if item.ok]
            state_ok = sum(
                1 for sid in sandbox_ids
                if wait_state(ctx.client, sid, "paused", timeout=300)
            )
            if measured:
                pause_walls.append(pause_wall)
                pause_total += len(pause_results)
                pause_failed += len(pause_results) - len(paused_ok)
                if state_ok < len(sandbox_ids):
                    pause_failed += len(sandbox_ids) - state_ok
                    ctx.note(f"第 {round_index} 轮：{len(sandbox_ids) - state_ok} 个沙箱未进入 paused 状态")

            def _resume(index: int):
                return ctx.client.resume_timed(sandbox_ids[index])

            resume_results, resume_wall = run_concurrent(_resume, len(sandbox_ids), concurrency)
            resumed_ok = [item for item in resume_results if item.ok]
            running_ok = sum(
                1 for sid in sandbox_ids
                if wait_state(ctx.client, sid, "running", timeout=300)
            )
            if measured:
                resume_walls.append(resume_wall)
                resume_total += len(resume_results)
                resume_failed += len(resume_results) - len(resumed_ok)
                if running_ok < len(sandbox_ids):
                    resume_failed += len(sandbox_ids) - running_ok
                    ctx.note(f"第 {round_index} 轮：{len(sandbox_ids) - running_ok} 个沙箱未恢复 running 状态")

        pause_metrics = wall_stats(pause_walls, unit_count=concurrency)
        resume_metrics = wall_stats(resume_walls, unit_count=concurrency)
        pause_rate = round((pause_total - pause_failed) * 100 / pause_total, 2) if pause_total else None
        resume_rate = round((resume_total - resume_failed) * 100 / resume_total, 2) if resume_total else None
        result["tiers"] = [{
            "concurrency": concurrency,
            "pause": {**pause_metrics, "success_rate": pause_rate},
            "resume": {**resume_metrics, "success_rate": resume_rate},
        }]
        if pause_failed or resume_failed:
            result["status"] = "failed"
            result["error"] = f"pause 失败 {pause_failed}/{pause_total}，resume 失败 {resume_failed}/{resume_total}"
    except Exception as exc:
        result["status"] = "failed"
        result["error"] = str(exc)[-500:]
    finally:
        outcomes = kill_ids(ctx.client, sandbox_ids)
        with ctx._lock:
            dropped = set(sandbox_ids)
            ctx.created_ids = [sid for sid in ctx.created_ids if sid not in dropped]
        if outcomes.get("failed"):
            ctx.note(f"沙箱清理存在失败：{outcomes}")
    return finish_result(result, ctx)


def execute(args: argparse.Namespace) -> int:
    cfg = bench_config.load(args.config)
    template = bench_config.resolve_template(args.template, cfg)
    ctx = build_context(
        template=template,
        result_root=bench_config.resolve_result_root(cfg),
        netns_growth_threshold=int(bench_config.global_param(cfg, "netns_growth_threshold")),
        sandbox_timeout=args.sandbox_timeout or int(bench_config.global_param(cfg, "sandbox_timeout")),
    )
    warmup = args.warmup if args.warmup is not None else int(bench_config.global_param(cfg, "warmup"))
    cli_tier = None
    if args.concurrency or args.rounds:
        cli_tier = {"concurrency": args.concurrency or 5, "rounds": args.rounds or 5}
    tiers = bench_config.tiers_for(cfg, "pause_resume", cli_tier)
    tier_results = []
    for tier in tiers:
        tier_results.append(
            run(
                ctx,
                concurrency=int(tier["concurrency"]),
                rounds=int(tier["rounds"]),
                warmup=warmup,
                pre_wait=float(tier.get("pre_wait", 0)),
            )
        )
        ensure_clean_slate(ctx, f"pause-resume-c{tier['concurrency']}")
    result = merge_tier_results("pause-resume", tier_results)
    drain_notes(ctx, result)
    cleanup_created(ctx)
    environment = collect_environment(ctx.client, ctx.template, template_source=ctx.template_source)
    json_path = bench_report.write_bench_json(ctx.result_dir, "pause-resume", result)
    if args.output:
        import shutil

        shutil.copyfile(json_path, args.output)
    bench_report.write_report(ctx.result_dir, [result], environment, ctx.run_id)
    print_json({"result_dir": str(ctx.result_dir), "tiers": result["tiers"]})
    return 0 if result["status"] == "ok" else 1
