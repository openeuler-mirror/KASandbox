"""Shared helpers for the benchmark subcommands."""

from __future__ import annotations

import argparse
import glob
import os
import platform
import re
import subprocess
import sys
import threading
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

from ..e2b_sdk_compat import connect_sandbox
from ..e2e_sdk_common import sdk_options
from .client import BenchClient, TimedResult
from .stats import percentile

TEST_RESULTS_ROOT = Path(__file__).resolve().parent.parent.parent / "test-results"
BENCH_METADATA_KEY = "bench_run_id"


@dataclass
class BenchContext:
    client: BenchClient
    template: str
    run_id: str
    result_dir: Path
    sandbox_timeout: int = 600
    template_source: str = "用户指定"
    netns_growth_threshold: int = 100
    host_net_baseline: dict[str, int] | None = None
    created_ids: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)
    _lock: threading.Lock = field(default_factory=threading.Lock, repr=False)

    @property
    def metadata(self) -> dict[str, str]:
        return {BENCH_METADATA_KEY: self.run_id}

    def track(self, sandbox_id: str | None) -> None:
        if sandbox_id:
            with self._lock:
                self.created_ids.append(sandbox_id)

    def note(self, message: str) -> None:
        with self._lock:
            self.notes.append(message)


def new_run_id() -> str:
    return datetime.now().strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:6]


def add_template_argument(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "-t", "--template",
        help="模板 ID；优先于 bench.toml 的 [global].template 与环境变量 BENCH_TEMPLATE_ID",
    )


def add_common_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--config", type=Path, help="bench 配置文件路径（默认 e2b-scripts/bench.toml）")
    parser.add_argument("--force", action="store_true", help="绕过 bench/test-e2e 互斥锁（删除旧锁继续）")


def guarded(handler):
    """Wrap a bench handler with the test-e2e/bench mutual-exclusion lock."""
    def wrapped(args: argparse.Namespace) -> int:
        if getattr(args, "print_config", False):
            return handler(args)
        from ..run_lock import RunLockError, run_lock

        try:
            with run_lock("bench", run_id=new_run_id(), force=getattr(args, "force", False)):
                return handler(args)
        except RunLockError as exc:
            print(str(exc), file=sys.stderr)
            return 1

    return wrapped


def build_context(
    *,
    template: str | None = None,
    run_id: str | None = None,
    result_root: Path | None = None,
    sandbox_timeout: int = 600,
    client_timeout: float = 600,
    netns_growth_threshold: int = 100,
) -> BenchContext:
    client = BenchClient(timeout=client_timeout)
    template_source = "用户指定"
    if not template:
        from .bench_template import ensure_bench_template

        template, _name, template_source = ensure_bench_template(client)
    run_id = run_id or new_run_id()
    root = (result_root or TEST_RESULTS_ROOT).expanduser().resolve()
    result_dir = root / f"{run_id}-bench"
    result_dir.mkdir(parents=True, exist_ok=True)
    ctx = BenchContext(
        client=client,
        template=template,
        run_id=run_id,
        result_dir=result_dir,
        sandbox_timeout=sandbox_timeout,
        template_source=template_source,
        netns_growth_threshold=netns_growth_threshold,
    )
    mark_host_net_baseline(ctx)
    return ctx


def drain_notes(ctx: BenchContext, result: dict[str, Any]) -> None:
    """把档间清理等动作产生的 notes 并入结果（merge 之后调用）。"""
    with ctx._lock:
        pending = list(ctx.notes)
        ctx.notes.clear()
    existing = result.setdefault("notes", [])
    for note in pending:
        if note not in existing:
            existing.append(note)


def merge_tier_results(name: str, tier_results: list[dict[str, Any]]) -> dict[str, Any]:
    """Merge several single-tier run() outputs into one multi-tier bench result."""
    merged = dict(tier_results[0])
    merged["tiers"] = []
    notes: list[str] = []
    errors: list[str] = []
    status = "ok"
    for item in tier_results:
        merged["tiers"].extend(item.get("tiers", []))
        for note in item.get("notes") or []:
            if note not in notes:
                notes.append(note)
        if item.get("error"):
            errors.append(str(item["error"]))
        if item.get("status") == "failed":
            status = "failed"
        elif item.get("status") == "aborted" and status == "ok":
            status = "aborted"
    merged["notes"] = notes
    merged["status"] = status
    merged["error"] = "; ".join(errors) if errors else None
    merged["finished_at"] = tier_results[-1].get("finished_at")
    merged["duration_seconds"] = round(
        sum(item.get("duration_seconds") or 0 for item in tier_results), 3
    )
    merged["params"] = {"profile_tiers": [item.get("params") for item in tier_results]}
    return merged


def run_concurrent(
    fn: Callable[[int], Any],
    count: int,
    concurrency: int,
) -> tuple[list[Any], float]:
    """Run fn(index) for range(count) with bounded concurrency; return (results, wall_ms).

    Barrier 同步起跑：任务号 < worker 数的首波任务先 barrier.wait()，主线程最后
    放行，wall 计时从放行瞬间起算。任务数 == 并发数（burst）时全量同步起跑；
    任务数 > 并发数（流水线）时只同步首波，后续任务自然流动（cyclic barrier 的
    下一波不会误锁）。barrier 异常破裂时各 worker 降级为直接执行，不挂死。
    """
    workers = max(1, min(concurrency, count))
    if count <= 0:
        return [], 0.0
    results: list[Any] = [None] * count
    barrier = threading.Barrier(workers + 1)

    def _wrapped(index: int) -> Any:
        if index < workers:
            try:
                barrier.wait(timeout=600)
            except threading.BrokenBarrierError:
                pass
        return fn(index)

    with ThreadPoolExecutor(max_workers=workers) as pool:
        futures = [pool.submit(_wrapped, index) for index in range(count)]
        try:
            barrier.wait(timeout=600)
        except threading.BrokenBarrierError:
            pass
        started = time.perf_counter()
        for index, future in enumerate(futures):
            results[index] = future.result()
    return results, (time.perf_counter() - started) * 1000


def kill_ids(client: BenchClient, sandbox_ids: list[str], *, concurrency: int = 16) -> dict[str, int]:
    """Delete sandboxes concurrently; return per-status outcome counts."""
    outcomes = {"deleted": 0, "not_found": 0, "failed": 0}
    unique_ids = list(dict.fromkeys(sandbox_ids))
    if not unique_ids:
        return outcomes
    lock = threading.Lock()

    def _kill(sandbox_id: str) -> None:
        try:
            response = client.kill(sandbox_id)
            outcome = "deleted" if response.status in (200, 204) else (
                "not_found" if response.status == 404 else "failed"
            )
        except Exception:
            outcome = "failed"
        with lock:
            outcomes[outcome] += 1

    with ThreadPoolExecutor(max_workers=max(1, min(concurrency, len(unique_ids)))) as pool:
        list(pool.map(_kill, unique_ids))
    return outcomes


def cleanup_created(ctx: BenchContext) -> dict[str, int]:
    with ctx._lock:
        sandbox_ids = list(ctx.created_ids)
        ctx.created_ids.clear()
    return kill_ids(ctx.client, sandbox_ids)


def cleanup_marked_sandboxes(
    client: BenchClient,
    *,
    run_id: str | None = None,
    include_all: bool = False,
) -> dict[str, Any]:
    """Kill sandboxes created by bench runs; --all widens the scope to every sandbox."""
    items = client.list_sandbox_items()
    targets: list[str] = []
    for item in items:
        sandbox_id = item.get("sandboxID") or item.get("sandboxId")
        metadata = item.get("metadata") if isinstance(item.get("metadata"), dict) else {}
        marker = metadata.get(BENCH_METADATA_KEY)
        if not sandbox_id:
            continue
        if include_all or (run_id and marker == run_id) or (run_id is None and marker):
            targets.append(str(sandbox_id))
    outcomes = kill_ids(client, targets)
    return {"listed": len(items), "matched": len(targets), **outcomes}


def _pgrep_count(name: str) -> int:
    try:
        completed = subprocess.run(
            ["pgrep", "-xc", name],
            capture_output=True, text=True, timeout=5, check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return 0
    value = completed.stdout.strip()
    return int(value) if completed.returncode == 0 and value.isdigit() else 0


def _active_nbd_count() -> int:
    count = 0
    for pid_file in glob.glob("/sys/block/nbd*/pid"):
        try:
            if Path(pid_file).read_text(encoding="utf-8").strip():
                count += 1
        except OSError:
            continue
    return count


FC_SOCKET_PATTERN = re.compile(r"/tmp/fc-([a-z0-9]+)-[a-z0-9]+\.sock")
# 沙箱 memory cgroup 候选路径：memory 子树挂点（v1 分离层级）与合成根两种布局
CGROUP_MEMORY_CANDIDATES = (
    "/sys/fs/cgroup/memory/e2b/sbx-{sid}",
    "/sys/fs/cgroup/e2b/sbx-{sid}",
)


def fc_pid_map() -> dict[str, int]:
    """firecracker 进程的 --api-sock /tmp/fc-<sandboxID>-<buildID>.sock 含沙箱 ID。"""
    mapping: dict[str, int] = {}
    for cmdline_path in glob.glob("/proc/[0-9]*/cmdline"):
        try:
            with open(cmdline_path, "rb") as stream:
                args = stream.read().decode("utf-8", "replace")
        except OSError:
            continue
        if "firecracker" not in args or "--api-sock" not in args:
            continue
        match = FC_SOCKET_PATTERN.search(args)
        if match:
            mapping[match.group(1)] = int(cmdline_path.split("/")[2])
    return mapping


def _read_cgroup_memory(sandbox_id: str) -> dict[str, Any] | None:
    for pattern in CGROUP_MEMORY_CANDIDATES:
        directory = Path(pattern.format(sid=sandbox_id))
        usage_file = directory / "memory.usage_in_bytes"
        if not usage_file.is_file():
            continue
        try:
            usage = int(usage_file.read_text(encoding="utf-8").strip())
            stat: dict[str, int] = {}
            stat_file = directory / "memory.stat"
            if stat_file.is_file():
                for line in stat_file.read_text(encoding="utf-8").splitlines():
                    key, _, value = line.partition(" ")
                    if value.strip().isdigit():
                        stat[key] = int(value.strip())
            return {"usage_bytes": usage, "stat": stat}
        except (OSError, ValueError):
            continue
    return None


_SMAPS_KEYS = (
    "Pss", "Pss_Anon", "Pss_File", "Pss_Shmem",
    "Private_Clean", "Private_Dirty", "Shared_Clean", "Shared_Dirty",
    "Private_Hugetlb", "Shared_Hugetlb",
)


def _read_smaps_rollup(pid: int) -> dict[str, int] | None:
    try:
        text = Path(f"/proc/{pid}/smaps_rollup").read_text(encoding="utf-8")
    except OSError:
        return None
    values: dict[str, int] = {}
    for line in text.splitlines():
        key, _, rest = line.partition(":")
        key = key.strip()
        if key in _SMAPS_KEYS:
            amount = rest.strip().split()
            if amount and amount[0].isdigit():
                values[key] = int(amount[0]) * 1024
    return values or None


def collect_memory_metrics(sandbox_ids: list[str]) -> dict[str, Any]:
    """对每个存活沙箱采集 cgroup v1 memory 用量 + FC 进程 smaps_rollup（PSS 等）。

    单个沙箱采集失败（cgroup 不存在/进程已退出）容错跳过并计数。
    注意：cgroup v1 共享页记在首个 touch 的 cgroup，Σ 总量准确但单沙箱均摊有偏差，
    故用 PSS 补充均摊口径；UFFD 懒加载下模板页体现为 Shared/File 页。
    """
    pid_map = fc_pid_map()
    details: list[dict[str, Any]] = []
    cgroup_missing = 0
    fc_missing = 0
    for sandbox_id in dict.fromkeys(sandbox_ids):
        entry: dict[str, Any] = {"sandbox_id": sandbox_id}
        cgroup = _read_cgroup_memory(sandbox_id)
        if cgroup is None:
            cgroup_missing += 1
        else:
            entry["cgroup_usage_mb"] = round(cgroup["usage_bytes"] / 1024 / 1024, 1)
            stat = cgroup["stat"]
            for field in ("cache", "rss", "anon", "swap", "mapped_file"):
                if field in stat:
                    entry[f"cgroup_{field}_mb"] = round(stat[field] / 1024 / 1024, 1)
        pid = pid_map.get(sandbox_id)
        smaps = _read_smaps_rollup(pid) if pid is not None else None
        if smaps is None:
            fc_missing += 1
        else:
            entry["fc_pid"] = pid
            entry["pss_mb"] = round(smaps.get("Pss", 0) / 1024 / 1024, 1)
            entry["private_dirty_mb"] = round(
                (smaps.get("Private_Dirty", 0) + smaps.get("Private_Hugetlb", 0)) / 1024 / 1024, 1
            )
            entry["shared_mb"] = round(
                (smaps.get("Shared_Clean", 0) + smaps.get("Shared_Dirty", 0)
                 + smaps.get("Shared_Hugetlb", 0)) / 1024 / 1024, 1
            )
            entry["pss_file_mb"] = round(smaps.get("Pss_File", 0) / 1024 / 1024, 1)
        details.append(entry)

    pss_values = sorted(item["pss_mb"] for item in details if "pss_mb" in item)
    private_values = [item["private_dirty_mb"] for item in details if "private_dirty_mb" in item]
    shared_values = [item["shared_mb"] for item in details if "shared_mb" in item]

    def _avg(values: list[float]) -> float | None:
        return round(sum(values) / len(values), 1) if values else None

    shared_sum = sum(shared_values)
    private_sum = sum(private_values)
    return {
        "requested": len(dict.fromkeys(sandbox_ids)),
        "sampled": len(details),
        "cgroup_missing": cgroup_missing,
        "fc_missing": fc_missing,
        "cgroup_usage_total_mb": round(
            sum(item.get("cgroup_usage_mb", 0) for item in details), 1
        ),
        "pss_avg_mb": _avg(pss_values),
        "pss_min_mb": pss_values[0] if pss_values else None,
        "pss_p95_mb": percentile(pss_values, 95) if pss_values else None,
        "pss_max_mb": pss_values[-1] if pss_values else None,
        "private_dirty_avg_mb": _avg(private_values),
        "shared_avg_mb": _avg(shared_values),
        "share_ratio": (
            round(shared_sum / (private_sum + shared_sum), 3)
            if private_sum + shared_sum > 0 else None
        ),
        "per_sandbox": details,
    }


def _netns_count() -> int:
    try:
        return len([
            entry for entry in os.listdir("/var/run/netns")
            if not entry.startswith(".")
        ])
    except OSError:
        return 0


def _host_veth_count() -> int:
    try:
        return len([
            entry for entry in os.listdir("/sys/class/net")
            if entry.startswith("veth-")
        ])
    except OSError:
        return 0


def _iptables_rule_count() -> int:
    try:
        completed = subprocess.run(
            ["iptables-save"], capture_output=True, text=True, timeout=15, check=False
        )
    except (OSError, subprocess.SubprocessError):
        return -1
    if completed.returncode != 0:
        return -1
    return len([line for line in completed.stdout.splitlines() if line.strip()])


def host_network_snapshot() -> dict[str, int]:
    """宿主网络残留观测：netns 数 / 主机侧 veth- 接口数 / iptables-save 行数。"""
    return {
        "netns": _netns_count(),
        "veth": _host_veth_count(),
        "iptables_rules": _iptables_rule_count(),
    }


def mark_host_net_baseline(ctx: BenchContext) -> None:
    ctx.host_net_baseline = host_network_snapshot()


def _marked_sandboxes(items: list[dict[str, Any]], run_id: str | None) -> list[str]:
    targets: list[str] = []
    for item in items:
        sandbox_id = item.get("sandboxID") or item.get("sandboxId")
        metadata = item.get("metadata") if isinstance(item.get("metadata"), dict) else {}
        marker = metadata.get(BENCH_METADATA_KEY)
        if sandbox_id and (marker == run_id if run_id else marker):
            targets.append(str(sandbox_id))
    return targets


def ensure_clean_slate(
    ctx: BenchContext,
    label: str,
    *,
    timeout: float = 300,
    stable_samples: int = 3,
    run_id: str | None = "__current__",
) -> bool:
    """清理本轮残留沙箱并等待运行时状态收敛（对标 run_v34_compare.sh 的模式）。

    1. DELETE 全部带 bench 标记的沙箱（默认仅本轮 run_id；run_id=None 清理任意轮次的
       bench 残留，用于 bench all pre-flight）；
    2. 每秒轮询：标记沙箱数==0 且 starting==0 且 firecracker/jailer/nbd 回到入口基线，
       且进程指标连续 stable_samples 个采样不变；
    3. 超时如实记录 warning 到 notes，返回 False；全程写入 result_dir/cleanup.log。
    外来（无 bench 标记）沙箱绝不删除。
    """
    scope = ctx.run_id if run_id == "__current__" else run_id
    log_path = ctx.result_dir / "cleanup.log"
    started = time.monotonic()

    def _line(message: str) -> None:
        stamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"
        with log_path.open("a", encoding="utf-8") as stream:
            stream.write(f"{stamp} label={label} {message}\n")

    baseline_firecracker = _pgrep_count("firecracker")
    baseline_jailer = _pgrep_count("jailer")
    baseline_nbd = _active_nbd_count()
    net_snapshot = host_network_snapshot()
    _line(
        f"baseline firecracker={baseline_firecracker} jailer={baseline_jailer} nbd={baseline_nbd} "
        f"netns={net_snapshot['netns']} veth={net_snapshot['veth']} "
        f"iptables_rules={net_snapshot['iptables_rules']}"
    )

    try:
        targets = _marked_sandboxes(ctx.client.list_sandbox_items(), scope)
    except Exception as exc:
        ctx.note(f"清理检查（{label}）：列出沙箱失败：{exc}")
        _line(f"list_error={exc}")
        targets = []
    if targets:
        outcomes = kill_ids(ctx.client, targets)
        with ctx._lock:
            dropped = set(targets)
            ctx.created_ids = [sid for sid in ctx.created_ids if sid not in dropped]
        _line(f"deleted marked={len(targets)} outcomes={outcomes}")

    stable = 0
    last_signature: tuple[int, int, int] | None = None
    converged = False
    last_snapshot = ""
    while time.monotonic() - started < timeout:
        try:
            items = ctx.client.list_sandbox_items()
            marked = len(_marked_sandboxes(items, scope))
            starting = sum(
                1 for item in items if str(item.get("state") or "").lower() == "starting"
            )
        except Exception as exc:
            _line(f"poll_error={exc}")
            stable = 0
            last_signature = None
            time.sleep(1)
            continue
        firecracker = _pgrep_count("firecracker")
        jailer = _pgrep_count("jailer")
        nbd = _active_nbd_count()
        net_snapshot = host_network_snapshot()
        signature = (firecracker, jailer, nbd)
        within_baseline = (
            firecracker <= baseline_firecracker
            and jailer <= baseline_jailer
            and nbd <= baseline_nbd
        )
        last_snapshot = (
            f"marked={marked} starting={starting} firecracker={firecracker} "
            f"jailer={jailer} nbd={nbd} netns={net_snapshot['netns']} "
            f"veth={net_snapshot['veth']} iptables_rules={net_snapshot['iptables_rules']} "
            f"stable={stable}/{stable_samples}"
        )
        _line(last_snapshot)
        if marked == 0 and starting == 0 and within_baseline and signature == last_signature:
            stable += 1
            if stable >= stable_samples:
                converged = True
                break
        else:
            stable = 0
        last_signature = signature
        time.sleep(1)

    final_snapshot = host_network_snapshot()
    baseline_net = ctx.host_net_baseline
    if baseline_net is not None and final_snapshot["netns"] >= 0 and baseline_net.get("netns", 0) >= 0:
        growth = final_snapshot["netns"] - baseline_net["netns"]
        if growth > ctx.netns_growth_threshold:
            message = (
                f"宿主网络残留增长 netns +{growth}"
                f"（{baseline_net['netns']} → {final_snapshot['netns']}，"
                f"阈值 {ctx.netns_growth_threshold}），疑似创建失败路径泄漏，"
                "建议 full 运行之间执行 bench clean-host"
            )
            ctx.note(message)
            _line(f"warning host-netns-growth netns_growth={growth}")
    _line(
        f"final netns={final_snapshot['netns']} veth={final_snapshot['veth']} "
        f"iptables_rules={final_snapshot['iptables_rules']}"
    )

    if converged:
        _line(f"converged elapsed={time.monotonic() - started:.1f}s")
        try:
            clean_host_orphans(ctx, f"{label}-orphans")
        except Exception as exc:
            ctx.note(f"档后孤儿资源清理失败（{label}）：{exc}")
            _line(f"orphan_cleanup_error={exc}")
        return True
    ctx.note(
        f"清理收敛超时（{label}）：{timeout}s 后仍未归零/回基线，最后采样 {last_snapshot}；"
        "后续档位数据可能受残留影响"
    )
    _line(f"timeout elapsed={time.monotonic() - started:.1f}s {last_snapshot}")
    return False


# v35 槽位 N ↔ HostIP 映射（k8s.io/utils/net GetIndexedIP，基址可通过
# SANDBOXES_HOST_NETWORK_CIDR 调整，本环境固定为 10.11.0.0/16）：
# HostIP = 10.11.(N>>8).(N&255)，N 从 1 开始（如 N=300 → 10.11.1.44）
MASQ_RULE_PATTERN = re.compile(
    r"^-A POSTROUTING -s 10\.11\.(\d+)\.(\d+)/32\s.*\bMASQUERADE\b"
)


def _slot_ids(names: list[str], prefix: str) -> set[int]:
    ids: set[int] = set()
    for name in names:
        suffix = name.removeprefix(prefix)
        if suffix.isdigit():
            ids.add(int(suffix))
    return ids


def _filter_orphan_rules(
    source: str,
    alive_veths: set[str],
    alive_slot_ids: set[int],
) -> tuple[str, int, int]:
    """单次遍历过滤两类孤儿规则；返回 (filtered, veth_removed, masq_removed)。

    1. 引用「不在存活集合中的 veth-*」的 -A 规则（双活槽位的规则一律保留）；
    2. `nat POSTROUTING -s 10.11.x.y/32 ... MASQUERADE`：HostIP 换算槽位号
       N=(b<<8)|c，N 不在存活槽位集合（ns-N 与 veth-N 的并集）即为孤儿。
    """
    import shlex

    kept: list[str] = []
    veth_removed = 0
    masq_removed = 0
    table = ""
    for line in source.splitlines():
        if line.startswith("*"):
            table = line[1:]
        elif line == "COMMIT":
            table = ""
        if table and line.startswith("-A "):
            masq_match = MASQ_RULE_PATTERN.match(line)
            if masq_match:
                slot = (int(masq_match.group(1)) << 8) | int(masq_match.group(2))
                if slot not in alive_slot_ids:
                    masq_removed += 1
                    continue
            else:
                tokens = shlex.split(line)
                references_orphan = any(
                    tokens[index] in {"-i", "-o", "--in-interface", "--out-interface"}
                    and index + 1 < len(tokens)
                    and tokens[index + 1].startswith("veth-")
                    and tokens[index + 1] not in alive_veths
                    for index in range(len(tokens))
                )
                if references_orphan:
                    veth_removed += 1
                    continue
        kept.append(line)
    return "\n".join(kept) + "\n", veth_removed, masq_removed


def _iptables_restore_checked(filtered: str, original: str, ctx: BenchContext, label: str) -> None:
    """iptables-restore 失败必须报错，并把原规则留在 /tmp 供排查。"""
    for attempt in range(1, 4):
        completed = subprocess.run(
            ["iptables-restore", "--wait=60"],
            input=filtered, capture_output=True, text=True, timeout=180, check=False,
        )
        if completed.returncode == 0:
            return
    dump = Path(f"/tmp/bench-iptables-orphans-{ctx.run_id}-{int(time.time())}.rules")
    dump.write_text(original, encoding="utf-8")
    raise RuntimeError(
        f"clean_host_orphans（{label}）iptables-restore 连续 3 次失败："
        f"{completed.stderr[-300:]}；原始规则已保存到 {dump}"
    )


def clean_host_orphans(ctx: BenchContext, label: str) -> dict[str, int]:
    """清理宿主网络孤儿资源：孤儿 veth（veth-N 无 ns-N）、引用已消失 veth 的规则、
    以及 HostIP 映射到已消失槽位的 10.11 MASQUERADE 规则。

    槽位 N 对应 netns ns-N + 宿主机 veth-N（1:1）。注意「ns-N 无 veth-N」的半对子
    不再自动删除：netns 生命周期归 orchestrator 网络池管理（mint 时先建 ns 再建 veth，
    存在短暂的半对子窗口；失效槽位由 orchestrator 启动时的归属标记回收自愈）。池化
    槽位的 ns 被外部删掉后，池内记录不会感知，下次分发该槽位会让 fc-netns-exec 因
    open /run/netns/ns-N 失败而连锁创建失败（毒槽位乒乓）。因此这里只统计告警，
    需要彻底清理时用 bench clean-host 并重启 template-manager。

    仅在无沙箱活动（firecracker=0 且 jailer=0）时执行。
    """
    import os

    stats = {
        "orphan_netns": 0,
        "orphan_veth": 0,
        "orphan_rules": 0,
        "orphan_masq_rules": 0,
        "skipped": 0,
    }
    log_path = ctx.result_dir / "cleanup.log"

    def _line(message: str) -> None:
        stamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"
        with log_path.open("a", encoding="utf-8") as stream:
            stream.write(f"{stamp} label={label} clean_host_orphans {message}\n")

    firecracker = _pgrep_count("firecracker")
    jailer = _pgrep_count("jailer")
    if firecracker or jailer:
        stats["skipped"] = 1
        _line(f"skipped firecracker={firecracker} jailer={jailer}")
        return stats

    try:
        netns_ids = _slot_ids(os.listdir("/var/run/netns"), "ns-")
    except OSError:
        netns_ids = set()
    try:
        veth_ids = _slot_ids(os.listdir("/sys/class/net"), "veth-")
    except OSError:
        veth_ids = set()

    orphan_netns = sorted(netns_ids - veth_ids)
    orphan_veth = sorted(veth_ids - netns_ids)

    if orphan_netns:
        # 不自动删除：这些 ns 可能属于池化槽位（mint 窗口或半失败残留），删了会毒化
        # 池内槽位。仅记录明细，交给 orchestrator 启动回收或 clean-host + 重启处理。
        stats["orphan_netns"] = len(orphan_netns)
        _line(
            f"orphan_netns kept (not deleted): {[f'ns-{slot}' for slot in orphan_netns]}；"
            "netns 归 orchestrator 网络池管理，如需清理请 bench clean-host 并重启 template-manager"
        )
    if orphan_veth:
        completed = subprocess.run(
            ["xargs", "-n1", "-P16", "ip", "link", "del"],
            input="\n".join(f"veth-{slot}" for slot in orphan_veth),
            capture_output=True, text=True, timeout=600, check=False,
        )
        stats["orphan_veth"] = len(orphan_veth) - completed.stderr.count("Cannot")

    try:
        alive_veths = {
            entry for entry in os.listdir("/sys/class/net") if entry.startswith("veth-")
        }
        alive_slot_ids = _slot_ids(os.listdir("/var/run/netns"), "ns-")
        alive_slot_ids |= _slot_ids(alive_veths, "veth-")
    except OSError:
        alive_veths = set()
        alive_slot_ids = set()
    completed = subprocess.run(
        ["iptables-save"], capture_output=True, text=True, timeout=60, check=False
    )
    if completed.returncode == 0:
        filtered, veth_removed, masq_removed = _filter_orphan_rules(
            completed.stdout, alive_veths, alive_slot_ids
        )
        if veth_removed or masq_removed:
            _iptables_restore_checked(filtered, completed.stdout, ctx, label)
            stats["orphan_rules"] = veth_removed
            stats["orphan_masq_rules"] = masq_removed
    else:
        _line(f"iptables-save failed: {completed.stderr[-200:]}")

    _line(
        f"orphan_netns={stats['orphan_netns']} orphan_veth={stats['orphan_veth']} "
        f"orphan_rules={stats['orphan_rules']} orphan_masq_rules={stats['orphan_masq_rules']}"
    )
    return stats


def pre_tier_settle(ctx: BenchContext, label: str, pre_wait: float) -> None:
    """档前等待与清理：pre_wait>0 时先静置等待（每 30s 打印剩余时间，不创建任何沙箱、
    不计入测量），再 clean_host_orphans + ensure_clean_slate 快速确认；pre_wait=0 时
    只做一次孤儿清理。
    """
    if pre_wait and pre_wait > 0:
        log_path = ctx.result_dir / "cleanup.log"
        _log_line(log_path, label, f"pre_wait start seconds={pre_wait}")
        print(f"[bench] {label}：档位前等待 {pre_wait:.0f}s 让系统池恢复…", file=sys.stderr, flush=True)
        deadline = time.monotonic() + pre_wait
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            time.sleep(min(30, remaining))
            remaining = deadline - time.monotonic()
            if remaining > 0:
                print(f"[bench] {label}：剩余 {remaining:.0f}s", file=sys.stderr, flush=True)
        _log_line(log_path, label, "pre_wait done")
        clean_host_orphans(ctx, f"{label}-settle-orphans")
        ensure_clean_slate(ctx, f"{label}-settle", timeout=120)
    else:
        clean_host_orphans(ctx, f"{label}-pre")


def _log_line(log_path: Path, label: str, message: str) -> None:
    stamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"
    with log_path.open("a", encoding="utf-8") as stream:
        stream.write(f"{stamp} label={label} {message}\n")


def delete_snapshot(client: BenchClient, snapshot_id: str) -> bool:
    try:
        response = client.delete_snapshot(snapshot_id)
        if response.status in (200, 204, 404):
            return True
    except Exception:
        pass
    try:
        from ..e2b_sdk_compat import delete_snapshot as sdk_delete_snapshot

        return bool(sdk_delete_snapshot(snapshot_id, **sdk_options()))
    except Exception:
        return False


def read_meminfo() -> dict[str, int]:
    values: dict[str, int] = {}
    with open("/proc/meminfo", encoding="utf-8") as stream:
        for line in stream:
            key, _, rest = line.partition(":")
            amount = rest.strip().split()
            if amount and amount[0].isdigit():
                values[key] = int(amount[0])
    return values


def collect_environment(client: BenchClient, template: str, *, template_source: str | None = None) -> dict[str, Any]:
    cpu_model = ""
    try:
        with open("/proc/cpuinfo", encoding="utf-8") as stream:
            for line in stream:
                key, _, value = line.partition(":")
                if key.strip().lower() in {"model name", "model", "hardware"} and value.strip():
                    cpu_model = value.strip()
                    break
    except OSError:
        pass
    if not cpu_model:
        try:
            import subprocess

            output = subprocess.run(
                ["lscpu"], capture_output=True, text=True, timeout=5, check=False
            ).stdout
            for line in output.splitlines():
                key, _, value = line.partition(":")
                if key.strip().lower() == "model name" and value.strip():
                    cpu_model = value.strip()
                    break
        except (OSError, subprocess.SubprocessError):
            pass
    if not cpu_model:
        processor = platform.processor()
        if processor and processor != platform.machine():
            cpu_model = processor
    os_release = ""
    try:
        with open("/etc/os-release", encoding="utf-8") as stream:
            for line in stream:
                if line.startswith("PRETTY_NAME="):
                    os_release = line.split("=", 1)[1].strip().strip('"')
                    break
    except OSError:
        pass
    meminfo = read_meminfo()
    template_info: dict[str, Any] = {}
    try:
        for item in client.list_templates():
            if item.get("templateID") == template or template in (item.get("aliases") or []):
                template_info = {
                    "templateID": item.get("templateID"),
                    "aliases": item.get("aliases") or [],
                    "cpuCount": item.get("cpuCount"),
                    "memoryMB": item.get("memoryMB"),
                    "diskSizeMB": item.get("diskSizeMB"),
                    "envdVersion": item.get("envdVersion"),
                }
                break
    except Exception:
        pass
    template_info.setdefault("templateID", template)
    if template_source:
        template_info["source"] = template_source
    return {
        "os": os_release or platform.platform(),
        "kernel": platform.release(),
        "arch": platform.machine(),
        "cpu_model": cpu_model or "unknown",
        "cpu_cores": os.cpu_count(),
        "mem_total_mb": round(meminfo.get("MemTotal", 0) / 1024),
        "api_url": client.api_url,
        "template": template_info,
    }


def wait_state(
    client: BenchClient,
    sandbox_id: str,
    expected: str,
    *,
    timeout: float = 120,
    interval: float = 0.5,
) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            response = client.get_sandbox(sandbox_id)
        except Exception:
            time.sleep(interval)
            continue
        state = None
        if isinstance(response.data, dict):
            raw = response.data.get("state")
            state = raw.lower() if isinstance(raw, str) else None
        if response.status == 200 and state == expected:
            return True
        time.sleep(interval)
    return False


def connect_sdk(sandbox_id: str, *, timeout: int = 300):
    return connect_sandbox(
        sandbox_id,
        timeout=timeout,
        **sdk_options(sandbox_id=sandbox_id),
    )


def run_in_sandbox(sandbox, command: str, *, user: str = "root") -> str:
    options = {"user": user} if user else {}
    result = sandbox.commands.run(command, **options)
    if result.exit_code != 0:
        raise RuntimeError(
            f"command failed with exit code {result.exit_code}: {result.stderr[-500:]}"
        )
    return result.stdout.strip()


def base_result(bench: str, ctx: BenchContext, params: dict[str, Any]) -> dict[str, Any]:
    return {
        "bench": bench,
        "run_id": ctx.run_id,
        "template": ctx.template,
        "template_source": ctx.template_source,
        "params": params,
        "status": "ok",
        "error": None,
        "notes": [],
        "started_at": datetime.now(timezone.utc).isoformat(),
    }


def finish_result(result: dict[str, Any], ctx: BenchContext) -> dict[str, Any]:
    result["finished_at"] = datetime.now(timezone.utc).isoformat()
    started = datetime.fromisoformat(result["started_at"])
    result["duration_seconds"] = round(
        (datetime.now(timezone.utc) - started).total_seconds(), 3
    )
    result["notes"] = list(ctx.notes)
    return result


def timed_create(ctx: BenchContext, template: str | None = None, *, timeout: int | None = None) -> TimedResult:
    result = ctx.client.create_timed(
        template or ctx.template,
        timeout=timeout or ctx.sandbox_timeout,
        metadata=ctx.metadata,
    )
    ctx.track(result.sandbox_id if result.ok else None)
    return result
