"""Template SDK cases with run-scoped build and tag handling."""

from __future__ import annotations

import time

from .e2e_models import CaseStatus
from .e2e_sdk_common import blocked_outcome, capability_blocked, evidence


BUILD_READY_TIMEOUT_SECONDS = 600
BUILD_STATUS_INTERVAL_SECONDS = 5


def _template(case):
    from e2b import Template

    base_image = str(case.parameters["base_image"])
    return (
        Template()
        .from_image(base_image)
        .run_cmd("printf sdk-template >/tmp/e2e-sdk-marker")
        .set_workdir("/app")
        .set_user("user")
    )


def _name(case) -> str:
    return str(case.parameters["name"])


def _wait_for_build(template, build_info):
    """Wait for the run-scoped asynchronous build before testing its name or tags."""
    from e2b import Template

    deadline = time.monotonic() + BUILD_READY_TIMEOUT_SECONDS
    last_status = None
    while True:
        status = Template.get_build_status(build_info, **_sdk_options())
        status_value = getattr(status, "status", None)
        normalized = getattr(status_value, "value", status_value)
        last_status = str(normalized).lower() if normalized is not None else "unknown"
        if last_status == "ready":
            return status, last_status
        if last_status == "error":
            reason = getattr(status, "reason", None)
            detail = getattr(reason, "message", None) or "Template background build entered error state"
            raise RuntimeError(detail)
        if time.monotonic() >= deadline:
            raise TimeoutError(
                f"Template background build did not become ready within "
                f"{BUILD_READY_TIMEOUT_SECONDS}s (last_status={last_status})"
            )
        time.sleep(BUILD_STATUS_INTERVAL_SECONDS)


def handle(case, context: dict[str, object], owner):
    from e2b import Template

    mode = str(case.parameters["mode"])
    try:
        if mode == "serialize":
            template = _template(case)
            as_json = Template.to_json(template)
            dockerfile = Template.to_dockerfile(template)
            passed = (
                str(case.parameters["base_image"]) in as_json
                and "FROM " in dockerfile
                and "sdk-template" in dockerfile
                and "/app" in dockerfile
            )
            return (
                CaseStatus.PASS if passed else CaseStatus.FAIL,
                f"json_bytes={len(as_json)}, dockerfile_lines={len(dockerfile.splitlines())}",
                [evidence({"json": as_json, "dockerfile": dockerfile})],
            )

        if mode == "background-build":
            template = _template(case)
            name = _name(case)
            build_info = Template.build_in_background(
                template,
                name,
                cpu_count=1,
                memory_mb=512,
                **_sdk_options(),
            )
            owner.ledger.record_template(build_info.template_id, name, case.case_id)
            names = context.setdefault("template_names", [])
            if isinstance(names, list):
                names.append(name)
            context["sdk_build_info"] = build_info
            status, status_value = _wait_for_build(template, build_info)
            context["sdk_build_status"] = status_value
            return (
                CaseStatus.PASS,
                f"template_id={build_info.template_id}, build_id={build_info.build_id}, status={status_value}",
                [evidence({"build_info": build_info, "status": status})],
            )

        if mode == "exists":
            name = _name(case)
            exists = Template.exists(name, **_sdk_options())
            return (
                CaseStatus.PASS if exists else CaseStatus.FAIL,
                f"template={name}, exists={exists}",
                [],
            )

        if mode == "tags":
            name = _name(case)
            tag = str(case.parameters["tag"])
            assigned = Template.assign_tags(name, tag, **_sdk_options())
            owner.ledger.record_template_tag(name, tag, case.case_id)
            tags_after_assign = Template.get_tags(name, **_sdk_options())
            visible = any(getattr(item, "tag", None) == tag for item in tags_after_assign)
            Template.remove_tags(name, tag, **_sdk_options())
            tags_after_remove = Template.get_tags(name, **_sdk_options())
            removed = not any(getattr(item, "tag", None) == tag for item in tags_after_remove)
            context["sdk_tag_removed"] = removed
            return (
                CaseStatus.PASS if visible and removed else CaseStatus.FAIL,
                f"tag={tag}, visible_after_assign={visible}, removed={removed}",
                [evidence({"assigned": assigned, "after_assign": tags_after_assign, "after_remove": tags_after_remove})],
            )

        raise ValueError(f"unknown extended Template mode: {mode}")
    except Exception as exc:
        if capability_blocked(exc):
            return blocked_outcome(exc)
        raise


def _sdk_options() -> dict[str, str]:
    import os

    values = {
        "api_key": os.environ.get("E2B_API_KEY", ""),
        "api_url": os.environ.get("E2B_API_URL", ""),
        "domain": os.environ.get("E2B_DOMAIN", ""),
    }
    return {key: value for key, value in values.items() if value}
