"""Small SDK compatibility helpers used by the feature scripts."""

from __future__ import annotations

import dataclasses
import argparse
import json
from datetime import date, datetime
from pathlib import Path
from typing import Any


def non_empty(value: str) -> str:
    value = value.strip()
    if not value:
        raise argparse.ArgumentTypeError("value must not be empty")
    return value


def resource_name(value: str) -> str:
    value = non_empty(value)
    if value.startswith("-"):
        raise argparse.ArgumentTypeError("resource name must not start with '-'")
    return value


def positive_int(value: str) -> int:
    try:
        parsed = int(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("value must be an integer") from exc
    if parsed <= 0:
        raise argparse.ArgumentTypeError("value must be greater than zero")
    return parsed


def connect_sandbox(sandbox_id: str):
    from e2b import Sandbox

    return Sandbox.connect(sandbox_id)


def to_plain_data(value: Any) -> Any:
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    if isinstance(value, (datetime, date, Path)):
        return str(value)
    if isinstance(value, bytes):
        return {"type": "bytes", "size": len(value)}
    if dataclasses.is_dataclass(value):
        return to_plain_data(dataclasses.asdict(value))
    if isinstance(value, dict):
        return {str(key): to_plain_data(item) for key, item in value.items()}
    if isinstance(value, (list, tuple, set)):
        return [to_plain_data(item) for item in value]
    for method_name in ("model_dump", "dict"):
        method = getattr(value, method_name, None)
        if callable(method):
            try:
                return to_plain_data(method())
            except TypeError:
                pass
    if hasattr(value, "__dict__"):
        public_values = {
            key: item
            for key, item in vars(value).items()
            if not key.startswith("_")
        }
        if public_values:
            return to_plain_data(public_values)
    return str(value)


def print_json(value: Any) -> None:
    print(json.dumps(to_plain_data(value), ensure_ascii=False, indent=2))


def parse_json_object(raw_value: str | None, option_name: str) -> dict[str, Any] | None:
    if raw_value is None:
        return None
    try:
        value = json.loads(raw_value)
    except json.JSONDecodeError as exc:
        raise ValueError(f"{option_name} must be valid JSON: {exc}") from exc
    if not isinstance(value, dict):
        raise ValueError(f"{option_name} must be a JSON object")
    return value


def collect_pages(result: Any, max_pages: int | None = None) -> list[Any]:
    if max_pages is not None and max_pages <= 0:
        raise ValueError("max_pages must be greater than zero")
    if isinstance(result, (list, tuple)):
        return list(result)
    next_items = getattr(result, "next_items", None)
    if not callable(next_items):
        return [result]

    items: list[Any] = []
    page_number = 0
    while max_pages is None or page_number < max_pages:
        page = next_items()
        page_number += 1
        if page:
            items.extend(page)
        if not getattr(result, "has_next", False):
            break
    return items
