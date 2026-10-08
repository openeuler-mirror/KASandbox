"""Timed REST client for benchmark operations on the E2B API."""

from __future__ import annotations

import re
import time
import urllib.parse
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Iterable

from ..e2e_api_client import ApiResponse, E2BApiClient, sandbox_id_from

# Go RFC3339Nano：小数秒可达 9 位，Python fromisoformat 仅接受 6 位，先截断再解析
_TS_PATTERN = re.compile(
    r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})?$"
)


def parse_rfc3339_epoch(value: Any) -> float | None:
    """把 RFC3339/RFC3339Nano 时间戳解析为 epoch 秒（失败返回 None）。"""
    if not isinstance(value, str):
        return None
    match = _TS_PATTERN.match(value.strip())
    if not match:
        return None
    base, fraction, zone = match.groups()
    micros = ((fraction or "") + "000000")[:6]
    suffix = "+00:00" if zone in (None, "Z") else zone
    try:
        return datetime.fromisoformat(f"{base}.{micros}{suffix}").timestamp()
    except ValueError:
        return None


@dataclass
class TimedResult:
    ok: bool
    latency_ms: float
    status: int | None = None
    sandbox_id: str | None = None
    data: Any = None
    error: str | None = None
    extra: dict[str, Any] = field(default_factory=dict)


class BenchClient(E2BApiClient):
    """E2B REST client with per-request timing and benchmark-oriented helpers."""

    def timed_request(
        self,
        method: str,
        path: str,
        *,
        payload: dict[str, Any] | None = None,
        timeout: float | None = None,
    ) -> TimedResult:
        started = time.perf_counter()
        try:
            response = self.request(method, path, payload=payload, timeout=timeout)
        except Exception as exc:
            return TimedResult(
                ok=False,
                latency_ms=(time.perf_counter() - started) * 1000,
                error=f"{type(exc).__name__}: {exc}"[-500:],
            )
        latency_ms = (time.perf_counter() - started) * 1000
        ok = 200 <= response.status < 300
        return TimedResult(
            ok=ok,
            latency_ms=latency_ms,
            status=response.status,
            sandbox_id=sandbox_id_from(response.data),
            data=response.data,
            error=None if ok else f"status={response.status}: {response.text}"[-500:],
        )

    def create_timed(
        self,
        template: str,
        *,
        timeout: int = 600,
        metadata: dict[str, str] | None = None,
        request_timeout: float | None = None,
    ) -> TimedResult:
        payload: dict[str, Any] = {"templateID": template, "timeout": timeout}
        if metadata:
            payload["metadata"] = metadata
        sent_wall = time.time()
        result = self.timed_request("POST", "/sandboxes", payload=payload, timeout=request_timeout)
        if result.ok and result.sandbox_id:
            result.extra["request_wall"] = sent_wall
            # 计时窗外回读服务端真值：startedAt 由 API 在 orchestrator 创建完成时写入。
            # 逐操作立即回读（沙箱必存活），代价是一次 loopback GET，不计入 latency_ms。
            started_at = self.sandbox_started_at(result.sandbox_id)
            if started_at is not None:
                result.extra["started_at"] = started_at
                result.extra["server_ms"] = round((started_at - sent_wall) * 1000, 1)
        return result

    def list_running_items(self) -> list[dict[str, Any]]:
        """GET /sandboxes（v1）：running 全量列表、不分页，元素含 startedAt。"""
        response = self.request("GET", "/sandboxes", timeout=60)
        if response.status != 200 or not isinstance(response.data, list):
            raise RuntimeError(f"failed to list running sandboxes: status={response.status}, body={response.text[-300:]}")
        return response.data

    def sandbox_started_at(self, sandbox_id: str) -> float | None:
        """GET /sandboxes/{id} 回读 startedAt（epoch 秒）；失败或缺失返回 None。"""
        try:
            response = self.get_sandbox(sandbox_id)
        except Exception:
            return None
        if response.status != 200 or not isinstance(response.data, dict):
            return None
        return parse_rfc3339_epoch(response.data.get("startedAt"))

    def create_snapshot_timed(self, sandbox_id: str) -> TimedResult:
        return self.timed_request("POST", self.sandbox_path(sandbox_id, "/snapshots"), payload={})

    def pause_timed(self, sandbox_id: str, *, memory: bool = True) -> TimedResult:
        return self.timed_request(
            "POST", self.sandbox_path(sandbox_id, "/pause"), payload={"memory": memory}
        )

    def resume_timed(self, sandbox_id: str, *, timeout: int = 300) -> TimedResult:
        return self.timed_request(
            "POST", self.sandbox_path(sandbox_id, "/resume"), payload={"timeout": timeout}
        )

    def kill(self, sandbox_id: str) -> ApiResponse:
        return self.request("DELETE", self.sandbox_path(sandbox_id), timeout=60)

    def list_sandbox_items(self) -> list[dict[str, Any]]:
        """GET /v2/sandboxes：与 SDK 一致，包含 running 与 paused（v1 只含 running）。"""
        response = self.request("GET", "/v2/sandboxes", timeout=60)
        if response.status != 200 or not isinstance(response.data, list):
            raise RuntimeError(f"failed to list sandboxes: status={response.status}, body={response.text[-300:]}")
        return response.data

    def delete_snapshot(self, snapshot_id: str) -> ApiResponse:
        encoded = urllib.parse.quote(snapshot_id, safe="")
        return self.request("DELETE", f"/snapshots/{encoded}", timeout=120)

    def list_templates(self) -> list[dict[str, Any]]:
        response = self.request("GET", "/templates", timeout=60)
        if response.status != 200 or not isinstance(response.data, list):
            return []
        return response.data


def server_ms_samples(results: Iterable[TimedResult]) -> list[float]:
    """从 TimedResult 列表提取服务端真值样本（extra["server_ms"]，由 create_timed 回读写入）。"""
    samples: list[float] = []
    for item in results:
        extra = getattr(item, "extra", None)
        if isinstance(extra, dict):
            value = extra.get("server_ms")
            if isinstance(value, (int, float)):
                samples.append(float(value))
    return samples


def server_batch_span_ms(results: Iterable[TimedResult]) -> float | None:
    """整批服务端跨度（毫秒）：最晚 startedAt − 最早请求发出时刻；无有效样本返回 None。"""
    starts: list[float] = []
    ends: list[float] = []
    for item in results:
        extra = getattr(item, "extra", None)
        if not isinstance(extra, dict):
            continue
        request_wall = extra.get("request_wall")
        started_at = extra.get("started_at")
        if isinstance(request_wall, (int, float)):
            starts.append(float(request_wall))
        if isinstance(started_at, (int, float)):
            ends.append(float(started_at))
    if not starts or not ends:
        return None
    return round((max(ends) - min(starts)) * 1000, 1)


def fetch_started_at_map(
    client: BenchClient,
    sandbox_ids: Iterable[str],
) -> dict[str, float]:
    """批量回读沙箱 startedAt（epoch 秒），返回 {sandbox_id: epoch}。

    先一次 GET /sandboxes（v1，running 全量不分页）覆盖绝大多数；缺失的
    （如恰好在回读前被回收）逐个 GET 详情兜底。供计时窗外的服务端真值回读使用。
    """
    wanted = {sid for sid in sandbox_ids if sid}
    found: dict[str, float] = {}
    if not wanted:
        return found
    try:
        for item in client.list_running_items():
            if not isinstance(item, dict):
                continue
            sid = item.get("sandboxID") or item.get("sandboxId")
            if sid in wanted:
                epoch = parse_rfc3339_epoch(item.get("startedAt"))
                if epoch is not None:
                    found[sid] = epoch
    except Exception:
        pass
    missing = sorted(wanted - found.keys())
    if missing:
        with ThreadPoolExecutor(max_workers=min(16, len(missing))) as pool:
            epochs = list(pool.map(client.sandbox_started_at, missing))
        for sid, epoch in zip(missing, epochs):
            if epoch is not None:
                found[sid] = epoch
    return found
