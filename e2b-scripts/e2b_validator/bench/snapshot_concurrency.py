"""bench snapshot-concurrency: snapshot creation wall time vs concurrency."""

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
from .stats import wall_stats


def register(subparsers) -> None:
    parser = subparsers.add_parser("snapshot-concurrency", help="N 个沙箱并发制作快照，测整批 wall time")
    parser.add_argument("-c", "--concurrency", type=positive_int, help="并发数（覆盖配置，单档）")
    parser.add_argument("-n", "--rounds", type=positive_int, help="正式测量轮数（覆盖配置，单档）")
    parser.add_argument("-w", "--warmup", type=int, help="热身轮数，结果丢弃（默认取 global.warmup）")
    add_template_argument(parser)
    add_common_arguments(parser)
    parser.add_argument("-o", "--output", help="JSON 报告输出路径")
    parser.add_argument("--sandbox-timeout", type=positive_int, help="沙箱生命周期秒数")
    parser.set_defaults(handler=guarded(execute))


def _snapshot_id(data) -> str | None:
    if isinstance(data, dict):
        for field in ("snapshotID", "snapshotId", "snapshot_id", "id"):
            value = data.get(field)
            if isinstance(value, str) and value:
                return value
    return None


def run(
    ctx: BenchContext,
    *,
    concurrency: int,
    rounds: int,
    warmup: int,
    pre_wait: float = 0,
) -> dict:
    params = {"concurrency": concurrency, "rounds": rounds, "warmup": warmup, "pre_wait": pre_wait}
    result = base_result("snapshot-concurrency", ctx, params)
    pre_tier_settle(ctx, f"snapshot-concurrency-c{concurrency}", pre_wait)

    walls: list[float] = []
    total_ops = 0
    failed_ops = 0
    for round_index in range(max(0, warmup) + rounds):
        measured = round_index >= max(0, warmup)
        sandbox_ids: list[str] = []
        instances: list[Any] = []
        for index in range(concurrency):
            # SDK 创建路径（与 max_test 脚本一致）
            created = sdk_engine.create_one(ctx.template, index)
            if created["ok"] and created["sandbox_id"]:
                ctx.track(created["sandbox_id"])
                sandbox_ids.append(created["sandbox_id"])
                instances.append(created["instance"])
        if not sandbox_ids:
            result["status"] = "failed"
            result["error"] = "无法创建快照源沙箱"
            return finish_result(result, ctx)

        def _snap(index: int):
            return sdk_engine.snapshot_one(instances[index])

        results, wall_ms = run_concurrent(_snap, len(sandbox_ids), concurrency)
        snapshot_ids = [_snapshot_id(item.data) for item in results if item.ok]
        if measured:
            walls.append(wall_ms)
            total_ops += len(results)
            failed_ops += sum(1 for item in results if not item.ok)
        for snapshot_id in snapshot_ids:
            if snapshot_id and not delete_snapshot(ctx.client, snapshot_id):
                ctx.note(f"快照 {snapshot_id} 删除失败")
        kill_ids(ctx.client, sandbox_ids)
        with ctx._lock:
            ctx.created_ids = [sid for sid in ctx.created_ids if sid not in set(sandbox_ids)]

    metrics = wall_stats(walls, unit_count=concurrency)
    success_rate = round((total_ops - failed_ops) * 100 / total_ops, 2) if total_ops else None
    result["tiers"] = [{
        "concurrency": concurrency,
        "metrics": metrics,
        "success_rate": success_rate,
    }]
    if failed_ops:
        result["status"] = "failed"
        result["error"] = f"{failed_ops}/{total_ops} 次快照请求失败"
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
    tiers = bench_config.tiers_for(cfg, "snapshot_concurrency", cli_tier)
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
        ensure_clean_slate(ctx, f"snapshot-concurrency-c{tier['concurrency']}")
    result = merge_tier_results("snapshot-concurrency", tier_results)
    drain_notes(ctx, result)
    environment = collect_environment(ctx.client, ctx.template, template_source=ctx.template_source)
    json_path = bench_report.write_bench_json(ctx.result_dir, "snapshot-concurrency", result)
    if args.output:
        import shutil

        shutil.copyfile(json_path, args.output)
    bench_report.write_report(ctx.result_dir, [result], environment, ctx.run_id)
    print_json({"result_dir": str(ctx.result_dir), "tiers": result["tiers"]})
    return 0 if result["status"] == "ok" else 1
