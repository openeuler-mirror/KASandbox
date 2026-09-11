"""Load, merge, validate, and render bench.toml configuration.

优先级：命令行参数 > --config 指定文件 > 默认 bench.toml > 代码内置兜底（BUILTIN）。
"""

from __future__ import annotations

import copy
import os
import tomllib
from pathlib import Path
from typing import Any

DEFAULT_CONFIG_PATH = Path(__file__).resolve().parent.parent.parent / "bench.toml"

BENCH_SECTIONS = (
    "create",
    "scale",
    "density",
    "snapshot_concurrency",
    "snapshot_dirty",
    "create_from_snapshot",
    "rollback",
    "clone",
    "pause_resume",
)

BUILTIN: dict[str, Any] = {
    "global": {
        "template": "",
        "warmup": 0,
        "sandbox_timeout": 600,
        "mem_threshold_pct": 15.0,
        "result_root": "test-results",
        "netns_growth_threshold": 100,
    },
    "create": {
        "tiers": [
            {"concurrency": 1, "requests": 20},
            {"concurrency": 10, "requests": 200},
            {"concurrency": 20, "requests": 300, "pre_wait": 180},
            {"concurrency": 50, "requests": 500, "pre_wait": 180},
        ],
    },
    "scale": {
        "tiers": [
            {"size": 1},
            {"size": 100},
            {"size": 200, "pre_wait": 180},
        ],
        "rounds": 3,
    },
    "density": {"batch_size": 50, "max_sandboxes": 500},
    "snapshot_concurrency": {
        "tiers": [
            {"concurrency": 1, "rounds": 5},
            {"concurrency": 5, "rounds": 5},
            {"concurrency": 10, "rounds": 5},
        ],
    },
    "snapshot_dirty": {
        "tiers": [
            {"dirty_mb": value, "rounds": 3}
            for value in (0, 10, 50, 100, 200, 500, 800, 1024)
        ],
    },
    "create_from_snapshot": {
        "tiers": [
            {"concurrency": 1, "rounds": 3},
            {"concurrency": 10, "rounds": 3},
            {"concurrency": 20, "rounds": 3},
            {"concurrency": 50, "rounds": 3},
        ],
    },
    "rollback": {
        "tiers": [
            {"concurrency": 1, "rounds": 5},
            {"concurrency": 5, "rounds": 5},
            {"concurrency": 10, "rounds": 5},
        ],
    },
    "clone": {
        "tiers": [
            {"n": 1, "concurrency": 1, "rounds": 5},
            {"n": 100, "concurrency": 10, "rounds": 2},
            {"n": 100, "concurrency": 20, "rounds": 2},
            {"n": 100, "concurrency": 50, "rounds": 2},
        ],
    },
    "pause_resume": {
        "tiers": [
            {"concurrency": 1, "rounds": 5},
            {"concurrency": 5, "rounds": 5},
            {"concurrency": 10, "rounds": 5},
        ],
    },
    "profiles": {
        "quick": {
            "create": {"tiers": [{"concurrency": 1, "requests": 10}, {"concurrency": 10, "requests": 20}]},
            "scale": {"tiers": [{"size": 1}, {"size": 10}], "rounds": 2},
            "density": {"batch_size": 10, "max_sandboxes": 20},
            "snapshot_concurrency": {"tiers": [{"concurrency": 1, "rounds": 3}, {"concurrency": 5, "rounds": 3}]},
            "snapshot_dirty": {"tiers": [{"dirty_mb": 0, "rounds": 2}, {"dirty_mb": 50, "rounds": 2}]},
            "create_from_snapshot": {"tiers": [{"concurrency": 1, "rounds": 2}, {"concurrency": 10, "rounds": 2}]},
            "rollback": {"tiers": [{"concurrency": 1, "rounds": 3}, {"concurrency": 5, "rounds": 3}]},
            "clone": {"tiers": [{"n": 1, "concurrency": 1, "rounds": 3}, {"n": 5, "concurrency": 5, "rounds": 2}]},
            "pause_resume": {"tiers": [{"concurrency": 1, "rounds": 2}, {"concurrency": 5, "rounds": 2}]},
        },
        "full": {},
    },
}

SECTION_ORDER = ("global",) + BENCH_SECTIONS


def _deep_merge(base: dict[str, Any], override: dict[str, Any]) -> dict[str, Any]:
    merged = copy.deepcopy(base)
    for key, value in override.items():
        if isinstance(value, dict) and isinstance(merged.get(key), dict):
            merged[key] = _deep_merge(merged[key], value)
        else:
            merged[key] = copy.deepcopy(value)
    return merged


def load_file(path: Path | None) -> dict[str, Any]:
    config_path = (path or DEFAULT_CONFIG_PATH).expanduser().resolve()
    if not config_path.is_file():
        if path is not None:
            raise FileNotFoundError(f"bench 配置文件不存在：{config_path}")
        return {}
    try:
        with config_path.open("rb") as stream:
            data = tomllib.load(stream)
    except tomllib.TOMLDecodeError as exc:
        raise ValueError(f"bench 配置文件解析失败：{config_path}: {exc}") from exc
    if not isinstance(data, dict):
        raise ValueError(f"bench 配置文件必须是 TOML 表：{config_path}")
    return data


def load(config_path: Path | None = None, *, profile: str | None = None) -> dict[str, Any]:
    """合并内置兜底、bench.toml 基础节、以及指定 profile 的档位覆盖。"""
    file_config = load_file(config_path)
    base = {key: value for key, value in file_config.items() if key != "profiles"}
    file_profile = (file_config.get("profiles") or {}).get(profile) if profile else None
    builtin_profile = BUILTIN["profiles"].get(profile or "") or {}

    merged = copy.deepcopy(BUILTIN)
    merged.pop("profiles", None)
    merged = _deep_merge(merged, builtin_profile)
    merged = _deep_merge(merged, base)
    if file_profile:
        merged = _deep_merge(merged, file_profile)
    if profile:
        merged["profile"] = profile
    return merged


def resolve_template(cli_value: str | None, config: dict[str, Any]) -> str:
    """模板 ID：-t/--template > 配置 global.template > 环境变量 BENCH_TEMPLATE_ID。

    全部为空时返回 ""，由 build_context 自动使用 2U2G 标准基准模板
    bench-standard-2c2g（查找复用或自动构建）。
    """
    return (
        (cli_value or "").strip()
        or str(config.get("global", {}).get("template") or "").strip()
        or os.environ.get("BENCH_TEMPLATE_ID", "").strip()
    )


def resolve_result_root(config: dict[str, Any]) -> Path:
    raw = str(config.get("global", {}).get("result_root") or "test-results")
    path = Path(raw).expanduser()
    if not path.is_absolute():
        path = Path(__file__).resolve().parent.parent.parent / path
    return path.resolve()


def global_param(config: dict[str, Any], key: str) -> Any:
    return config.get("global", {}).get(key, BUILTIN["global"].get(key))


def tiers_for(config: dict[str, Any], section: str, cli_tier: dict[str, Any] | None = None) -> list[dict[str, Any]]:
    """CLI 显式档位整体覆盖配置档位；否则使用配置中的 tiers。"""
    if cli_tier is not None:
        return [dict(cli_tier)]
    tiers = config.get(section, {}).get("tiers")
    if not tiers:
        raise ValueError(f"配置缺少 [{section}] tiers；请在 bench.toml 中补齐或通过命令行指定档位")
    return [dict(tier) for tier in tiers]


def scale_tiers(config: dict[str, Any]) -> list[dict[str, Any]]:
    """scale 档位：[[scale.tiers]]（size + 可选 pre_wait）或旧的 sizes 数组。

    sizes 只会由用户配置显式给出（内置与 profiles 均用 tiers），
    因此两者同时存在时以 sizes 为准（用户显式意图优先）。
    """
    section = config.get("scale", {})
    sizes = section.get("sizes")
    if sizes:
        return [{"size": int(size)} for size in sizes]
    tiers = section.get("tiers")
    if tiers:
        return [dict(tier) for tier in tiers]
    return [dict(tier) for tier in BUILTIN["scale"]["tiers"]]


def _toml_scalar(value: Any) -> str:
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, str):
        escaped = value.replace("\\", "\\\\").replace('"', '\\"')
        return f'"{escaped}"'
    if isinstance(value, float):
        return repr(value)
    return str(value)


def _toml_value(value: Any) -> str:
    if isinstance(value, list) and not any(isinstance(item, (dict, list)) for item in value):
        return "[" + ", ".join(_toml_scalar(item) for item in value) + "]"
    return _toml_scalar(value)


def dump_toml(config: dict[str, Any]) -> str:
    """把合并后的生效配置回显为 TOML（不含 profiles 节）。"""
    lines: list[str] = []
    profile = config.get("profile")
    lines.append(
        f"# bench 生效配置（profile={profile or 'default'}）"
        "——由 内置兜底 < bench.toml < profile 覆盖 < 命令行 合并而来"
    )
    for section in SECTION_ORDER:
        body = config.get(section)
        if not isinstance(body, dict):
            continue
        lines.append("")
        lines.append(f"[{section}]")
        for key, value in body.items():
            if isinstance(value, list) and value and all(isinstance(item, dict) for item in value):
                continue
            lines.append(f"{key} = {_toml_value(value)}")
        for key, value in body.items():
            if isinstance(value, list) and value and all(isinstance(item, dict) for item in value):
                for item in value:
                    lines.append("")
                    lines.append(f"[[{section}.{key}]]")
                    for item_key, item_value in item.items():
                        lines.append(f"{item_key} = {_toml_value(item_value)}")
    return "\n".join(lines) + "\n"
