"""Environment configuration shared by all E2B test scripts."""

from __future__ import annotations

import argparse
import os
import re
import shlex
from pathlib import Path


DEFAULT_ENV_FILE = Path(__file__).resolve().parent.parent / ".env"
ENV_NAME_PATTERN = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def add_config_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "--env-file",
        type=Path,
        help="Path to an env file. Defaults to e2b_test_script/.env when present.",
    )


def parse_env_file(path: Path, *, required: bool = True) -> dict[str, str]:
    path = path.expanduser().resolve()
    if not path.is_file():
        if required:
            raise FileNotFoundError(f"Environment file does not exist: {path}")
        return {}

    values: dict[str, str] = {}
    for line_number, raw_line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[7:].lstrip()
        if "=" not in line:
            raise ValueError(f"Invalid env entry at {path}:{line_number}")
        key, raw_value = line.split("=", 1)
        key = key.strip()
        if not ENV_NAME_PATTERN.fullmatch(key):
            raise ValueError(f"Invalid env name at {path}:{line_number}: {key or '<empty>'}")
        try:
            parsed_values = shlex.split(raw_value, comments=True, posix=True)
        except ValueError as exc:
            raise ValueError(f"Invalid env value at {path}:{line_number}: {exc}") from exc
        value = " ".join(parsed_values)
        values[key] = value
    return values


def load_env_file(path: Path, *, required: bool = True) -> None:
    for key, value in parse_env_file(path, required=required).items():
        os.environ.setdefault(key, value)


def configure_environment(env_file: Path | None = None) -> None:
    if env_file is not None:
        load_env_file(env_file)
    else:
        load_env_file(DEFAULT_ENV_FILE, required=False)


def require_api_key() -> str:
    api_key = os.getenv("E2B_API_KEY")
    if not api_key:
        raise ValueError(
            "E2B_API_KEY is required. For self-hosted auto-discovery, run the same "
            "command through 'bash start.sh <subcommand> ...'. Otherwise set the key "
            "in the environment or pass a configured --env-file."
        )
    return api_key


def api_base_url() -> str:
    return os.getenv("E2B_API_URL", "https://api.e2b.app").rstrip("/")
