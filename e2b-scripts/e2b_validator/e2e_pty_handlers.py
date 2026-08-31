"""PTY SDK cases."""

from __future__ import annotations

from .e2e_models import CaseStatus
from .e2e_sdk_common import evidence, sdk_sandbox


def handle(case, context: dict[str, object], owner):
    from e2b import PtySize

    sandbox = sdk_sandbox(owner)
    mode = str(case.parameters["mode"])
    handle = sandbox.pty.create(PtySize(rows=24, cols=80), cwd="/tmp", envs={"E2E_PTY": "ready"})
    owner.ledger.record_pty(sandbox.sandbox_id, handle.pid, case.case_id)

    if mode == "create-input":
        sandbox.pty.send_stdin(handle.pid, b"printf 'pty:%s\\n' \"$E2E_PTY\"\nexit\n")
        output: list[bytes] = []
        result = handle.wait(on_pty=output.append)
        terminal = b"".join(output).decode("utf-8", "replace")
        passed = result.exit_code == 0 and "pty:ready" in terminal
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"pid={handle.pid}, exit_code={result.exit_code}", [evidence({"result": result, "pty": terminal})]

    if mode == "resize":
        sandbox.pty.resize(handle.pid, PtySize(rows=40, cols=120))
        sandbox.pty.send_stdin(handle.pid, b"printf 'resize-ok\\n'\nexit\n")
        output: list[bytes] = []
        result = handle.wait(on_pty=output.append)
        terminal = b"".join(output).decode("utf-8", "replace")
        passed = result.exit_code == 0 and "resize-ok" in terminal
        return CaseStatus.PASS if passed else CaseStatus.FAIL, f"pid={handle.pid}, exit_code={result.exit_code}", [evidence({"result": result, "pty": terminal})]

    if mode == "connect-kill":
        connected = sandbox.pty.connect(handle.pid, timeout=30)
        killed = sandbox.pty.kill(connected.pid)
        return CaseStatus.PASS if killed else CaseStatus.FAIL, f"pid={handle.pid}, killed={killed}", []

    raise ValueError(f"unknown extended PTY mode: {mode}")
