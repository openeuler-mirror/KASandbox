"""Sandbox inspection, lifecycle, and network SDK cases."""

from __future__ import annotations

from .e2e_models import CaseStatus
from .e2b_sdk_compat import pause_sandbox
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
        pause_sandbox(sandbox)
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
