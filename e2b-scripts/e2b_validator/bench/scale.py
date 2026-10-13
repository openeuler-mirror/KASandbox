"""bench scale: batch-launch N sandboxes of one template and measure end-to-end wall time."""

from __future__ import annotations

import argparse

from ..e2b_common import positive_int, print_json
from . import breakdown as bench_breakdown
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
    ensure_clean_slate,
    finish_result,
    guarded,
    pre_tier_settle,
    read_meminfo,
)
from .stats import timing_stats


def _parse_sizes(raw: str) -> list[int]:
    sizes: list[int] = []
    for part in raw.split(","):
        part = part.strip()
        if not part:
            continue
        try:
            value = int(part)
        except ValueError as exc:
            raise argparse.ArgumentTypeError(f"--sizes 含非法整数：{part}") from exc
        if value <= 0:
            raise argparse.ArgumentTypeError("--sizes 必须为正整数")
        sizes.append(value)
    if not sizes:
        raise argparse.ArgumentTypeError("--sizes 不能为空")
    return sizes


def register(subparsers) -> None:
    parser = subparsers.add_parser("scale", help="同一模板一次性并发拉起 N 个沙箱，测整批 wall（含内存安全闸）")
    parser.add_argument("--sizes", type=_parse_sizes, help="规模档位，逗号分隔（如 1,5,100）；覆盖配置")
    parser.add_argument("--rounds", type=positive_int, help="每档正式测量轮数（默认取配置 scale.rounds）")
    parser.add_argument("-w", "--warmup", type=int, help="热身轮数，结果丢弃（默认取 global.warmup）")
    add_template_argument(parser)
    add_common_arguments(parser)
    parser.add_argument("-o", "--output", help="JSON 报告输出路径")
    parser.add_argument("--sandbox-timeout", type=positive_int, help="沙箱生命周期秒数（防泄漏兜底）")
    parser.add_argument(
        "--enforce-criteria",
        action="store_true",
        help="配置 [[criteria.scale]] 的达标标准不满足时以非零退出码结束（默认只在报告里标出）",
    )
    parser.set_defaults(handler=guarded(execute))


def judge(tiers: list[dict], criteria: list[dict] | None) -> list[dict]:
    """达标判定：该档位每一轮整批耗时都小于上限且没有创建失败才算达标；没跑到的档位记为未测。"""
    verdicts = []
    for rule in criteria or []:
        size, limit = int(rule["size"]), float(rule["wall_ms_max"])
        verdict = {"name": f"{size} 并发整批耗时 < {limit:g} ms", "passed": None, "actual_ms": None, "detail": "本次没有运行该档位"}
        tier = next((item for item in tiers if item["size"] == size), None)
        if tier is not None:
            metrics = tier.get("metrics") or {}
            walls = metrics.get("wall_rounds_ms") or []
            failed = metrics.get("failed") or 0
            verdict["passed"] = bool(walls) and max(walls) < limit and not failed
            verdict["actual_ms"] = max(walls) if walls else None
            verdict["detail"] = (
                f"{len(walls)} 轮中最差一轮 {max(walls):.0f} ms" + (f"，{failed} 个沙箱创建失败" if failed else "")
                if walls
                else "该档位未完成"
            )
        verdicts.append(verdict)
    return verdicts


def run(
    ctx: BenchContext,
    *,
    tiers: list[dict],
    rounds: int,
    warmup: int,
    mem_threshold_pct: float,
    breakdown: dict | None = None,
    criteria: list[dict] | None = None,
) -> dict:
    params = {
        "tiers": tiers,
        "rounds": rounds,
        "warmup": warmup,
        "mem_threshold_pct": mem_threshold_pct,
    }
    result = base_result("scale", ctx, params)
    meminfo = read_meminfo()
    total_kb = meminfo.get("MemTotal", 0)
    threshold_kb = total_kb * mem_threshold_pct / 100

    tier_results: list[dict] = []
    aborted = False
    for tier_spec in tiers:
        size = int(tier_spec["size"])
        pre_wait = float(tier_spec.get("pre_wait", 0))
        meminfo = read_meminfo()
        available_kb = meminfo.get("MemAvailable", 0)
        if available_kb < threshold_kb:
            aborted = True
            ctx.note(
                f"内存安全闸触发：MemAvailable {round(available_kb / 1024)} MiB 低于阈值 "
                f"{round(threshold_kb / 1024)} MiB（总内存 {mem_threshold_pct}%），"
                f"中止规模 {size} 档及其后档位"
            )
            tier_results.append({"size": size, "pre_wait": pre_wait, "status": "aborted", "metrics": None})
            break

        pre_tier_settle(ctx, f"scale-n{size}", pre_wait)
        collector = bench_breakdown.collector(breakdown, ctx.note)
        walls: list[float] = []
        create_times: list[float] = []
        server_times: list[float] = []
        server_spans: list[float] = []
        destroy_times: list[float] = []
        total_ops = 0
        failed_ops = 0
        total_wall_ms = 0.0
        for round_index in range(max(0, warmup) + rounds):
            measured = round_index >= max(0, warmup)
            if collector:
                collector.arm()
            # SDK 创建路径（与用户 max_test 脚本一致）：每批 ≤150 并发、失败自动补充
            batch = sdk_engine.batch_create(ctx, ctx.template, size, concurrency=size)
            if collector and measured:
                collector.add_batch(batch)
            destroy = sdk_engine.destroy_all(ctx, batch["instances"])
            if measured:
                walls.append(batch["wall_ms"])
                create_times.extend(batch["create_times_ms"])
                server_times.extend(batch["server_times_ms"])
                if batch.get("server_batch_span_ms") is not None:
                    server_spans.append(batch["server_batch_span_ms"])
                destroy_times.extend(destroy["kill_times_ms"])
                total_wall_ms += batch["wall_ms"]
                total_ops += batch["success"] + batch["failed"]
                failed_ops += batch["failed"]

        # 统计口径：单沙箱创建时间的 avg/p50/p90/p95/max（wall/均摊/吞吐另行保留）
        wall_avg = sum(walls) / len(walls) if walls else None
        metrics: dict = {
            "count": total_ops,
            "success": total_ops - failed_ops,
            "failed": failed_ops,
            "success_rate": round((total_ops - failed_ops) * 100 / total_ops, 2) if total_ops else None,
            "rounds": len(walls),
            "unit_count": size,
            "wall_ms": round(wall_avg, 1) if wall_avg is not None else None,
            "wall_rounds_ms": [round(wall, 1) for wall in walls],  # 各轮整批耗时，达标判定取最差一轮
            "per_unit_avg_ms": round(wall_avg / size, 1) if wall_avg is not None and size else None,
            "throughput_per_s": (
                round((total_ops - failed_ops) * 1000 / total_wall_ms, 1) if total_wall_ms > 0 else None
            ),
        }
        metrics.update(sdk_engine.latency_stats(create_times))
        if server_times:
            # server_*：服务端真值口径（startedAt - 请求发出时刻），剔除客户端 Python 开销
            metrics.update(sdk_engine.latency_stats(server_times, "server"))
            metrics["server_samples"] = len(server_times)
        if server_spans:
            # 整批服务端跨度：最早请求发出 → 最晚 startedAt（多轮取均值，与 wall 并列对照）
            metrics["server_batch_span_ms"] = round(sum(server_spans) / len(server_spans), 1)
        metrics.update(sdk_engine.latency_stats(destroy_times, "destroy"))
        tier = {"size": size, "pre_wait": pre_wait, "status": "ok", "metrics": metrics}
        if failed_ops:
            tier["status"] = "failed"
            ctx.note(f"规模 {size} 档：{failed_ops}/{total_ops} 个沙箱创建失败")
        tier_breakdown = collector.result() if collector else None
        if tier_breakdown:
            tier["breakdown"] = tier_breakdown
            print(f"\n【耗时分解】规模 {size}\n  " + "\n  ".join(bench_breakdown.render(tier_breakdown)))
        tier_results.append(tier)
        ensure_clean_slate(ctx, f"scale-n{size}")

    result["tiers"] = tier_results
    result["criteria"] = judge(tier_results, criteria)
    for verdict in result["criteria"]:
        label = {True: "达标", False: "不达标", None: "未测"}[verdict["passed"]]
        print(f"【达标判定】{verdict['name']}：{label}（{verdict['detail']}）")
    tier_statuses = {tier["status"] for tier in tier_results}
    if aborted:
        result["status"] = "aborted"
        result["error"] = "内存安全闸触发，部分档位中止（已记录中止前数据）"
    elif "failed" in tier_statuses:
        result["status"] = "failed"
        result["error"] = "存在创建失败的档位，详见 notes"
    return finish_result(result, ctx)


def execute(args: argparse.Namespace) -> int:
    cfg = bench_config.load(args.config)
    template = bench_config.resolve_template(args.template, cfg)
    ctx = build_context(
        template=template,
        result_root=bench_config.resolve_result_root(cfg),
        netns_growth_threshold=int(bench_config.global_param(cfg, "netns_growth_threshold")),
        # SDK 创建路径的沙箱生命周期固定 3600s（sdk_engine.CREATE_TIMEOUT，与用户脚本一致）
        sandbox_timeout=max(
            1800,
            args.sandbox_timeout or int(bench_config.global_param(cfg, "sandbox_timeout")),
        ),
    )
    sizes = args.sizes
    if sizes:
        scale_tier_list = [{"size": int(size)} for size in sizes]
    else:
        scale_tier_list = bench_config.scale_tiers(cfg)
    rounds = args.rounds or int(cfg["scale"].get("rounds", 3))
    warmup = args.warmup if args.warmup is not None else int(bench_config.global_param(cfg, "warmup"))
    result = run(
        ctx,
        tiers=scale_tier_list,
        rounds=rounds,
        warmup=warmup,
        mem_threshold_pct=float(bench_config.global_param(cfg, "mem_threshold_pct")),
        breakdown=cfg.get("breakdown"),
        criteria=(cfg.get("criteria") or {}).get("scale"),
    )
    environment = collect_environment(ctx.client, ctx.template, template_source=ctx.template_source)
    json_path = bench_report.write_bench_json(ctx.result_dir, "scale", result)
    if args.output:
        import shutil

        shutil.copyfile(json_path, args.output)
    bench_report.write_report(ctx.result_dir, [result], environment, ctx.run_id)
    print_json({"result_dir": str(ctx.result_dir), "tiers": bench_report.without_breakdown(result["tiers"])})
    unmet = args.enforce_criteria and any(verdict["passed"] is False for verdict in result["criteria"])
    return 0 if result["status"] == "ok" and not unmet else 1
