"""Small REST client for lifecycle assertions not exposed by the Python SDK."""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class ApiResponse:
    status: int
    data: Any
    text: str
    headers: dict[str, str]


class E2BApiClient:
    def __init__(self, *, timeout: float = 120):
        api_url = os.environ.get("E2B_API_URL", "").strip().rstrip("/")
        api_key = os.environ.get("E2B_API_KEY", "").strip()
        if not api_url:
            raise ValueError("E2B_API_URL is required for lifecycle REST tests")
        if not api_key:
            raise ValueError("E2B_API_KEY is required for lifecycle REST tests")
        self.api_url = api_url
        self.api_key = api_key
        self.timeout = timeout

    def request(
        self,
        method: str,
        path: str,
        *,
        payload: dict[str, Any] | None = None,
        query: dict[str, str] | None = None,
        timeout: float | None = None,
    ) -> ApiResponse:
        url = f"{self.api_url}/{path.lstrip('/')}"
        if query:
            url = f"{url}?{urllib.parse.urlencode(query)}"
        body = None if payload is None else json.dumps(payload).encode("utf-8")
        headers = {
            "Accept": "application/json",
            "X-API-Key": self.api_key,
        }
        if body is not None:
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(url, data=body, headers=headers, method=method.upper())
        try:
            with urllib.request.urlopen(request, timeout=timeout or self.timeout) as response:
                return self._response(response.status, response.read(), response.headers)
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            response = self._response(exc.code, raw, exc.headers)
            if exc.code >= 500:
                raise RuntimeError(f"E2B API {method.upper()} {path} returned {exc.code}: {response.text}") from exc
            return response
        except urllib.error.URLError as exc:
            raise RuntimeError(f"E2B API {method.upper()} {path} failed: {exc.reason}") from exc

    @staticmethod
    def _response(status: int, raw: bytes, headers) -> ApiResponse:
        text = raw.decode("utf-8", "replace")
        data: Any = None
        if text.strip():
            try:
                data = json.loads(text)
            except json.JSONDecodeError:
                data = None
        return ApiResponse(
            status=status,
            data=data,
            text=text,
            headers={key: value for key, value in headers.items()} if headers is not None else {},
        )

    @staticmethod
    def sandbox_path(sandbox_id: str, suffix: str = "") -> str:
        encoded = urllib.parse.quote(sandbox_id, safe="")
        return f"/sandboxes/{encoded}{suffix}"

    def create_sandbox(self, payload: dict[str, Any]) -> ApiResponse:
        return self.request("POST", "/sandboxes", payload=payload)

    def get_sandbox(self, sandbox_id: str) -> ApiResponse:
        return self.request("GET", self.sandbox_path(sandbox_id))

    def pause(self, sandbox_id: str, *, memory: bool = True) -> ApiResponse:
        return self.request("POST", self.sandbox_path(sandbox_id, "/pause"), payload={"memory": memory})

    def resume(self, sandbox_id: str, *, timeout: int = 120) -> ApiResponse:
        return self.request("POST", self.sandbox_path(sandbox_id, "/resume"), payload={"timeout": timeout})

    def connect(self, sandbox_id: str, *, timeout: int = 120) -> ApiResponse:
        return self.request("POST", self.sandbox_path(sandbox_id, "/connect"), payload={"timeout": timeout})

    def set_timeout(self, sandbox_id: str, *, timeout: int) -> ApiResponse:
        return self.request("POST", self.sandbox_path(sandbox_id, "/timeout"), payload={"timeout": timeout})

    def list_snapshots(self, *, sandbox_id: str | None = None) -> ApiResponse:
        query = {"sandboxID": sandbox_id} if sandbox_id else None
        return self.request("GET", "/snapshots", query=query)


def sandbox_id_from(value: Any) -> str | None:
    if not isinstance(value, dict):
        return None
    for field in ("sandboxID", "sandboxId", "sandbox_id", "id"):
        candidate = value.get(field)
        if isinstance(candidate, str) and candidate:
            return candidate
    return None


def sandbox_state_from(value: Any) -> str | None:
    if not isinstance(value, dict):
        return None
    state = value.get("state")
    return state.lower() if isinstance(state, str) else None


def snapshot_ids_from(value: Any) -> list[str]:
    items = value
    if isinstance(value, dict):
        items = value.get("snapshots", value.get("items", []))
    if not isinstance(items, list):
        return []
    snapshot_ids: list[str] = []
    for item in items:
        if not isinstance(item, dict):
            continue
        for field in ("snapshotID", "snapshotId", "snapshot_id", "id"):
            candidate = item.get(field)
            if isinstance(candidate, str) and candidate:
                snapshot_ids.append(candidate)
                break
    return snapshot_ids
