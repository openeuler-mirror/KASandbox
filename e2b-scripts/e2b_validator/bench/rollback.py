"""bench rollback: checkpoint + restore-to-own-checkpoint round trip per sandbox."""

from __future__ import annotations

import argparse

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
    collect_environment,
    drain_notes,
    delete_snapshot,
    finish_result,
    guarded,
    kill_ids,
    pre_tier_settle,
    ensure_clean_slate,
    merge_tier_results,
    run_concurrent,
)
from .snapshot_concurrency import _snapshot_id
from .stats import wall_stats


def register(subparsers) -> None:
    parser = subparsers.add_parser(
        "rollback",
        help="沙箱各自打 checkpoint 并恢复（KASandbox 只能回滚到自己创建的 checkpoint）",
    )
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
    result = base_result("rollback", ctx, params)
    pre_tier_settle(ctx, f"rollback-c{concurrency}", pre_wait)
    source_ids: list[str] = []
    source_instances: list[Any] = []
    try:
        for index in range(concurrency):
            # SDK 创建路径（与 max_test 脚本一致）
            created = sdk_engine.create_one(ctx.template, index)
            if not created["ok"] or not created["sandbox_id"]:
                raise RuntimeError(f"无法创建测试沙箱: {created['error']}")
            ctx.track(created["sandbox_id"])
            source_ids.append(created["sandbox_id"])
            source_instances.append(created["instance"])

        walls: list[float] = []
        total_ops = 0
        failed_ops = 0

        def _rollback(index: int):
            """一次 rollback = 自身 create_snapshot 打点 + 基于该 checkpoint 恢复。"""
            checkpoint = sdk_engine.snapshot_one(source_instances[index])
            if not checkpoint.ok:
                return checkpoint
            snapshot_id = _snapshot_id(checkpoint.data)
            restored = ctx.client.create_timed(
                snapshot_id,
                timeout=ctx.sandbox_timeout,
                metadata=ctx.metadata,
            )
            combined = checkpoint.latency_ms + restored.latency_ms
            restored.extra["checkpoint_ms"] = checkpoint.latency_ms
            restored.extra["restore_ms"] = restored.latency_ms
            restored.latency_ms = combined
            if restored.ok and restored.sandbox_id:
                ctx.track(restored.sandbox_id)
                kill_ids(ctx.client, [restored.sandbox_id])
                with ctx._lock:
                    if restored.sandbox_id in ctx.created_ids:
                        ctx.created_ids.remove(restored.sandbox_id)
            if snapshot_id and not delete_snapshot(ctx.client, snapshot_id):
                ctx.note(f"checkpoint {snapshot_id} 删除失败")
            return restored

        for round_index in range(max(0, warmup) + rounds):
            measured = round_index >= max(0, warmup)
            results, wall_ms = run_concurrent(_rollback, len(source_ids), concurrency)
            if measured:
                walls.append(wall_ms)
                total_ops += len(results)
                failed_ops += sum(1 for item in results if not item.ok)

        metrics = wall_stats(walls, unit_count=concurrency)
        success_rate = round((total_ops - failed_ops) * 100 / total_ops, 2) if total_ops else None
        result["tiers"] = [{
            "concurrency": concurrency,
            "metrics": metrics,
            "success_rate": success_rate,
        }]
        if failed_ops:
            result["status"] = "failed"
            result["error"] = f"{failed_ops}/{total_ops} 次 rollback 失败"
    except Exception as exc:
        result["status"] = "failed"
        result["error"] = str(exc)[-500:]
    finally:
        outcomes = kill_ids(ctx.client, source_ids)
        with ctx._lock:
            dropped = set(source_ids)
            ctx.created_ids = [sid for sid in ctx.created_ids if sid not in dropped]
        if outcomes.get("failed"):
            ctx.note(f"源沙箱清理存在失败：{outcomes}")
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
    tiers = bench_config.tiers_for(cfg, "rollback", cli_tier)
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
        ensure_clean_slate(ctx, f"rollback-c{tier['concurrency']}")
    result = merge_tier_results("rollback", tier_results)
    drain_notes(ctx, result)
    cleanup_created(ctx)
    environment = collect_environment(ctx.client, ctx.template, template_source=ctx.template_source)
    json_path = bench_report.write_bench_json(ctx.result_dir, "rollback", result)
    if args.output:
        import shutil

        shutil.copyfile(json_path, args.output)
    bench_report.write_report(ctx.result_dir, [result], environment, ctx.run_id)
    print_json({"result_dir": str(ctx.result_dir), "tiers": result["tiers"]})
    return 0 if result["status"] == "ok" else 1
