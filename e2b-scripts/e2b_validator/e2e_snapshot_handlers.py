"""Snapshot SDK cases with run-scoped cleanup."""

from __future__ import annotations

from .e2e_models import CaseStatus
from .e2e_sdk_common import (
    create_sdk_sandbox,
    evidence,
    run_path,
    sdk_options,
    sdk_sandbox,
    suppress_expected_sdk_status,
)
from .e2e_verifiers import kill_sandbox


def handle(case, context: dict[str, object], owner):
    from e2b import Sandbox

    mode = str(case.parameters["mode"])
    snapshot_id = context.get("snapshot_id")

    if mode == "create-list":
        sandbox = create_sdk_sandbox(
            owner,
            case_id=case.case_id,
            template=owner.fixture_template,
            timeout=900,
        )
        source_id = sandbox.sandbox_id
        marker = run_path(context, "snapshot", "marker.txt")
        sandbox.files.write(marker, "snapshot-content")
        snapshot = sandbox.create_snapshot()
        owner.ledger.record_snapshot(snapshot.snapshot_id, case.case_id)
        context["snapshot_id"] = snapshot.snapshot_id
        context["snapshot_marker"] = marker
        context["snapshot_source_sandbox_id"] = source_id
        # Snapshot creation pauses the source Sandbox; reconnect resumes it.
        sandbox = sdk_sandbox(owner, source_id, refresh=True)
        items = sandbox.list_snapshots().next_items()
        passed = any(item.snapshot_id == snapshot.snapshot_id for item in items)
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"snapshot_id={snapshot.snapshot_id}", [evidence(items)]

    if not isinstance(snapshot_id, str):
        raise RuntimeError("snapshot fixture is unavailable")

    if mode == "restore":
        restored = Sandbox.create(snapshot_id, timeout=300, **sdk_options())
        owner.ledger.record_sandbox(restored.sandbox_id, case.case_id)
        sandbox_ids = context.setdefault("sandbox_ids", [])
        if isinstance(sandbox_ids, list):
            sandbox_ids.append(restored.sandbox_id)
        context["snapshot_restored_sandbox_id"] = restored.sandbox_id
        restored = sdk_sandbox(owner, restored.sandbox_id, refresh=True)
        marker = str(context["snapshot_marker"])
        passed = restored.files.read(marker) == "snapshot-content"
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"restored_sandbox_id={restored.sandbox_id}", []

    if mode == "delete":
        restored_id = context.get("snapshot_restored_sandbox_id")
        if isinstance(restored_id, str):
            killed = kill_sandbox(restored_id)
            if not killed:
                return CaseStatus.FAIL, f"restored Sandbox could not be stopped before Snapshot deletion: {restored_id}", []
            cache = context.get("sdk_sandboxes")
            if isinstance(cache, dict):
                cache.pop(restored_id, None)
        deleted = Sandbox.delete_snapshot(snapshot_id, **sdk_options())
        context["snapshot_deleted"] = deleted
        return CaseStatus.PASS if deleted else CaseStatus.FAIL, f"deleted={deleted}", []

    if mode == "delete-missing":
        with suppress_expected_sdk_status(404):
            deleted = Sandbox.delete_snapshot(snapshot_id, **sdk_options())
        return CaseStatus.PASS if not deleted else CaseStatus.FAIL, f"second_delete={deleted}", []

    raise ValueError(f"unknown extended snapshot mode: {mode}")
