"""Shared SDK helpers for extended E2E cases."""

from __future__ import annotations

import json
import logging
import os
from contextlib import contextmanager
from typing import Any

from .e2e_models import CaseStatus
from .e2b_sdk_compat import connect_sandbox
from .self_hosted_runtime import sandbox_url_for


CAPABILITY_PATTERNS = (
    "not supported",
    "not implemented",
    "unimplemented",
    "unsupported",
    "update the template",
    "rebuild your template",
    "requires envd",
    "envd version",
    "template builder not found",
    "no available build client",
    "failed to get builder client",
    "failed to place sandbox",
    "failed to schedule sandbox",
    "feature is disabled",
)


@contextmanager
def suppress_expected_sdk_status(*status_codes: int):
    """Hide only the SDK's generic log line for an explicitly expected status."""
    messages = {f"Response {status}" for status in status_codes}

    class ExpectedStatusFilter(logging.Filter):
        def filter(self, record: logging.LogRecord) -> bool:
            return record.getMessage() not in messages

    logger = logging.getLogger("e2b.api")
    status_filter = ExpectedStatusFilter()
    logger.addFilter(status_filter)
    try:
        yield
    finally:
        logger.removeFilter(status_filter)


def evidence(value: Any, *, limit: int = 3000) -> str:
    """Serialize evidence without letting one SDK response dominate the report."""
    try:
        rendered = json.dumps(value, ensure_ascii=False, default=str, indent=2)
    except (TypeError, ValueError):
        rendered = str(value)
    return rendered[-limit:]


def capability_blocked(exc: BaseException | str) -> bool:
    text = str(exc).lower()
    return any(pattern in text for pattern in CAPABILITY_PATTERNS)


def blocked_outcome(exc: BaseException | str):
    return CaseStatus.BLOCKED, f"SDK capability unavailable: {exc}", [str(exc)[-3000:]]


def run_path(context: dict[str, object], *parts: str) -> str:
    run_id = str(context["run_id"])
    suffix = "/".join(part.strip("/") for part in parts if part)
    return f"/tmp/e2e-sdk-{run_id}" + (f"/{suffix}" if suffix else "")


def sdk_options(*, sandbox_id: str | None = None) -> dict[str, object]:
    """Build explicit SDK options so each Sandbox gets its own proxy route."""
    options: dict[str, object] = {
        "api_key": os.environ.get("E2B_API_KEY"),
        "api_url": os.environ.get("E2B_API_URL"),
        "domain": os.environ.get("E2B_DOMAIN"),
    }
    options = {key: value for key, value in options.items() if value}
    if sandbox_id:
        domain = os.environ.get("E2B_DOMAIN", "").strip()
        if not domain:
            raise ValueError("E2B_DOMAIN is required for SDK Sandbox routing")
        try:
            port = int(os.environ.get("E2B_PROXY_PORT", "3002"))
        except ValueError as exc:
            raise ValueError("E2B_PROXY_PORT must be an integer") from exc
        use_ssl = os.environ.get("E2B_HTTP_SSL", "false").lower() in {"1", "true", "yes", "on"}
        options["sandbox_url"] = sandbox_url_for(sandbox_id, domain, port=port, use_ssl=use_ssl)
    return options


def sdk_sandbox(owner, sandbox_id: str | None = None, *, refresh: bool = False):
    """Connect to the shared or an owned Sandbox with a per-ID route."""
    target_id = sandbox_id or owner._main_sandbox()
    cache = owner.context.setdefault("sdk_sandboxes", {})
    if not isinstance(cache, dict):
        raise RuntimeError("invalid SDK Sandbox cache")
    if refresh or target_id not in cache:
        cache[target_id] = connect_sandbox(
            target_id,
            timeout=900,
            **sdk_options(sandbox_id=target_id),
        )
    return cache[target_id]


def create_sdk_sandbox(owner, *, case_id: str, **create_options):
    """Create an isolated SDK Sandbox and record only this run's resource."""
    from e2b import Sandbox

    sandbox = Sandbox.create(**create_options, **sdk_options())
    owner.ledger.record_sandbox(sandbox.sandbox_id, case_id)
    sandbox_ids = owner.context.setdefault("sandbox_ids", [])
    if isinstance(sandbox_ids, list):
        sandbox_ids.append(sandbox.sandbox_id)
    return sdk_sandbox(owner, sandbox.sandbox_id, refresh=True)


def result_fields(result: Any) -> dict[str, object]:
    return {
        "stdout": getattr(result, "stdout", ""),
        "stderr": getattr(result, "stderr", ""),
        "exit_code": getattr(result, "exit_code", None),
        "error": getattr(result, "error", None),
    }
