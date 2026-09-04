"""Checkpoint and restore cases built on E2B persistent Snapshots."""

from __future__ import annotations

import time

from .e2b_sdk_compat import create_snapshot, delete_snapshot
from .e2e_api_client import E2BApiClient, snapshot_ids_from
from .e2e_models import CaseStatus
from .e2e_sdk_common import (
    create_sdk_sandbox,
    evidence,
    run_path,
    sdk_options,
    sdk_sandbox,
    suppress_expected_sdk_status,
)


def _record_sandbox(owner, sandbox, case_id: str):
    owner.ledger.record_sandbox(sandbox.sandbox_id, case_id)
    sandbox_ids = owner.context.setdefault("sandbox_ids", [])
    if isinstance(sandbox_ids, list):
        sandbox_ids.append(sandbox.sandbox_id)
    return sdk_sandbox(owner, sandbox.sandbox_id, refresh=True)


def _restore(owner, snapshot_id: str, case_id: str):
    from e2b import Sandbox

    restored = Sandbox.create(snapshot_id, timeout=300, **sdk_options())
    return _record_sandbox(owner, restored, case_id)


def _checkpoint(owner, sandbox, case_id: str):
    snapshot = create_snapshot(
        sandbox,
        **sdk_options(sandbox_id=sandbox.sandbox_id),
    )
    owner.ledger.record_snapshot(snapshot.snapshot_id, case_id)
    return snapshot.snapshot_id


def _create_source(owner, case_id: str):
    return create_sdk_sandbox(
        owner,
        case_id=case_id,
        template=owner.fixture_template,
        timeout=900,
    )


def _command_text(sandbox, command: str, *, user: str | None = None) -> str:
    options = {"user": user} if user else {}
    result = sandbox.commands.run(command, **options)
    if result.exit_code != 0:
        raise RuntimeError(f"command failed with exit code {result.exit_code}: {result.stderr}")
    return result.stdout.strip()


def handle(case, context: dict[str, object], owner):
    mode = str(case.parameters["mode"])

    if mode == "create-list":
        source = _create_source(owner, case.case_id)
        marker = run_path(context, "checkpoint", "base-state.txt")
        home_marker = f"/home/user-sandbox/e2e-{context['run_id']}.txt"
        etc_marker = f"/etc/e2e-checkpoint-{context['run_id']}.txt"
        source.files.write(marker, "checkpoint-base")
        _command_text(
            source,
            (
                "mkdir -p /home/user-sandbox; "
                f"printf home-checkpoint > {home_marker}; "
                f"printf etc-checkpoint > {etc_marker}"
            ),
            user="root",
        )
        snapshot_id = _checkpoint(owner, source, case.case_id)
        source = sdk_sandbox(owner, source.sandbox_id, refresh=True)
        response = E2BApiClient().list_snapshots(sandbox_id=source.sandbox_id)
        visible = response.status == 200 and snapshot_id in snapshot_ids_from(response.data)
        context.update({
            "checkpoint_source_id": source.sandbox_id,
            "checkpoint_snapshot_id": snapshot_id,
            "checkpoint_marker": marker,
            "checkpoint_home_marker": home_marker,
            "checkpoint_etc_marker": etc_marker,
        })
        return (
            CaseStatus.PASS if visible else CaseStatus.FAIL,
            f"snapshot_id={snapshot_id}, listed={visible}",
            [evidence({"status": response.status, "data": response.data})],
        )

    if mode == "restore-new-id":
        snapshot_id = str(context["checkpoint_snapshot_id"])
        source_id = str(context["checkpoint_source_id"])
        restored = _restore(owner, snapshot_id, case.case_id)
        marker = str(context["checkpoint_marker"])
        content = restored.files.read(marker)
        passed = restored.sandbox_id != source_id and content == "checkpoint-base"
        context["checkpoint_restored_id"] = restored.sandbox_id
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"source_id={source_id}, restored_id={restored.sandbox_id}",
            [evidence({"marker": marker, "content": content})],
        )

    if mode == "two-stage-rollback":
        source = _create_source(owner, case.case_id)
        marker = run_path(context, "checkpoint-two-stage", "state.txt")
        source.files.write(marker, "state-1")
        first_id = _checkpoint(owner, source, case.case_id)
        source = sdk_sandbox(owner, source.sandbox_id, refresh=True)
        source.files.write(marker, "state-2")
        second_id = _checkpoint(owner, source, case.case_id)
        source = sdk_sandbox(owner, source.sandbox_id, refresh=True)
        source.files.write(marker, "dirty-state")
        first = _restore(owner, first_id, case.case_id)
        second = _restore(owner, second_id, case.case_id)
        first_value = first.files.read(marker)
        second_value = second.files.read(marker)
        passed = first_value == "state-1" and second_value == "state-2"
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"checkpoint1={first_value!r}, checkpoint2={second_value!r}",
            [evidence({"checkpoint1": first_id, "checkpoint2": second_id})],
        )

    if mode == "source-restore-isolation":
        source = sdk_sandbox(owner, str(context["checkpoint_source_id"]))
        restored = sdk_sandbox(owner, str(context["checkpoint_restored_id"]))
        marker = run_path(context, "checkpoint", "isolation.txt")
        source.files.write(marker, "source-only")
        source_value_before = source.files.read(marker)
        restored.files.write(marker, "restored-only")
        source_value_after = source.files.read(marker)
        restored_value = restored.files.read(marker)
        passed = (
            source_value_before == "source-only"
            and source_value_after == "source-only"
            and restored_value == "restored-only"
        )
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"source={source_value_after!r}, restored={restored_value!r}",
            [],
        )

    if mode == "source-continues":
        source = sdk_sandbox(owner, str(context["checkpoint_source_id"]), refresh=True)
        result = source.commands.run("printf checkpoint-source-running")
        passed = result.exit_code == 0 and "checkpoint-source-running" in result.stdout
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"source_id={source.sandbox_id}, exit_code={result.exit_code}",
            [evidence(result)],
        )

    if mode == "memory-process":
        source = _create_source(owner, case.case_id)
        heartbeat_dir = run_path(context, "checkpoint-memory")
        source.files.make_dir(heartbeat_dir)
        marker = f"{heartbeat_dir}/heartbeat.txt"
        handle = source.commands.run(
            f"while true; do printf x >> {marker}; sleep 1; done",
            background=True,
        )
        owner.ledger.record_background_process(source.sandbox_id, handle.pid, case.case_id)
        time.sleep(2)
        snapshot_id = _checkpoint(owner, source, case.case_id)
        restored = _restore(owner, snapshot_id, case.case_id)
        before = int(_command_text(restored, f"wc -c < {marker}"))
        time.sleep(2)
        after = int(_command_text(restored, f"wc -c < {marker}"))
        passed = before > 0 and after > before
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"heartbeat_bytes_before={before}, after={after}",
            [evidence({"source_pid": handle.pid, "restored_id": restored.sandbox_id})],
        )

    if mode == "system-paths":
        restored = sdk_sandbox(owner, str(context["checkpoint_restored_id"]))
        home_value = _command_text(restored, f"cat {context['checkpoint_home_marker']}", user="root")
        etc_value = _command_text(restored, f"cat {context['checkpoint_etc_marker']}", user="root")
        passed = home_value == "home-checkpoint" and etc_value == "etc-checkpoint"
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"home={home_value!r}, etc={etc_value!r}",
            [],
        )

    if mode == "missing-deleted":
        from e2b import Sandbox

        source = _create_source(owner, case.case_id)
        snapshot_id = _checkpoint(owner, source, case.case_id)
        source = sdk_sandbox(owner, source.sandbox_id, refresh=True)
        deleted = delete_snapshot(snapshot_id, **sdk_options())
        missing_id = f"missing-checkpoint-{context['run_id']}"
        rejected: dict[str, str] = {}
        unexpected: dict[str, str] = {}
        for label, candidate in (("deleted", snapshot_id), ("missing", missing_id)):
            try:
                with suppress_expected_sdk_status(404):
                    restored = Sandbox.create(candidate, timeout=120, **sdk_options())
            except Exception as exc:
                rejected[label] = f"{type(exc).__name__}: {exc}"
            else:
                _record_sandbox(owner, restored, case.case_id)
                unexpected[label] = restored.sandbox_id
        passed = deleted and set(rejected) == {"deleted", "missing"} and not unexpected
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"deleted={deleted}, rejected={sorted(rejected)}, unexpected={unexpected}",
            [evidence({"rejected": rejected, "unexpected": unexpected})],
        )

    raise ValueError(f"unknown checkpoint/restore mode: {mode}")
