"""bench replay-nolifecycle / replay-provision: 无 Pause/Resume 的轨迹回放梯度密度测试。

语义来源：cubesandbox-replay-suite 的 agent/replay_agent_no_lifecycle.py、
scripts/run_replays.py 与 agent/replay_schedule.py：
- 每个任务生命周期：create 沙箱 → workdir 检查 → 按 delay_time 逐条执行命令 → delete，
  全程无 pause/resume；submit/finish/done 是结束标记，不发送到沙箱；
- 命令超时只允许 10/30/300 秒，默认超时即停止该轨迹并标记失败，
  --command-timeout-continue 时记录后继续；非零退出只记录（nonzero_exit_commands）不停；
- create 失败重试 0 次（瞬断也不重试，与 replay.py 的 _retry 不同）；delay 原速；
- --target-count 按 largest-remainder 分摊到各 workload（移植 scale_workloads，
  某 workload 被抹成 0 报错），超过轨迹数时循环复用（cycle 语义）。

有意偏差（相对对方）：保留我们的工程能力——发射间隔、控制面 QPS 限流、
内存安全闸（MemAvailable 低于阈值停止发射新任务，在途任务跑完）、dry-run；
delete 失败不中断整场，由收尾 owned-ids 逐条 GET 404 核对（cleanup_verified）兜底。
"""

from __future__ import annotations

import argparse
import hashlib
import json
import threading
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from e2b import CommandExitException, TimeoutException

from ..e2b_common import positive_int, print_json
from . import config as bench_config
from . import sdk_engine
from .client import BenchClient
from .common import (
    add_common_arguments,
    connect_sdk,
    guarded,
    new_run_id,
    read_meminfo,
)
from .replay_template import ensure_replay_task_template, find_task_template
from .scheduler import (
    OperationType,
    RunningSlotScheduler,
    SmoothRateLimiter,
    is_transient_sandbox_error,
)
from .trajectory import ReplayStep, load_trajectory, slugify, wrap_action

SUBMIT_ACTIONS = {"submit", "finish", "done"}
COMMAND_TIMEOUT_CHOICES = (10, 30, 300)
TEMPLATE_PREFIX = "swr60"
TEMPLATE_CPU = 2
TEMPLATE_MEMORY_MB = 4096
DEFAULT_CATALOG = Path(__file__).resolve().parents[2] / "catalogs" / "swerebench-arm64-60.json"
DEFAULT_TRAJECTORY_ROOT = Path("/opt/fqy/swerebench-offline-replays/replays")
MEMORY_SAMPLE_INTERVAL_SEC = 5.0
COMMAND_TOTAL_KEYS = (
    "commands",
    "succeeded_commands",
    "failed_commands",
    "timed_out_commands",
    "nonzero_exit_commands",
)


def _iso_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _is_timeout_error(exc: BaseException) -> bool:
    text = f"{type(exc).__name__}: {exc}".lower()
    return "timeout" in text or "timed out" in text or "deadline" in text


def _resolve_param(cli_value, section: dict, key: str, default):
    return cli_value if cli_value is not None else section.get(key, default)


# --- catalog 装载与 largest-remainder 分摊 ---


@dataclass(frozen=True)
class CatalogEntry:
    """catalog 中一个镜像 workload：一条轨迹 + swr60 模板 + 执行 workdir。"""

    name: str
    template: str
    image: str
    workdir: str
    instance_id: str
    replay_file: str
    trajectory_path: Path | None
    # 可选：该 workload 每条命令额外注入的环境变量（如 vitest 需 CI=true 以跳过 watch 模式）
    env: tuple[tuple[str, str], ...] = ()


def _parse_env(value: Any, field: str) -> tuple[tuple[str, str], ...]:
    if value is None:
        return ()
    if not isinstance(value, dict) or not all(
        isinstance(k, str) and k and isinstance(v, str) for k, v in value.items()
    ):
        raise ValueError(f"{field}.env 必须是 {{字符串: 字符串}} 的 JSON object")
    return tuple(sorted(value.items()))


def load_catalog(
    catalog_path: str | Path,
    trajectory_root: str | Path | None = None,
) -> list[CatalogEntry]:
    """加载镜像目录 JSON；trajectory_root 非空时校验每个 replay_file 存在。"""
    path = Path(catalog_path).expanduser().resolve()
    if not path.is_file():
        raise FileNotFoundError(f"catalog 不存在：{path}")
    raw: Any = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(raw, list) or not raw:
        raise ValueError(f"catalog 必须是非空 JSON 数组：{path}")
    root = Path(trajectory_root).expanduser().resolve() if trajectory_root else None
    entries: list[CatalogEntry] = []
    names: set[str] = set()
    for index, item in enumerate(raw):
        field = f"catalog[{index}]"
        if not isinstance(item, dict):
            raise ValueError(f"{field} 必须是 JSON object")
        for key in ("name", "image", "workdir", "instance_id", "replay_file"):
            if not isinstance(item.get(key), str) or not item[key].strip():
                raise ValueError(f"{field}.{key} 必须是非空字符串")
        name = item["name"].strip()
        slug = slugify(name)
        if slug in names:
            raise ValueError(f"catalog 名称 slug 后重复：{name}")
        names.add(slug)
        trajectory_path: Path | None = None
        if root is not None:
            trajectory_path = root / item["replay_file"].strip()
            if not trajectory_path.is_file():
                raise FileNotFoundError(f"轨迹文件不存在：{trajectory_path}（{field}）")
        entries.append(
            CatalogEntry(
                name=name,
                template=f"{TEMPLATE_PREFIX}-{slug}",
                image=item["image"].strip(),
                workdir=item["workdir"].strip(),
                instance_id=item["instance_id"].strip(),
                replay_file=item["replay_file"].strip(),
                trajectory_path=trajectory_path,
                env=_parse_env(item.get("env"), field),
            )
        )
    return entries


def apportion_counts(weights: list[int], total: int) -> list[int]:
    """largest-remainder 分摊（移植 replay_schedule.scale_workloads）。

    按权重把 total 分摊到各 workload；某 workload 被抹成 0 时报错，
    不静默丢弃镜像。
    """
    if total < 1:
        raise ValueError("total 必须是正整数")
    denominator = sum(weights)
    divisions = [divmod(total * weight, denominator) for weight in weights]
    counts = [quotient for quotient, _ in divisions]
    order = sorted(range(len(weights)), key=lambda i: (-divisions[i][1], i))
    for index in order[: total - sum(counts)]:
        counts[index] += 1
    if any(count == 0 for count in counts):
        raise ValueError(
            f"分摊后存在 0 实例的 workload（total={total}，workloads={len(weights)}）；"
            "增大 --target-count 或缩减 catalog"
        )
    return counts


@dataclass(frozen=True)
class NoLifecycleTask:
    """一次无生命周期回放任务（steps 已剔除 submit/finish/done 终止标记及之后）。"""

    workload: str
    template: str
    workdir: str
    trajectory: str
    trajectory_path: Path
    steps: tuple[ReplayStep, ...]
    has_terminal: bool
    cycle: int  # 该 workload 内第几次复用（1 起）
    env: tuple[tuple[str, str], ...] = ()  # 来自 catalog 的额外命令环境变量


def _entry_steps(entry: CatalogEntry) -> tuple[tuple[ReplayStep, ...], bool]:
    """加载轨迹并截断终止标记：submit/finish/done 不发送，只执行其前的步骤。"""
    assert entry.trajectory_path is not None
    steps = load_trajectory(entry.trajectory_path)
    terminal = next(
        (pos for pos, step in enumerate(steps) if step.action.lower() in SUBMIT_ACTIONS),
        None,
    )
    if terminal is None:
        return tuple(steps), False
    return tuple(steps[:terminal]), True


def build_tier_tasks(
    entries: list[CatalogEntry], counts: list[int]
) -> list[NoLifecycleTask]:
    """平滑加权轮询交错各 workload（对齐 build_mixed_schedule），单负载内循环复用。"""
    if len(entries) != len(counts):
        raise ValueError("entries 与 counts 长度不一致")
    remaining = list(counts)
    current = [0] * len(entries)
    emitted = [0] * len(entries)
    total = sum(counts)
    tasks: list[NoLifecycleTask] = []
    steps_cache: dict[int, tuple[tuple[ReplayStep, ...], bool]] = {}
    while len(tasks) < total:
        active = [i for i in range(len(entries)) if remaining[i]]
        active_weight = sum(counts[i] for i in active)
        for i in active:
            current[i] += counts[i]
        selected = max(active, key=lambda i: current[i])
        current[selected] -= active_weight
        if selected not in steps_cache:
            steps_cache[selected] = _entry_steps(entries[selected])
        steps, has_terminal = steps_cache[selected]
        emitted[selected] += 1
        remaining[selected] -= 1
        entry = entries[selected]
        tasks.append(
            NoLifecycleTask(
                workload=entry.name,
                template=entry.template,
                workdir=entry.workdir,
                trajectory=entry.replay_file,
                trajectory_path=entry.trajectory_path,
                steps=steps,
                has_terminal=has_terminal,
                cycle=emitted[selected],
                env=entry.env,
            )
        )
    return tasks


# --- 模板就位（replay-provision 与执行前检查共用） ---


def ensure_templates(
    client: BenchClient,
    entries: list[CatalogEntry],
    *,
    auto_provision: bool,
) -> dict[str, str]:
    """检查每个 workload 模板存在、ready 且规格为 2U4G；返回 name→templateID。

    缺模板默认报错并提示先跑 replay-provision；auto_provision 时自动串行构建补齐。
    """
    mapping: dict[str, str] = {}
    missing: list[CatalogEntry] = []
    for entry in entries:
        found = find_task_template(
            client, entry.template, cpu=TEMPLATE_CPU, memory_mb=TEMPLATE_MEMORY_MB
        )
        if found:
            mapping[entry.template] = str(found["templateID"])
        else:
            missing.append(entry)
    if not missing:
        print(f"[replay-nolifecycle] 模板检查通过：{len(mapping)} 个 swr60-* 模板 ready（2U4G）")
        return mapping
    if not auto_provision:
        names = "、".join(entry.template for entry in missing[:10])
        raise ValueError(
            f"缺少 {len(missing)} 个可用模板（ready 且 2U4G）：{names}"
            f"{'…' if len(missing) > 10 else ''}；"
            "先运行 bench replay-provision 构建补齐，或加 --auto-provision 自动构建"
        )
    print(f"[replay-nolifecycle] {len(missing)} 个模板缺失，自动串行构建补齐…")
    for entry in missing:
        template_id, _name, _source = ensure_replay_task_template(
            client,
            name=entry.template,
            image=entry.image,
            cpu=TEMPLATE_CPU,
            memory_mb=TEMPLATE_MEMORY_MB,
        )
        mapping[entry.template] = template_id
    return mapping


# --- 单档位执行核心（replay-nolifecycle CLI 与 replay-matrix 共用） ---


def run_tier(
    *,
    client: BenchClient,
    tasks: list[NoLifecycleTask],
    concurrency: int,
    command_timeout_sec: int,
    command_timeout_continue: bool,
    launch_interval_sec: float,
    control_plane_qps: float,
    mem_threshold_pct: float,
    sample_interval_sec: float,
    sandbox_timeout: int,
    output_dir: Path,
    run_id: str,
) -> dict[str, Any]:
    """执行一档无生命周期回放，写 replay-result.json / owned-ids.json，返回汇总。"""
    output_dir.mkdir(parents=True, exist_ok=True)
    target_count = len(tasks)
    scheduler = RunningSlotScheduler(concurrency)
    limiter = SmoothRateLimiter(qps=control_plane_qps, inflight_cap=concurrency)
    sdk_engine._ensure_shared_api_client()

    lock = threading.Lock()
    abort_event = threading.Event()
    sampler_stop = threading.Event()
    records: list[dict[str, Any]] = []
    owned_records: list[dict[str, Any]] = []
    step_details: list[dict[str, Any]] = []
    command_ms_samples: list[float] = []
    running_samples: list[int] = []
    memory_curve: list[dict[str, Any]] = []
    notes: list[str] = []

    baseline = read_meminfo()
    total_kb = baseline.get("MemTotal", 0)
    threshold_kb = total_kb * mem_threshold_pct / 100
    started_monotonic = time.monotonic()
    started_at = _iso_now()

    def _note(message: str) -> None:
        with lock:
            notes.append(message)
        print(f"[replay-nolifecycle] {message}", flush=True)

    def _memory_sampler() -> None:
        while not sampler_stop.wait(MEMORY_SAMPLE_INTERVAL_SEC):
            available_kb = read_meminfo().get("MemAvailable", 0)
            with lock:
                memory_curve.append(
                    {
                        "elapsed_s": round(time.monotonic() - started_monotonic, 1),
                        "mem_available_mb": round(available_kb / 1024),
                    }
                )
            if available_kb < threshold_kb and not abort_event.is_set():
                abort_event.set()
                _note(
                    f"内存安全闸触发：MemAvailable {round(available_kb / 1024)} MiB "
                    f"低于阈值 {round(threshold_kb / 1024)} MiB（总内存 {mem_threshold_pct}%），"
                    "停止发射新任务，在途任务跑完"
                )

    def _api_sampler() -> None:
        while not sampler_stop.is_set():
            try:
                count = len(client.list_running_items())
                with lock:
                    running_samples.append(count)
            except Exception as exc:
                with lock:
                    notes.append(f"RUNNING 采样失败：{type(exc).__name__}: {exc}")
            sampler_stop.wait(sample_interval_sec)

    def _kill(sandbox_id: str) -> bool:
        """delete 不重试；失败记入台账，由收尾 GET 404 核对兜底。"""
        try:
            with limiter.slot(OperationType.CLEANUP):
                response = client.kill(sandbox_id)
            return response.status in (200, 204, 404)
        except Exception as exc:
            _note(f"沙箱 {sandbox_id} delete 失败：{str(exc)[:120]}（收尾核对兜底）")
            return False

    def _check_workdir(sandbox, workdir: str) -> None:
        import shlex

        command = f"bash -lc {shlex.quote(f'test -d {shlex.quote(workdir)}')}"
        try:
            completed = sandbox.commands.run(command, timeout=30, user="root")
            exit_code = completed.exit_code
        except CommandExitException as exc:
            exit_code = exc.exit_code
        if exit_code != 0:
            raise RuntimeError(f"workdir 不可达：{workdir}（exit={exit_code}）")

    def _run_command(sandbox, task: NoLifecycleTask, step: ReplayStep, step_index: int, task_index: int) -> dict[str, Any]:
        detail: dict[str, Any] = {
            "task_index": task_index,
            "workload": task.workload,
            "trajectory": task.trajectory,
            "cycle": task.cycle,
            "step_index": step_index,
            "action": step.action,
            "delay": step.delay_time_sec,
            "duration_ms": None,
            "exit_code": None,
            "timed_out": False,
            "infrastructure_error": False,
            "error": None,
        }
        started = time.perf_counter()
        try:
            with limiter.slot(OperationType.COMMAND):
                completed = sandbox.commands.run(
                    wrap_action(step.action, task.workdir),
                    timeout=command_timeout_sec,
                    user="root",
                    envs=dict(task.env) or None,
                )
            detail["exit_code"] = completed.exit_code
        except CommandExitException as exc:
            # 非零退出是负载内容本身：记录后继续（对齐 stop_on_error=False）
            detail["exit_code"] = exc.exit_code
            detail["error"] = (exc.stderr or "")[-300:]
        except Exception as exc:
            if isinstance(exc, TimeoutException) or _is_timeout_error(exc):
                detail["timed_out"] = True
                detail["error"] = f"{type(exc).__name__}: {exc}"[:300]
            else:
                detail["infrastructure_error"] = is_transient_sandbox_error(exc)
                detail["error"] = f"{type(exc).__name__}: {exc}"[:300]
        detail["duration_ms"] = round((time.perf_counter() - started) * 1000, 1)
        return detail

    def _execute_task(index: int) -> None:
        task = tasks[index]
        create_key = uuid.uuid4().hex
        record: dict[str, Any] = {
            "index": index,
            "workload": task.workload,
            "template": task.template,
            "trajectory": task.trajectory,
            "cycle": task.cycle,
            "env": dict(task.env),
            "sandbox_id": None,
            "create_key": create_key,
            "ok": False,
            "submitted": False,
            "replay_completed": False,
            "stopped_early": False,
            "commands": 0,
            "succeeded_commands": 0,
            "failed_commands": 0,
            "timed_out_commands": 0,
            "nonzero_exit_commands": 0,
            "create_ms": None,
            "error": None,
            "started_at": _iso_now(),
            "completed_at": None,
        }
        owned: dict[str, Any] = {
            "create_key": create_key,
            "sandbox_id": None,
            "deleted": False,
            "template": task.template,
            "workload": task.workload,
        }
        with lock:
            owned_records.append(owned)
        # lease 持有整个任务生命周期（create→命令→delete），对齐对方
        # 「无 pause 变体在 create 前预约 RUNNING 名额并持有到清理结束」
        lease = scheduler.acquire(f"nolifecycle-{index}")
        sandbox_id: str | None = None
        task_started = time.monotonic()
        try:
            # create 失败重试 0 次（对齐对方 create_retries=0，瞬断也不重试）
            with limiter.slot(OperationType.CREATE):
                outcome = client.create_timed(
                    task.template,
                    timeout=sandbox_timeout,
                    metadata={
                        "bench_run_id": run_id,
                        "replay_create_key": create_key,
                        "sandbox_lifecycle_enabled": "false",
                    },
                )
            record["create_ms"] = round(outcome.latency_ms, 1)
            if not outcome.ok or not outcome.sandbox_id:
                raise RuntimeError(f"create 失败：{outcome.error or '响应缺少 sandbox id'}")
            sandbox_id = outcome.sandbox_id
            owned["sandbox_id"] = sandbox_id
            record["sandbox_id"] = sandbox_id
            sandbox = connect_sdk(sandbox_id, timeout=sandbox_timeout)
            _check_workdir(sandbox, task.workdir)

            for step_index, step in enumerate(task.steps):
                if step.delay_time_sec > 0:
                    time.sleep(step.delay_time_sec)
                detail = _run_command(sandbox, task, step, step_index, index)
                with lock:
                    step_details.append(detail)
                    command_ms_samples.append(detail["duration_ms"])
                record["commands"] += 1
                if detail["timed_out"]:
                    record["timed_out_commands"] += 1
                    record["failed_commands"] += 1
                    if not command_timeout_continue:
                        record["stopped_early"] = True
                        record["error"] = (
                            f"第 {step_index + 1} 步命令超时（{command_timeout_sec}s），"
                            "未开 --command-timeout-continue，停止回放"
                        )
                        break
                    continue
                if detail["exit_code"] == 0:
                    record["succeeded_commands"] += 1
                    continue
                record["failed_commands"] += 1
                if detail["exit_code"] is not None:
                    # 非零退出：记录 nonzero_exit_commands，不停止轨迹
                    record["nonzero_exit_commands"] += 1
                    continue
                # 非超时异常（基础设施或其他错误）：停止该轨迹
                record["stopped_early"] = True
                record["error"] = (
                    f"第 {step_index + 1} 步命令异常：{detail['error']}"
                )
                break

            record["replay_completed"] = not record["stopped_early"]
            record["submitted"] = task.has_terminal and record["replay_completed"]
            record["ok"] = record["replay_completed"]
        except Exception as exc:
            record["error"] = f"{type(exc).__name__}: {exc}"[:300]
        finally:
            if sandbox_id is not None:
                owned["deleted"] = _kill(sandbox_id)
            lease.release()
            record["elapsed_sec"] = round(time.monotonic() - task_started, 3)
            record["completed_at"] = _iso_now()
            with lock:
                records.append(record)

    mem_sampler = threading.Thread(
        target=_memory_sampler, name="bench-nolifecycle-meminfo", daemon=True
    )
    api_sampler = threading.Thread(
        target=_api_sampler, name="bench-nolifecycle-running", daemon=True
    )
    mem_sampler.start()
    api_sampler.start()
    launched = 0
    try:
        with ThreadPoolExecutor(max_workers=min(concurrency, target_count)) as pool:
            futures = []
            for index in range(target_count):
                if abort_event.is_set():
                    break
                futures.append(pool.submit(_execute_task, index))
                launched += 1
                if launch_interval_sec > 0 and index + 1 < target_count:
                    # 发射间隔分段睡眠，内存闸触发时能及时停发
                    deadline = time.monotonic() + launch_interval_sec
                    while not abort_event.is_set() and time.monotonic() < deadline:
                        time.sleep(min(0.1, deadline - time.monotonic()))
            done = 0
            for future in futures:
                future.result()
                done += 1
                if done % 25 == 0 or done == len(futures):
                    print(f"[replay-nolifecycle] 进度 {done}/{len(futures)}", flush=True)
    finally:
        sampler_stop.set()
        mem_sampler.join(timeout=10)
        api_sampler.join(timeout=10)
        scheduler.close()
    replay_wall_sec = time.monotonic() - started_monotonic

    # 收尾：delete 失败的再补一次，然后逐条 GET 核对 404（对齐对方
    # collect() 的 cleanup_verified = ledger_deleted && api_absent 语义；
    # GET 对已删沙箱可能持续 500，回退 /sandboxes 全量列表核对）
    for owned in owned_records:
        if owned.get("sandbox_id") and not owned.get("deleted"):
            owned["deleted"] = _kill(owned["sandbox_id"])
    live_ids: set[str] | None = None
    for owned in owned_records:
        sandbox_id = owned.get("sandbox_id")
        if not sandbox_id:
            owned["api_absent"] = None
            continue
        try:
            owned["api_absent"] = client.get_sandbox(sandbox_id).status == 404
        except Exception:
            if live_ids is None:
                try:
                    live_ids = {
                        str(item.get("sandboxID") or item.get("sandboxId"))
                        for item in client.list_sandbox_items()
                    }
                except Exception:
                    live_ids = set()
            owned["api_absent"] = sandbox_id not in live_ids
            owned["api_absent_fallback"] = True
    cleanup_verified = all(
        owned["deleted"] and owned.get("api_absent")
        for owned in owned_records
        if owned.get("sandbox_id")
    )

    succeeded_tasks = sum(1 for record in records if record["ok"])
    command_totals = {
        key: sum(record[key] for record in records) for key in COMMAND_TOTAL_KEYS
    }
    if abort_event.is_set():
        status = "aborted"
        error = "内存安全闸触发，提前停止发射新任务（已记录中止前数据）"
    elif succeeded_tasks == 0:
        status = "failed"
        error = "没有任务回放成功"
    else:
        status = "ok"
        error = None
    result: dict[str, Any] = {
        "schema_version": 2,
        "version": "0.4.0-no-lifecycle",
        "tool": "bench replay-nolifecycle",
        "sandbox_lifecycle_enabled": False,
        "create_retries": 0,
        "run_id": run_id,
        "status": status,
        "error": error,
        "notes": notes,
        "started_at": started_at,
        "completed_at": _iso_now(),
        "replay_wall_sec": round(replay_wall_sec, 3),
        "concurrency": concurrency,
        "target_count": target_count,
        "launched": launched,
        "command_timeout_sec": command_timeout_sec,
        "command_timeout_continue": command_timeout_continue,
        "launch_interval_sec": launch_interval_sec,
        "control_plane_qps": control_plane_qps,
        "mem_threshold_pct": mem_threshold_pct,
        "sample_interval_sec": sample_interval_sec,
        "tasks": len(records),
        "succeeded_tasks": succeeded_tasks,
        "failed_tasks": len(records) - succeeded_tasks,
        "submitted": sum(1 for record in records if record["submitted"]),
        "replay_completed": sum(1 for record in records if record["replay_completed"]),
        "succeeded": succeeded_tasks == target_count and not abort_event.is_set(),
        "command_totals": command_totals,
        **command_totals,
        "running_slots": scheduler.snapshot(),
        "control_plane": limiter.snapshot(),
        "peak_running": max(running_samples, default=None),
        "running_observation": {
            "samples": len(running_samples),
            "peak_running": max(running_samples, default=None),
            "average_running": (
                round(sum(running_samples) / len(running_samples), 1)
                if running_samples
                else None
            ),
        },
        "latency": {"command": sdk_engine.latency_stats(command_ms_samples)},
        "baseline": {
            "mem_total_mb": round(total_kb / 1024),
            "mem_available_mb": round(baseline.get("MemAvailable", 0) / 1024),
            "threshold_mb": round(threshold_kb / 1024),
        },
        "memory_curve": memory_curve,
        "cleanup_verified": cleanup_verified,
        "steps": step_details,
        "results": records,
    }
    (output_dir / "replay-result.json").write_text(
        json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8"
    )
    (output_dir / "owned-ids.json").write_text(
        json.dumps(
            {
                "run_id": run_id,
                "verified_at": _iso_now(),
                "cleanup_verified": cleanup_verified,
                "total": len(owned_records),
                "created": sum(1 for owned in owned_records if owned.get("sandbox_id")),
                "deleted": sum(1 for owned in owned_records if owned.get("deleted")),
                "absent": sum(1 for owned in owned_records if owned.get("api_absent")),
                "records": owned_records,
            },
            ensure_ascii=False,
            indent=2,
        ),
        encoding="utf-8",
    )
    return result


def print_tier_summary(result: dict[str, Any], output_dir: Path) -> None:
    """终端汇总表（bench 风格）。"""
    totals = result["command_totals"]
    slots = result["running_slots"]
    command = result["latency"]["command"]
    print("\n【无 Pause/Resume 回放结果】")
    print(
        f"  状态: {result['status']} | 任务: {result['succeeded_tasks']}/{result['target_count']} 成功"
        f" | 到达终止标记: {result['submitted']} | 清理核验: {result['cleanup_verified']}"
    )
    print(
        f"  命令: 共 {totals['commands']} 条，成功 {totals['succeeded_commands']}，"
        f"失败 {totals['failed_commands']}（超时 {totals['timed_out_commands']}，"
        f"非零退出 {totals['nonzero_exit_commands']}）"
    )
    print(
        f"  RUNNING 名额: maximum={slots['maximum']} peak_active={slots['peak_active']} "
        f"平均排队={slots['average_queue_wait_sec']:.2f}s | API 峰值 RUNNING: {result['peak_running']}"
    )
    if command.get("p95_ms") is not None:
        print(
            f"  命令延迟: avg={command['avg_ms']:.0f}ms p50={command['p50_ms']:.0f}ms "
            f"p95={command['p95_ms']:.0f}ms max={command['max_ms']:.0f}ms"
        )
    print(f"  回放 wall: {result['replay_wall_sec']:.1f}s | 输出: {output_dir}")
    if result.get("error"):
        print(f"  错误: {result['error']}")


# --- replay-nolifecycle 子命令 ---


def add_runtime_arguments(parser: argparse.ArgumentParser) -> None:
    """单档执行器与矩阵驱动共享的运行时参数。"""
    parser.add_argument("--catalog", type=Path, help=f"镜像目录 JSON（默认 {DEFAULT_CATALOG}）")
    parser.add_argument(
        "--trajectory-root", type=Path, help=f"轨迹根目录（默认 {DEFAULT_TRAJECTORY_ROOT}）"
    )
    parser.add_argument(
        "--command-timeout-sec",
        type=int,
        choices=COMMAND_TIMEOUT_CHOICES,
        help="单条命令超时秒（只允许 10/30/300，默认 300）",
    )
    parser.add_argument(
        "--command-timeout-continue",
        action="store_true",
        help="命令超时记录后继续回放（默认超时即停止该轨迹并标记失败）",
    )
    parser.add_argument("--launch-interval-sec", type=float, help="相邻任务发射最小间隔秒（默认 0.3）")
    parser.add_argument("--control-plane-qps", type=float, help="全局控制面 QPS（默认 100）")
    parser.add_argument(
        "--mem-threshold-pct",
        type=float,
        help="内存安全闸：MemAvailable 低于总内存该百分比时停止发射新任务（默认取 global.mem_threshold_pct）",
    )
    parser.add_argument("--sample-interval", type=float, help="API RUNNING 采样间隔秒（默认 1.0）")
    parser.add_argument(
        "--auto-provision",
        action="store_true",
        help="模板缺失时自动串行构建补齐（默认报错并提示先跑 replay-provision）",
    )
    parser.add_argument("--dry-run", action="store_true", help="只校验 catalog/轨迹并打印调度预览，不联网")
    add_common_arguments(parser)
    parser.add_argument("--sandbox-timeout", type=positive_int, help="沙箱生命周期秒数")


def resolve_runtime(args: argparse.Namespace, cfg: dict[str, Any]) -> dict[str, Any]:
    """命令行 > bench.toml [replay_nolifecycle] > 内置默认。"""
    section = cfg.get("replay_nolifecycle", {})
    command_timeout_sec = int(_resolve_param(args.command_timeout_sec, section, "command_timeout_sec", 300))
    if command_timeout_sec not in COMMAND_TIMEOUT_CHOICES:
        raise ValueError("command-timeout-sec 只允许 10/30/300")
    return {
        "catalog": args.catalog or Path(section.get("catalog", str(DEFAULT_CATALOG))),
        "trajectory_root": args.trajectory_root
        or Path(section.get("trajectory_root", str(DEFAULT_TRAJECTORY_ROOT))),
        "command_timeout_sec": command_timeout_sec,
        "command_timeout_continue": bool(args.command_timeout_continue),
        "launch_interval_sec": float(_resolve_param(args.launch_interval_sec, section, "launch_interval_sec", 0.3)),
        "control_plane_qps": float(_resolve_param(args.control_plane_qps, section, "control_plane_qps", 100)),
        "mem_threshold_pct": (
            args.mem_threshold_pct
            if args.mem_threshold_pct is not None
            else float(bench_config.global_param(cfg, "mem_threshold_pct"))
        ),
        "sample_interval_sec": float(_resolve_param(args.sample_interval, section, "sample_interval", 1.0)),
        "sandbox_timeout": args.sandbox_timeout
        or max(3600, int(bench_config.global_param(cfg, "sandbox_timeout"))),
    }


def register(subparsers) -> None:
    parser = subparsers.add_parser(
        "replay-nolifecycle",
        help="无 Pause/Resume 轨迹回放：create→workdir 检查→按 delay_time 逐条命令→delete（swr60 镜像目录）",
    )
    add_runtime_arguments(parser)
    parser.add_argument(
        "--target-count",
        type=positive_int,
        help="总任务数（默认 = catalog 镜像数，即每镜像 1 次；按 largest-remainder 分摊，超出循环复用）",
    )
    parser.add_argument(
        "-c",
        "--concurrency",
        type=positive_int,
        help="worker 数 = 同时存活沙箱数上限（默认 60；每 worker 同一时刻只跑一个任务）",
    )
    parser.add_argument(
        "-o",
        "--output-dir",
        type=Path,
        help="输出目录（默认 test-results/<时间戳>-bench/），内含 replay-result.json 与 owned-ids.json",
    )
    parser.set_defaults(handler=guarded(execute))
    _register_provision(subparsers)


def _dry_run_preview(
    runtime: dict[str, Any],
    entries: list[CatalogEntry],
    tasks: list[NoLifecycleTask],
    counts: list[int],
    concurrency: int,
) -> None:
    step_counts = [len(task.steps) for task in tasks]
    delay_total = sum(step.delay_time_sec for task in tasks for step in task.steps)
    workload_counts = {entry.name: count for entry, count in zip(entries, counts)}
    print(
        f"[replay-nolifecycle dry-run] catalog={runtime['catalog']}（{len(entries)} 个镜像）；"
        f"目标 {len(tasks)} 任务，并发 {concurrency}，命令超时 {runtime['command_timeout_sec']}s"
    )
    print_json(
        {
            "dry_run": True,
            "sandbox_lifecycle_enabled": False,
            "catalog": str(runtime["catalog"]),
            "trajectory_root": str(runtime["trajectory_root"]),
            "target_count": len(tasks),
            "concurrency": concurrency,
            "apportion": {
                "method": "largest-remainder",
                "sum": sum(counts),
                "min": min(counts),
                "max": max(counts),
                "workload_counts": workload_counts,
            },
            "steps_per_task": {
                "min": min(step_counts),
                "max": max(step_counts),
                "avg": round(sum(step_counts) / len(step_counts), 1),
            },
            "delay_time_total_sec": round(delay_total, 1),
            "terminal_marker_tasks": sum(1 for task in tasks if task.has_terminal),
            "templates": [entry.template for entry in entries],
            "template_spec": f"{TEMPLATE_CPU}C/{TEMPLATE_MEMORY_MB}MiB",
            "command_timeout_sec": runtime["command_timeout_sec"],
            "command_timeout_continue": runtime["command_timeout_continue"],
            "launch_interval_sec": runtime["launch_interval_sec"],
            "control_plane_qps": runtime["control_plane_qps"],
            "mem_threshold_pct": runtime["mem_threshold_pct"],
            "sample_interval_sec": runtime["sample_interval_sec"],
        }
    )


def execute(args: argparse.Namespace) -> int:
    cfg = bench_config.load(args.config)
    section = cfg.get("replay_nolifecycle", {})
    runtime = resolve_runtime(args, cfg)
    entries = load_catalog(runtime["catalog"], runtime["trajectory_root"])
    target_count = int(_resolve_param(args.target_count, section, "target_count", len(entries)))
    concurrency = int(_resolve_param(args.concurrency, section, "concurrency", len(entries)))
    counts = apportion_counts([1] * len(entries), target_count)
    tasks = build_tier_tasks(entries, counts)

    if args.dry_run:
        _dry_run_preview(runtime, entries, tasks, counts, concurrency)
        return 0

    client = BenchClient(timeout=600)
    ensure_templates(client, entries, auto_provision=args.auto_provision)
    run_id = new_run_id()
    output_dir = (
        args.output_dir or bench_config.resolve_result_root(cfg) / f"{run_id}-bench"
    ).expanduser().resolve()
    result = run_tier(
        client=client,
        tasks=tasks,
        concurrency=concurrency,
        command_timeout_sec=runtime["command_timeout_sec"],
        command_timeout_continue=runtime["command_timeout_continue"],
        launch_interval_sec=runtime["launch_interval_sec"],
        control_plane_qps=runtime["control_plane_qps"],
        mem_threshold_pct=runtime["mem_threshold_pct"],
        sample_interval_sec=runtime["sample_interval_sec"],
        sandbox_timeout=runtime["sandbox_timeout"],
        output_dir=output_dir,
        run_id=run_id,
    )
    print_tier_summary(result, output_dir)
    return 0 if result["succeeded"] and result["cleanup_verified"] else 1


# --- replay-provision 子命令 ---


def _register_provision(subparsers) -> None:
    parser = subparsers.add_parser(
        "replay-provision",
        help="按 catalog 检查/构建 swr60-* 模板（2U4G），输出 templates.json（name→templateID）",
    )
    parser.add_argument("--catalog", type=Path, help=f"镜像目录 JSON（默认 {DEFAULT_CATALOG}）")
    parser.add_argument("--jobs", type=positive_int, help="并行构建数（默认 4）")
    parser.add_argument("--dry-run", action="store_true", help="只打印模板计划，不联网")
    parser.add_argument(
        "-o",
        "--output",
        type=Path,
        help="templates.json 输出路径（默认 <catalog 同名>.templates.json）",
    )
    add_common_arguments(parser)
    parser.set_defaults(handler=guarded(execute_provision))


def execute_provision(args: argparse.Namespace) -> int:
    cfg = bench_config.load(args.config)
    section = cfg.get("replay_provision", {})
    catalog = args.catalog or Path(section.get("catalog", str(DEFAULT_CATALOG)))
    entries = load_catalog(catalog)
    jobs = int(_resolve_param(args.jobs, section, "jobs", 4))
    output = (
        args.output
        or Path(catalog).expanduser().resolve().parent
        / (Path(catalog).stem + ".templates.json")
    )

    if args.dry_run:
        print(
            f"[replay-provision dry-run] catalog={catalog}，共 {len(entries)} 个模板"
            f"（{TEMPLATE_CPU}C/{TEMPLATE_MEMORY_MB}MiB，并行 {jobs}）；dry-run 不联网，状态待真实运行时检查："
        )
        for entry in entries:
            print(f"  {entry.template}  ←  {entry.image}  [状态: 未检查]")
        print(f"输出将写入：{output}")
        return 0

    client = BenchClient(timeout=600)
    mapping: dict[str, str] = {}
    missing: list[CatalogEntry] = []
    for entry in entries:
        found = find_task_template(
            client, entry.template, cpu=TEMPLATE_CPU, memory_mb=TEMPLATE_MEMORY_MB
        )
        if found:
            mapping[entry.template] = str(found["templateID"])
        else:
            missing.append(entry)
    print(
        f"[replay-provision] 已存在 {len(mapping)} 个，待构建 {len(missing)} 个（并行 {jobs}）",
        flush=True,
    )

    failures: dict[str, str] = {}

    def _build(entry: CatalogEntry) -> None:
        try:
            template_id, _name, _source = ensure_replay_task_template(
                client,
                name=entry.template,
                image=entry.image,
                cpu=TEMPLATE_CPU,
                memory_mb=TEMPLATE_MEMORY_MB,
            )
            mapping[entry.template] = template_id
            print(f"[replay-provision] 完成 {entry.template} → {template_id}", flush=True)
        except Exception as exc:
            failures[entry.template] = f"{type(exc).__name__}: {exc}"[:300]
            print(f"[replay-provision] 失败 {entry.template}：{failures[entry.template]}", flush=True)

    if missing:
        with ThreadPoolExecutor(max_workers=min(jobs, len(missing))) as pool:
            list(pool.map(_build, missing))

    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(
        json.dumps(mapping, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    print_json(
        {
            "catalog": str(Path(catalog).expanduser().resolve()),
            "templates": len(mapping),
            "built": len(missing) - len(failures),
            "failed": failures,
            "output": str(output),
        }
    )
    return 0 if not failures and len(mapping) == len(entries) else 1
