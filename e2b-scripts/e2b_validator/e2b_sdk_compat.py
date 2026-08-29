"""Narrow runtime compatibility for known E2B SDK capability differences."""

from __future__ import annotations

import asyncio
import inspect
import sys
from typing import Any


_CONNECT_FALLBACK_WARNING = (
    "E2B SDK compatibility fallback active: Sandbox.connect() referenced "
    "an undefined envd_version; the installed SDK was not modified."
)
_PAUSE_FALLBACK_WARNING = (
    "E2B SDK compatibility fallback active: Sandbox.pause() is unavailable; "
    "using Sandbox.beta_pause(); the installed SDK was not modified."
)
_SNAPSHOT_FALLBACK_WARNING = (
    "E2B SDK compatibility fallback active: synchronous Snapshot APIs are "
    "unavailable; using the AsyncSandbox bridge; the installed SDK was not modified."
)
_emitted_warnings: set[str] = set()


def _is_envd_version_name_error(exc: NameError) -> bool:
    return getattr(exc, "name", None) == "envd_version" or "envd_version" in str(exc)


def _warn_once(message: str) -> None:
    if message not in _emitted_warnings:
        print(message, file=sys.stderr)
        _emitted_warnings.add(message)


def _run_async(awaitable):
    """Resolve one SDK awaitable from the synchronous E2E runner."""
    if not inspect.isawaitable(awaitable):
        return awaitable
    try:
        asyncio.get_running_loop()
    except RuntimeError:
        return asyncio.run(awaitable)

    if inspect.iscoroutine(awaitable):
        awaitable.close()
    raise RuntimeError(
        "E2B SDK compatibility bridge cannot run inside an active asyncio event loop"
    )


def _sandbox_id(sandbox: Any) -> str:
    sandbox_id = getattr(sandbox, "sandbox_id", None)
    if not isinstance(sandbox_id, str) or not sandbox_id:
        raise AttributeError("Sandbox instance does not expose a valid sandbox_id")
    return sandbox_id


def _connect_with_upstream_logic(
    sandbox_id: str,
    timeout: int | None,
    options: dict[str, Any],
):
    """Reproduce the official SDK connection constructor in project scope."""
    from e2b import Sandbox
    from e2b.api.client.types import Unset
    from e2b.connection_config import ConnectionConfig
    from e2b.sandbox_sync.sandbox_api import SandboxApi
    from packaging.version import Version

    sandbox = SandboxApi._cls_connect(
        sandbox_id=sandbox_id,
        timeout=timeout,
        **options,
    )

    sandbox_headers: dict[str, str] = {}
    envd_access_token = sandbox.envd_access_token
    if envd_access_token is not None and not isinstance(envd_access_token, Unset):
        sandbox_headers["X-Access-Token"] = envd_access_token

    connection_config = ConnectionConfig(
        extra_sandbox_headers=sandbox_headers,
        **options,
    )
    return Sandbox(
        sandbox_id=sandbox.sandbox_id,
        sandbox_domain=sandbox.domain,
        connection_config=connection_config,
        envd_version=Version(sandbox.envd_version),
        envd_access_token=envd_access_token,
        traffic_access_token=sandbox.traffic_access_token,
    )


def connect_sandbox(
    sandbox_id: str,
    *,
    timeout: int | None = None,
    **options: Any,
):
    """Use the native SDK connection and handle only the known envd defect."""
    from e2b import Sandbox

    try:
        return Sandbox.connect(sandbox_id, timeout=timeout, **options)
    except NameError as exc:
        if not _is_envd_version_name_error(exc):
            raise
        _warn_once(_CONNECT_FALLBACK_WARNING)
        return _connect_with_upstream_logic(sandbox_id, timeout, options)


def pause_sandbox(sandbox: Any, **options: Any):
    """Prefer Sandbox.pause() and use beta_pause() only when pause is absent."""
    native = getattr(sandbox, "pause", None)
    if callable(native):
        return native(**options)

    fallback = getattr(sandbox, "beta_pause", None)
    if callable(fallback):
        _warn_once(_PAUSE_FALLBACK_WARNING)
        return fallback(**options)

    raise AttributeError(
        "Installed E2B SDK exposes neither Sandbox.pause() nor Sandbox.beta_pause()"
    )


def create_snapshot(sandbox: Any, **options: Any):
    """Prefer the synchronous Snapshot API and bridge to AsyncSandbox if absent."""
    native = getattr(sandbox, "create_snapshot", None)
    if callable(native):
        return native(**options)

    from e2b import AsyncSandbox

    fallback = getattr(AsyncSandbox, "create_snapshot", None)
    if not callable(fallback):
        raise AttributeError(
            "Installed E2B SDK exposes neither Sandbox.create_snapshot() "
            "nor AsyncSandbox.create_snapshot()"
        )
    _warn_once(_SNAPSHOT_FALLBACK_WARNING)
    return _run_async(fallback(_sandbox_id(sandbox), **options))


def list_snapshot_items(sandbox: Any, **options: Any) -> list[Any]:
    """Return the first Snapshot page through the available SDK implementation."""
    native = getattr(sandbox, "list_snapshots", None)
    if callable(native):
        paginator = native(**options)
    else:
        from e2b import AsyncSandbox

        fallback = getattr(AsyncSandbox, "list_snapshots", None)
        if not callable(fallback):
            raise AttributeError(
                "Installed E2B SDK exposes neither Sandbox.list_snapshots() "
                "nor AsyncSandbox.list_snapshots()"
            )
        _warn_once(_SNAPSHOT_FALLBACK_WARNING)
        paginator = fallback(_sandbox_id(sandbox), **options)

    paginator = _run_async(paginator)
    next_items = getattr(paginator, "next_items", None)
    if not callable(next_items):
        raise AttributeError("Snapshot paginator does not expose next_items()")
    return _run_async(next_items())


def delete_snapshot(snapshot_id: str, **options: Any) -> bool:
    """Prefer Sandbox.delete_snapshot() and bridge to AsyncSandbox if absent."""
    from e2b import AsyncSandbox, Sandbox

    native = getattr(Sandbox, "delete_snapshot", None)
    if callable(native):
        return bool(native(snapshot_id, **options))

    fallback = getattr(AsyncSandbox, "delete_snapshot", None)
    if not callable(fallback):
        raise AttributeError(
            "Installed E2B SDK exposes neither Sandbox.delete_snapshot() "
            "nor AsyncSandbox.delete_snapshot()"
        )
    _warn_once(_SNAPSHOT_FALLBACK_WARNING)
    return bool(_run_async(fallback(snapshot_id, **options)))
