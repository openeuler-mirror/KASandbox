"""bench clone: derive N new sandboxes from a running source via its checkpoint."""

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
    parser = subparsers.add_parser("clone", help="源沙箱打 checkpoint 后并发派生 N 个新沙箱（源保持运行）")
    parser.add_argument("-n", type=positive_int, help="每轮派生的新沙箱总数（覆盖配置，单档）")
    parser.add_argument("-c", "--concurrency", type=positive_int, help="并发数（覆盖配置，单档）")
    parser.add_argument("--rounds", type=positive_int, help="正式测量轮数（覆盖配置，单档）")
    parser.add_argument("-w", "--warmup", type=int, help="热身轮数，结果丢弃（默认取 global.warmup）")
    add_template_argument(parser)
    add_common_arguments(parser)
    parser.add_argument("-o", "--output", help="JSON 报告输出路径")
    parser.add_argument("--sandbox-timeout", type=positive_int, help="沙箱生命周期秒数")
    parser.set_defaults(handler=guarded(execute))


def run(
    ctx: BenchContext,
    *,
    n: int,
    concurrency: int,
    rounds: int,
    warmup: int,
    pre_wait: float = 0,
) -> dict:
    params = {"n": n, "concurrency": concurrency, "rounds": rounds, "warmup": warmup, "pre_wait": pre_wait}
    result = base_result("clone", ctx, params)
    pre_tier_settle(ctx, f"clone-n{n}-c{concurrency}", pre_wait)
    source_id = None
    snapshot_id = None
    try:
        created = timed_create(ctx)
        if not created.ok or not created.sandbox_id:
            raise RuntimeError(f"无法创建源沙箱: {created.error}")
        source_id = created.sandbox_id
        snap = ctx.client.create_snapshot_timed(source_id)
        if not snap.ok:
            raise RuntimeError(f"源沙箱 checkpoint 失败: {snap.error}")
        snapshot_id = _snapshot_id(snap.data)
        if not snapshot_id:
            raise RuntimeError(f"checkpoint 响应缺少 snapshotID: {snap.data}")
        ctx.note(f"源沙箱 {source_id} 保持运行，checkpoint：{snapshot_id}")

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

            results, wall_ms = run_concurrent(_one, n, concurrency)
            clone_ids = [item.sandbox_id for item in results if item.ok and item.sandbox_id]
            if measured:
                walls.append(wall_ms)
                total_ops += len(results)
                failed_ops += sum(1 for item in results if not item.ok)
            kill_ids(ctx.client, clone_ids)
            with ctx._lock:
                dropped = set(clone_ids)
                ctx.created_ids = [sid for sid in ctx.created_ids if sid not in dropped]

        metrics = wall_stats(walls, unit_count=n)
        success_rate = round((total_ops - failed_ops) * 100 / total_ops, 2) if total_ops else None
        result["tiers"] = [{
            "label": f"{n} 个沙箱 {concurrency} 并发",
            "n": n,
            "concurrency": concurrency,
            "metrics": metrics,
            "success_rate": success_rate,
        }]
        if failed_ops:
            result["status"] = "failed"
            result["error"] = f"{failed_ops}/{total_ops} 次 clone 失败"
    except Exception as exc:
        result["status"] = "failed"
        result["error"] = str(exc)[-500:]
    finally:
        if snapshot_id and not delete_snapshot(ctx.client, snapshot_id):
            ctx.note(f"checkpoint {snapshot_id} 删除失败")
        if source_id:
            kill_ids(ctx.client, [source_id])
            with ctx._lock:
                if source_id in ctx.created_ids:
                    ctx.created_ids.remove(source_id)
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
    if args.n or args.concurrency or args.rounds:
        cli_tier = {
            "n": args.n or 10,
            "concurrency": args.concurrency or 10,
            "rounds": args.rounds or 2,
        }
    tiers = bench_config.tiers_for(cfg, "clone", cli_tier)
    tier_results = []
    for tier in tiers:
        tier_results.append(
            run(
                ctx,
                n=int(tier["n"]),
                concurrency=int(tier["concurrency"]),
                rounds=int(tier["rounds"]),
                warmup=warmup,
                pre_wait=float(tier.get("pre_wait", 0)),
            )
        )
        ensure_clean_slate(ctx, f"clone-n{tier['n']}-c{tier['concurrency']}")
    result = merge_tier_results("clone", tier_results)
    drain_notes(ctx, result)
    environment = collect_environment(ctx.client, ctx.template, template_source=ctx.template_source)
    json_path = bench_report.write_bench_json(ctx.result_dir, "clone", result)
    if args.output:
        import shutil

        shutil.copyfile(json_path, args.output)
    bench_report.write_report(ctx.result_dir, [result], environment, ctx.run_id)
    print_json({"result_dir": str(ctx.result_dir), "tiers": result["tiers"]})
    return 0 if result["status"] == "ok" else 1
