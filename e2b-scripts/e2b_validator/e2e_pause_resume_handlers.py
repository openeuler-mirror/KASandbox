"""Pause, resume, connect, auto-pause, and auto-resume lifecycle cases."""

from __future__ import annotations

import datetime
import hashlib
import time

from .e2e_api_client import E2BApiClient, sandbox_id_from, sandbox_state_from
from .e2e_models import CaseStatus
from .e2e_sdk_common import evidence, run_path, sdk_sandbox


def _record_sandbox(owner, sandbox_id: str, case_id: str) -> None:
    owner.ledger.record_sandbox(sandbox_id, case_id)
    sandbox_ids = owner.context.setdefault("sandbox_ids", [])
    if isinstance(sandbox_ids, list):
        sandbox_ids.append(sandbox_id)


def _auto_resume_payload(enabled: bool, style: str) -> dict[str, object]:
    if style == "policy":
        return {"policy": "any" if enabled else "off"}
    if style == "enabled":
        return {"enabled": enabled}
    raise ValueError(f"unknown autoResume payload style: {style}")


def _auto_resume_styles(context: dict[str, object]) -> list[str]:
    result = context.get("prc_auto_resume_result")
    preferred = result.get("style") if isinstance(result, dict) else None
    styles = ["policy", "enabled"]
    if preferred in styles:
        styles.remove(preferred)
        styles.insert(0, preferred)
    return styles


def _auto_resume_unavailable(value: object) -> bool:
    text = str(value).lower().replace("_", "-")
    markers = (
        "auto-resume disabled",
        "auto resume disabled",
        "autoresume disabled",
        "auto-resume is disabled",
        "auto resume is disabled",
    )
    return any(marker in text for marker in markers)


def _filesystem_capability(result: dict[str, object]) -> str:
    boot_before = result.get("boot_before")
    boot_after = result.get("boot_after")
    process_state = result.get("process_state")
    content = result.get("content")
    cold_boot = bool(boot_before and boot_after and boot_before != boot_after)
    process_missing = process_state == "missing"
    filesystem_retained = content == "filesystem-only"
    if cold_boot and process_missing and filesystem_retained:
        return "supported"
    if boot_before == boot_after and process_state == "alive" and filesystem_retained:
        return "unsupported"
    return "inconclusive"


def _create(
    owner,
    case_id: str,
    *,
    timeout: int = 600,
    auto_pause: bool = False,
    auto_pause_memory: bool | None = None,
    auto_resume: bool | None = None,
    auto_resume_style: str | None = None,
) -> tuple[E2BApiClient, str]:
    client = E2BApiClient()
    payload: dict[str, object] = {
        "templateID": owner.fixture_template,
        "timeout": timeout,
        "autoPause": auto_pause,
        "secure": True,
    }
    if auto_pause_memory is not None:
        payload["autoPauseMemory"] = auto_pause_memory
    if auto_resume is not None:
        payload["autoResume"] = _auto_resume_payload(
            auto_resume,
            auto_resume_style or "policy",
        )
    response = client.create_sandbox(payload)
    sandbox_id = sandbox_id_from(response.data)
    if response.status != 201 or not sandbox_id:
        raise RuntimeError(
            f"lifecycle Sandbox creation failed: status={response.status}, body={response.text}"
        )
    _record_sandbox(owner, sandbox_id, case_id)
    return client, sandbox_id


def _wait_state(
    client: E2BApiClient,
    sandbox_id: str,
    expected: str,
    *,
    timeout: float = 45,
    interval: float = 1,
) -> tuple[bool, str | None, list[str]]:
    deadline = time.monotonic() + timeout
    observations: list[str] = []
    last_state = None
    while time.monotonic() < deadline:
        response = client.get_sandbox(sandbox_id)
        last_state = sandbox_state_from(response.data)
        observations.append(f"status={response.status}, state={last_state}")
        if response.status == 200 and last_state == expected:
            return True, last_state, observations[-10:]
        time.sleep(interval)
    return False, last_state, observations[-10:]


def _run_text(sandbox, command: str, *, user: str | None = None) -> str:
    options = {"user": user} if user else {}
    result = sandbox.commands.run(command, **options)
    if result.exit_code != 0:
        raise RuntimeError(f"command failed with exit code {result.exit_code}: {result.stderr}")
    return result.stdout.strip()


def _remaining_seconds(data) -> float | None:
    if not isinstance(data, dict):
        return None
    raw = data.get("endAt", data.get("end_at"))
    if not isinstance(raw, str):
        return None
    try:
        end_at = datetime.datetime.fromisoformat(raw.replace("Z", "+00:00"))
    except ValueError:
        return None
    return (end_at - datetime.datetime.now(datetime.timezone.utc)).total_seconds()


def handle(case, context: dict[str, object], owner):
    mode = str(case.parameters["mode"])

    if mode == "full-pause":
        client, sandbox_id = _create(owner, case.case_id)
        response = client.pause(sandbox_id, memory=True)
        paused, state, observations = _wait_state(client, sandbox_id, "paused")
        passed = response.status == 204 and paused
        context["prc_full_sandbox_id"] = sandbox_id
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"pause_status={response.status}, state={state}",
            observations,
        )

    if mode == "explicit-resume":
        client = E2BApiClient()
        sandbox_id = str(context["prc_full_sandbox_id"])
        response = client.resume(sandbox_id, timeout=300)
        running, state, observations = _wait_state(client, sandbox_id, "running")
        passed = (
            response.status == 201
            and sandbox_id_from(response.data) == sandbox_id
            and running
        )
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"resume_status={response.status}, sandbox_id={sandbox_id}, state={state}",
            [evidence(response.data), *observations],
        )

    if mode == "connect-resume":
        client, sandbox_id = _create(owner, case.case_id)
        paused = client.pause(sandbox_id, memory=True)
        pause_ready, _, pause_observations = _wait_state(client, sandbox_id, "paused")
        response = client.connect(sandbox_id, timeout=300)
        running, state, observations = _wait_state(client, sandbox_id, "running")
        passed = (
            paused.status == 204
            and pause_ready
            and response.status == 201
            and sandbox_id_from(response.data) == sandbox_id
            and running
        )
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"connect_status={response.status}, sandbox_id={sandbox_id}, state={state}",
            [evidence(response.data), *pause_observations, *observations],
        )

    if mode == "full-process":
        client, sandbox_id = _create(owner, case.case_id)
        sandbox = sdk_sandbox(owner, sandbox_id, refresh=True)
        handle = sandbox.commands.run("sleep 300", background=True)
        owner.ledger.record_background_process(sandbox_id, handle.pid, case.case_id)
        paused = client.pause(sandbox_id, memory=True)
        pause_ready, _, pause_observations = _wait_state(client, sandbox_id, "paused")
        resumed = client.resume(sandbox_id, timeout=300)
        running, _, run_observations = _wait_state(client, sandbox_id, "running")
        sandbox = sdk_sandbox(owner, sandbox_id, refresh=True)
        processes = sandbox.commands.list()
        process_visible = any(process.pid == handle.pid for process in processes)
        passed = (
            paused.status == 204
            and pause_ready
            and resumed.status == 201
            and running
            and process_visible
        )
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"pid={handle.pid}, process_visible={process_visible}",
            [*pause_observations, *run_observations, evidence([vars(process) for process in processes])],
        )

    if mode == "full-filesystem":
        client, sandbox_id = _create(owner, case.case_id)
        sandbox = sdk_sandbox(owner, sandbox_id, refresh=True)
        marker = run_path(context, "pause-resume", "full-memory-marker.txt")
        sandbox.files.write(marker, "full-memory-filesystem")
        paused = client.pause(sandbox_id, memory=True)
        pause_ready, _, pause_observations = _wait_state(client, sandbox_id, "paused")
        resumed = client.resume(sandbox_id, timeout=300)
        running, _, run_observations = _wait_state(client, sandbox_id, "running")
        sandbox = sdk_sandbox(owner, sandbox_id, refresh=True)
        content = sandbox.files.read(marker)
        passed = (
            paused.status == 204
            and pause_ready
            and resumed.status == 201
            and running
            and content == "full-memory-filesystem"
        )
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"pause_status={paused.status}, resume_status={resumed.status}, content={content!r}",
            [*pause_observations, *run_observations],
        )

    if mode == "timeout-refresh":
        client, resume_id = _create(owner, case.case_id)
        client.pause(resume_id, memory=True)
        resume_paused, _, resume_pause_observations = _wait_state(client, resume_id, "paused")
        resume_response = client.resume(resume_id, timeout=420)
        resume_running, _, resume_run_observations = _wait_state(client, resume_id, "running")
        resume_info = client.get_sandbox(resume_id)
        resume_remaining = _remaining_seconds(resume_info.data)

        _, connect_id = _create(owner, case.case_id)
        client.pause(connect_id, memory=True)
        connect_paused, _, connect_pause_observations = _wait_state(client, connect_id, "paused")
        connect_response = client.connect(connect_id, timeout=480)
        connect_running, _, connect_run_observations = _wait_state(client, connect_id, "running")
        connect_info = client.get_sandbox(connect_id)
        connect_remaining = _remaining_seconds(connect_info.data)
        passed = (
            resume_paused
            and resume_response.status == 201
            and resume_running
            and connect_paused
            and connect_response.status == 201
            and connect_running
            and resume_remaining is not None
            and 300 <= resume_remaining <= 450
            and connect_remaining is not None
            and 360 <= connect_remaining <= 510
        )
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"resume_remaining={resume_remaining}, connect_remaining={connect_remaining}",
            [
                *resume_pause_observations,
                *resume_run_observations,
                *connect_pause_observations,
                *connect_run_observations,
                evidence({"resume": resume_info.data, "connect": connect_info.data}),
            ],
        )

    if mode == "repeat-pause":
        client, sandbox_id = _create(owner, case.case_id)
        first = client.pause(sandbox_id, memory=True)
        paused, _, observations = _wait_state(client, sandbox_id, "paused")
        second = client.pause(sandbox_id, memory=True)
        passed = first.status == 204 and paused and second.status == 409
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"first_status={first.status}, second_status={second.status}",
            [*observations, second.text[-2000:]],
        )

    if mode == "running-connect":
        client, sandbox_id = _create(owner, case.case_id)
        response = client.connect(sandbox_id, timeout=300)
        passed = response.status == 200 and sandbox_id_from(response.data) == sandbox_id
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"connect_status={response.status}, sandbox_id={sandbox_id_from(response.data)}",
            [evidence(response.data)],
        )

    if mode == "missing-lifecycle":
        client = E2BApiClient()
        digest = hashlib.sha256(str(context["run_id"]).encode("utf-8")).hexdigest()
        sandbox_id = f"z{digest[:20]}"
        statuses = {
            "pause": client.pause(sandbox_id, memory=True).status,
            "resume": client.resume(sandbox_id, timeout=120).status,
            "connect": client.connect(sandbox_id, timeout=120).status,
        }
        passed = all(status == 404 for status in statuses.values())
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"statuses={statuses}",
            [evidence(statuses)],
        )

    if mode == "filesystem-resume":
        client, sandbox_id = _create(owner, case.case_id)
        sandbox = sdk_sandbox(owner, sandbox_id, refresh=True)
        marker = run_path(context, "pause-resume", "filesystem-only-marker.txt")
        sandbox.files.write(marker, "filesystem-only")
        boot_before = _run_text(sandbox, "cat /proc/sys/kernel/random/boot_id", user="root")
        handle = sandbox.commands.run("sleep 300", background=True)
        owner.ledger.record_background_process(sandbox_id, handle.pid, case.case_id)
        paused = client.pause(sandbox_id, memory=False)
        pause_ready, _, pause_observations = _wait_state(client, sandbox_id, "paused")
        resumed = client.resume(sandbox_id, timeout=300)
        running, _, run_observations = _wait_state(client, sandbox_id, "running")
        sandbox = sdk_sandbox(owner, sandbox_id, refresh=True)
        boot_after = _run_text(sandbox, "cat /proc/sys/kernel/random/boot_id", user="root")
        content = sandbox.files.read(marker)
        process_state = _run_text(
            sandbox,
            f"if kill -0 {handle.pid} 2>/dev/null; then printf alive; else printf missing; fi",
            user="root",
        )
        context["prc_filesystem_result"] = {
            "sandbox_id": sandbox_id,
            "boot_before": boot_before,
            "boot_after": boot_after,
            "content": content,
            "process_state": process_state,
            "pid": handle.pid,
        }
        context["prc_filesystem_result"]["capability"] = _filesystem_capability(
            context["prc_filesystem_result"]
        )
        passed = (
            paused.status == 204
            and pause_ready
            and resumed.status == 201
            and running
            and sandbox_id_from(resumed.data) == sandbox_id
        )
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            (
                f"pause_status={paused.status}, resume_status={resumed.status}, "
                f"sandbox_id={sandbox_id}, "
                f"filesystem_capability={context['prc_filesystem_result']['capability']}"
            ),
            [*pause_observations, *run_observations, evidence(context["prc_filesystem_result"])],
        )

    if mode == "filesystem-connect":
        client, sandbox_id = _create(owner, case.case_id)
        sandbox = sdk_sandbox(owner, sandbox_id, refresh=True)
        marker = run_path(context, "pause-resume", "filesystem-connect.txt")
        sandbox.files.write(marker, "connect-cold-boot")
        paused = client.pause(sandbox_id, memory=False)
        pause_ready, _, pause_observations = _wait_state(client, sandbox_id, "paused")
        connected = client.connect(sandbox_id, timeout=300)
        running, _, run_observations = _wait_state(client, sandbox_id, "running")
        sandbox = sdk_sandbox(owner, sandbox_id, refresh=True)
        content = sandbox.files.read(marker)
        passed = (
            paused.status == 204
            and pause_ready
            and connected.status == 201
            and running
            and sandbox_id_from(connected.data) == sandbox_id
            and content == "connect-cold-boot"
        )
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"connect_status={connected.status}, content={content!r}",
            [*pause_observations, *run_observations, evidence(connected.data)],
        )

    if mode == "auto-pause":
        client, sandbox_id = _create(
            owner,
            case.case_id,
            timeout=int(case.parameters.get("timeout", 10)),
            auto_pause=True,
            auto_pause_memory=True,
        )
        paused, state, observations = _wait_state(
            client,
            sandbox_id,
            "paused",
            timeout=float(case.parameters.get("grace_seconds", 35)),
        )
        return (
            CaseStatus.PASS if paused else CaseStatus.FAIL,
            f"sandbox_id={sandbox_id}, final_state={state}",
            observations,
        )

    if mode == "auto-resume":
        attempts: list[str] = []
        for style in _auto_resume_styles(context):
            try:
                client, sandbox_id = _create(
                    owner,
                    case.case_id,
                    timeout=300,
                    auto_pause=True,
                    auto_pause_memory=True,
                    auto_resume=True,
                    auto_resume_style=style,
                )
            except RuntimeError as exc:
                if "status=400" in str(exc):
                    attempts.append(f"style={style}, create_rejected={exc}")
                    continue
                raise

            sandbox = sdk_sandbox(owner, sandbox_id, refresh=True)
            paused_response = client.pause(sandbox_id, memory=True)
            paused, _, pause_observations = _wait_state(client, sandbox_id, "paused")
            if paused_response.status != 204 or not paused:
                return (
                    CaseStatus.FAIL,
                    (
                        f"style={style}, pause_status={paused_response.status}, "
                        "Sandbox did not reach paused state"
                    ),
                    [*attempts, *pause_observations],
                )

            try:
                result = sandbox.commands.run("printf auto-resume-data-plane")
            except Exception as exc:
                attempts.append(f"style={style}, data_plane_error={type(exc).__name__}: {exc}")
                if _auto_resume_unavailable(exc):
                    continue
                raise

            running, state, run_observations = _wait_state(
                client,
                sandbox_id,
                "running",
                timeout=60,
            )
            passed = (
                result.exit_code == 0
                and "auto-resume-data-plane" in result.stdout
                and running
            )
            if passed:
                context["prc_auto_resume_result"] = {
                    "capability": "supported",
                    "style": style,
                    "sandbox_id": sandbox_id,
                }
                return (
                    CaseStatus.PASS,
                    f"style={style}, command_exit={result.exit_code}, final_state={state}",
                    [*attempts, *pause_observations, *run_observations, evidence(result)],
                )

            result_evidence = evidence(result)
            attempts.append(
                f"style={style}, command_exit={result.exit_code}, final_state={state}, "
                f"result={result_evidence}"
            )
            if _auto_resume_unavailable(result_evidence):
                continue
            return (
                CaseStatus.FAIL,
                f"style={style}, command_exit={result.exit_code}, final_state={state}",
                [*attempts, *pause_observations, *run_observations],
            )

        context["prc_auto_resume_result"] = {
            "capability": "unsupported",
            "attempts": attempts,
        }
        return (
            CaseStatus.BLOCKED,
            "autoResume capability is not exposed by this deployment",
            attempts,
        )

    raise ValueError(f"unknown pause/resume mode: {mode}")
