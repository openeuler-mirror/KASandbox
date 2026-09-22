"""bench replay-matrix: 无 Pause/Resume 超分梯度矩阵驱动。

语义来源：cubesandbox-replay-suite 的 scripts/run_no_lifecycle_oversubscription_benchmark.py：
- 梯度档位：--start/--step 递推或 --tiers 显式指定，每档 concurrency = target_count
  = 档位值（total_multiplier=1 语义），同进程调用 replay-nolifecycle 的执行核心；
- 档间 cooldown（默认 180s）；档失败（执行失败/清理核验失败/成功率不足）即终止矩阵；
- 拐点自动停止：该档 command p95 > 第一档（基准档）p95 的 --degradation-multiplier 倍
  （默认 3.0，0=禁用）也判定到达拐点并终止；两种终止原因都写入 matrix.json；
- 输出 report.md 的中文表头与对方完全一致，matrix.json 含 input_and_tool_hashes
  （catalog/轨迹/脚本的 sha256）。

有意偏差：对方把宿主检查委托给外部脚本，这里内置本机只读检查（netns/nbd/
firecracker/jailer 进程/MemAvailable，与档前基线对比），核验不过同样终止矩阵。
"""

from __future__ import annotations

import argparse
import json
import math
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from ..e2b_common import print_json
from . import config as bench_config
from .client import BenchClient
from .common import (
    _active_nbd_count,
    _netns_count,
    _pgrep_count,
    add_common_arguments,
    guarded,
    new_run_id,
    read_meminfo,
)
from .replay_nolifecycle import (
    add_runtime_arguments,
    apportion_counts,
    build_tier_tasks,
    ensure_templates,
    load_catalog,
    print_tier_summary,
    resolve_runtime,
    run_tier,
    sha256_file,
)

TOOL_NAME = "bench replay-matrix（对齐 cubesandbox-replay-suite no-lifecycle oversubscription driver）"
SCRIPT_FILES = (
    Path(__file__).resolve(),
    (Path(__file__).resolve().parent / "replay_nolifecycle.py"),
)


def _iso_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _parse_tiers(value: str) -> list[int]:
    try:
        tiers = sorted({int(part.strip()) for part in value.split(",")})
    except ValueError:
        raise argparse.ArgumentTypeError("--tiers 必须是逗号分隔的正整数") from None
    if not tiers or tiers[0] < 1:
        raise argparse.ArgumentTypeError("--tiers 必须是逗号分隔的正整数")
    return tiers


def host_snapshot() -> dict[str, Any]:
    """本机只读宿主残留观测（不依赖外部脚本）。"""
    return {
        "netns": _netns_count(),
        "nbd": _active_nbd_count(),
        "firecracker": _pgrep_count("firecracker"),
        "jailer": _pgrep_count("jailer"),
        "orchestrator": _pgrep_count("orchestrator"),
        "mem_available_mb": round(read_meminfo().get("MemAvailable", 0) / 1024),
    }


def host_check_ok(baseline: dict[str, Any], current: dict[str, Any]) -> bool:
    """残留判定：firecracker/jailer 进程与 nbd/netns 占用须回到档前基线。"""
    return (
        current["firecracker"] <= baseline["firecracker"]
        and current["jailer"] <= baseline["jailer"]
        and current["nbd"] <= baseline["nbd"]
        and current["netns"] <= baseline["netns"]
    )


def register(subparsers) -> None:
    parser = subparsers.add_parser(
        "replay-matrix",
        help="无 Pause/Resume 超分梯度：按档位（concurrency=target=档位值）逐档驱动 replay-nolifecycle，拐点自动停止",
    )
    add_runtime_arguments(parser)
    parser.add_argument("--start", type=int, help="起始档位（默认 120）")
    parser.add_argument("--step", type=int, help="档位步长（默认 60）")
    parser.add_argument(
        "--tiers",
        type=_parse_tiers,
        help="显式档位列表（如 120,180,240），覆盖 --start/--step",
    )
    parser.add_argument("--max-tier", type=int, help="档位上限保护（默认 600）")
    parser.add_argument("--cooldown", type=float, help="档间冷却秒（默认 180）")
    parser.add_argument(
        "--degradation-multiplier",
        type=float,
        help="拐点判定：该档 command p95 > 基准档 p95 的该倍数即终止（默认 3.0，0=禁用）",
    )
    parser.add_argument(
        "--min-success-rate",
        type=float,
        help="档位有效所需的最低任务成功率（默认 1.0，即全部成功）",
    )
    parser.add_argument(
        "--baseline-p95-ms",
        type=float,
        help="直接注入基准 command p95（ms），跳过基准档实测；注入后第一档也参与拐点判定",
    )
    parser.add_argument(
        "-o",
        "--output-dir",
        type=Path,
        help="矩阵输出目录（默认 test-results/<时间戳>-bench/；每档一个 tier-N/ 子目录）",
    )
    parser.set_defaults(handler=guarded(execute))


def _resolve_matrix_params(args: argparse.Namespace, cfg: dict[str, Any]) -> dict[str, Any]:
    section = cfg.get("replay_matrix", {})

    def _r(cli_value, key: str, default):
        return cli_value if cli_value is not None else section.get(key, default)

    start = int(_r(args.start, "start", 120))
    step = int(_r(args.step, "step", 60))
    max_tier = int(_r(args.max_tier, "max_tier", 600))
    cooldown = float(_r(args.cooldown, "cooldown", 180))
    degradation = float(_r(args.degradation_multiplier, "degradation_multiplier", 3.0))
    min_success_rate = float(_r(args.min_success_rate, "min_success_rate", 1.0))
    baseline_p95_ms = _r(args.baseline_p95_ms, "baseline_p95_ms", None)
    if baseline_p95_ms is not None:
        baseline_p95_ms = float(baseline_p95_ms)
        if not math.isfinite(baseline_p95_ms) or baseline_p95_ms <= 0:
            raise ValueError("baseline-p95-ms 必须是正数")
    if args.tiers:
        tiers = [tier for tier in args.tiers if tier <= max_tier]
        if not tiers:
            raise ValueError(f"--tiers 全部超过 --max-tier {max_tier}")
    else:
        if start < 1 or step < 1 or max_tier < start:
            raise ValueError("要求 1 <= start <= max-tier 且 step >= 1")
        tiers = list(range(start, max_tier + 1, step))
    if cooldown < 0 or not math.isfinite(cooldown):
        raise ValueError("cooldown 必须是非负有限值")
    if degradation < 0 or not math.isfinite(degradation):
        raise ValueError("degradation-multiplier 必须是非负有限值（0=禁用）")
    if not 0 < min_success_rate <= 1:
        raise ValueError("min-success-rate 必须在 (0, 1] 区间")
    return {
        "start": start,
        "step": step,
        "max_tier": max_tier,
        "tiers": tiers,
        "cooldown": cooldown,
        "degradation_multiplier": degradation,
        "min_success_rate": min_success_rate,
        "baseline_p95_ms": baseline_p95_ms,
    }


def _render_report(report: dict[str, Any]) -> str:
    """与对方 report.md 完全一致的表结构；末尾追加命令延迟小节（不破坏主表）。"""
    lines = [
        "# 无 Pause/Resume 超分梯度\n",
        "倍率是相对基准 N 的并发倍数，不是物理 CPU/内存超分比。API 峰值不是宿主密度证明。\n",
        "| 轮次 | 请求并发 | 实际并发 | 任务数 | 状态 | 结果 | API 峰值 | 失败命令 |",
        "| --- | ---: | ---: | ---: | --- | --- | ---: | ---: |",
    ]
    for row in report["trials"]:
        lines.append(
            f"| {row['case_id']} | {row['requested_concurrency']} | {row['concurrency']} | "
            f"{row['target_count']} | {row['execution']} | {row['result']} | "
            f"{row.get('peak_running', 'unknown')} | {row.get('failed_commands', 'unknown')} |"
        )
    lines.append("")
    lines.append("## 命令延迟（拐点观测）\n")
    lines.append("| 轮次 | command p95 (ms) | command max (ms) | 相对基准 p95 倍率 |")
    lines.append("| --- | ---: | ---: | ---: |")
    baseline_p95 = (report.get("termination") or {}).get("baseline_command_p95_ms")
    for row in report["trials"]:
        p95 = row.get("command_p95_ms")
        ratio = (
            round(p95 / baseline_p95, 2)
            if isinstance(p95, (int, float)) and isinstance(baseline_p95, (int, float)) and baseline_p95
            else "—"
        )
        lines.append(
            f"| {row['case_id']} | {p95 if p95 is not None else 'unknown'} | "
            f"{row.get('command_max_ms', 'unknown')} | {ratio} |"
        )
    termination = report.get("termination") or {}
    if termination.get("terminated"):
        lines.append("")
        lines.append(f"终止原因：{termination.get('reason')}")
    return "\n".join(lines) + "\n"


def execute(args: argparse.Namespace) -> int:
    cfg = bench_config.load(args.config)
    runtime = resolve_runtime(args, cfg)
    matrix = _resolve_matrix_params(args, cfg)
    entries = load_catalog(runtime["catalog"], runtime["trajectory_root"])
    tiers: list[int] = matrix["tiers"]

    hash_paths = [
        Path(runtime["catalog"]).expanduser().resolve(),
        *SCRIPT_FILES,
        *(entry.trajectory_path for entry in entries if entry.trajectory_path),
    ]
    hashes = {str(path): sha256_file(path) for path in hash_paths}

    # 分摊校验（dry-run 与实跑共用，任一档分摊失败直接报错）
    tier_counts = {tier: apportion_counts([1] * len(entries), tier) for tier in tiers}

    if args.dry_run:
        print(
            f"[replay-matrix dry-run] catalog={runtime['catalog']}（{len(entries)} 个镜像）；"
            f"档位 {tiers}；cooldown={matrix['cooldown']}s；"
            f"拐点倍率={matrix['degradation_multiplier']}"
        )
        preview_tiers = []
        for tier in tiers:
            counts = tier_counts[tier]
            tasks = build_tier_tasks(entries, counts)
            preview_tiers.append(
                {
                    "case_id": f"tier-{tier}",
                    "concurrency": tier,
                    "target_count": tier,
                    "ratio": round(tier / matrix["start"], 3),
                    "workload_counts_sum": sum(counts),
                    "workload_counts_min": min(counts),
                    "workload_counts_max": max(counts),
                    "workload_counts": {
                        entry.name: count for entry, count in zip(entries, counts)
                    },
                    "tasks": len(tasks),
                }
            )
        print_json(
            {
                "dry_run": True,
                "sandbox_lifecycle_enabled": False,
                "execution": "planned",
                "base": matrix["start"],
                "start": matrix["start"],
                "step": matrix["step"],
                "cooldown_sec": matrix["cooldown"],
                "degradation_multiplier": matrix["degradation_multiplier"],
                "min_success_rate": matrix["min_success_rate"],
                "command_timeout_sec": runtime["command_timeout_sec"],
                "launch_interval_sec": runtime["launch_interval_sec"],
                "control_plane_qps": runtime["control_plane_qps"],
                "mem_threshold_pct": runtime["mem_threshold_pct"],
                "sample_interval_sec": runtime["sample_interval_sec"],
                "tiers": preview_tiers,
            }
        )
        return 0

    client = BenchClient(timeout=600)
    ensure_templates(client, entries, auto_provision=args.auto_provision)

    run_id = new_run_id()
    output_dir = (
        args.output_dir or bench_config.resolve_result_root(cfg) / f"{run_id}-bench"
    ).expanduser().resolve()
    output_dir.mkdir(parents=True, exist_ok=False)

    baseline_host = host_snapshot()
    report: dict[str, Any] = {
        "schema_version": 1,
        "tool": TOOL_NAME,
        "sandbox_lifecycle_enabled": False,
        "run_id": run_id,
        "started_at": _iso_now(),
        "completed_at": None,
        "execution": "running",
        "result": "unknown",
        "base": matrix["start"],
        "start": matrix["start"],
        "step": matrix["step"],
        "max_tier": matrix["max_tier"],
        "total_multiplier": 1,
        "cooldown_sec": matrix["cooldown"],
        "min_success_rate": matrix["min_success_rate"],
        "host_baseline": baseline_host,
        "input_and_tool_hashes": hashes,
        "trials": [],
        "termination": {
            "terminated": False,
            "reason": None,
            "degradation_multiplier": matrix["degradation_multiplier"],
            "baseline_command_p95_ms": None,
        },
    }

    def save() -> None:
        (output_dir / "matrix.json").write_text(
            json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8"
        )
        (output_dir / "report.md").write_text(_render_report(report), encoding="utf-8")

    save()
    baseline_p95: float | None = matrix["baseline_p95_ms"]
    if baseline_p95 is not None:
        report["termination"]["baseline_command_p95_ms"] = baseline_p95
        report["termination"]["baseline_source"] = "injected"
        save()
    exit_code = 0
    terminated = False
    for position, tier in enumerate(tiers):
        counts = tier_counts[tier]
        trial: dict[str, Any] = {
            "case_id": f"tier-{tier}",
            "ratio": round(tier / matrix["start"], 3),
            "requested_concurrency": tier,
            "concurrency": tier,
            "target_count": tier,
            "workload_counts": {
                entry.name: count for entry, count in zip(entries, counts)
            },
            "execution": "running",
            "result": "unknown",
            "valid": None,
            "started_at": _iso_now(),
            "output_dir": str(output_dir / f"tier-{tier}"),
        }
        report["trials"].append(trial)
        save()
        print(
            f"\n[replay-matrix] 档位 {tier}（{position + 1}/{len(tiers)}）："
            f"concurrency=target={tier}",
            flush=True,
        )
        tasks = build_tier_tasks(entries, counts)
        result = run_tier(
            client=client,
            tasks=tasks,
            concurrency=tier,
            command_timeout_sec=runtime["command_timeout_sec"],
            command_timeout_continue=runtime["command_timeout_continue"],
            launch_interval_sec=runtime["launch_interval_sec"],
            control_plane_qps=runtime["control_plane_qps"],
            mem_threshold_pct=runtime["mem_threshold_pct"],
            sample_interval_sec=runtime["sample_interval_sec"],
            sandbox_timeout=runtime["sandbox_timeout"],
            output_dir=output_dir / f"tier-{tier}",
            run_id=f"{run_id}-tier-{tier}",
        )
        print_tier_summary(result, output_dir / f"tier-{tier}")

        current_host = host_snapshot()
        host_ok = host_check_ok(baseline_host, current_host)
        success_rate = result["succeeded_tasks"] / result["target_count"]
        tier_exit = 0 if result["succeeded"] else 1
        failed_commands = result["command_totals"]["failed_commands"]
        invalid_reasons: list[str] = []
        if tier_exit:
            invalid_reasons.append(f"执行失败（{result['failed_tasks']} 个任务未成功）")
        if not result["cleanup_verified"]:
            invalid_reasons.append("owned ids 清理核验未过（delete/GET 404 核对失败）")
        if not host_ok:
            invalid_reasons.append(f"宿主残留核验未过：{current_host}（基线 {baseline_host}）")
        if success_rate < matrix["min_success_rate"]:
            invalid_reasons.append(
                f"成功率 {success_rate:.3f} 低于阈值 {matrix['min_success_rate']}"
            )
        tier_valid = not invalid_reasons
        command_stats = result["latency"]["command"]
        trial.update(
            execution="finished",
            result=("pass" if not failed_commands else "mixed") if tier_valid else "fail",
            valid=tier_valid,
            completed_at=_iso_now(),
            exit_code=tier_exit,
            peak_running=result["peak_running"],
            running_slots=result["running_slots"],
            failed_commands=failed_commands,
            cleanup_verified=result["cleanup_verified"],
            replay_wall_sec=result["replay_wall_sec"],
            replay_tasks_per_sec=(
                round(result["target_count"] / result["replay_wall_sec"], 4)
                if result["replay_wall_sec"]
                else None
            ),
            success_rate=round(success_rate, 4),
            host_check={
                "ok": host_ok,
                "baseline": baseline_host,
                "current": current_host,
            },
            command_p95_ms=command_stats.get("p95_ms"),
            command_max_ms=command_stats.get("max_ms"),
        )
        if invalid_reasons:
            trial["invalid_reasons"] = invalid_reasons

        if baseline_p95 is None and command_stats.get("p95_ms") is not None:
            baseline_p95 = command_stats["p95_ms"]
            report["termination"]["baseline_command_p95_ms"] = baseline_p95

        termination_reason: str | None = None
        if not tier_valid:
            termination_reason = "档位无效：" + "；".join(invalid_reasons)
            exit_code = 1
        elif (
            matrix["degradation_multiplier"] > 0
            and baseline_p95
            and command_stats.get("p95_ms") is not None
            and (matrix["baseline_p95_ms"] is not None or tier != tiers[0])
            and command_stats["p95_ms"] > baseline_p95 * matrix["degradation_multiplier"]
        ):
            termination_reason = (
                f"拐点：command p95 {command_stats['p95_ms']}ms > "
                f"基准档 {baseline_p95}ms × {matrix['degradation_multiplier']}"
            )
            trial["degradation_triggered"] = True
        save()
        if termination_reason:
            report["termination"].update(terminated=True, reason=termination_reason)
            report["execution"] = "aborted"
            print(f"[replay-matrix] 终止：{termination_reason}", flush=True)
            terminated = True
            break
        if position + 1 < len(tiers):
            cooldown = matrix["cooldown"]
            if cooldown > 0:
                print(f"[replay-matrix] 档间冷却 {cooldown:.0f}s…", flush=True)
                deadline = time.monotonic() + cooldown
                while True:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        break
                    time.sleep(min(30, remaining))

    if not terminated:
        report["execution"] = "finished"
    report["result"] = (
        "fail"
        if any(trial["valid"] is False for trial in report["trials"])
        else ("mixed" if any(trial["result"] == "mixed" for trial in report["trials"]) else "pass")
    )
    report["completed_at"] = _iso_now()
    report["exit_code"] = exit_code
    save()

    print("\n【矩阵汇总】")
    print(_render_report(report))
    print_json(
        {
            "result_dir": str(output_dir),
            "execution": report["execution"],
            "result": report["result"],
            "termination": report["termination"],
        }
    )
    return exit_code
