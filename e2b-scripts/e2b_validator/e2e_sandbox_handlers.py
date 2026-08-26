"""Sandbox inspection, lifecycle, metric, and network SDK cases."""

from __future__ import annotations

import time

from .e2e_models import CaseStatus
from .e2e_sdk_common import create_sdk_sandbox, evidence, sdk_sandbox


def handle_sandbox(case, context: dict[str, object], owner):
    mode = str(case.parameters["mode"])

    if mode == "pause-resume":
        sandbox = create_sdk_sandbox(
            owner,
            case_id=case.case_id,
            template=owner.fixture_template,
            timeout=600,
        )
        sandbox_id = sandbox.sandbox_id
        sandbox.pause()
        reconnected = sdk_sandbox(owner, sandbox_id, refresh=True)
        result = reconnected.commands.run("printf pause-resume")
        passed = result.exit_code == 0 and "pause-resume" in result.stdout
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"sandbox_id={sandbox_id}, exit_code={result.exit_code}", [evidence(result)]

    sandbox = sdk_sandbox(owner)

    if mode == "inspect":
        running = sandbox.is_running()
        info = sandbox.get_info()
        passed = running and info.sandbox_id == owner._main_sandbox()
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"running={running}, sandbox_id={info.sandbox_id}", [evidence(info)]

    if mode == "set-timeout":
        sandbox.set_timeout(int(case.parameters["timeout"]))
        passed = sandbox.is_running()
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"timeout={case.parameters['timeout']}, running={passed}", []

    if mode == "reconnect":
        reconnected = sdk_sandbox(owner, owner._main_sandbox(), refresh=True)
        result = reconnected.commands.run("printf sdk-reconnected")
        passed = result.exit_code == 0 and "sdk-reconnected" in result.stdout
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"exit_code={result.exit_code}", [evidence(result)]

    if mode == "metrics":
        from e2b import SandboxException

        deadline = time.monotonic() + 30
        metrics = []
        last_error = None
        attempts = 0
        while time.monotonic() < deadline:
            attempts += 1
            try:
                metrics = sandbox.get_metrics()
                last_error = None
                if metrics:
                    break
            except SandboxException as exc:
                last_error = exc
            time.sleep(0.5)

        if not metrics:
            details = [evidence(last_error)] if last_error is not None else []
            error_summary = f"，last_error={last_error}" if last_error is not None else ""
            return CaseStatus.FAIL, f"30s 内未获得 Sandbox 指标，attempts={attempts}{error_summary}", details

        required_fields = (
            "cpu_count",
            "cpu_used_pct",
            "mem_total",
            "mem_used",
            "disk_total",
            "disk_used",
            "timestamp",
        )
        sample = metrics[0]
        missing = [field for field in required_fields if getattr(sample, field, None) is None]
        passed = not missing
        return (
            CaseStatus.PASS if passed else CaseStatus.FAIL,
            f"metric_samples={len(metrics)}, attempts={attempts}, missing_fields={missing}",
            [evidence(sample)],
        )

    raise ValueError(f"unknown extended sandbox mode: {mode}")


def handle_network(case, context: dict[str, object], owner):
    mode = str(case.parameters["mode"])
    if mode == "host":
        sandbox = sdk_sandbox(owner)
        host = sandbox.get_host(8080)
        expected_id = owner._main_sandbox()
        passed = expected_id in host and "8080" in host
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"host={host}", []

    raise ValueError(f"unknown extended network mode: {mode}")
