"""JSON result files and Chinese Markdown benchmark reports."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

BENCH_TITLES = {
    "create": "基于 Template 创建沙箱（启动延迟与并发扩展）",
    "scale": "规模测试（一次性并发拉起 N 个沙箱的整批 wall）",
    "density": "单机部署密度（每沙箱内存开销）",
    "snapshot-concurrency": "Snapshot 制作耗时与并发的关系",
    "snapshot-dirty": "Snapshot 制作耗时与 Dirty Page 的关系",
    "create-from-snapshot": "基于 Snapshot 启动沙箱",
    "rollback": "Rollback（回滚到自身 checkpoint）",
    "clone": "Clone（从源沙箱派生新沙箱）",
    "pause-resume": "Pause / Resume",
}


def _ms(value: float | None) -> str:
    return f"{value:.1f} ms" if isinstance(value, (int, float)) else "—"


def _num(value: float | None, unit: str = "") -> str:
    if not isinstance(value, (int, float)):
        return "—"
    return f"{value:.1f}{unit}" if unit else f"{value:g}"


def _pct(value: float | None) -> str:
    return f"{value:.2f}%" if isinstance(value, (int, float)) else "—"


def _stats_cells(stats: dict[str, Any]) -> list[str]:
    return [
        _ms(stats.get("avg_ms")),
        _ms(stats.get("min_ms")),
        _ms(stats.get("p95_ms")),
        _ms(stats.get("max_ms")),
    ]


def _render_create(result: dict[str, Any]) -> list[str]:
    lines = [
        "| 并发 | 请求数 | avg | min | p95 | max | wall | 单沙箱均摊 | 吞吐 | 成功率 |",
        "|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for tier in result.get("tiers", []):
        metrics = tier["metrics"]
        lines.append(
            f"| {tier['concurrency']} | {metrics['count']} | "
            + " | ".join(_stats_cells(metrics))
            + f" | {_ms(metrics.get('wall_ms'))} | {_ms(metrics.get('per_ms'))} "
            + f"| {_num(metrics.get('throughput_per_s'))} 个/s | {_pct(metrics.get('success_rate'))} |"
        )
    return lines


def _render_scale(result: dict[str, Any]) -> list[str]:
    lines = [
        "| 规模 | create avg | create p50 | create p90 | create p95 | create max | "
        "wall avg | 单沙箱均摊 | 吞吐 | destroy avg | destroy p95 | 成功率 |",
        "|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for tier in result.get("tiers", []):
        metrics = tier.get("metrics")
        if not metrics:
            lines.append(f"| {tier['size']} | 中止（内存安全闸） | — | — | — | — | — | — | — | — | — | — |")
            continue
        lines.append(
            f"| {tier['size']} | {_ms(metrics.get('avg_ms'))} | {_ms(metrics.get('p50_ms'))} | "
            f"{_ms(metrics.get('p90_ms'))} | {_ms(metrics.get('p95_ms'))} | {_ms(metrics.get('max_ms'))} | "
            f"{_ms(metrics.get('wall_ms'))} | {_ms(metrics.get('per_unit_avg_ms'))} "
            f"| {_num(metrics.get('throughput_per_s'))} 个/s | {_ms(metrics.get('destroy_avg_ms'))} "
            f"| {_ms(metrics.get('destroy_p95_ms'))} | {_pct(metrics.get('success_rate'))} |"
        )
    return lines


def _render_density(result: dict[str, Any]) -> list[str]:
    lines = [
        "| 存活沙箱数 | 系统可用内存（free，参考） | Σ cgroup usage | PSS 均摊 | "
        "私有脏页均摊 | 共享页均摊 | 共享比例 | 单沙箱开销（free 口径，参考） |",
        "|---:|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for tier in result.get("tiers", []):
        memory = tier.get("memory") or {}
        overhead = tier.get("overhead_mb_per_sandbox")
        share_ratio = memory.get("share_ratio")

        def _mb(key: str) -> str:
            value = memory.get(key)
            return f"{value:.1f} MB" if isinstance(value, (int, float)) else "—"

        lines.append(
            f"| {tier['alive']} | {tier['available_mb']:,} MiB | "
            f"{_mb('cgroup_usage_total_mb')} | "
            f"{_mb('pss_avg_mb')} | "
            f"{_mb('private_dirty_avg_mb')} | "
            f"{_mb('shared_avg_mb')} | "
            + (f"{share_ratio * 100:.1f}%" if isinstance(share_ratio, (int, float)) else "—")
            + " | "
            + (f"~{overhead:.1f} MB" if isinstance(overhead, (int, float)) else "—")
            + " |"
        )
    return lines


def _render_wall_tiers(result: dict[str, Any], *, unit_label: str) -> list[str]:
    lines = [
        f"| 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | {unit_label} | 成功率 |",
        "|---:|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for tier in result.get("tiers", []):
        metrics = tier["metrics"]
        lines.append(
            f"| {tier['concurrency']} | {metrics.get('rounds', '—')} | "
            + " | ".join(_stats_cells(metrics))
            + f" | {_ms(metrics.get('per_unit_avg_ms'))} | {_pct(tier.get('success_rate'))} |"
        )
    return lines


def _render_snapshot_dirty(result: dict[str, Any]) -> list[str]:
    lines = [
        "| 写入量 | snapshot avg | snapshot min | snapshot p95 | snapshot max | "
        "create sandbox avg | create sandbox min | create sandbox p95 | create sandbox max |",
        "|---:|---:|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for tier in result.get("tiers", []):
        snapshot = tier["snapshot"]
        create = tier["create_from_snapshot"]
        lines.append(
            f"| {tier['dirty_mb']} MB | "
            + " | ".join(_stats_cells(snapshot))
            + " | "
            + " | ".join(_stats_cells(create))
            + " |"
        )
    return lines


def _render_create_from_snapshot(result: dict[str, Any]) -> list[str]:
    lines = [
        "| 并发 | 每轮沙箱数 | 轮数 | wall avg | wall min | wall p95 | wall max | per-sandbox avg | 成功率 |",
        "|---:|---:|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for tier in result.get("tiers", []):
        metrics = tier["metrics"]
        lines.append(
            f"| {tier['concurrency']} | {tier['per_round']} | {metrics.get('rounds', '—')} | "
            + " | ".join(_stats_cells(metrics))
            + f" | {_ms(metrics.get('per_unit_avg_ms'))} | {_pct(tier.get('success_rate'))} |"
        )
    return lines


def _render_clone(result: dict[str, Any]) -> list[str]:
    lines = [
        "| 场景 | n | 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | per-clone avg | 成功率 |",
        "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for tier in result.get("tiers", []):
        metrics = tier["metrics"]
        lines.append(
            f"| {tier['label']} | {tier['n']} | {tier['concurrency']} | {metrics.get('rounds', '—')} | "
            + " | ".join(_stats_cells(metrics))
            + f" | {_ms(metrics.get('per_unit_avg_ms'))} | {_pct(tier.get('success_rate'))} |"
        )
    return lines


def _render_pause_resume(result: dict[str, Any]) -> list[str]:
    lines: list[str] = []
    for key, title, unit_label in (
        ("pause", "Pause 测试数据", "per-pause avg"),
        ("resume", "Resume 测试数据", "per-resume avg"),
    ):
        lines.append(f"**{title}：**")
        lines.append("")
        lines.append(f"| 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | {unit_label} | 成功率 |")
        lines.append("|---:|---:|---:|---:|---:|---:|---:|---:|")
        for tier in result.get("tiers", []):
            metrics = tier[key]
            lines.append(
                f"| {tier['concurrency']} | {metrics.get('rounds', '—')} | "
                + " | ".join(_stats_cells(metrics))
                + f" | {_ms(metrics.get('per_unit_avg_ms'))} | {_pct(metrics.get('success_rate'))} |"
            )
        lines.append("")
    return lines[:-1]


RENDERERS = {
    "create": _render_create,
    "scale": _render_scale,
    "density": _render_density,
    "snapshot-concurrency": lambda r: _render_wall_tiers(r, unit_label="per-snapshot avg"),
    "snapshot-dirty": _render_snapshot_dirty,
    "create-from-snapshot": _render_create_from_snapshot,
    "rollback": lambda r: _render_wall_tiers(r, unit_label="per-rollback avg"),
    "clone": _render_clone,
    "pause-resume": _render_pause_resume,
}


def write_bench_json(result_dir: Path, name: str, payload: dict[str, Any]) -> Path:
    result_dir.mkdir(parents=True, exist_ok=True)
    path = result_dir / f"bench_{name.replace('-', '_')}.json"
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
    return path


def render_report(
    results: list[dict[str, Any]],
    environment: dict[str, Any],
    run_id: str,
) -> str:
    template = environment.get("template") or {}
    lines = [
        "# KASandbox 性能基准测试报告",
        "",
        f"- 运行编号：`{run_id}`",
        f"- 对标口径：CubeSandbox 性能基准文章（avg / min / p95 / max / wall / per / 吞吐 / 成功率）",
        "",
        "## 1. 测试环境",
        "",
        "| 项目 | 详情 |",
        "|---|---|",
        f"| OS | {environment.get('os', 'unknown')} |",
        f"| 内核 | {environment.get('kernel', 'unknown')} {environment.get('arch', '')} |",
        f"| CPU 型号 | {environment.get('cpu_model', 'unknown')} |",
        f"| CPU 核数 | {environment.get('cpu_cores', 'unknown')} 逻辑核 |",
        f"| 内存总量 | {environment.get('mem_total_mb', 0):,} MiB |",
        f"| API 地址 | {environment.get('api_url', 'unknown')} |",
        (
            f"| 沙箱模板 | `{template.get('templateID', 'unknown')}`"
            f"（{', '.join(template.get('aliases') or []) or '无别名'}，"
            f"{template.get('cpuCount', '?')} vCPU / {template.get('memoryMB', '?')} MiB，"
            f"磁盘 {template.get('diskSizeMB', '?')} MB，"
            f"来源：{template.get('source', '用户指定')}）|"
        ),
        "",
        "指标说明：avg / min / p95 / max 为单请求延迟（毫秒）；wall 为整批端到端耗时；"
        "per 为 wall ÷ 操作数的均摊耗时；吞吐为每秒完成操作数。预热已禁用（warmup=0），每轮都计入正式测量。",
        "",
        "## 2. 测试结果总览",
        "",
        "| 测试项 | 状态 | 说明 |",
        "|---|---|---|",
    ]
    for result in results:
        name = result.get("bench", "unknown")
        title = BENCH_TITLES.get(name, name)
        status = result.get("status", "unknown")
        status_label = {"ok": "成功", "failed": "失败", "aborted": "中止"}.get(status, status)
        detail = result.get("error") or ""
        lines.append(f"| {title} | {status_label} | {detail} |")

    lines.extend(["", "## 3. 各测试项详细数据", ""])
    for index, result in enumerate(results, 1):
        name = result.get("bench", "unknown")
        title = BENCH_TITLES.get(name, name)
        lines.append(f"### 3.{index} {title}")
        lines.append("")
        status_label = {"ok": "成功", "failed": "失败", "aborted": "中止"}.get(
            result.get("status"), result.get("status", "unknown")
        )
        lines.append(f"- 状态：{status_label}")
        lines.append(f"- 参数：`{json.dumps(result.get('params', {}), ensure_ascii=False, sort_keys=True)}`")
        if result.get("error"):
            lines.append(f"- 错误：{result['error']}")
        renderer = RENDERERS.get(name)
        if renderer and result.get("tiers"):
            lines.append("")
            lines.extend(renderer(result))
        for note in result.get("notes") or []:
            lines.append(f"- 注：{note}")
        lines.append("")

    failed = [r for r in results if r.get("status") != "ok"]
    lines.extend([
        "## 4. 结论",
        "",
        (
            f"全部 {len(results)} 个测试项执行完成，成功率口径见各节数据表。"
            if not failed
            else f"{len(results) - len(failed)}/{len(results)} 个测试项成功；失败项："
            + "、".join(r.get("bench", "unknown") for r in failed)
        ),
        "",
    ])
    return "\n".join(lines)


def write_report(result_dir: Path, results: list[dict[str, Any]], environment: dict[str, Any], run_id: str) -> Path:
    result_dir.mkdir(parents=True, exist_ok=True)
    path = result_dir / "report.md"
    path.write_text(render_report(results, environment, run_id), encoding="utf-8")
    return path
