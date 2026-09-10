"""默认基准模板：bench-standard-2c2g（2 vCPU / 2048 MiB，对标 CubeSandbox 文章口径）。

未传 -t/--template 且 bench.toml 的 global.template 为空时自动使用：
先按名称查找 ready 且本机构建产物齐全的既有模板，命中即复用；否则走 SDK
Template API 自动构建（与 e2e fixture 相同的 Harbor base image 发现机制）。
"""

from __future__ import annotations

import os
import sys
import time
from pathlib import Path
from types import SimpleNamespace
from typing import Any

from ..e2e_base_image import discover_base_image
from .client import BenchClient

BENCH_TEMPLATE_NAME = "bench-standard-2c2g"
BENCH_TEMPLATE_CPU = 2
BENCH_TEMPLATE_MEMORY_MB = 2048
BUILD_POLL_INTERVAL = 10
BUILD_TIMEOUT = 1200


def _log(message: str) -> None:
    print(f"[bench] {message}", file=sys.stderr, flush=True)


def _local_artifacts_root() -> Path:
    return Path(os.environ.get("LOCAL_TEMPLATE_STORAGE_BASE_PATH", "/tmp/templates"))


def local_artifacts_ready(build_id: str | None) -> bool:
    """template-manager/orchestrator 以 raw_exec 共享本地产物目录；空目录会导致 sandbox files not found。"""
    if not build_id:
        return False
    artifact_dir = _local_artifacts_root() / build_id
    try:
        return artifact_dir.is_dir() and any(artifact_dir.iterdir())
    except OSError:
        return False


def _template_names(template: dict[str, Any]) -> list[str]:
    names: list[str] = []
    for field in ("aliases", "names"):
        values = template.get(field)
        if isinstance(values, list):
            names.extend(str(value) for value in values if value)
    return names


def find_standard_template(client: BenchClient) -> dict[str, Any] | None:
    """按名称找 ready 且本地产物齐全的 bench-standard-2c2g。"""
    try:
        templates = client.list_templates()
    except Exception as exc:
        _log(f"查询模板列表失败：{exc}")
        return None
    for template in templates:
        if BENCH_TEMPLATE_NAME not in _template_names(template):
            continue
        status = str(template.get("buildStatus") or "").lower()
        build_id = template.get("buildID")
        if status == "ready" and local_artifacts_ready(build_id):
            return template
        _log(
            f"找到 {BENCH_TEMPLATE_NAME}（{template.get('templateID')}）但不可用："
            f"buildStatus={status or 'unknown'}, 本地产物={'就绪' if local_artifacts_ready(build_id) else '缺失'}"
        )
    return None


def _build_standard_template(base_image: str, *, skip_cache: bool) -> None:
    """复用 create_template 的 SDK 构建路径（Template.build 会阻塞到构建结束）。"""
    from e2b import Template

    from ..create_template import _build_definition, _supported_kwargs

    definition = _build_definition(
        Template,
        SimpleNamespace(dockerfile=None, dockerfile_content=None, base_image=base_image),
    )

    def _log_build(entry: Any) -> None:
        _log(f"构建日志: {entry}")

    build_options = _supported_kwargs(
        Template.build,
        {
            "cpu_count": BENCH_TEMPLATE_CPU,
            "memory_mb": BENCH_TEMPLATE_MEMORY_MB,
            "skip_cache": skip_cache,
            "on_build_logs": _log_build,
        },
    )
    import inspect

    parameters = inspect.signature(Template.build).parameters
    if "alias" in parameters and "name" not in parameters:
        Template.build(definition, alias=BENCH_TEMPLATE_NAME, **build_options)
    else:
        Template.build(definition, BENCH_TEMPLATE_NAME, **build_options)


def ensure_bench_template(client: BenchClient) -> tuple[str, str, str]:
    """返回 (template_id, name, source_label)；source_label 为 自动基准模板（复用|新建）。"""
    existing = find_standard_template(client)
    if existing:
        template_id = str(existing["templateID"])
        _log(f"复用基准模板 {template_id}（{BENCH_TEMPLATE_NAME}，2 vCPU / 2048 MiB）")
        return template_id, BENCH_TEMPLATE_NAME, "自动基准模板（复用）"

    base = discover_base_image(None, client.list_templates())
    _log(
        f"未找到可用基准模板，自动创建 {BENCH_TEMPLATE_NAME}"
        f"（{BENCH_TEMPLATE_CPU} vCPU / {BENCH_TEMPLATE_MEMORY_MB} MiB，"
        f"base image: {base.image}，来源：{base.source}）"
    )
    _build_standard_template(base.image, skip_cache=False)

    deadline = time.monotonic() + BUILD_TIMEOUT
    attempt = 0
    while time.monotonic() < deadline:
        attempt += 1
        template = find_standard_template(client)
        if template:
            template_id = str(template["templateID"])
            _log(f"基准模板构建完成并复用：{template_id}（{BENCH_TEMPLATE_NAME}）")
            return template_id, BENCH_TEMPLATE_NAME, "自动基准模板（新建）"
        _log(f"等待基准模板就绪（第 {attempt} 次检查）…")
        time.sleep(BUILD_POLL_INTERVAL)

    raise RuntimeError(
        f"基准模板 {BENCH_TEMPLATE_NAME} 构建后未在 {BUILD_TIMEOUT}s 内达到 ready 且本地产物就绪；"
        "请检查 template-manager 日志与本地产物目录 "
        f"{_local_artifacts_root()}，或改用 -t/--template 指定已有 ready 模板"
    )
