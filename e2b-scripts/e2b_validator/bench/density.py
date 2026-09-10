"""bench density: single-host sandbox density with a memory safety gate."""

from __future__ import annotations

import argparse
import time

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
    clean_host_orphans,
    collect_memory_metrics,
    ensure_clean_slate,
    collect_environment,
    finish_result,
    guarded,
    read_meminfo,
    run_concurrent,
    timed_create,
)


def register(subparsers) -> None:
    parser = subparsers.add_parser("density", help="分批累积创建沙箱，测量单沙箱内存开销（含内存安全闸）")
    parser.add_argument("-c", "--concurrency", type=positive_int, help="每批并发数（默认 50）")
    parser.add_argument("--batch-size", type=positive_int, help="每批创建数量（默认取配置）")
    parser.add_argument("--max-sandboxes", type=positive_int, help="累积存活上限（默认取配置）")
    parser.add_argument(
        "--mem-threshold-pct",
        type=float,
        help="内存安全闸：MemAvailable 低于总内存该百分比时立即中止（默认取 global.mem_threshold_pct）",
    )
    parser.add_argument("--keep-sandboxes", action="store_true", help="测试结束后保留沙箱（默认清理）")
    add_template_argument(parser)
    add_common_arguments(parser)
    parser.add_argument("-o", "--output", help="JSON 报告输出路径")
    parser.add_argument("--sandbox-timeout", type=positive_int, help="沙箱生命周期秒数")
    parser.set_defaults(handler=guarded(execute))


def run(
    ctx: BenchContext,
    *,
    concurrency: int,
    batch_size: int,
    max_sandboxes: int,
    mem_threshold_pct: float,
    keep_sandboxes: bool,
) -> dict:
    params = {
        "concurrency": concurrency,
        "batch_size": batch_size,
        "max_sandboxes": max_sandboxes,
        "mem_threshold_pct": mem_threshold_pct,
    }
    result = base_result("density", ctx, params)
    clean_host_orphans(ctx, "density-pre")
    ctx.note(
        "内存口径说明：Σ cgroup usage 为 v1 memory 控制器总量（共享页记首个 touch 者，"
        "总量准、均摊偏）；PSS 均摊来自 FC 进程 smaps_rollup；私有脏页均摊含 "
        "Private_Hugetlb（VM 内存大页、UFFD 懒加载主体，本机大页池不计入 MemAvailable，"
        "故 free 口径会低估真实占用）；每批创建后静置 2.5s 再采集"
    )
    baseline = read_meminfo()
    total_kb = baseline.get("MemTotal", 0)
    baseline_available_kb = baseline.get("MemAvailable", 0)
    threshold_kb = total_kb * mem_threshold_pct / 100
    result["baseline"] = {
        "mem_total_mb": round(total_kb / 1024),
        "mem_available_mb": round(baseline_available_kb / 1024),
        "threshold_mb": round(threshold_kb / 1024),
    }

    tiers = [{
        "alive": 0,
        "available_mb": round(baseline_available_kb / 1024),
        "overhead_mb_per_sandbox": None,
    }]
    alive = 0
    aborted = False
    while alive < max_sandboxes:
        meminfo = read_meminfo()
        available_kb = meminfo.get("MemAvailable", 0)
        if available_kb < threshold_kb:
            aborted = True
            ctx.note(
                f"内存安全闸触发：MemAvailable {round(available_kb / 1024)} MiB "
                f"低于阈值 {round(threshold_kb / 1024)} MiB（总内存 {mem_threshold_pct}%），"
                f"在存活 {alive} 个沙箱时中止"
            )
            break
        batch = min(batch_size, max_sandboxes - alive)
        results, _wall_ms = run_concurrent(lambda _i: timed_create(ctx), batch, concurrency)
        succeeded = [item for item in results if item.ok]
        alive += len(succeeded)
        failed = len(results) - len(succeeded)
        if failed:
            ctx.note(f"批次目标 {batch} 个，失败 {failed} 个")
        # 静置让 UFFD 懒加载落定后再采集（cgroup v1 + smaps_rollup 双口径）
        time.sleep(2.5)
        with ctx._lock:
            alive_ids = list(ctx.created_ids)
        memory = collect_memory_metrics(alive_ids)
        meminfo = read_meminfo()
        available_mb = round(meminfo.get("MemAvailable", 0) / 1024)
        overhead = None
        if alive:
            overhead = (baseline_available_kb - meminfo.get("MemAvailable", 0)) / 1024 / alive
        if overhead is not None and overhead < 0:
            ctx.note(
                "均摊开销为负值：主机其他活动释放的内存超过沙箱占用，"
                "该数据点受主机噪声主导，建议以 PSS 均摊为准"
            )
        if memory["cgroup_missing"] or memory["fc_missing"]:
            ctx.note(
                f"内存采集部分缺失：cgroup_missing={memory['cgroup_missing']} "
                f"fc_missing={memory['fc_missing']}（本机无独立沙箱 memory cgroup 时属预期）"
            )
        tiers.append({
            "alive": alive,
            "available_mb": available_mb,
            "overhead_mb_per_sandbox": round(overhead, 1) if overhead is not None else None,
            "memory": memory,
            "batch_failures": failed,
        })
        if failed == batch:
            ctx.note("整批创建失败，提前结束密度测试")
            break

    result["tiers"] = tiers
    if not keep_sandboxes:
        outcomes = cleanup_created(ctx)
        ctx.note(f"密度测试结束，已清理沙箱：{outcomes}")
        ensure_clean_slate(ctx, "density-final")
    else:
        ctx.note(f"按 --keep-sandboxes 保留 {alive} 个存活沙箱；使用 bench kill-all 清理")
    if aborted:
        result["status"] = "aborted"
        result["error"] = "内存安全闸触发，测试提前中止（已记录中止前数据）"
    elif alive == 0:
        result["status"] = "failed"
        result["error"] = "未能创建任何沙箱"
    return finish_result(result, ctx)


def execute(args: argparse.Namespace) -> int:
    cfg = bench_config.load(args.config)
    template = bench_config.resolve_template(args.template, cfg)
    ctx = build_context(
        template=template,
        result_root=bench_config.resolve_result_root(cfg),
        netns_growth_threshold=int(bench_config.global_param(cfg, "netns_growth_threshold")),
        sandbox_timeout=args.sandbox_timeout or max(
            3600, int(bench_config.global_param(cfg, "sandbox_timeout"))
        ),
    )
    section = cfg["density"]
    result = run(
        ctx,
        concurrency=args.concurrency or 50,
        batch_size=args.batch_size or int(section["batch_size"]),
        max_sandboxes=args.max_sandboxes or int(section["max_sandboxes"]),
        mem_threshold_pct=(
            args.mem_threshold_pct
            if args.mem_threshold_pct is not None
            else float(bench_config.global_param(cfg, "mem_threshold_pct"))
        ),
        keep_sandboxes=args.keep_sandboxes,
    )
    environment = collect_environment(ctx.client, ctx.template, template_source=ctx.template_source)
    json_path = bench_report.write_bench_json(ctx.result_dir, "density", result)
    if args.output:
        import shutil

        shutil.copyfile(json_path, args.output)
    bench_report.write_report(ctx.result_dir, [result], environment, ctx.run_id)
    print_json({"result_dir": str(ctx.result_dir), "baseline": result["baseline"], "tiers": result["tiers"]})
    return 0 if result["status"] in ("ok", "aborted") else 1
