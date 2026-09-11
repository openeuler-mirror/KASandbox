"""bench create: template-based sandbox creation latency and concurrency (cube-bench 等价物)."""

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
    pre_tier_settle,
    ensure_clean_slate,
    finish_result,
    guarded,
    merge_tier_results,
)
from .stats import timing_stats


def register(subparsers) -> None:
    parser = subparsers.add_parser("create", help="并发创建沙箱，测量启动延迟（对标 cube-bench）")
    parser.add_argument("-c", "--concurrency", type=positive_int, help="并发数（覆盖配置，单档）")
    parser.add_argument("-n", "--requests", type=positive_int, help="总请求数（覆盖配置，单档）")
    parser.add_argument("-w", "--warmup", type=int, help="热身轮数，结果丢弃（默认取 global.warmup）")
    parser.add_argument(
        "-m", "--mode",
        choices=("create-only", "create-kill"),
        default="create-kill",
        help="create-only 保留存活（供 density/kill-all 使用）；create-kill 测完即删（默认）",
    )
    add_template_argument(parser)
    add_common_arguments(parser)
    parser.add_argument("-o", "--output", help="JSON 报告输出路径（默认写入 test-results/<run_id>-bench/）")
    parser.add_argument("--sandbox-timeout", type=positive_int, help="沙箱生命周期秒数（防泄漏兜底）")
    parser.set_defaults(handler=guarded(execute))


def run(
    ctx: BenchContext,
    *,
    concurrency: int,
    requests: int,
    warmup: int,
    mode: str,
    pre_wait: float = 0,
) -> dict:
    params = {
        "concurrency": concurrency,
        "requests": requests,
        "warmup": warmup,
        "mode": mode,
        "pre_wait": pre_wait,
    }
    result = base_result("create", ctx, params)
    pre_tier_settle(ctx, f"create-c{concurrency}", pre_wait)

    for _ in range(max(0, warmup)):
        warm = sdk_engine.create_one(ctx.template, task_id=0)
        if warm["ok"]:
            ctx.track(warm["sandbox_id"])
            sdk_engine.destroy_all(ctx, [warm["instance"]])

    # SDK 创建路径（与用户 max_test 脚本一致）：每批并发 min(150, concurrency)，失败自动补充
    batch = sdk_engine.batch_create(ctx, ctx.template, requests, concurrency=concurrency)
    latencies = batch["create_times_ms"]
    errors = batch["errors"]
    metrics = timing_stats(latencies, wall_ms=batch["wall_ms"], attempted=requests)
    metrics.update(sdk_engine.percentile_metrics(latencies))
    if mode == "create-kill":
        sdk_engine.destroy_all(ctx, batch["instances"])
    tier = {"concurrency": concurrency, "metrics": metrics}
    if errors:
        tier["errors"] = errors[:10]
    if mode == "create-only":
        with ctx._lock:
            tier["alive_sandbox_ids"] = list(ctx.created_ids)
        ctx.note(
            f"create-only 模式保留 {metrics['success']} 个存活沙箱（无 metadata 标记，"
            "bench kill-all 扫不到）；由服务端沙箱 timeout（3600s）兜底回收"
        )
    result["tiers"] = [tier]
    if metrics["failed"]:
        result["status"] = "failed"
        result["error"] = f"{metrics['failed']}/{metrics['count']} 个请求失败"
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
    if args.concurrency or args.requests:
        cli_tier = {
            "concurrency": args.concurrency or 1,
            "requests": args.requests or 20,
        }
    tiers = bench_config.tiers_for(cfg, "create", cli_tier)
    tier_results = []
    for tier in tiers:
        tier_results.append(
            run(
                ctx,
                concurrency=int(tier["concurrency"]),
                requests=int(tier["requests"]),
                warmup=warmup,
                mode=args.mode,
                pre_wait=float(tier.get("pre_wait", 0)),
            )
        )
        # create-only 的沙箱按设计保留存活（无 metadata 标记，由服务端 timeout 兜底回收）
        if args.mode == "create-kill":
            ensure_clean_slate(ctx, f"create-c{tier['concurrency']}")
    result = merge_tier_results("create", tier_results)
    drain_notes(ctx, result)
    environment = collect_environment(ctx.client, ctx.template, template_source=ctx.template_source)
    json_path = bench_report.write_bench_json(ctx.result_dir, "create", result)
    if args.output:
        import shutil

        shutil.copyfile(json_path, args.output)
    bench_report.write_report(ctx.result_dir, [result], environment, ctx.run_id)
    print_json({"result_dir": str(ctx.result_dir), "tiers": result["tiers"]})
    return 0 if result["status"] == "ok" else 1
