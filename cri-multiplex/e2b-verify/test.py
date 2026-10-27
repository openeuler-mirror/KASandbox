"""Create a sandbox through the local E2B API (snapshot repair smoke test)."""
import os
import sys
import time
from pathlib import Path

from e2b import Sandbox


def load_env() -> None:
    """按 lib/python_env.py 的优先级加载凭证（环境变量 > secrets.env > .env > config.json）。"""
    lib_dir = str(Path(__file__).resolve().parent / "lib")
    if lib_dir not in sys.path:
        sys.path.insert(0, lib_dir)
    from python_env import load_verify_env  # noqa: E402

    load_verify_env()


load_env()
alias = os.environ.get("E2B_TEMPLATE_ALIAS", "ubuntu-22-04-custom-1")
original_time = time.time()
sbx = Sandbox.create(alias, timeout=120)
try:
    print(time.time() - original_time)
    print(sbx.is_running())
    print(sbx.commands.run("whoami"))
finally:
    sbx.kill()
