"""Percentile statistics for benchmark latency samples."""

from __future__ import annotations

import math


def percentile(sorted_samples: list[float], pct: float) -> float:
    """Nearest-rank percentile; samples must be sorted ascending."""
    if not sorted_samples:
        raise ValueError("percentile requires at least one sample")
    rank = max(1, math.ceil(pct / 100 * len(sorted_samples)))
    return sorted_samples[rank - 1]


def _round(value: float | None) -> float | None:
    return round(value, 1) if value is not None else None


def timing_stats(samples_ms: list[float], *, wall_ms: float | None = None, attempted: int | None = None) -> dict:
    """avg/min/p95/max in ms plus optional wall, per-operation, and throughput metrics."""
    attempted = len(samples_ms) if attempted is None else attempted
    ordered = sorted(samples_ms)
    stats = {
        "count": attempted,
        "success": len(samples_ms),
        "failed": attempted - len(samples_ms),
        "success_rate": round(len(samples_ms) * 100 / attempted, 2) if attempted else None,
        "avg_ms": _round(sum(ordered) / len(ordered)) if ordered else None,
        "min_ms": _round(ordered[0]) if ordered else None,
        "p95_ms": _round(percentile(ordered, 95)) if ordered else None,
        "max_ms": _round(ordered[-1]) if ordered else None,
    }
    if wall_ms is not None:
        stats["wall_ms"] = _round(wall_ms)
        if attempted:
            stats["per_ms"] = _round(wall_ms / attempted)
            stats["throughput_per_s"] = _round(len(samples_ms) * 1000 / wall_ms) if wall_ms > 0 else None
    return stats


def wall_stats(walls_ms: list[float], *, unit_count: int) -> dict:
    """Statistics over per-round wall times, with the amortized per-unit average."""
    stats = timing_stats(walls_ms)
    stats["rounds"] = len(walls_ms)
    stats["unit_count"] = unit_count
    if stats["avg_ms"] is not None and unit_count:
        stats["per_unit_avg_ms"] = _round(stats["avg_ms"] / unit_count)
    return stats
