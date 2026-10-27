"""Build an E2B template through the local API (optional snapshot-repair fallback)."""
import os
import sys
from pathlib import Path


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
