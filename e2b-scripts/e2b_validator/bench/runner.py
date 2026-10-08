"""bench all: orchestrate every benchmark and emit one aggregated Chinese report."""

from __future__ import annotations

import argparse
import json
import traceback

from ..e2b_common import print_json
from . import (
    clone,
    create,
    create_from_snapshot,
    density,
    pause_resume,
    report as bench_report,
    rollback,
    scale,
    snapshot_concurrency,
    snapshot_dirty,
)
from . import config as bench_config
from .common import (
    BenchContext,
    add_common_arguments,
    add_template_argument,
    base_result,
    build_context,
    cleanup_created,
    clean_host_orphans,
    ensure_clean_slate,
    collect_environment,
    drain_notes,
    mark_host_net_baseline,
    finish_result,
    guarded,
    merge_tier_results,
)

BENCH_RUNNERS = {
    "create": create.run,
    "scale": scale.run,
    "density": density.run,
    "snapshot-concurrency": snapshot_concurrency.run,
    "snapshot-dirty": snapshot_dirty.run,
    "create-from-snapshot": create_from_snapshot.run,
    "rollback": rollback.run,
    "clone": clone.run,
    "pause-resume": pause_resume.run,
}


def register(subparsers) -> None:
    parser = subparsers.add_parser("all", help="按 profile 编排全部 8 个测试项并生成汇总中文报告")
    parser.add_argument(
        "--profile",
        choices=("quick", "full"),
        default="quick",
        help="quick 小规模自检（默认）；full 完整档位，对标 CubeSandbox 文章",
    )
    parser.add_argument(
        "--print-config",
        action="store_true",
        help="打印合并后的生效配置（TOML 回显）后退出，不执行测试",
    )
    add_template_argument(parser)
    add_common_arguments(parser)
    parser.add_argument("--sandbox-timeout", type=int, help="沙箱生命周期秒数（覆盖配置）")
    parser.set_defaults(handler=guarded(execute))


def _bench_param_sets(cfg: dict, warmup: int) -> dict[str, list[dict]]:
    global_cfg = cfg["global"]
    mem_threshold_pct = float(global_cfg["mem_threshold_pct"])
    return {
        "create": [
            dict(concurrency=int(t["concurrency"]), requests=int(t["requests"]), warmup=warmup, mode="create-kill",
                 pre_wait=float(t.get("pre_wait", 0)))
            for t in cfg["create"]["tiers"]
        ],
        "scale": [
            dict(
                tiers=bench_config.scale_tiers(cfg),
                rounds=int(cfg["scale"].get("rounds", 3)),
                warmup=warmup,
                mem_threshold_pct=mem_threshold_pct,
            )
        ],
        "density": [
            dict(
                concurrency=50,
                batch_size=int(cfg["density"]["batch_size"]),
                max_sandboxes=int(cfg["density"]["max_sandboxes"]),
                mem_threshold_pct=mem_threshold_pct,
                keep_sandboxes=False,
            )
        ],
        "snapshot-concurrency": [
            dict(concurrency=int(t["concurrency"]), rounds=int(t["rounds"]), warmup=warmup,
                 pre_wait=float(t.get("pre_wait", 0)))
            for t in cfg["snapshot_concurrency"]["tiers"]
        ],
        "snapshot-dirty": [
            dict(dirty_mb=int(t["dirty_mb"]), rounds=int(t["rounds"]), warmup=warmup,
                 pre_wait=float(t.get("pre_wait", 0)))
            for t in cfg["snapshot_dirty"]["tiers"]
        ],
        "create-from-snapshot": [
            dict(concurrency=int(t["concurrency"]), rounds=int(t["rounds"]), warmup=warmup,
                 pre_wait=float(t.get("pre_wait", 0)))
            for t in cfg["create_from_snapshot"]["tiers"]
        ],
        "rollback": [
            dict(concurrency=int(t["concurrency"]), rounds=int(t["rounds"]), warmup=warmup,
                 pre_wait=float(t.get("pre_wait", 0)))
            for t in cfg["rollback"]["tiers"]
        ],
        "clone": [
            dict(n=int(t["n"]), concurrency=int(t["concurrency"]), rounds=int(t["rounds"]), warmup=warmup,
                 pre_wait=float(t.get("pre_wait", 0)))
            for t in cfg["clone"]["tiers"]
        ],
        "pause-resume": [
            dict(concurrency=int(t["concurrency"]), rounds=int(t["rounds"]), warmup=warmup,
                 pre_wait=float(t.get("pre_wait", 0)))
            for t in cfg["pause_resume"]["tiers"]
        ],
    }


def run_all(ctx: BenchContext, cfg: dict) -> list[dict]:
    warmup = int(cfg["global"]["warmup"])
    param_sets = _bench_param_sets(cfg, warmup)
    results: list[dict] = []
    for name, runner in BENCH_RUNNERS.items():
        tiers = param_sets[name]
        tier_results: list[dict] = []
        mark_host_net_baseline(ctx)
        print(f"[bench all] 开始 {name}（{len(tiers)} 个档位）", flush=True)
        for params in tiers:
            tier_label = params.get(
                "concurrency", params.get("dirty_mb", params.get("n", params.get("sizes", "-")))
            )
            try:
                tier_results.append(runner(ctx, **params))
            except Exception as exc:
                failed = base_result(name, ctx, params)
                failed["status"] = "failed"
                failed["error"] = f"{type(exc).__name__}: {exc}"[-500:]
                failed["tiers"] = []
                ctx.note(f"{name} 档位 {params} 异常：{traceback.format_exc(limit=3)}")
                tier_results.append(finish_result(failed, ctx))
            finally:
                cleanup_created(ctx)
                ensure_clean_slate(ctx, f"{name}-tier-{tier_label}")
                ctx.notes.clear()
        ensure_clean_slate(ctx, f"after-{name}")
        merged = merge_tier_results(name, tier_results)
        drain_notes(ctx, merged)
        bench_report.write_bench_json(ctx.result_dir, name, merged)
        results.append(merged)
        print(f"[bench all] 完成 {name}：{merged['status']}", flush=True)
    return results


def execute(args: argparse.Namespace) -> int:
    if args.print_config:
        cfg = bench_config.load(args.config, profile=args.profile)
        print(bench_config.dump_toml(cfg), end="")
        return 0

    cfg = bench_config.load(args.config, profile=args.profile)
    template = bench_config.resolve_template(args.template, cfg)
    ctx = build_context(
        template=template,
        result_root=bench_config.resolve_result_root(cfg),
        netns_growth_threshold=int(bench_config.global_param(cfg, "netns_growth_threshold")),
        sandbox_timeout=args.sandbox_timeout or int(bench_config.global_param(cfg, "sandbox_timeout")),
    )
    # pre-flight：清掉上一轮 bench（任意 run_id）的残留并等待收敛，再清宿主孤儿资源
    print("[bench all] pre-flight 清理遗留 bench 沙箱…", flush=True)
    ensure_clean_slate(ctx, "preflight", run_id=None)
    clean_host_orphans(ctx, "preflight-orphans")
    environment = collect_environment(ctx.client, ctx.template, template_source=ctx.template_source)
    results = run_all(ctx, cfg)
    cleanup_created(ctx)
    bench_report.write_report(ctx.result_dir, results, environment, ctx.run_id)
    summary = {
        "run_id": ctx.run_id,
        "profile": args.profile,
        "result_dir": str(ctx.result_dir),
        "benches": [
            {"bench": item["bench"], "status": item["status"], "error": item.get("error")}
            for item in results
        ],
    }
    (ctx.result_dir / "bench_all.json").write_text(
        json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8"
    )
    print_json(summary)
    return 0 if all(item["status"] == "ok" for item in results) else 1
