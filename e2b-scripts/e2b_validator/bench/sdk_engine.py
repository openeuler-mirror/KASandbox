"""SDK 创建引擎：与用户 max_test 脚本完全一致的 Sandbox.create 分批并发实现。

从 max_test_e2b_with_exec_command_with_batch_until_sucess.current.py 移植核心语义：
SDK `Sandbox.create(template, timeout=3600)`、ThreadPoolExecutor 每批 ≤150 并发、
失败自动补充（≤10 轮）、不使用 barrier、成功沙箱全部保留实例直到统一销毁。
已去掉脚本的 monkey-patch 调试追踪代码（traced_* / POC mode，与计时无关）。
"""

from __future__ import annotations

import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

from e2b import Sandbox

from ..e2e_sdk_common import sdk_options
from .client import TimedResult
from .common import BenchContext

# 与用户脚本保持一致的常量
BATCH_SIZE = 600  # 每批并发创建的最大数量（超过则分批）
MAX_RETRY_ROUNDS = 10  # 失败补充创建的最大轮数，防止无限循环
CREATE_TIMEOUT = 3600  # 沙箱生命周期秒数（与脚本一致，服务端兜底回收）
DESTROY_CONCURRENCY = 32  # 统一销毁的并发度（串行 kill 在数百沙箱时卡顿）


def percentile(sorted_values: list[float], pct: float) -> float | None:
    """线性插值百分位（与用户脚本口径一致；输入需升序，空列表返回 None）。"""
    if not sorted_values:
        return None
    if len(sorted_values) == 1:
        return sorted_values[0]
    index = (len(sorted_values) - 1) * pct / 100
    lower = int(index)
    upper = min(lower + 1, len(sorted_values) - 1)
    weight = index - lower
    return sorted_values[lower] * (1 - weight) + sorted_values[upper] * weight


def percentile_metrics(create_times_ms: list[float]) -> dict[str, float | None]:
    """由创建耗时样本（毫秒）产出 P50/P90/P99/max，与脚本「新增汇总」口径一致。"""
    ordered = sorted(create_times_ms)

    def _rounded(value: float | None) -> float | None:
        return round(value, 1) if value is not None else None

    return {
        "create_p50_ms": _rounded(percentile(ordered, 50)),
        "create_p90_ms": _rounded(percentile(ordered, 90)),
        "create_p99_ms": _rounded(percentile(ordered, 99)),
        "create_max_ms": _rounded(ordered[-1] if ordered else None),
    }


def latency_stats(samples_ms: list[float], prefix: str = "") -> dict[str, float | None]:
    """单操作耗时统计：avg / p50 / p90 / p95 / max（毫秒，线性插值百分位）。

    prefix 为空时键名为 avg_ms/p50_ms/...；否则为 <prefix>_avg_ms/...（如 destroy_avg_ms）。
    """
    ordered = sorted(samples_ms)

    def _rounded(value: float | None) -> float | None:
        return round(value, 1) if value is not None else None

    def _key(name: str) -> str:
        return f"{prefix}_{name}_ms" if prefix else f"{name}_ms"

    return {
        _key("avg"): _rounded(sum(ordered) / len(ordered)) if ordered else None,
        _key("p50"): _rounded(percentile(ordered, 50)),
        _key("p90"): _rounded(percentile(ordered, 90)),
        _key("p95"): _rounded(percentile(ordered, 95)),
        _key("max"): _rounded(ordered[-1] if ordered else None),
    }


def create_one(template: str, task_id: int, *, timeout: int = CREATE_TIMEOUT) -> dict[str, Any]:
    """创建单个沙箱（仅创建不销毁），返回结果 dict（成功时含存活实例 instance）。

    与用户脚本 create_sandbox 一致：perf_counter 包住 Sandbox.create；不带 metadata。
    """
    start = time.perf_counter()
    try:
        sbx = Sandbox.create(template, timeout=timeout, **sdk_options())
        create_time = time.perf_counter() - start
        return {
            "ok": True,
            "task_id": task_id,
            "sandbox_id": sbx.sandbox_id,
            "create_time_s": create_time,
            "instance": sbx,
        }
    except Exception as exc:
        return {
            "ok": False,
            "task_id": task_id,
            "error": f"{type(exc).__name__}: {exc}",
            "total_time_s": time.perf_counter() - start,
            "instance": None,
        }


def batch_create(
    ctx: BenchContext,
    template: str,
    target_num: int,
    *,
    concurrency: int,
) -> dict[str, Any]:
    """分批并发创建 target_num 个沙箱，失败自动补充（与脚本 batch_create_sandboxes 一致）。

    每批并发 min(BATCH_SIZE, concurrency, 剩余数)，批内失败数在下一批自动补充，
    最多 MAX_RETRY_ROUNDS 轮；不使用 barrier。成功沙箱实例保留在返回值的
    instances 中（存活），sandbox_id 同时登记进 ctx.created_ids 作为残留清理兜底。
    """
    results: list[dict[str, Any]] = []
    instances: list[Any] = []
    create_times_ms: list[float] = []
    errors: list[str] = []
    batch_size = max(1, min(BATCH_SIZE, concurrency))

    print(f"\n{'=' * 70}")
    print(
        f"开始创建 {target_num} 个沙箱"
        f"（每批并发 ≤{batch_size}，失败自动补充 ≤{MAX_RETRY_ROUNDS} 轮）..."
    )

    success_count = 0
    retry_round = 0
    batch_num = 1
    start_time = time.perf_counter()

    while success_count < target_num:
        remaining = target_num - success_count
        current_batch = min(batch_size, remaining)
        if current_batch <= 0:
            break

        print(
            f"\n--- 第 {batch_num} 批：并发创建 {current_batch} 个沙箱"
            f"（进度: {success_count}/{target_num}）---"
        )
        batch_start = time.perf_counter()
        batch_success = 0
        batch_failed = 0

        with ThreadPoolExecutor(max_workers=current_batch) as executor:
            futures = [
                executor.submit(create_one, template, batch_num * 1000 + i)
                for i in range(current_batch)
            ]
            for future in as_completed(futures):
                item = future.result()
                results.append(item)
                if item["ok"]:
                    batch_success += 1
                    success_count += 1
                    instances.append(item["instance"])
                    create_times_ms.append(item["create_time_s"] * 1000)
                    ctx.track(item["sandbox_id"])
                    print(
                        f"[成功] 任务 {item['task_id']:4d} | "
                        f"启动耗时: {item['create_time_s']:.2f}s | "
                        f"沙箱ID: {item['sandbox_id'][:8]}..."
                    )
                else:
                    batch_failed += 1
                    errors.append(item["error"])
                    print(
                        f"[失败] 任务 {item['task_id']:4d} | "
                        f"耗时: {item['total_time_s']:.2f}s | "
                        f"原因: {item['error'][:50]}"
                    )

        batch_duration = time.perf_counter() - batch_start
        print(
            f"--- 第 {batch_num} 批完成，成功: {batch_success}, "
            f"失败: {batch_failed}, 耗时 {batch_duration:.2f}秒 ---"
        )

        batch_num += 1
        retry_round += 1

        # 有失败且未达目标时自动补充创建，达到最大轮数则放弃
        if batch_failed > 0 and success_count < target_num:
            need_supplement = target_num - success_count
            print(f"⚠️  检测到 {batch_failed} 个失败，还需补充创建 {need_supplement} 个沙箱以达到目标...")
            if retry_round >= MAX_RETRY_ROUNDS:
                print(f"⚠️  警告：已达到最大重试次数 {MAX_RETRY_ROUNDS}，停止补充创建")
                break

    wall_s = time.perf_counter() - start_time
    failed_count = target_num - success_count

    print("\n【本轮结果】")
    print(f"  目标创建数: {target_num} | 实际成功: {success_count} | 失败: {max(0, failed_count)}")
    if target_num > 0:
        print(f"  成功率: {success_count / target_num * 100:.1f}%")
    print(f"  本轮耗时: {wall_s:.2f}秒")

    return {
        "target": target_num,
        "success": success_count,
        "failed": max(0, failed_count),
        "wall_ms": wall_s * 1000,
        "create_times_ms": create_times_ms,
        "errors": errors,
        "instances": instances,
        "results": results,
    }


def snapshot_one(sandbox: Any) -> TimedResult:
    """通过 SDK（e2b_sdk_compat.create_snapshot）打一次快照并计时。

    返回与 REST 路径相同的 TimedResult（data 规整为 {"snapshotID": ...}），
    便于与既有调用方和清理逻辑兼容。
    """
    from ..e2b_sdk_compat import create_snapshot as sdk_create_snapshot

    start = time.perf_counter()
    try:
        snap = sdk_create_snapshot(sandbox)
        latency_ms = (time.perf_counter() - start) * 1000
    except Exception as exc:
        return TimedResult(
            ok=False,
            latency_ms=(time.perf_counter() - start) * 1000,
            error=f"{type(exc).__name__}: {exc}"[:500],
        )
    snapshot_id = None
    for field in ("snapshot_id", "snapshotID", "snapshotId", "id"):
        value = getattr(snap, field, None) if not isinstance(snap, dict) else snap.get(field)
        if isinstance(value, str) and value:
            snapshot_id = value
            break
    return TimedResult(
        ok=True,
        latency_ms=latency_ms,
        status=201,
        data={"snapshotID": snapshot_id} if snapshot_id else None,
    )


def destroy_all(
    ctx: BenchContext,
    instances: list[Any],
    *,
    concurrency: int = DESTROY_CONCURRENCY,
) -> dict[str, Any]:
    """并发销毁一批存活沙箱实例（串行 kill 在数百沙箱时明显卡顿），统计每个沙箱的销毁耗时。

    销毁成功的 sandbox_id 从 ctx.created_ids 移除；返回销毁统计与耗时样本。
    """
    if not instances:
        return {"destroyed": 0, "failed": 0, "wall_ms": 0.0, "kill_times_ms": []}
    workers = max(1, min(concurrency, len(instances)))
    print(f"\n🧹 开始销毁所有沙箱（共 {len(instances)} 个，并发 {workers}）...")
    destroyed = 0
    failed = 0
    done = 0
    kill_times_ms: list[float] = []
    lock = threading.Lock()

    def _kill(sbx: Any) -> None:
        nonlocal destroyed, failed, done
        start = time.perf_counter()
        try:
            sbx.kill()
            elapsed_ms = (time.perf_counter() - start) * 1000
            with lock:
                destroyed += 1
                kill_times_ms.append(elapsed_ms)
            with ctx._lock:
                if sbx.sandbox_id in ctx.created_ids:
                    ctx.created_ids.remove(sbx.sandbox_id)
        except Exception as exc:
            with lock:
                failed += 1
            print(f"  [销毁失败] 沙箱ID {sbx.sandbox_id[:8]}... | 原因: {str(exc)[:50]}")
        with lock:
            done += 1
            current = done
        if current % 50 == 0 or current == len(instances):
            print(f"  已销毁 {current}/{len(instances)} 个沙箱...")

    wall_start = time.perf_counter()
    with ThreadPoolExecutor(max_workers=workers) as executor:
        list(executor.map(_kill, instances))
    wall_ms = (time.perf_counter() - wall_start) * 1000
    print("\n【销毁结果】")
    print(f"  总计销毁成功: {destroyed} | 销毁失败: {failed} | 耗时: {wall_ms / 1000:.2f}秒")
    if kill_times_ms:
        stats = latency_stats(kill_times_ms)
        print(
            f"  单沙箱销毁耗时 avg: {stats['avg_ms']:.0f}ms | P50: {stats['p50_ms']:.0f}ms | "
            f"P90: {stats['p90_ms']:.0f}ms | P95: {stats['p95_ms']:.0f}ms | max: {stats['max_ms']:.0f}ms"
        )
    return {
        "destroyed": destroyed,
        "failed": failed,
        "wall_ms": wall_ms,
        "kill_times_ms": kill_times_ms,
    }
