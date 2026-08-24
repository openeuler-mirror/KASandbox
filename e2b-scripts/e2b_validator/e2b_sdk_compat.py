"""Project-scoped compatibility for known E2B SDK connection defects."""

from __future__ import annotations

import sys
from typing import Any


_FALLBACK_WARNING = (
    "E2B SDK compatibility fallback active: Sandbox.connect() referenced "
    "an undefined envd_version; the installed SDK was not modified."
)
_fallback_warning_emitted = False


def _is_envd_version_name_error(exc: NameError) -> bool:
    return getattr(exc, "name", None) == "envd_version" or "envd_version" in str(exc)


def _warn_fallback() -> None:
    global _fallback_warning_emitted
    if not _fallback_warning_emitted:
        print(_FALLBACK_WARNING, file=sys.stderr)
        _fallback_warning_emitted = True


def _connect_with_upstream_logic(
    sandbox_id: str,
    timeout: int | None,
    options: dict[str, Any],
):
    """Construct the Sandbox using the official E2B 2.20.0 connection logic."""
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
    """Use native connect first and handle only the known envd_version NameError."""
    from e2b import Sandbox

    try:
        return Sandbox.connect(sandbox_id, timeout=timeout, **options)
    except NameError as exc:
        if not _is_envd_version_name_error(exc):
            raise
        _warn_fallback()
        return _connect_with_upstream_logic(sandbox_id, timeout, options)
