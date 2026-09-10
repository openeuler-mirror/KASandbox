"""Mutual-exclusion lock between test-e2e and bench runs."""

from __future__ import annotations

import atexit
import json
import os
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path

LOCK_PATH = Path(__file__).resolve().parent.parent / "test-results" / ".run.lock"


class RunLockError(RuntimeError):
    """Raised when another run already holds the lock."""


def _read_lock() -> dict:
    try:
        value = json.loads(LOCK_PATH.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return value if isinstance(value, dict) else {}


def acquire_run_lock(holder: str, *, run_id: str = "", force: bool = False):
    """Create the lock file atomically; return a release callable.

    Raises RunLockError when another holder owns the lock and force is False.
    The lock is always released at interpreter exit via atexit.
    """
    LOCK_PATH.parent.mkdir(parents=True, exist_ok=True)
    payload = {
        "holder": holder,
        "run_id": run_id,
        "pid": os.getpid(),
        "started_at": datetime.now(timezone.utc).isoformat(),
    }

    def _create() -> None:
        fd = os.open(LOCK_PATH, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o644)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(payload, stream, ensure_ascii=False)

    try:
        _create()
    except FileExistsError:
        if not force:
            current = _read_lock()
            raise RunLockError(
                "检测到另一个测试运行持有互斥锁："
                f"holder={current.get('holder', 'unknown')}, "
                f"run_id={current.get('run_id', 'unknown')}, "
                f"started_at={current.get('started_at', 'unknown')}, "
                f"pid={current.get('pid', 'unknown')}。"
                "test-e2e 与 bench 不能同时运行；确认对方已结束后重试，"
                "或加 --force 删除旧锁强制继续"
            )
        try:
            LOCK_PATH.unlink()
        except OSError:
            pass
        _create()

    released = False

    def release() -> None:
        nonlocal released
        if released:
            return
        released = True
        try:
            existing = _read_lock()
            if existing.get("pid") == payload["pid"] and existing.get("started_at") == payload["started_at"]:
                LOCK_PATH.unlink()
        except OSError:
            pass

    atexit.register(release)
    return release


@contextmanager
def run_lock(holder: str, *, run_id: str = "", force: bool = False):
    release = acquire_run_lock(holder, run_id=run_id, force=force)
    try:
        yield
    finally:
        release()
