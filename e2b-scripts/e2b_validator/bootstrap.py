"""Dependency installation and local validation for the one-command launcher."""

from __future__ import annotations

import compileall
import importlib.metadata
import re
import subprocess
import sys
from pathlib import Path


PROJECT_DIR = Path(__file__).resolve().parent.parent
PACKAGE_DIR = Path(__file__).resolve().parent
MINIMUM_PYTHON = (3, 10)
MINIMUM_E2B = (2, 19, 0)
E2B_REQUIREMENT = "e2b>=2.19.0,<3"


def check_python_version() -> None:
    if sys.version_info < MINIMUM_PYTHON:
        required = ".".join(map(str, MINIMUM_PYTHON))
        current = ".".join(map(str, sys.version_info[:3]))
        raise RuntimeError(f"Python {required}+ is required; current version is {current}")


def _version_tuple(raw_version: str) -> tuple[int, ...]:
    parts: list[int] = []
    for part in raw_version.split("."):
        match = re.match(r"\d+", part)
        if not match:
            break
        parts.append(int(match.group(0)))
    return tuple(parts)


def e2b_dependency_ready() -> bool:
    try:
        installed = importlib.metadata.version("e2b")
    except importlib.metadata.PackageNotFoundError:
        return False
    return _version_tuple(installed) >= MINIMUM_E2B


def ensure_dependencies(*, skip_install: bool = False) -> None:
    if e2b_dependency_ready():
        return
    if skip_install:
        raise RuntimeError("E2B SDK 2.19+ is not installed and dependency installation was disabled")
    completed = subprocess.run(
        [sys.executable, "-m", "pip", "install", E2B_REQUIREMENT],
        cwd=PROJECT_DIR,
        check=False,
    )
    if completed.returncode != 0:
        raise RuntimeError(f"Dependency installation failed with exit code {completed.returncode}")


def compile_sources() -> None:
    if not compileall.compile_file(PROJECT_DIR / "start.py", quiet=1):
        raise RuntimeError("Python source compilation failed")
    if not compileall.compile_dir(PACKAGE_DIR, quiet=1):
        raise RuntimeError("Python package compilation failed")
