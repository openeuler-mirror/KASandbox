"""bench create-from-snapshot: concurrent sandbox creation from a prepared snapshot."""

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
    timed_create,
)
from .snapshot_concurrency import _snapshot_id
from .stats import wall_stats


def register(subparsers) -> None:
    parser = subparsers.add_parser("create-from-snapshot", help="基于同一快照并发启动沙箱")
    parser.add_argument("-c", "--concurrency", type=positive_int, help="每轮并发创建数（覆盖配置，单档）")
    parser.add_argument("-n", "--rounds", type=positive_int, help="正式测量轮数（覆盖配置，单档）")
    parser.add_argument("-w", "--warmup", type=int, help="热身轮数，结果丢弃（默认取 global.warmup）")
    add_template_argument(parser)
    add_common_arguments(parser)
    parser.add_argument("-o", "--output", help="JSON 报告输出路径")
    parser.add_argument("--sandbox-timeout", type=positive_int, help="沙箱生命周期秒数")
    parser.set_defaults(handler=guarded(execute))


def prepare_snapshot(ctx: BenchContext) -> str:
    created = timed_create(ctx)
    if not created.ok or not created.sandbox_id:
        raise RuntimeError(f"无法创建快照源沙箱: {created.error}")
    source_id = created.sandbox_id
    try:
        snap = ctx.client.create_snapshot_timed(source_id)
        if not snap.ok:
            raise RuntimeError(f"快照制作失败: {snap.error}")
        snapshot_id = _snapshot_id(snap.data)
        if not snapshot_id:
            raise RuntimeError(f"快照响应缺少 snapshotID: {snap.data}")
        return snapshot_id
    finally:
        kill_ids(ctx.client, [source_id])
        with ctx._lock:
            if source_id in ctx.created_ids:
                ctx.created_ids.remove(source_id)


def run(
    ctx: BenchContext,
    *,
    concurrency: int,
    rounds: int,
    warmup: int,
    pre_wait: float = 0,
) -> dict:
    params = {"concurrency": concurrency, "rounds": rounds, "warmup": warmup, "pre_wait": pre_wait}
    result = base_result("create-from-snapshot", ctx, params)
    pre_tier_settle(ctx, f"create-from-snapshot-c{concurrency}", pre_wait)
    snapshot_id = None
    try:
        snapshot_id = prepare_snapshot(ctx)
        ctx.note(f"基准快照：{snapshot_id}")

        walls: list[float] = []
        total_ops = 0
        failed_ops = 0
        for round_index in range(max(0, warmup) + rounds):
            measured = round_index >= max(0, warmup)

            def _one(_index: int):
                item = ctx.client.create_timed(
                    snapshot_id,
                    timeout=ctx.sandbox_timeout,
                    metadata=ctx.metadata,
                )
                ctx.track(item.sandbox_id if item.ok else None)
                return item

            results, wall_ms = run_concurrent(_one, concurrency, concurrency)
            created_ids = [item.sandbox_id for item in results if item.ok and item.sandbox_id]
            if measured:
                walls.append(wall_ms)
                total_ops += len(results)
                failed_ops += sum(1 for item in results if not item.ok)
            kill_ids(ctx.client, created_ids)
            with ctx._lock:
                dropped = set(created_ids)
                ctx.created_ids = [sid for sid in ctx.created_ids if sid not in dropped]

        metrics = wall_stats(walls, unit_count=concurrency)
        success_rate = round((total_ops - failed_ops) * 100 / total_ops, 2) if total_ops else None
        result["tiers"] = [{
            "concurrency": concurrency,
            "per_round": concurrency,
            "metrics": metrics,
            "success_rate": success_rate,
        }]
        if failed_ops:
            result["status"] = "failed"
            result["error"] = f"{failed_ops}/{total_ops} 次创建失败"
    except Exception as exc:
        result["status"] = "failed"
        result["error"] = str(exc)[-500:]
    finally:
        if snapshot_id and not delete_snapshot(ctx.client, snapshot_id):
            ctx.note(f"基准快照 {snapshot_id} 删除失败")
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
        cli_tier = {"concurrency": args.concurrency or 10, "rounds": args.rounds or 3}
    tiers = bench_config.tiers_for(cfg, "create_from_snapshot", cli_tier)
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
        ensure_clean_slate(ctx, f"create-from-snapshot-c{tier['concurrency']}")
    result = merge_tier_results("create-from-snapshot", tier_results)
    drain_notes(ctx, result)
    environment = collect_environment(ctx.client, ctx.template, template_source=ctx.template_source)
    json_path = bench_report.write_bench_json(ctx.result_dir, "create-from-snapshot", result)
    if args.output:
        import shutil

        shutil.copyfile(json_path, args.output)
    bench_report.write_report(ctx.result_dir, [result], environment, ctx.run_id)
    print_json({"result_dir": str(ctx.result_dir), "tiers": result["tiers"]})
    return 0 if result["status"] == "ok" else 1
