"""Create a sandbox through the local E2B API (snapshot repair smoke test)."""
import os
import time
from pathlib import Path

from e2b import Sandbox


def load_env() -> None:
    env_path = Path(__file__).with_name(".env")
    if not env_path.exists():
        return
    for raw in env_path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        os.environ.setdefault(key.strip(), value.strip().strip('"').strip("'"))


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
