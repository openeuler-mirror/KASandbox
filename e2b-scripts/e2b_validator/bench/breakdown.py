"""沙箱创建耗时分解：按 sandbox_id 把客户端、API、orchestrator 三层耗时拼成一棵树。

服务端数据来自 API 与 orchestrator 日志里的 "[PerfTrace] {json}" 汇总行（每请求每组件一行，
见 packages/shared/pkg/perftrace）。每个有子阶段的节点下补一行"（未打点）"：父阶段时长减去
子阶段的时间并集，并行的子阶段不会被重复扣除。采集失败只记一条说明，不影响测试结果。
"""

from __future__ import annotations

import glob
import json
import os
import re
import time
import unicodedata
import urllib.request
from datetime import datetime, timezone
from typing import Any, Callable

PREFIX = "[PerfTrace] "
UNACCOUNTED = "（未打点）"
CLIENT_ROOT = "Sandbox.create（客户端）"
LABELS = {"api": "API：POST /sandboxes", "orchestrator": "orchestrator：Create", "total": "ResumeSandbox（total）"}
# orchestrator 的记录挂在 API 的这个阶段下面
GRPC_PHASE = "node.SandboxCreate (gRPC)"
# 报告里平均耗时低于该值（毫秒）的阶段合并成一行；JSON 里始终是完整的
FOLD_BELOW_MS = 0.5
TIMELINE = (
    ("client_send", "客户端发出请求"),
    ("api_recv", "请求到达 API"),
    ("orch_recv", "请求到达 orchestrator"),
    ("orch_done", "orchestrator 返回"),
    ("api_done", "API 返回"),
    ("client_done", "客户端拿到结果"),
)

_STAMP = re.compile(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(\.\d+)?Z")
_DECODER = json.JSONDecoder()


def _epoch(line: str) -> float | None:
    """日志行行首时间戳（UTC）对应的 epoch 秒。"""
    match = _STAMP.match(line)
    if not match:
        return None
    base = datetime.strptime(match.group(1), "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc)
    return base.timestamp() + float(match.group(2) or 0)


def _nomad_token(section: dict[str, Any]) -> str:
    """nomad ACL 令牌：环境变量 NOMAD_TOKEN 优先，其次读 nomad_env_file 里的同名变量（可带 export 前缀）。"""
    token = os.environ.get("NOMAD_TOKEN", "")
    env_file = section.get("nomad_env_file") or ""
    if not token and os.path.isfile(env_file):
        with open(env_file, encoding="utf-8", errors="replace") as stream:
            for line in stream:
                key, _, value = line.strip().removeprefix("export ").partition("=")
                if key == "NOMAD_TOKEN":
                    token = value.strip("\"'")
    return token


def _nomad_tail(section: dict[str, Any], component: str, since: float) -> str:
    """经 nomad HTTP API 读某组件日志的末尾，不够覆盖 since 时加大一次。"""
    addr = (section.get("nomad_addr") or os.environ.get("NOMAD_ADDR") or "http://127.0.0.1:4646").rstrip("/")
    token = _nomad_token(section)

    def get(path: str) -> bytes:
        request = urllib.request.Request(addr + path, headers={"X-Nomad-Token": token} if token else {})
        with urllib.request.urlopen(request, timeout=15) as response:
            return response.read()

    job = section.get(f"{component}_job") or ("api" if component == "api" else "template-manager")
    running = [item for item in json.loads(get(f"/v1/job/{job}/allocations")) if item.get("ClientStatus") == "running"]
    if not running:
        raise RuntimeError(f"nomad job {job} 没有 running 的 alloc")
    alloc = max(running, key=lambda item: item.get("CreateTime", 0))["ID"]
    text = ""
    for size in (4 << 20, 64 << 20):
        query = f"task=start&type=stdout&plain=true&origin=end&offset={size}"
        text = get(f"/v1/client/fs/logs/{alloc}?{query}").decode("utf-8", errors="replace")
        first = next((stamp for stamp in map(_epoch, text.splitlines()[1:50]) if stamp is not None), None)
        if len(text) < size // 2 or (first is not None and first < since):
            break
    return text


def read_records(section: dict[str, Any], since: float, wanted: set[str] | None = None) -> dict[str, dict[str, dict]]:
    """读出 since 之后的汇总记录：{sandbox_id: {component: record}}，同一沙箱同一组件取最后一条。"""
    found: dict[str, dict[str, dict]] = {}
    for component in ("api", "orchestrator"):
        if section.get("source") == "file":
            paths = [path for path in glob.glob(section.get(f"{component}_log") or "") if os.path.getmtime(path) >= since - 2]
            text = ""
            for path in sorted(paths, key=os.path.getmtime):
                with open(path, encoding="utf-8", errors="replace") as stream:
                    text += stream.read()
        else:
            text = _nomad_tail(section, component, since)
        for line in text.splitlines():
            index = line.find(PREFIX)
            stamp = _epoch(line)
            if index < 0 or (stamp is not None and stamp < since):
                continue
            try:
                record, _ = _DECODER.raw_decode(line, index + len(PREFIX))
                sandbox_id = record["sandbox_id"]
            except (ValueError, KeyError, TypeError):
                continue
            if sandbox_id and (wanted is None or sandbox_id in wanted):
                found.setdefault(sandbox_id, {})[record["component"]] = record
    return found


def _node(name: str, dur_ms: float, start_ms: float | None = 0.0) -> dict[str, Any]:
    # start 为相对所属组件请求起点的偏移；跨组件挂上来的子树没有共同时钟，为 None
    return {"name": name, "dur": dur_ms, "start": start_ms, "children": []}


def _tree(record: dict[str, Any]) -> dict[str, Any]:
    """一条汇总记录 → 以该组件请求为根的树（跨组件挂接，根的 start 为 None）。"""
    total = record["total_us"]
    root = _node(LABELS.get(record["component"], record["component"]), total / 1000, None)
    nodes = {0: root}
    for phase in record.get("phases") or []:
        # 未结束的阶段按"持续到请求结束"计
        dur = phase["d"] if phase["d"] >= 0 else max(total - phase["s"], 0)
        nodes[phase["i"]] = _node(LABELS.get(phase["n"], phase["n"]), dur / 1000, phase["s"] / 1000)
        nodes.get(phase["p"], root)["children"].append(nodes[phase["i"]])
    return root


def assemble(create_ms: float, records: dict[str, dict]) -> dict[str, Any]:
    """一个沙箱的整棵树：客户端 → API → orchestrator；缺哪层就跳过哪层。"""
    root = parent = _node(CLIENT_ROOT, create_ms)
    if "api" in records:
        api = _tree(records["api"])
        root["children"].append(api)
        parent = next((node for node in _walk(api) if node["name"] == GRPC_PHASE), api)
    if "orchestrator" in records:
        parent["children"].append(_tree(records["orchestrator"]))
    return root


def _walk(node: dict[str, Any]):
    yield node
    for child in node["children"]:
        yield from _walk(child)


def uncovered(node: dict[str, Any]) -> float:
    """父阶段里没有被子阶段覆盖的时长：同一时钟的子阶段取时间并集，跨组件的子树按总时长扣。"""
    low = node["start"] or 0.0
    high = low + node["dur"]
    covered = sum(min(child["dur"], node["dur"]) for child in node["children"] if child["start"] is None)
    end = low
    for child in sorted((child for child in node["children"] if child["start"] is not None), key=lambda c: c["start"]):
        start, stop = max(child["start"], end), min(child["start"] + child["dur"], high)
        if stop > start:
            covered += stop - start
            end = stop
    return max(node["dur"] - covered, 0.0)


def _bucket(name: str) -> dict[str, Any]:
    return {"name": name, "samples": [], "starts": [], "rest": [], "children": {}}


def _merge(bucket: dict[str, Any], nodes: list[dict[str, Any]]) -> None:
    """把一个沙箱里同一路径上的同名阶段（可能出现多次）并入汇总。"""
    bucket["samples"].append(sum(node["dur"] for node in nodes))
    starts = [node["start"] for node in nodes if node["start"] is not None]
    if starts:
        bucket["starts"].append(min(starts))
    groups: dict[str, list] = {}
    for node in nodes:
        for child in node["children"]:
            groups.setdefault(child["name"], []).append(child)
    if groups:
        bucket["rest"].append(sum(uncovered(node) for node in nodes if node["children"]))
    for name, children in groups.items():
        _merge(bucket["children"].setdefault(name, _bucket(name)), children)


def _percentile(ordered: list[float], pct: float) -> float:
    index = (len(ordered) - 1) * pct / 100
    lower = int(index)
    upper = min(lower + 1, len(ordered) - 1)
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (index - lower)


def _stats(name: str, samples: list[float], total: float) -> dict[str, Any]:
    ordered = sorted(samples)
    return {
        "name": name,
        "count": len(ordered),
        "avg_ms": round(sum(ordered) / len(ordered), 3),
        "p50_ms": round(_percentile(ordered, 50), 3),
        "p95_ms": round(_percentile(ordered, 95), 3),
        "max_ms": round(ordered[-1], 3),
        # 占比按总时间算，只在部分样本里出现的阶段不会被高估
        "share_pct": round(sum(ordered) * 100 / total, 2) if total else None,
    }


def _export(bucket: dict[str, Any], total: float) -> dict[str, Any]:
    out = _stats(bucket["name"], bucket["samples"], total)
    # 子阶段按开始时间排，跨组件挂上来的排在后面，"（未打点）"放最后
    def order(child: dict[str, Any]) -> tuple[bool, float]:
        return (not child["starts"], _percentile(sorted(child["starts"]), 50) if child["starts"] else 0.0)

    children = [_export(child, total) for child in sorted(bucket["children"].values(), key=order)]
    if bucket["rest"]:
        children.append(_stats(UNACCOUNTED, bucket["rest"], total))
    if children:
        out["children"] = children
    return out


class Collector:
    """一个档位的采集器：每轮创建前 arm()，创建后 add_batch(batch)，档位结束 result()。"""

    def __init__(self, section: dict[str, Any], note: Callable[[str], None]) -> None:
        self.section = section
        self.note = note
        self.since = 0.0
        self.settle = 5.0  # 等日志落盘的最长秒数
        self.expected = {"api", "orchestrator"}  # 还在等的组件；一直没有记录的会被去掉
        self.bucket = _bucket(CLIENT_ROOT)
        self.matched: dict[str, int] = {}
        self.events: dict[str, list[float]] = {}
        self.warned = False

    def arm(self) -> None:
        self.since = time.time()

    def _records(self, wanted: set[str]) -> dict[str, dict[str, dict]]:
        """读本轮沙箱的服务端记录；日志落盘有延迟，等到每个沙箱在各组件都有记录或超时为止。"""
        deadline = time.monotonic() + (self.settle if self.expected else 0.0)
        while True:
            try:
                found = read_records(self.section, self.since, wanted)
                reason = "服务端日志里没有本轮沙箱的 [PerfTrace] 记录"
            except Exception as exc:
                found, reason = {}, f"读服务端日志失败（{type(exc).__name__}: {exc}）"
            if self.expected and all(self.expected <= set(found.get(sid, {})) for sid in wanted):
                return found
            if time.monotonic() >= deadline:
                # 服务端没有打点或读不到的组件，后面的轮次不必再等
                self.expected &= {component for item in found.values() for component in item}
                if not found and not self.warned:
                    self.warned = True
                    self.note(f"耗时分解：{reason}，只统计客户端总耗时")
                return found
            time.sleep(0.2)

    def add_batch(self, batch: dict[str, Any]) -> None:
        """并入一轮批量创建的结果（sdk_engine.batch_create 的返回值）。"""
        items = {item["sandbox_id"].split("-")[0]: item for item in batch.get("results") or [] if item.get("ok")}
        if not items:
            return
        records = self._records(set(items))
        for sandbox_id, item in items.items():
            mine = records.get(sandbox_id, {})
            if any(not record.get("ok", True) for record in mine.values()):
                continue
            _merge(self.bucket, [assemble(item["create_time_s"] * 1000, mine)])
            release = item.get("release_wall")
            events = {} if release is None else {
                "client_send": item["request_wall"],
                "client_done": item["request_wall"] + item["create_time_s"],
            }
            for component, key in (("api", "api"), ("orchestrator", "orch")):
                if component in mine:
                    self.matched[component] = self.matched.get(component, 0) + 1
                    if release is not None:
                        events[f"{key}_recv"] = mine[component]["start_unix_us"] / 1e6
                        events[f"{key}_done"] = (mine[component]["start_unix_us"] + mine[component]["total_us"]) / 1e6
            for key, value in events.items():
                self.events.setdefault(key, []).append((value - release) * 1000)

    def result(self) -> dict[str, Any] | None:
        """本档位的汇总分解；没有样本返回 None。"""
        if not self.bucket["samples"]:
            return None
        out = {
            "samples": len(self.bucket["samples"]),
            "matched": self.matched,
            "tree": _export(self.bucket, sum(self.bucket["samples"])),
        }
        server = [value for key, values in self.events.items() if not key.startswith("client_") for value in values]
        # 服务端时间戳落在客户端发出与拿到结果之间才可信，否则说明不同机或时钟未同步
        if server and min(server) >= min(self.events["client_send"]) - 5 and max(server) <= max(self.events["client_done"]) + 5:
            out["timeline"] = {
                key: {
                    "min_ms": round(min(values), 1),
                    "p50_ms": round(_percentile(sorted(values), 50), 1),
                    "max_ms": round(max(values), 1),
                }
                for key, values in self.events.items()
            }
        return out


def collector(section: dict[str, Any] | None, note: Callable[[str], None]) -> Collector | None:
    """按 [breakdown] 配置创建采集器；enabled = false 时返回 None。"""
    section = dict(section or {})
    return Collector(section, note) if section.get("enabled", True) else None


def _width(text: str) -> int:
    return sum(2 if unicodedata.east_asian_width(char) in "WF" else 1 for char in text)


def _row(label: str, width: int, cells: list[Any]) -> str:
    texts = [f"{cell:.1f}" if isinstance(cell, float) else "—" if cell is None else str(cell) for cell in cells]
    return label + " " * max(width - _width(label), 0) + "".join(" " * max(9 - _width(text), 0) + text for text in texts)


def render(result: dict[str, Any]) -> list[str]:
    """文本树：每个阶段一行（次数 / 平均 / P50 / P95 / 最大 / 占比），其后是整批时间线。"""
    rows: list[tuple[str, dict[str, Any]]] = []

    def visit(node: dict[str, Any], prefix: str, connector: str) -> None:
        rows.append((prefix + connector + node["name"], node))
        children = node.get("children") or []
        small = [child for child in children if child["avg_ms"] < FOLD_BELOW_MS and child["name"] != UNACCOUNTED]
        if len(small) > 1:
            folded = {
                "name": f"其余 {len(small)} 个阶段（各 < {FOLD_BELOW_MS:g} ms）",
                "count": max(child["count"] for child in small),
                "avg_ms": sum(child["avg_ms"] for child in small),
                "share_pct": sum(child["share_pct"] or 0 for child in small),
            }
            children = [child for child in children if child not in small]
            children.insert(len(children) - (1 if children and children[-1]["name"] == UNACCOUNTED else 0), folded)
        child_prefix = prefix + ("" if not connector else "  " if connector == "└ " else "│ ")
        for index, child in enumerate(children):
            visit(child, child_prefix, "└ " if index == len(children) - 1 else "├ ")

    visit(result["tree"], "", "")
    matched = "、".join(f"{component} {count}" for component, count in sorted(result.get("matched", {}).items()))
    lines = [f"样本 {result['samples']} 个沙箱" + (f"；关联到服务端记录：{matched}" if matched else "")]
    width = max(max(_width(label) for label, _ in rows) + 2, 30)
    lines.append(_row("阶段（毫秒）", width, ["次数", "平均", "P50", "P95", "最大", "占比"]))
    for label, node in rows:
        share = node.get("share_pct")
        cells = [node["count"], float(node["avg_ms"]), node.get("p50_ms"), node.get("p95_ms"), node.get("max_ms")]
        lines.append(_row(label, width, cells + [f"{share:.1f}%" if share is not None else None]))
    timeline = result.get("timeline")
    if timeline:
        lines += ["", _row("整批时间线（距起跑，毫秒）", 30, ["最早", "中位", "最晚"])]
        for key, title in TIMELINE:
            if key in timeline:
                lines.append(_row(title, 30, [timeline[key][k] for k in ("min_ms", "p50_ms", "max_ms")]))
    return lines
