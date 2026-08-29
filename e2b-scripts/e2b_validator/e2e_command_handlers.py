"""Extended Commands SDK cases, including background process behavior."""

from __future__ import annotations

from .e2e_models import CaseStatus
from .e2e_sdk_common import evidence, result_fields, sdk_sandbox


def handle(case, context: dict[str, object], owner):
    sandbox = sdk_sandbox(owner)
    mode = str(case.parameters["mode"])

    if mode == "wait":
        handle = sandbox.commands.run("printf background-wait", background=True)
        owner.ledger.record_background_process(sandbox.sandbox_id, handle.pid, case.case_id)
        result = handle.wait()
        passed = result.exit_code == 0 and "background-wait" in result.stdout
        return _outcome(passed, "background wait", result)

    if mode == "list":
        handle = sandbox.commands.run("sleep 30", background=True)
        owner.ledger.record_background_process(sandbox.sandbox_id, handle.pid, case.case_id)
        processes = sandbox.commands.list()
        passed = any(process.pid == handle.pid for process in processes)
        killed = handle.kill()
        return (CaseStatus.PASS if passed and killed else CaseStatus.FAIL,
                f"pid={handle.pid}, listed={passed}, killed={killed}", [evidence([vars(item) for item in processes])])

    if mode == "stdin":
        handle = sandbox.commands.run("read value; printf 'stdin:%s' \"$value\"", background=True, stdin=True)
        owner.ledger.record_background_process(sandbox.sandbox_id, handle.pid, case.case_id)
        sandbox.commands.send_stdin(handle.pid, "e2e-input\n")
        result = handle.wait()
        return _outcome(result.exit_code == 0 and "stdin:e2e-input" in result.stdout, "background stdin", result)

    if mode == "reconnect":
        handle = sandbox.commands.run("sleep 1; printf reconnect-ok", background=True)
        owner.ledger.record_background_process(sandbox.sandbox_id, handle.pid, case.case_id)
        handle.disconnect()
        result = sandbox.commands.connect(handle.pid, timeout=30).wait()
        return _outcome(result.exit_code == 0 and "reconnect-ok" in result.stdout, "background reconnect", result)

    if mode == "handle-kill":
        handle = sandbox.commands.run("sleep 60", background=True)
        owner.ledger.record_background_process(sandbox.sandbox_id, handle.pid, case.case_id)
        killed = handle.kill()
        return CaseStatus.PASS if killed else CaseStatus.FAIL, f"pid={handle.pid}, killed={killed}", []

    if mode == "missing-pid":
        killed = sandbox.commands.kill(2_147_483_647)
        return CaseStatus.PASS if not killed else CaseStatus.FAIL, f"missing_pid_killed={killed}", []

    if mode == "stream":
        stdout: list[str] = []
        stderr: list[str] = []
        result = sandbox.commands.run("printf stream-out; printf stream-err >&2", on_stdout=stdout.append, on_stderr=stderr.append)
        passed = result.exit_code == 0 and "stream-out" in "".join(stdout) and "stream-err" in "".join(stderr)
        return CaseStatus.PASS if passed else CaseStatus.FAIL, "stream callbacks captured output", [evidence({"result": result_fields(result), "stdout": stdout, "stderr": stderr})]

    if mode == "stream-error":
        from e2b import CommandExitException

        stdout: list[str] = []
        stderr: list[str] = []
        try:
            sandbox.commands.run("printf stream-error >&2; exit 9", on_stdout=stdout.append, on_stderr=stderr.append)
        except CommandExitException as exc:
            passed = exc.exit_code == 9 and "stream-error" in "".join(stderr)
            return CaseStatus.PASS if passed else CaseStatus.FAIL, f"stream exit_code={exc.exit_code}", [evidence({"result": result_fields(exc), "stdout": stdout, "stderr": stderr})]
        return CaseStatus.FAIL, "stream command unexpectedly exited with code 0", []

    raise ValueError(f"unknown extended command mode: {mode}")


def _outcome(passed: bool, title: str, result):
    return CaseStatus.PASS if passed else CaseStatus.FAIL, f"{title}, exit_code={result.exit_code}", [evidence(result_fields(result))]
