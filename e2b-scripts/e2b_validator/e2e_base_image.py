"""Discover a reusable container image for isolated E2E template builds."""

from __future__ import annotations

import os
import re
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

from .e2b_config import parse_env_file


DEFAULT_DEPLOYMENT_FILES = (
    Path("/opt/e2b-infra/dep/.env"),
    Path("/opt/e2b-infra/dep/template-manager.hcl"),
    Path("/opt/e2b-infra/dep/template-manager.yaml"),
    Path("/opt/e2b-infra/rendered/template-manager.hcl"),
)
IMAGE_KEY_PATTERN = re.compile(
    r"(?:^|[_.-])(?:base|source|default|e2e)[_.-]?image(?:$|[_.-])|"
    r"^(?:baseImage|sourceImage|defaultImage|e2eBaseImage)$",
    re.IGNORECASE,
)
IMAGE_VALUE_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/@:-]*$")
ASSIGNMENT_PATTERN = re.compile(
    r"(?im)^\s*(?P<key>[A-Za-z_][A-Za-z0-9_.-]*image[A-Za-z0-9_.-]*)\s*[:=]\s*['\"]?(?P<value>[^'\"\s,#}]+)"
)
FROM_PATTERN = re.compile(r"(?im)^\s*FROM\s+(?P<value>[^\s]+)")


class BaseImageDiscoveryError(RuntimeError):
    """Raised when isolated E2E testing has no reliable source image."""


@dataclass(frozen=True)
class BaseImageResolution:
    image: str
    source: str


def _is_placeholder(value: str) -> bool:
    normalized = value.strip().lower()
    return not normalized or normalized.startswith("replace_") or (
        normalized.startswith("<") and normalized.endswith(">")
    )


def _is_image_reference(value: Any) -> bool:
    if not isinstance(value, str):
        return False
    value = value.strip()
    if _is_placeholder(value) or not IMAGE_VALUE_PATTERN.fullmatch(value):
        return False
    # A bare image name is valid for Docker, but arbitrary words in metadata are not.
    return "/" in value or ":" in value or value in {"ubuntu", "debian", "alpine", "busybox"}


def _add_candidate(candidates: list[BaseImageResolution], image: Any, source: str) -> None:
    if not _is_image_reference(image):
        return
    normalized = str(image).strip()
    if all(candidate.image != normalized for candidate in candidates):
        candidates.append(BaseImageResolution(normalized, source))


def _template_image_candidates(value: Any, *, path: str = "template") -> Iterable[BaseImageResolution]:
    if isinstance(value, dict):
        for key, child in value.items():
            child_path = f"{path}.{key}"
            if IMAGE_KEY_PATTERN.search(str(key)) and _is_image_reference(child):
                yield BaseImageResolution(str(child).strip(), child_path)
            yield from _template_image_candidates(child, path=child_path)
    elif isinstance(value, list):
        for index, child in enumerate(value):
            yield from _template_image_candidates(child, path=f"{path}[{index}]")


def _deployment_image_candidates(path: Path) -> Iterable[BaseImageResolution]:
    if not path.is_file():
        return
    try:
        if path.name == ".env":
            values = parse_env_file(path)
            for key, value in values.items():
                if IMAGE_KEY_PATTERN.search(key) and _is_image_reference(value):
                    yield BaseImageResolution(value.strip(), f"{path}:{key}")
            return
        text = path.read_text(encoding="utf-8", errors="replace")
    except (OSError, ValueError):
        return
    for match in ASSIGNMENT_PATTERN.finditer(text):
        key = match.group("key")
        value = match.group("value")
        if IMAGE_KEY_PATTERN.search(key) and _is_image_reference(value):
            yield BaseImageResolution(value, f"{path}:{key}")
    for match in FROM_PATTERN.finditer(text):
        value = match.group("value")
        if _is_image_reference(value):
            yield BaseImageResolution(value, f"{path}:FROM")


def _expand_deployment_values(values: dict[str, str]) -> dict[str, str]:
    """Expand the simple $NAME references commonly used by deployment .env files."""
    expanded = dict(values)
    variable = re.compile(r"\$(?:\{(?P<braced>[A-Za-z_][A-Za-z0-9_]*)\}|(?P<plain>[A-Za-z_][A-Za-z0-9_]*))")
    for _ in range(len(expanded) + 1):
        changed = False
        for key, value in tuple(expanded.items()):
            replacement = variable.sub(
                lambda match: expanded.get(match.group("braced") or match.group("plain"), match.group(0)),
                value,
            )
            if replacement != value:
                expanded[key] = replacement
                changed = True
        if not changed:
            break
    return expanded


def _registry_prefixes(values: dict[str, str]) -> tuple[str, ...]:
    project = values.get("HARBOR_PROJECT") or values.get("REGISTRY_PROJECT")
    if not project:
        return ()
    hosts = []
    for key in ("REGISTRY_URL", "HARBOR_HOST"):
        value = values.get(key, "").strip().rstrip("/")
        if value and value not in hosts:
            hosts.append(value)
    server_ip = values.get("SERVER_IP", "").strip()
    https_port = values.get("HARBOR_HTTPS_PORT", "").strip()
    if server_ip and https_port:
        host = f"{server_ip}:{https_port}"
        if host not in hosts:
            hosts.insert(0, host)
    return tuple(f"{host}/{project.strip('/')}/" for host in hosts)


def _local_registry_candidates(deployment_files: Iterable[Path]) -> Iterable[BaseImageResolution]:
    """Use API-node Docker cache only after it is tied to the configured Harbor project."""
    prefixes: tuple[str, ...] = ()
    for path in deployment_files:
        if path.name != ".env" or not path.is_file():
            continue
        try:
            prefixes = _registry_prefixes(_expand_deployment_values(parse_env_file(path)))
        except (OSError, ValueError):
            continue
        if prefixes:
            break
    if not prefixes:
        return
    try:
        completed = subprocess.run(
            ["docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}"],
            text=True,
            encoding="utf-8",
            errors="replace",
            capture_output=True,
            timeout=8,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return
    if completed.returncode != 0:
        return
    images = [line.strip() for line in completed.stdout.splitlines()]
    matching = []
    for image in images:
        if image == "<none>:<none>" or "/ubuntu:" not in image.lower():
            continue
        prefix_index = next((index for index, prefix in enumerate(prefixes) if image.startswith(prefix)), None)
        if prefix_index is not None:
            matching.append((prefix_index, image))
    # Prefer the configured TLS Registry endpoint, then custom Ubuntu images.
    matching.sort(key=lambda candidate: (candidate[0], "custom" not in candidate[1].lower(), candidate[1]))
    for _, image in matching:
        yield BaseImageResolution(image, "local Docker image matched to self-hosted Harbor")


def discover_base_image(
    explicit_image: str | None,
    templates: list[dict[str, Any]],
    *,
    deployment_files: Iterable[Path] = DEFAULT_DEPLOYMENT_FILES,
) -> BaseImageResolution:
    """Resolve the image without ever choosing a historical Template as the fixture."""
    deployment_files = tuple(deployment_files)
    if isinstance(explicit_image, str) and explicit_image.strip():
        image = explicit_image.strip()
        if not _is_image_reference(image):
            raise BaseImageDiscoveryError(f"Invalid base image reference: {image}")
        return BaseImageResolution(image, "--base-image or E2B_E2E_BASE_IMAGE")

    candidates: list[BaseImageResolution] = []
    for template in templates:
        for candidate in _template_image_candidates(template):
            _add_candidate(candidates, candidate.image, candidate.source)

    # Environment values win over deployment files so operators can override discovery.
    for key, value in os.environ.items():
        if IMAGE_KEY_PATTERN.search(key):
            _add_candidate(candidates, value, f"environment:{key}")
    for path in deployment_files:
        for candidate in _deployment_image_candidates(path):
            _add_candidate(candidates, candidate.image, candidate.source)
    for candidate in _local_registry_candidates(deployment_files):
        _add_candidate(candidates, candidate.image, candidate.source)

    if candidates:
        return candidates[0]
    raise BaseImageDiscoveryError(
        "Unable to discover an E2E base image from visible Template metadata or local "
        "self-hosted configuration. Set E2B_E2E_BASE_IMAGE once in .env, or run "
        "'bash start.sh test-e2e --all --base-image <registry>/<project>/<image>:<tag>'."
    )
