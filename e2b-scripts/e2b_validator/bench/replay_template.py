"""replay 真实轨迹的默认任务模板：django-money-task-v2（1 vCPU / 2048 MiB）。

真实轨迹模式（--trajectory-dir）未传 -t/--template 且 bench.toml global.template
为空时自动使用：先按名称查找 ready 且本地产物齐全的既有模板，命中即复用；
否则从配置的 Harbor 任务镜像自动构建（对齐 replay-aenv README §10 的模板规格）。

镜像本身不存在时的完整链路（docker build base → build task → push registry）
由 e2b-scripts/prepare-replay-image.sh 完成，本模块只负责「镜像已在 registry
可达」之后的模板创建。
"""

from __future__ import annotations

import sys
import time
from types import SimpleNamespace
from typing import Any

from .bench_template import local_artifacts_ready
from .client import BenchClient

REPLAY_TASK_TEMPLATE_NAME = "django-money-task-2c2g"
REPLAY_TASK_TEMPLATE_IMAGE = "193.30.8.2:30443/e2b-orchestration/django-money:poc_v2"
REPLAY_TASK_TEMPLATE_CPU = 2
REPLAY_TASK_TEMPLATE_MEMORY_MB = 2048
BUILD_POLL_INTERVAL = 10
BUILD_TIMEOUT = 1800


def _log(message: str) -> None:
    print(f"[bench] {message}", file=sys.stderr, flush=True)


def find_task_template(
    client: BenchClient, name: str, *, cpu: int, memory_mb: int
) -> dict[str, Any] | None:
    """按名称找 ready、本地产物齐全且规格匹配的任务模板（规格不符视为不可用）。"""
    try:
        templates = client.list_templates()
    except Exception as exc:
        _log(f"查询模板列表失败：{exc}")
        return None
    for template in templates:
        names: list[str] = []
        for field in ("aliases", "names"):
            values = template.get(field)
            if isinstance(values, list):
                names.extend(str(value) for value in values if value)
        # 名称可能带 e2b/ 前缀（API 返回值），两个写法都算命中
        if name not in names and f"e2b/{name}" not in names:
            continue
        status = str(template.get("buildStatus") or "").lower()
        build_id = template.get("buildID")
        tpl_cpu = template.get("cpuCount")
        tpl_mem = template.get("memoryMB")
        spec_match = tpl_cpu == cpu and tpl_mem == memory_mb
        if status == "ready" and local_artifacts_ready(build_id) and spec_match:
            return template
        _log(
            f"找到 {name}（{template.get('templateID')}）但不可用："
            f"buildStatus={status or 'unknown'}, 本地产物={'就绪' if local_artifacts_ready(build_id) else '缺失'}, "
            f"规格={tpl_cpu}C/{tpl_mem}MiB（要求 {cpu}C/{memory_mb}MiB）"
        )
    return None


def _build_task_template(name: str, base_image: str, cpu: int, memory_mb: int) -> None:
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
            "cpu_count": cpu,
            "memory_mb": memory_mb,
            "skip_cache": False,
            "on_build_logs": _log_build,
        },
    )
    import inspect

    parameters = inspect.signature(Template.build).parameters
    if "alias" in parameters and "name" not in parameters:
        Template.build(definition, alias=name, **build_options)
    else:
        Template.build(definition, name, **build_options)


def ensure_replay_task_template(
    client: BenchClient,
    *,
    name: str = REPLAY_TASK_TEMPLATE_NAME,
    image: str = REPLAY_TASK_TEMPLATE_IMAGE,
    cpu: int = REPLAY_TASK_TEMPLATE_CPU,
    memory_mb: int = REPLAY_TASK_TEMPLATE_MEMORY_MB,
) -> tuple[str, str, str]:
    """返回 (template_id, name, source_label)；source_label 为 自动任务模板（复用|新建）。"""
    existing = find_task_template(client, name, cpu=cpu, memory_mb=memory_mb)
    if existing:
        template_id = str(existing["templateID"])
        _log(f"复用任务模板 {template_id}（{name}，{cpu} vCPU / {memory_mb} MiB）")
        return template_id, name, "自动任务模板（复用）"

    _log(
        f"未找到可用任务模板，自动创建 {name}"
        f"（{cpu} vCPU / {memory_mb} MiB，base image: {image}）"
    )
    _build_task_template(name, image, cpu, memory_mb)

    deadline = time.monotonic() + BUILD_TIMEOUT
    attempt = 0
    while time.monotonic() < deadline:
        attempt += 1
        template = find_task_template(client, name, cpu=cpu, memory_mb=memory_mb)
        if template:
            template_id = str(template["templateID"])
            _log(f"任务模板构建完成并复用：{template_id}（{name}）")
            return template_id, name, "自动任务模板（新建）"
        _log(f"等待任务模板就绪（第 {attempt} 次检查）…")
        time.sleep(BUILD_POLL_INTERVAL)

    raise RuntimeError(
        f"任务模板 {name} 构建后未在 {BUILD_TIMEOUT}s 内达到 ready 且本地产物就绪；"
        "请检查 template-manager 日志与本地产物目录，确认 registry 中的任务镜像可达，"
        "或改用 -t/--template 指定已有 ready 模板"
    )
