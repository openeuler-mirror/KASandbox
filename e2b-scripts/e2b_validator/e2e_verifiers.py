"""Independent parsing and verification helpers for E2B E2E tests."""

from __future__ import annotations

import hashlib
import json
import time
from pathlib import Path
from typing import Any, Callable


def extract_last_json(output: str) -> Any:
    decoder = json.JSONDecoder()
    candidates: list[tuple[int, int, Any]] = []
    for index, character in enumerate(output):
        if character not in "[{":
            continue
        try:
            value, relative_end = decoder.raw_decode(output[index:])
        except json.JSONDecodeError:
            continue
        candidates.append((index, index + relative_end, value))
    if not candidates:
        raise ValueError("Command output does not contain JSON")

    complete_documents = [item for item in candidates if not output[item[1]:].strip()]
    if complete_documents:
        return min(complete_documents, key=lambda item: item[0])[2]

    furthest_end = max(item[1] for item in candidates)
    return min((item for item in candidates if item[1] == furthest_end), key=lambda item: item[0])[2]


def extract_resource_id(data: Any, resource_type: str) -> str | None:
    if not isinstance(data, dict):
        return None
    fields = {
        "sandbox": ("sandbox_id", "sandboxId", "sandboxID", "id"),
        "template": ("template_id", "templateId", "templateID", "id"),
    }.get(resource_type, ())
    for field in fields:
        value = data.get(field)
        if isinstance(value, str) and value:
            return value
    for value in data.values():
        nested = extract_resource_id(value, resource_type)
        if nested:
            return nested
    return None


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def plain_items(value: Any) -> list[Any]:
    if isinstance(value, dict):
        for key in ("sandboxes", "templates", "items"):
            items = value.get(key)
            if isinstance(items, list):
                return items
    return value if isinstance(value, list) else []


def item_has_id(items: Any, resource_id: str, resource_type: str) -> bool:
    return any(extract_resource_id(item, resource_type) == resource_id for item in plain_items(items))


def wait_until(predicate: Callable[[], bool], timeout: float, interval: float = 1.0) -> tuple[bool, float]:
    started = time.monotonic()
    while True:
        if predicate():
            return True, time.monotonic() - started
        elapsed = time.monotonic() - started
        if elapsed >= timeout:
            return False, elapsed
        time.sleep(min(interval, max(0.0, timeout - elapsed)))


def kill_sandbox(sandbox_id: str) -> bool:
    from e2b import Sandbox

    class_kill = getattr(Sandbox, "_cls_kill", None)
    if callable(class_kill):
        result = class_kill(sandbox_id)
        return result is not False
    sandbox = Sandbox.connect(sandbox_id)
    result = sandbox.kill()
    return result is not False
