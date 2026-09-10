"""Timed REST client for benchmark operations on the E2B API."""

from __future__ import annotations

import time
import urllib.parse
from dataclasses import dataclass, field
from typing import Any

from ..e2e_api_client import ApiResponse, E2BApiClient, sandbox_id_from


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
        return self.timed_request("POST", "/sandboxes", payload=payload, timeout=request_timeout)

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
