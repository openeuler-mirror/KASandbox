"""bench snapshot-dirty: snapshot creation time vs dirty page size."""

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
    connect_sdk,
    delete_snapshot,
    pre_tier_settle,
    ensure_clean_slate,
    finish_result,
    guarded,
    kill_ids,
    merge_tier_results,
    run_in_sandbox,
)
from .snapshot_concurrency import _snapshot_id
from .stats import timing_stats


def register(subparsers) -> None:
    parser = subparsers.add_parser("snapshot-dirty", help="沙箱内 dd 写入 /dev/shm 控制脏页，测快照与恢复耗时")
    parser.add_argument("-d", "--dirty-mb", type=int, help="脏页写入量 MB（覆盖配置，单档）")
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
    dirty_mb: int,
    rounds: int,
    warmup: int,
    pre_wait: float = 0,
) -> dict:
    params = {"dirty_mb": dirty_mb, "rounds": rounds, "warmup": warmup, "pre_wait": pre_wait}
    result = base_result("snapshot-dirty", ctx, params)
    pre_tier_settle(ctx, f"snapshot-dirty-{dirty_mb}mb", pre_wait)

    snapshot_samples: list[float] = []
    create_samples: list[float] = []
    failures: list[str] = []
    for round_index in range(max(0, warmup) + rounds):
        measured = round_index >= max(0, warmup)
        # SDK 创建路径（与 max_test 脚本一致）
        created = sdk_engine.create_one(ctx.template, round_index)
        if not created["ok"] or not created["sandbox_id"]:
            result["status"] = "failed"
            result["error"] = f"无法创建测试沙箱: {created['error']}"
            return finish_result(result, ctx)
        ctx.track(created["sandbox_id"])
        source_id = created["sandbox_id"]
        restored_id = None
        snapshot_id = None
        try:
            if dirty_mb > 0:
                sandbox = connect_sdk(source_id)
                # /dev/shm 默认只有内存的一半（2 GiB 沙箱约 1 GiB），1024MB 档会写爆；
                # 显式挂载专用 tmpfs（上限只受 VM 内存限制），测完 umount 清理
                run_in_sandbox(
                    sandbox,
                    "mkdir -p /mnt/bench-dirty && "
                    f"mount -t tmpfs -o size={dirty_mb + 64}m tmpfs /mnt/bench-dirty && "
                    f"dd if=/dev/zero of=/mnt/bench-dirty/data bs=1M count={dirty_mb} status=none && sync",
                )
            snap = ctx.client.create_snapshot_timed(source_id)
            if not snap.ok:
                raise RuntimeError(f"snapshot failed: {snap.error}")
            snapshot_id = _snapshot_id(snap.data)
            restored = ctx.client.create_timed(
                snapshot_id,
                timeout=ctx.sandbox_timeout,
                metadata=ctx.metadata,
            )
            ctx.track(restored.sandbox_id if restored.ok else None)
            if not restored.ok:
                raise RuntimeError(f"create-from-snapshot failed: {restored.error}")
            restored_id = restored.sandbox_id
            if measured:
                snapshot_samples.append(snap.latency_ms)
                create_samples.append(restored.latency_ms)
        except Exception as exc:
            failures.append(f"round={round_index}: {exc}"[-300:])
            if measured:
                result["status"] = "failed"
        finally:
            if dirty_mb > 0:
                try:
                    run_in_sandbox(connect_sdk(source_id), "umount /mnt/bench-dirty")
                except Exception:
                    pass
            for victim in (restored_id, source_id):
                if victim:
                    kill_ids(ctx.client, [victim])
                    with ctx._lock:
                        if victim in ctx.created_ids:
                            ctx.created_ids.remove(victim)
            if snapshot_id and not delete_snapshot(ctx.client, snapshot_id):
                ctx.note(f"快照 {snapshot_id} 删除失败")

    result["tiers"] = [{
        "dirty_mb": dirty_mb,
        "snapshot": timing_stats(snapshot_samples),
        "create_from_snapshot": timing_stats(create_samples),
    }]
    if failures:
        result["error"] = "; ".join(failures[:5])
        result["status"] = "failed" if len(failures) >= rounds else result["status"]
    return finish_result(result, ctx)


def execute(args: argparse.Namespace) -> int:
    if args.dirty_mb is not None and args.dirty_mb < 0:
        raise ValueError("--dirty-mb 不能为负数")
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
    if args.dirty_mb is not None or args.rounds:
        cli_tier = {"dirty_mb": args.dirty_mb if args.dirty_mb is not None else 0, "rounds": args.rounds or 3}
    tiers = bench_config.tiers_for(cfg, "snapshot_dirty", cli_tier)
    tier_results = []
    for tier in tiers:
        tier_results.append(
            run(
                ctx,
                dirty_mb=int(tier["dirty_mb"]),
                rounds=int(tier["rounds"]),
                warmup=warmup,
                pre_wait=float(tier.get("pre_wait", 0)),
            )
        )
        ensure_clean_slate(ctx, f"snapshot-dirty-{tier['dirty_mb']}mb")
    result = merge_tier_results("snapshot-dirty", tier_results)
    drain_notes(ctx, result)
    environment = collect_environment(ctx.client, ctx.template, template_source=ctx.template_source)
    json_path = bench_report.write_bench_json(ctx.result_dir, "snapshot-dirty", result)
    if args.output:
        import shutil

        shutil.copyfile(json_path, args.output)
    bench_report.write_report(ctx.result_dir, [result], environment, ctx.run_id)
    print_json({"result_dir": str(ctx.result_dir), "tiers": result["tiers"]})
    return 0 if result["status"] == "ok" else 1
