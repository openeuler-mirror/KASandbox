"""Template fixture selection helpers for real E2E sandbox cases."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


class TemplateFixtureError(RuntimeError):
    """Raised when the current environment cannot provide a ready template."""


@dataclass(frozen=True)
class TemplateFixture:
    """A template name accepted by the sandbox creation API."""

    name: str
    template_id: str | None
    build_status: str


def _string_values(value: Any) -> list[str]:
    if isinstance(value, str) and value.strip():
        return [value.strip()]
    if isinstance(value, (list, tuple, set)):
        return [item.strip() for item in value if isinstance(item, str) and item.strip()]
    return []


def template_names(template: Any) -> list[str]:
    """Return aliases first because sandbox creation accepts aliases directly."""
    if not isinstance(template, dict):
        return []
    names: list[str] = []
    for field in ("aliases", "names", "alias", "name", "template_name", "templateName"):
        for value in _string_values(template.get(field)):
            if value not in names:
                names.append(value)
    return names


def template_id(template: Any) -> str | None:
    if not isinstance(template, dict):
        return None
    for field in ("templateID", "template_id", "id"):
        value = template.get(field)
        if isinstance(value, str) and value.strip():
            return value.strip()
    return None


def template_build_status(template: Any) -> str:
    if not isinstance(template, dict):
        return "unknown"
    for field in ("buildStatus", "build_status", "status"):
        value = template.get(field)
        if isinstance(value, str) and value.strip():
            return value.strip().lower()
    return "unknown"


def templates_from_payload(payload: Any) -> list[dict[str, Any]]:
    """Normalize SDK and REST list-templates output into template dictionaries."""
    if isinstance(payload, dict):
        payload = payload.get("templates", [])
    return [item for item in payload if isinstance(item, dict)] if isinstance(payload, list) else []


def summarize_templates(templates: list[dict[str, Any]]) -> str:
    if not templates:
        return "(none)"
    values = []
    for template in templates:
        label = "/".join(template_names(template)) or template_id(template) or "(unnamed)"
        values.append(f"{label}:{template_build_status(template)}")
    return ", ".join(values)


def select_fixture(templates: list[dict[str, Any]], requested: str) -> TemplateFixture:
    """Resolve one explicit ready template without choosing from historical records."""
    requested = requested.strip() if isinstance(requested, str) else ""
    if not requested:
        raise TemplateFixtureError(
            "An explicit template name or ID is required; baseline mode builds its own fixture"
        )

    for template in templates:
        candidates = [*template_names(template), *(value for value in [template_id(template)] if value)]
        if requested in candidates:
            status = template_build_status(template)
            if status != "ready":
                raise TemplateFixtureError(
                    f"Requested template '{requested}' is not ready (status={status}); "
                    f"visible templates: {summarize_templates(templates)}"
                )
            return TemplateFixture(requested, template_id(template), status)
    raise TemplateFixtureError(
        f"Requested template '{requested}' was not found; "
        f"visible templates: {summarize_templates(templates)}"
    )


def select_fixture_template(templates: list[dict[str, Any]], requested: str) -> str:
    """Compatibility wrapper for callers that only need the explicit template name."""
    return select_fixture(templates, requested).name


def ready_template_names(templates: list[dict[str, Any]], *, exclude: set[str] | None = None) -> list[str]:
    """Return distinct ready aliases for bounded fallback placement probes."""
    excluded = exclude or set()
    names: list[str] = []
    for template in templates:
        if template_build_status(template) != "ready":
            continue
        for name in template_names(template):
            if name not in excluded and name not in names:
                names.append(name)
    return names
