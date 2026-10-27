"""Build an E2B template through the local API (optional snapshot-repair fallback)."""
import os
import sys
from pathlib import Path


def load_env() -> None:
    """按 lib/python_env.py 的优先级加载凭证（环境变量 > secrets.env > .env > config.json）。"""
    lib_dir = str(Path(__file__).resolve().parent / "lib")
    if lib_dir not in sys.path:
        sys.path.insert(0, lib_dir)
    from python_env import load_verify_env  # noqa: E402

    load_verify_env()


load_env()

from e2b import Template, default_build_logger  # noqa: E402


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("Usage: python build_prod.py <image_name>")
        print("Example: python build_prod.py golang:1.24")
        sys.exit(1)

    image_name = sys.argv[1]
    alias = image_name.replace(":", "-").replace(".", "-")
    registry = os.environ.get(
        "E2B_TEMPLATE_IMAGE_REGISTRY", "193.12.7.2:30443/e2b-orchestration"
    )
    dockerfile = f"FROM {registry}/{image_name}"

    print(f"Building template: {alias}")
    print(f"Dockerfile: {dockerfile}")
    Template.build(
        Template().from_dockerfile(dockerfile),
        alias=alias,
        cpu_count=1,
        memory_mb=1024,
        on_build_logs=default_build_logger(),
    )
