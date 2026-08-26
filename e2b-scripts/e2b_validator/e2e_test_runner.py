"""Run detailed real E2B cases and persist evidence after every case."""

from __future__ import annotations

import argparse
import json
import os
import shlex
import subprocess
import sys
import tempfile
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable

from .e2b_common import collect_pages, to_plain_data
from .e2b_config import add_config_arguments
from .e2e_base_image import BaseImageDiscoveryError, discover_base_image
from .e2e_diagnostics import build_diagnostics, print_case_result, print_run_summary
from .e2e_models import CaseResult, CaseStatus, RunSummary, TestCase
from .e2e_report import E2EReporter
from .e2e_resources import ResourceLedger
from .e2e_test_cases import build_cases
from .e2e_template_fixture import (
    TemplateFixture,
    TemplateFixtureError,
    ready_template_names,
    select_fixture,
    template_id as listed_template_id,
    template_names,
    templates_from_payload,
)
from .e2e_verifiers import extract_last_json, extract_resource_id, item_has_id, kill_sandbox, sha256_file
from .e2e_sdk_common import (
    capability_blocked,
    sdk_options,
    sdk_sandbox,
    suppress_expected_sdk_status,
)


PROJECT_DIR = Path(__file__).resolve().parent.parent
BLOCK_PATTERNS = (
    "i/o timeout", "connection timed out", "temporary failure", "no route to host",
    "connection refused", "context deadline exceeded", "failed to pull", "registry",
    "allocation", "unavailable", "502 bad gateway", "503 service unavailable",
)
TEMPLATE_BUILDER_BLOCK_PATTERNS = (
    "template builder not found",
    "failed to get builder client",
    "error when getting available build client",
    "no available build client",
)
SANDBOX_PLACEMENT_PATTERNS = (
    "failed to place sandbox",
    "failed to schedule sandbox",
)
SANDBOX_ROUTE_PENDING_PATTERNS = (
    "failed to route request to sandbox",
    "sandbox timeout",
    "connection refused",
    "bad gateway",
    "service unavailable",
)
MAX_FALLBACK_TEMPLATE_PROBES = 3

NEGATIVE_CASE_SUCCESS = {
    "SB-009": "不存在的模板已被服务端拒绝",
    "CMD-011": "命令已按预期超时",
    "CMD-013": "不存在的工作目录已被拒绝",
    "CMD-014": "不存在的 sandbox ID 已被拒绝",
}


class CommandResult:
    def __init__(self, argv: list[str], returncode: int, stdout: str, stderr: str, duration: float):
        self.argv = argv
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr
        self.duration = duration

    @property
    def combined(self) -> str:
        return f"{self.stdout}\n{self.stderr}".strip()

    def json(self):
        return extract_last_json(self.stdout)


class CLIInvoker:
    def __init__(self, reporter: E2EReporter):
        self.reporter = reporter

    def run(self, args: list[str], *, timeout: int = 180, sandbox_id: str | None = None) -> CommandResult:
        command = [sys.executable, "-m", "e2b_validator.build_prod", *args]
        environment = os.environ.copy()
        if sandbox_id and not environment.get("E2B_SANDBOX_URL"):
            from .self_hosted_runtime import sandbox_url_for

            domain = environment.get("E2B_DOMAIN")
            if domain:
                ssl = environment.get("E2B_HTTP_SSL", "false").lower() in {"1", "true", "yes", "on"}
                proxy_port = int(environment.get("E2B_PROXY_PORT", "3002"))
                environment["E2B_SANDBOX_URL"] = sandbox_url_for(sandbox_id, domain, port=proxy_port, use_ssl=ssl)
        started = time.monotonic()
        try:
            completed = subprocess.run(
                command, cwd=PROJECT_DIR, env=environment, text=True,
                encoding="utf-8", errors="replace", capture_output=True,
                timeout=timeout, check=False,
            )
            result = CommandResult(command, completed.returncode, completed.stdout, completed.stderr, time.monotonic() - started)
        except subprocess.TimeoutExpired as exc:
            stdout = exc.stdout.decode("utf-8", "replace") if isinstance(exc.stdout, bytes) else (exc.stdout or "")
            stderr = exc.stderr.decode("utf-8", "replace") if isinstance(exc.stderr, bytes) else (exc.stderr or "")
            result = CommandResult(command, 124, stdout, f"{stderr}\ncommand timeout after {timeout}s", time.monotonic() - started)
        self.reporter.log_command(command, result.returncode, result.stdout, result.stderr, result.duration)
        return result


def _is_blocked(text: str) -> bool:
    lowered = text.lower()
    return any(pattern in lowered for pattern in BLOCK_PATTERNS)


def _template_build_blocked(text: str) -> bool:
    lowered = text.lower()
    return any(pattern in lowered for pattern in TEMPLATE_BUILDER_BLOCK_PATTERNS) or _is_blocked(text)


def _template_build_blocked_message(returncode: int) -> str:
    return (
        "Template 构建链路不可用：template-builder 未注册或无可用 build client "
        f"(returncode={returncode})；请检查 template-builder Nomad Job、Consul 服务注册、"
        "allocation 状态和资源限制"
    )


def _sandbox_placement_blocked(text: str) -> bool:
    lowered = text.lower()
    return any(pattern in lowered for pattern in SANDBOX_PLACEMENT_PATTERNS)


def _sandbox_placement_message(returncode: int) -> str:
    return (
        "Sandbox placement unavailable: server returned Failed to place sandbox "
        f"(returncode={returncode}); check the new Nomad template-manager allocation, "
        "Nomad scheduling, Firecracker/cgroup OOM, and restart policy"
    )

def _sandbox_route_pending(text: str) -> bool:
    lowered = text.lower()
    return any(pattern in lowered for pattern in SANDBOX_ROUTE_PENDING_PATTERNS)


def _find_text(value, field: str) -> str:
    if isinstance(value, dict):
        candidate = value.get(field)
        if isinstance(candidate, str):
            return candidate
        return "\n".join(filter(None, (_find_text(item, field) for item in value.values())))
    if isinstance(value, list):
        return "\n".join(filter(None, (_find_text(item, field) for item in value)))
    return ""


def _negative_case_outcome(case: TestCase, returncode: int) -> tuple[CaseStatus, str]:
    if returncode != 0:
        detail = NEGATIVE_CASE_SUCCESS.get(case.case_id, f"{case.title}场景已返回预期错误")
        return (
            CaseStatus.PASS,
            f"负向用例验证通过：{detail}（被测命令退出码={returncode}）",
        )
    return (
        CaseStatus.FAIL,
        f"负向用例验证失败：{case.title}未返回预期错误（被测命令退出码={returncode}）",
    )


def _load_visible_templates() -> list[dict]:
    """Use the control-plane API so fixture selection has one stable data shape."""
    from .e2b_config import require_api_key
    from .list_templates import _list_with_rest_api

    return templates_from_payload(_list_with_rest_api(require_api_key()))


def _create_fixture_template(
    invoker: CLIInvoker,
    *,
    run_id: str,
    base_image: str,
    requested_template: str | None,
) -> tuple[TemplateFixture, bool]:
    """Build an isolated fixture unless the caller explicitly checks one template."""
    if requested_template:
        return select_fixture(_load_visible_templates(), requested_template), False

    fixture_name = f"e2e-{run_id}-fixture"
    created = invoker.run(
        [
            "create-template", "--name", fixture_name,
            "--base-image", base_image,
            "--cpu-count", "1",
            "--memory-mb", "512",
        ],
        timeout=660,
    )
    if created.returncode != 0:
        raise RuntimeError(
            f"Automatic baseline fixture build failed "
            f"(returncode={created.returncode}): {created.combined[-1500:]}"
        )

    # The SDK build normally waits for completion. Polling also handles asynchronous deployments.
    last_error = "fixture did not become visible"
    for attempt in range(12):
        templates = _load_visible_templates()
        try:
            return select_fixture(templates, fixture_name), True
        except TemplateFixtureError as exc:
            last_error = str(exc)
            if attempt < 11:
                time.sleep(5)
    raise RuntimeError(
        f"Automatic fixture template '{fixture_name}' was created but is not ready: {last_error}"
    )


def _probe_template_placement(invoker: CLIInvoker, template: str) -> tuple[bool, str]:
    """Create and remove a short-lived sandbox to distinguish template and scheduler failures."""
    result = invoker.run(
        ["create-sandbox", "--template", template, "--timeout", "60"],
        timeout=120,
    )
    if result.returncode != 0:
        return False, f"template={template}: {result.combined[-1500:]}"
    sandbox_id = extract_resource_id(result.json(), "sandbox")
    if not sandbox_id:
        return False, f"template={template}: sandbox creation returned no sandbox ID"
    try:
        kill_sandbox(sandbox_id)
    except Exception as exc:
        return False, f"template={template}: placement succeeded but cleanup failed: {exc}"
    return True, f"template={template}: placement probe passed"


def _select_runnable_fixture(
    invoker: CLIInvoker,
    fixture: TemplateFixture,
    *,
    requested_template: str | None,
) -> tuple[TemplateFixture, list[str]]:
    """Keep the isolated fixture first, then use ready templates only as placement diagnostics."""
    primary_ok, primary_detail = _probe_template_placement(invoker, fixture.name)
    evidence = [primary_detail]
    if primary_ok or requested_template:
        return fixture, evidence

    candidates = ready_template_names(
        _load_visible_templates(),
        exclude={fixture.name},
    )[:MAX_FALLBACK_TEMPLATE_PROBES]
    for candidate in candidates:
        candidate_ok, candidate_detail = _probe_template_placement(invoker, candidate)
        evidence.append(candidate_detail)
        if candidate_ok:
            return select_fixture(_load_visible_templates(), candidate), evidence
    return fixture, evidence


class E2ERunner:
    def __init__(
        self,
        cases: list[TestCase],
        result_dir: Path,
        *,
        handlers: dict[str, Callable] | None = None,
        markdown_path: Path | None = None,
        run_id: str | None = None,
        cleanup: bool = True,
        fixture_template: str = "",
        fixture_created: TemplateFixture | None = None,
        placement_probe_evidence: list[str] | None = None,
    ):
        self.cases = cases
        self.result_dir = result_dir
        self.run_id = run_id or result_dir.name
        self.reporter = E2EReporter(result_dir, markdown_path or result_dir / "report.md")
        self.invoker = CLIInvoker(self.reporter)
        self.ledger = ResourceLedger(result_dir / "resources.json", self.run_id)
        self.context: dict[str, object] = {
            "run_id": self.run_id,
            "sandbox_ids": [],
            "template_names": [],
            "sdk_sandboxes": {},
            "watchers": {},
        }
        self.cleanup_enabled = cleanup
        self.cleanup_results: list[dict] = []
        self.fixture_template = fixture_template
        self.fixture_created = fixture_created
        self.context["placement_probe_evidence"] = placement_probe_evidence or []
        if fixture_created is not None:
            self.ledger.record_template(
                fixture_created.template_id or fixture_created.name,
                fixture_created.name,
                "FIXTURE-TEMPLATE",
            )
        self.handlers = handlers or {
            "expect_cli_error": self._expect_cli_error,
            "create_sandbox": self._create_sandbox,
            "sandbox_lifecycle": self._sandbox_lifecycle,
            "create_template": self._create_template,
            "run_command": self._run_command,
            "upload_file": self._upload_file,
            "download_file": self._download_file,
            "list_sandboxes": self._list_sandboxes,
            "list_templates": self._list_templates,
            "extended-command": self._extended_command,
            "extended-filesystem": self._extended_filesystem,
            "extended-sandbox": self._extended_sandbox,
            "extended-network": self._extended_network,
            "extended-snapshot": self._extended_snapshot,
            "extended-checkpoint": self._extended_checkpoint,
            "extended-pause-resume": self._extended_pause_resume,
            "extended-pty": self._extended_pty,
            "extended-template": self._extended_template,
        }

    def run(self) -> RunSummary:
        summary = RunSummary(self.run_id, environment={
            "api_url": os.getenv("E2B_API_URL", ""),
            "domain": os.getenv("E2B_DOMAIN", ""),
            "python": sys.version.split()[0],
        })
        statuses: dict[str, CaseStatus] = {}
        self.reporter.save(summary)
        try:
            statuses.update(self._prepare_fixtures())
            for case in self.cases:
                started_at = datetime.now(timezone.utc).isoformat()
                started = time.monotonic()
                failed_dependencies = [dep for dep in case.depends_on if statuses.get(dep) != CaseStatus.PASS]
                if failed_dependencies:
                    outcome = (CaseStatus.SKIPPED, f"依赖用例未通过：{', '.join(failed_dependencies)}", [])
                    error_type = "dependency"
                else:
                    try:
                        outcome = self.handlers[case.scenario](case, self.context)
                        error_type = None
                    except Exception as exc:
                        if capability_blocked(exc):
                            outcome = (
                                CaseStatus.BLOCKED,
                                f"SDK capability unavailable: {exc}",
                                [str(exc)[-3000:]],
                            )
                        else:
                            outcome = (CaseStatus.FAIL, f"{type(exc).__name__}: {exc}", [])
                        error_type = type(exc).__name__
                status, actual, evidence = outcome
                diagnostics = build_diagnostics(
                    case,
                    status,
                    actual,
                    evidence,
                    error_type=error_type,
                    result_dir=self.result_dir,
                )
                result = CaseResult(
                    case=case,
                    status=status,
                    duration_seconds=time.monotonic() - started,
                    actual=actual,
                    evidence=evidence,
                    started_at=started_at,
                    error_type=error_type,
                    diagnostics=diagnostics,
                )
                summary.results.append(result)
                statuses[case.case_id] = status
                self.reporter.save(summary)
                print_case_result(result, len(summary.results), len(self.cases))
        finally:
            self.cleanup_results = self._cleanup() if self.cleanup_enabled else []
            summary.finished_at = datetime.now(timezone.utc).isoformat()
            self.reporter.save(
                summary,
                self.cleanup_results,
                resources=self.ledger.resources,
                cleanup_enabled=self.cleanup_enabled,
            )
        return summary

    def _prepare_fixtures(self) -> dict[str, CaseStatus]:
        selected = {case.case_id for case in self.cases}
        needs_main = any("SB-001" in case.depends_on for case in self.cases)
        if not needs_main or "SB-001" in selected or isinstance(self.context.get("main_sandbox"), str):
            return {}

        result = self.invoker.run(
            ["create-sandbox", "--template", self.fixture_template, "--timeout", "600"],
            timeout=120,
        )
        sandbox_id = extract_resource_id(result.json(), "sandbox") if result.returncode == 0 else None
        if not sandbox_id:
            self.context["fixture_error"] = result.combined[-1500:]
            return {}
        self.ledger.record_sandbox(sandbox_id, "FIXTURE-SB-001")
        self.context["sandbox_ids"].append(sandbox_id)
        self.context["main_sandbox"] = sandbox_id
        return {"SB-001": CaseStatus.PASS}

    def _cleanup(self) -> list[dict]:
        def sandbox_is_visible(sandbox_id: str) -> bool:
            from e2b import Sandbox

            return item_has_id(collect_pages(Sandbox.list()), sandbox_id, "sandbox")

        def cleaner(sandbox_id: str) -> bool | tuple[str, str]:
            try:
                with suppress_expected_sdk_status(404):
                    killed = kill_sandbox(sandbox_id)
            except Exception as exc:
                if any(marker in str(exc).lower() for marker in ("404", "not found", "does not exist", "expired")):
                    return "expired", "sandbox already expired before cleanup"
                raise
            if killed:
                return True
            if not sandbox_is_visible(sandbox_id):
                return "expired", "sandbox expired before cleanup"
            return False
        # Release process handles before removing their owning Sandbox.
        cleanup = self._cleanup_sdk_resources()
        before = len(self.ledger.cleanup)
        self.ledger.cleanup_sandboxes(cleaner)
        cleanup.extend(self.ledger.cleanup[before:])
        # Snapshot and Template deletion require every run-owned Sandbox to be gone.
        cleanup.extend(self._cleanup_snapshots())
        cleanup.extend(self._cleanup_templates())
        return cleanup

    def _cleanup_sdk_resources(self) -> list[dict]:
        """Release only background resources registered by this run."""
        from .e2e_sdk_common import sdk_sandbox

        results: list[dict] = []
        watchers = self.context.get("watchers", {})
        for resource in self.ledger.resources:
            if resource["run_id"] != self.run_id or resource["kind"] not in {
                "background-process", "pty", "watcher", "template-tag",
            }:
                continue
            kind = resource["kind"]
            resource_id = resource["id"]
            status = "cleaned"
            detail = "cleanup call completed"
            try:
                if kind in {"background-process", "pty"}:
                    sandbox_id, raw_pid = resource_id.rsplit(":", 1)
                    pid = int(raw_pid)
                    with suppress_expected_sdk_status(404):
                        sandbox = sdk_sandbox(self, sandbox_id)
                        result = (
                            sandbox.commands.kill(pid)
                            if kind == "background-process"
                            else sandbox.pty.kill(pid)
                        )
                    if result is False:
                        status, detail = "already-stopped", "process was already stopped"
                elif kind == "watcher":
                    label = resource_id.split(":", 1)[1]
                    watcher = watchers.get(label) if isinstance(watchers, dict) else None
                    if watcher is not None:
                        watcher.stop()
                    else:
                        status, detail = "already-stopped", "watcher handle was no longer present"
                elif kind == "template-tag":
                    from e2b import Template

                    template_name, tag = resource_id.rsplit(":", 1)
                    Template.remove_tags(template_name, tag, **sdk_options())
            except Exception as exc:
                text = str(exc).lower()
                if any(marker in text for marker in ("404", "not found", "does not exist", "expired")):
                    status, detail = "already-absent", "resource was absent during cleanup"
                else:
                    status, detail = "cleanup-failed", f"{type(exc).__name__}: {exc}"
            self.ledger.record_cleanup(kind, resource_id, status, detail)
            results.append({"kind": kind, "id": resource_id, "status": status, "detail": detail})
        return results

    def _cleanup_snapshots(self) -> list[dict]:
        """Delete only Snapshot IDs recorded by this run, after Sandbox cleanup."""
        from e2b import Sandbox

        results: list[dict] = []
        cleaned_ids: set[str] = set()
        for resource in self.ledger.resources:
            if resource["kind"] != "snapshot" or resource["run_id"] != self.run_id:
                continue
            resource_id = resource["id"]
            if resource_id in cleaned_ids:
                continue
            cleaned_ids.add(resource_id)
            try:
                with suppress_expected_sdk_status(404):
                    deleted = Sandbox.delete_snapshot(resource_id, **sdk_options())
                status = "cleaned" if deleted else "already-deleted"
                detail = "Snapshot deleted after run-owned Sandboxes" if deleted else "Snapshot was already absent"
            except Exception as exc:
                text = str(exc).lower()
                if any(marker in text for marker in ("404", "not found", "does not exist")):
                    status, detail = "already-absent", "Snapshot was already absent during cleanup"
                else:
                    status, detail = "cleanup-failed", f"{type(exc).__name__}: {exc}"
            self.ledger.record_cleanup("snapshot", resource_id, status, detail)
            results.append({"kind": "snapshot", "id": resource_id, "status": status, "detail": detail})
        return results

    def _cleanup_templates(self) -> list[dict]:
        """Delete only Template IDs recorded under the current E2E run."""
        from e2b.api.client.api.templates import delete_templates_template_id
        from e2b.api.client_sync import get_api_client
        from e2b.connection_config import ConnectionConfig

        results: list[dict] = []
        cleaned_ids: set[str] = set()
        for resource in self.ledger.resources:
            if resource["kind"] != "template" or resource["run_id"] != self.run_id:
                continue

            template_id = resource["id"]
            template_name = resource.get("name")
            if template_id in cleaned_ids:
                continue
            cleaned_ids.add(template_id)

            if not isinstance(template_name, str) or self.run_id not in template_name:
                status, detail = "cleanup-skipped", "Template name did not pass run-scoped safety validation"
            elif template_id == template_name:
                status, detail = "cleanup-skipped", "Template ID was not returned; refusing name-based deletion"
            else:
                try:
                    client = get_api_client(ConnectionConfig(**sdk_options()))
                    response = delete_templates_template_id.sync_detailed(
                        template_id,
                        client=client,
                    )
                    if response.status_code == 404:
                        status, detail = "already-absent", "Template was already absent during cleanup"
                    elif response.status_code >= 300:
                        status, detail = "cleanup-failed", f"HTTP {response.status_code} while deleting Template"
                    else:
                        status, detail = "cleaned", "Template deleted by its run-scoped ID"
                except Exception as exc:
                    text = str(exc).lower()
                    if any(marker in text for marker in ("404", "not found", "does not exist")):
                        status, detail = "already-absent", "Template was already absent during cleanup"
                    else:
                        status, detail = "cleanup-failed", f"{type(exc).__name__}: {exc}"

            self.ledger.record_cleanup("template", template_id, status, detail)
            results.append({"kind": "template", "id": template_id, "status": status, "detail": detail})
        return results

    def _main_sandbox(self) -> str:
        value = self.context.get("main_sandbox")
        if not isinstance(value, str):
            raise RuntimeError("shared sandbox is unavailable")
        return value

    def _sandbox_args(self, parameters: dict) -> list[str]:
        args = ["create-sandbox"]
        for option in ("template", "timeout"):
            if parameters.get(option) is not None:
                args.extend([f"--{option}", str(parameters[option])])
        for option in ("metadata", "envs"):
            if parameters.get(option) is not None:
                args.extend([f"--{option}", json.dumps(parameters[option], ensure_ascii=False)])
        if parameters.get("secure"):
            args.append("--secure")
        return args

    def _expect_cli_error(self, case: TestCase, _context: dict):
        result = self.invoker.run(list(case.parameters["argv"]), timeout=30)
        passed = result.returncode != 0
        return (CaseStatus.PASS if passed else CaseStatus.FAIL, f"returncode={result.returncode}", [result.combined[-1000:]])

    def _verify_sandbox_connection(self, sandbox_id: str) -> tuple[bool, list[str]]:
        """Wait for client-proxy route registration without hiding persistent failures."""
        evidence: list[str] = []
        for attempt in range(1, 7):
            verify = self.invoker.run(
                ["run-command", "--sandbox-id", sandbox_id, "--command", "printf E2E_CONNECTED"],
                timeout=60,
                sandbox_id=sandbox_id,
            )
            if verify.returncode == 0 and "E2E_CONNECTED" in verify.stdout:
                evidence.append(f"attempt={attempt}, returncode=0, connected=true")
                return True, evidence
            evidence.append(
                f"attempt={attempt}, returncode={verify.returncode}\n{verify.combined[-1000:]}"
            )
            if attempt == 6 or not _sandbox_route_pending(verify.combined):
                break
            time.sleep(3)
        return False, evidence

    def _create_sandbox(self, case: TestCase, _context: dict):
        result = self.invoker.run(self._sandbox_args(case.parameters), timeout=120)
        expected_error = bool(case.parameters.get("expect_error"))
        if expected_error:
            status, actual = _negative_case_outcome(case, result.returncode)
            return status, actual, [result.combined[-1500:]]
        if result.returncode != 0:
            if _sandbox_placement_blocked(result.combined):
                evidence = [result.combined[-2000:]]
                evidence.extend(str(item) for item in self.context.get("placement_probe_evidence", []))
                return (
                    CaseStatus.BLOCKED,
                    _sandbox_placement_message(result.returncode),
                    evidence,
                )
            status = CaseStatus.BLOCKED if _is_blocked(result.combined) else CaseStatus.FAIL
            return status, f"创建失败，returncode={result.returncode}", [result.combined[-2000:]]
        payload = result.json()
        sandbox_id = extract_resource_id(payload, "sandbox")
        if not sandbox_id:
            return CaseStatus.FAIL, "创建输出缺少 sandbox ID", [result.stdout[-1500:]]
        self.ledger.record_sandbox(sandbox_id, case.case_id)
        self.context["sandbox_ids"].append(sandbox_id)
        if case.parameters.get("context_key") == "main":
            self.context["main_sandbox"] = sandbox_id
        if case.case_id == "SB-002":
            self.context["metadata_sandbox_id"] = sandbox_id
            self.context["metadata_expected"] = case.parameters.get("metadata", {})
        connected, connection_evidence = self._verify_sandbox_connection(sandbox_id)
        if not connected:
            combined_evidence = "\n".join(connection_evidence)
            if _sandbox_route_pending(combined_evidence):
                return (
                    CaseStatus.BLOCKED,
                    f"已创建 {sandbox_id}，但 client-proxy 在 6 次尝试后仍无法路由到 sandbox",
                    connection_evidence,
                )
            return CaseStatus.FAIL, f"已创建 {sandbox_id}，但连接验证失败", connection_evidence
        if case.parameters.get("envs"):
            env_verify = self.invoker.run(["run-command", "--sandbox-id", sandbox_id, "--command", "printf %s \"$E2E_RUN_ID\""], timeout=60, sandbox_id=sandbox_id)
            if self.run_id not in env_verify.stdout:
                return CaseStatus.FAIL, f"已创建 {sandbox_id}，但 envs 未生效", [env_verify.combined[-1000:]]
        if case.parameters.get("secure"):
            secure_fields = {
                key: value
                for key, value in (payload.items() if isinstance(payload, dict) else [])
                if "secure" in key.lower() or "token" in key.lower()
            }
            observable = any(value not in (None, "", False, [], {}) for value in secure_fields.values())
            if not observable:
                return CaseStatus.BLOCKED, "secure 参数已透传，但服务端未暴露可观察安全属性", [
                    json.dumps({"secure_fields": sorted(secure_fields), "observable": False}, ensure_ascii=False)
                ]
            return CaseStatus.PASS, f"创建并连接成功，secure 属性可观察：{sorted(secure_fields)}", [
                json.dumps({"secure_fields": sorted(secure_fields), "observable": True}, ensure_ascii=False)
            ]
        return CaseStatus.PASS, f"创建并连接成功：{sandbox_id}", [json.dumps(payload, ensure_ascii=False)]

    def _sandbox_lifecycle(self, case: TestCase, _context: dict):
        result = self.invoker.run(self._sandbox_args(case.parameters), timeout=90)
        if result.returncode != 0:
            if _sandbox_placement_blocked(result.combined):
                evidence = [result.combined[-1500:]]
                evidence.extend(str(item) for item in self.context.get("placement_probe_evidence", []))
                return (
                    CaseStatus.BLOCKED,
                    _sandbox_placement_message(result.returncode),
                    evidence,
                )
            return CaseStatus.FAIL, "生命周期沙箱创建失败", [result.combined[-1500:]]
        sandbox_id = extract_resource_id(result.json(), "sandbox")
        if not sandbox_id:
            return CaseStatus.FAIL, "创建输出缺少 sandbox ID", [result.stdout]
        self.ledger.record_sandbox(sandbox_id, case.case_id)
        self.context["sandbox_ids"].append(sandbox_id)
        return self._observe_lifecycle(case, sandbox_id)

    def _observe_lifecycle(self, case: TestCase, sandbox_id: str):
        requested = int(case.parameters["timeout"])
        grace = int(case.parameters.get("grace_seconds", 35))
        observations = []

        def observe(label: str) -> bool:
            listed = self.invoker.run(["list-sandboxes"], timeout=30)
            if listed.returncode != 0:
                observations.append({"at": label, "error": listed.combined[-1000:]})
                return False
            payload = listed.json()
            items = payload.get("sandboxes", []) if isinstance(payload, dict) else []
            matched = next(
                (item for item in items if isinstance(item, dict) and item.get("sandbox_id") == sandbox_id),
                None,
            )
            observations.append({
                "at": label,
                "visible": matched is not None,
                "state": matched.get("state") if isinstance(matched, dict) else None,
                "started_at": matched.get("started_at") if isinstance(matched, dict) else None,
                "end_at": matched.get("end_at") if isinstance(matched, dict) else None,
            })
            return matched is not None

        if not observe("T+0"):
            return CaseStatus.FAIL, "timeout sandbox was not visible immediately after creation", [
                json.dumps(observations, ensure_ascii=False)
            ]
        time.sleep(requested + 2)
        still_visible = observe(f"T+{requested + 2}")
        if still_visible:
            time.sleep(max(0, grace - requested - 2))
            final_visible = observe(f"T+{grace}")
        else:
            final_visible = False
        elapsed_note = f"requested={requested}s, checked through T+{grace}s"
        return CaseStatus.PASS if not final_visible else CaseStatus.FAIL, (
            f"{elapsed_note}; final visible={final_visible}"
        ), [json.dumps(observations, ensure_ascii=False)]

    def _create_template(self, case: TestCase, _context: dict):
        p = case.parameters
        args = ["create-template", "--name", str(p["name"])]
        for source in ("base_image", "dockerfile_content", "dockerfile"):
            if p.get(source) is not None:
                args.extend([f"--{source.replace('_', '-')}", str(p[source])])
        args.extend(["--cpu-count", str(p.get("cpu_count", 1)), "--memory-mb", str(p.get("memory_mb", 1024))])
        if p.get("skip_cache"):
            args.append("--skip-cache")
        result = self.invoker.run(args, timeout=int(p.get("timeout", 600)))
        expected_error = bool(p.get("expect_error"))
        if result.returncode != 0:
            if expected_error:
                name = str(p["name"])
                matches = [
                    template
                    for template in _load_visible_templates()
                    if name in template_names(template)
                ]
                if len(matches) > 1:
                    return (
                        CaseStatus.FAIL,
                        "无效模板构建被拒绝，但服务端返回多个同名 Template，无法安全登记清理",
                        [result.combined[-2000:], json.dumps(matches, ensure_ascii=False)[-2500:]],
                    )
                if matches:
                    partial_id = listed_template_id(matches[0])
                    if not partial_id:
                        return (
                            CaseStatus.FAIL,
                            "无效模板构建被拒绝，但服务端残留 Template 未返回 ID",
                            [result.combined[-2000:], json.dumps(matches[0], ensure_ascii=False)[-2000:]],
                        )
                    self.ledger.record_template(partial_id, name, case.case_id)
                    return (
                        CaseStatus.PASS,
                        "无效模板构建被拒绝；服务端 error Template 已登记清理",
                        [result.combined[-2000:], json.dumps(matches[0], ensure_ascii=False)[-2000:]],
                    )
                return CaseStatus.PASS, "无效模板构建被拒绝，服务端未产生 Template", [result.combined[-2500:]]
            if _template_build_blocked(result.combined):
                return (
                    CaseStatus.BLOCKED,
                    _template_build_blocked_message(result.returncode),
                    [result.combined[-3000:]],
                )
            return CaseStatus.FAIL, f"模板构建失败，returncode={result.returncode}", [result.combined[-3000:]]
        if expected_error:
            return CaseStatus.FAIL, "无效模板意外构建成功", [result.stdout[-2000:]]
        payload = result.json()
        template_id = extract_resource_id(payload, "template") or str(p["name"])
        self.ledger.record_template(template_id, str(p["name"]), case.case_id)
        self.context["template_names"].append(str(p["name"]))
        return CaseStatus.PASS, f"模板构建完成：{p['name']}", [json.dumps(payload, ensure_ascii=False)[-2500:]]

    def _run_command(self, case: TestCase, _context: dict):
        p = case.parameters
        if p.get("client_error"):
            return self._expect_cli_error(TestCase(case.case_id, case.business, case.title, case.purpose, case.preconditions, {"argv": ["run-command", "--sandbox-id", self._main_sandbox(), "--command", ""]}, case.steps, case.expected, "expect_cli_error"), _context)
        sandbox_id = str(p.get("sandbox_id") or self._main_sandbox())
        args = ["run-command", "--sandbox-id", sandbox_id, "--command", str(p["command"])]
        for option in ("cwd", "user", "timeout"):
            if p.get(option) is not None:
                args.extend([f"--{option}", str(p[option])])
        if p.get("envs") is not None:
            args.extend(["--envs", json.dumps(p["envs"], ensure_ascii=False)])
        result = self.invoker.run(args, timeout=max(30, int(p.get("timeout", 20)) + 10), sandbox_id=sandbox_id)
        if p.get("expect_error"):
            status, actual = _negative_case_outcome(case, result.returncode)
            return status, actual, [result.combined[-1500:]]
        expected_exit = p.get("expected_exit")
        contains = str(p.get("contains", ""))
        passed = result.returncode == expected_exit and (not contains or contains in result.combined)
        return (CaseStatus.PASS if passed else CaseStatus.FAIL, f"returncode={result.returncode}, expected={expected_exit}", [result.combined[-2000:]])

    def _materialize_upload(self, path: Path, parameters: dict) -> None:
        kind, content = parameters.get("content_type"), str(parameters.get("content", ""))
        if kind == "hex":
            path.write_bytes(bytes.fromhex(content))
        elif kind == "repeat":
            path.write_bytes(b"E2B-DATA" * (int(content) * 32))
        else:
            path.write_text(content, encoding="utf-8")

    def _upload_file(self, case: TestCase, _context: dict):
        sandbox_id = self._main_sandbox()
        p = case.parameters
        file_user = str(p.get("user") or "")
        upload_user_args = ["--user", file_user] if file_user else []
        with tempfile.TemporaryDirectory(prefix=f"{self.run_id}-") as temp_dir:
            local = Path(temp_dir) / "source.bin"
            if not p.get("missing_local"):
                self._materialize_upload(local, p)
            remote = str(p["remote_path"])
            parent = str(Path(remote).parent).replace("\\", "/")
            self.invoker.run(
                ["run-command", "--sandbox-id", sandbox_id, "--command", f"mkdir -p {parent}"],
                sandbox_id=sandbox_id,
            )

            def probe(user: str):
                command = "id; whoami; stat -c '%U:%G %a %n' " + shlex.quote(remote)
                return self.invoker.run(
                    ["run-command", "--sandbox-id", sandbox_id, "--user", user, "--command", command],
                    timeout=30,
                    sandbox_id=sandbox_id,
                )

            setup_probe = None
            if p.get("initial_content") is not None:
                initial = Path(temp_dir) / "initial.bin"
                self._materialize_upload(initial, {
                    "content_type": p.get("initial_content_type", "text"),
                    "content": p["initial_content"],
                })
                setup = self.invoker.run(
                    [
                        "upload-file", "--sandbox-id", sandbox_id,
                        "--local-path", str(initial), "--remote-path", remote,
                        *upload_user_args,
                    ],
                    timeout=90,
                    sandbox_id=sandbox_id,
                )
                if setup.returncode != 0:
                    return CaseStatus.FAIL, "initial upload failed", [setup.combined[-1000:]]
                setup_probe = probe(file_user or "user")

            result = self.invoker.run(
                [
                    "upload-file", "--sandbox-id", sandbox_id,
                    "--local-path", str(local), "--remote-path", remote,
                    *upload_user_args,
                ],
                timeout=90,
                sandbox_id=sandbox_id,
            )
            if p.get("missing_local"):
                return (
                    CaseStatus.PASS if result.returncode != 0 else CaseStatus.FAIL,
                    f"returncode={result.returncode}",
                    [result.combined[-1000:]],
                )
            if result.returncode != 0:
                evidence = [result.combined[-1500:]]
                if setup_probe is not None:
                    evidence.append("initial_write_probe:\n" + setup_probe.combined[-1500:])
                evidence.append("user_probe:\n" + probe(file_user or "user").combined[-1500:])
                evidence.append("root_probe:\n" + probe("root").combined[-1500:])
                root_overwrite = self.invoker.run(
                    [
                        "upload-file", "--sandbox-id", sandbox_id,
                        "--local-path", str(local), "--remote-path", remote,
                        "--user", "root",
                    ],
                    timeout=90,
                    sandbox_id=sandbox_id,
                )
                evidence.append("root_overwrite:\n" + root_overwrite.combined[-1500:])
                delete = self.invoker.run(
                    [
                        "run-command", "--sandbox-id", sandbox_id,
                        "--user", "root", "--command",
                        f"rm -f {shlex.quote(remote)}",
                    ],
                    timeout=30,
                    sandbox_id=sandbox_id,
                )
                evidence.append("delete_then_reupload.delete:\n" + delete.combined[-1500:])
                retry = self.invoker.run(
                    [
                        "upload-file", "--sandbox-id", sandbox_id,
                        "--local-path", str(local), "--remote-path", remote,
                        *upload_user_args,
                    ],
                    timeout=90,
                    sandbox_id=sandbox_id,
                )
                evidence.append("delete_then_reupload.upload:\n" + retry.combined[-1500:])
                return CaseStatus.FAIL, "upload failed; permission diagnostics collected", evidence

            local_hash = sha256_file(local)
            remote_hash_result = self.invoker.run(
                ["run-command", "--sandbox-id", sandbox_id, "--command", f"sha256sum {remote}"],
                sandbox_id=sandbox_id,
            )
            passed = remote_hash_result.returncode == 0 and local_hash in remote_hash_result.stdout
            return (
                CaseStatus.PASS if passed else CaseStatus.FAIL,
                f"local_sha256={local_hash}",
                [remote_hash_result.combined[-1500:]],
            )

    def _download_file(self, case: TestCase, _context: dict):
        sandbox_id = self._main_sandbox()
        p = case.parameters
        if p.get("setup_command"):
            setup = self.invoker.run(["run-command", "--sandbox-id", sandbox_id, "--command", str(p["setup_command"])], sandbox_id=sandbox_id)
            if setup.returncode != 0:
                return CaseStatus.FAIL, "远端测试文件准备失败", [setup.combined]
        with tempfile.TemporaryDirectory(prefix=f"{self.run_id}-") as temp_dir:
            local = Path(temp_dir) / "nested" / "download.bin"
            if p.get("local_exists") or p.get("overwrite"):
                local.parent.mkdir(parents=True, exist_ok=True)
                local.write_text("old", encoding="utf-8")
            args = ["download-file", "--sandbox-id", sandbox_id, "--remote-path", str(p["remote_path"]), "--local-path", str(local)]
            if p.get("overwrite"):
                args.append("--overwrite")
            result = self.invoker.run(args, timeout=90, sandbox_id=sandbox_id)
            if p.get("expect_error") or p.get("local_exists"):
                unchanged = not p.get("local_exists") or local.read_text(encoding="utf-8") == "old"
                passed = result.returncode != 0 and unchanged
                return (CaseStatus.PASS if passed else CaseStatus.FAIL, f"returncode={result.returncode}, protected={unchanged}", [result.combined[-1000:]])
            if result.returncode != 0 or not local.is_file():
                return CaseStatus.FAIL, "下载失败或本地文件缺失", [result.combined[-1500:]]
            remote_hash = self.invoker.run(["run-command", "--sandbox-id", sandbox_id, "--command", f"sha256sum {p['remote_path']}"], sandbox_id=sandbox_id)
            local_hash = sha256_file(local)
            passed = remote_hash.returncode == 0 and local_hash in remote_hash.stdout
            return (CaseStatus.PASS if passed else CaseStatus.FAIL, f"local_sha256={local_hash}", [remote_hash.combined[-1500:]])

    def _list_sandboxes(self, case: TestCase, _context: dict):
        args = ["list-sandboxes"]
        if case.parameters.get("max_pages") is not None:
            args.extend(["--max-pages", str(case.parameters["max_pages"])])
        result = self.invoker.run(args, timeout=60)
        if result.returncode != 0:
            return CaseStatus.FAIL, "查询沙箱失败", [result.combined[-1500:]]
        payload = result.json()
        assertion = case.parameters.get("assertion")
        if assertion == "main-visible":
            passed = item_has_id(payload, self._main_sandbox(), "sandbox")
        elif assertion == "run-visible":
            ids = self.context.get("sandbox_ids", [])
            passed = any(item_has_id(payload, sandbox_id, "sandbox") for sandbox_id in ids)
        elif assertion == "metadata-visible":
            target_id = self.context.get("metadata_sandbox_id")
            expected = self.context.get("metadata_expected")
            matched = next(
                (
                    item
                    for item in (payload.get("sandboxes", []) if isinstance(payload, dict) else [])
                    if isinstance(item, dict) and item.get("sandbox_id") == target_id
                ),
                None,
            )
            actual_metadata = matched.get("metadata") if isinstance(matched, dict) else None
            passed = matched is not None and actual_metadata == expected
        else:
            passed = isinstance(payload, dict) and isinstance(payload.get("sandboxes"), list)
        actual = f"count={payload.get('count', 'unknown')}"
        if assertion == "metadata-visible":
            actual = f"metadata_visible={passed}, sandbox_id={self.context.get('metadata_sandbox_id', 'unknown')}"
        return (CaseStatus.PASS if passed else CaseStatus.FAIL, actual, [json.dumps(payload, ensure_ascii=False)[-2000:]])

    def _list_templates(self, case: TestCase, _context: dict):
        args = ["list-templates"]
        if case.parameters.get("max_pages") is not None:
            args.extend(["--max-pages", str(case.parameters["max_pages"])])
        result = self.invoker.run(args, timeout=60)
        if result.returncode != 0:
            return CaseStatus.FAIL, "查询模板失败", [result.combined[-1500:]]
        payload = result.json()
        serialized = json.dumps(payload, ensure_ascii=False)
        assertion = case.parameters.get("assertion")
        if assertion == "fixture-visible":
            fixture_template = str(case.parameters.get("fixture_template", ""))
            passed = fixture_template in serialized
        elif assertion == "run-visible":
            names = self.context.get("template_names", [])
            passed = not names or any(name in serialized for name in names)
        else:
            passed = isinstance(payload, dict) and isinstance(payload.get("templates"), list)
        actual = f"count={payload.get('count', 'unknown')}"
        if assertion == "fixture-visible":
            actual += f", fixture_template={case.parameters.get('fixture_template', '')}"
        return (CaseStatus.PASS if passed else CaseStatus.FAIL, actual, [serialized[-2500:]])

    def _run_extended(self, module_name: str, function_name: str, case: TestCase):
        module = __import__(f"e2b_validator.{module_name}", fromlist=[function_name])
        handler = getattr(module, function_name)
        return handler(case, self.context, self)

    def _extended_command(self, case, _context):
        return self._run_extended("e2e_command_handlers", "handle", case)

    def _extended_filesystem(self, case, _context):
        return self._run_extended("e2e_filesystem_handlers", "handle", case)

    def _extended_sandbox(self, case, _context):
        return self._run_extended("e2e_sandbox_handlers", "handle_sandbox", case)

    def _extended_network(self, case, _context):
        return self._run_extended("e2e_sandbox_handlers", "handle_network", case)

    def _extended_snapshot(self, case, _context):
        return self._run_extended("e2e_snapshot_handlers", "handle", case)

    def _extended_checkpoint(self, case, _context):
        return self._run_extended("e2e_checkpoint_handlers", "handle", case)

    def _extended_pause_resume(self, case, _context):
        return self._run_extended("e2e_pause_resume_handlers", "handle", case)

    def _extended_pty(self, case, _context):
        return self._run_extended("e2e_pty_handlers", "handle", case)

    def _extended_template(self, case, _context):
        return self._run_extended("e2e_template_handlers", "handle", case)


def register_subcommand(subparsers) -> None:
    parser = subparsers.add_parser("test-e2e", help="Run real E2B end-to-end business cases")
    add_config_arguments(parser)
    parser.add_argument("--all", action="store_true", help="Run the complete E2B SDK and API validation catalog")
    parser.add_argument("--case", action="append", dest="case_ids", help="Run one case ID; repeat to select more")
    parser.add_argument(
        "--template",
        help="Check this ready template directly; default: build an isolated fixture from --base-image",
    )
    parser.add_argument(
        "--base-image",
        default=os.getenv("E2B_E2E_BASE_IMAGE"),
        help="Base image for the isolated baseline fixture and TP-001/TP-002/TP-003",
    )
    parser.add_argument("--result-root", type=Path, default=PROJECT_DIR / "test-results")
    parser.add_argument("--keep-sandboxes", action="store_true", help="Do not clean this run's sandbox resources")
    parser.set_defaults(handler=execute)


def select_cases_with_dependencies(cases: list[TestCase], requested_ids: set[str]) -> list[TestCase]:
    """Return requested cases plus their transitive dependencies in catalog order."""
    by_id = {case.case_id: case for case in cases}
    unknown = requested_ids - set(by_id)
    if unknown:
        raise ValueError(f"Unknown E2E case IDs: {', '.join(sorted(unknown))}")

    selected: set[str] = set()
    visiting: set[str] = set()

    def include(case_id: str) -> None:
        if case_id in selected:
            return
        if case_id in visiting:
            raise ValueError(f"Cyclic E2E dependency detected at {case_id}")
        visiting.add(case_id)
        case = by_id[case_id]
        for dependency in case.depends_on:
            if dependency not in by_id:
                raise ValueError(f"E2E case {case_id} depends on unknown case {dependency}")
            include(dependency)
        visiting.remove(case_id)
        selected.add(case_id)

    for case_id in requested_ids:
        include(case_id)
    return [case for case in cases if case.case_id in selected]


def execute(args: argparse.Namespace) -> int:
    if not args.all and not args.case_ids:
        raise ValueError("test-e2e requires --all or at least one --case")
    try:
        base_image = discover_base_image(args.base_image, _load_visible_templates())
    except BaseImageDiscoveryError as exc:
        raise ValueError(str(exc)) from exc
    requested_case_ids = set(args.case_ids or [])
    if requested_case_ids:
        validation_cases = build_cases(
            "case-validation",
            template=args.template or "case-validation-template",
            base_image=base_image.image,
        )
        select_cases_with_dependencies(validation_cases, requested_case_ids)
    run_id = datetime.now().strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:6]
    result_dir = args.result_root.expanduser().resolve() / run_id
    report_path = result_dir / "report.md"
    fixture_reporter = E2EReporter(result_dir, report_path)
    fixture_invoker = CLIInvoker(fixture_reporter)
    fixture_template, fixture_created = _create_fixture_template(
        fixture_invoker,
        run_id=run_id,
        base_image=base_image.image,
        requested_template=args.template,
    )
    selected_fixture, placement_evidence = _select_runnable_fixture(
        fixture_invoker,
        fixture_template,
        requested_template=args.template,
    )
    cases = build_cases(run_id, template=selected_fixture.name, base_image=base_image.image)
    if requested_case_ids:
        cases = select_cases_with_dependencies(cases, requested_case_ids)
    runner = E2ERunner(
        cases,
        result_dir,
        markdown_path=report_path,
        run_id=run_id,
        cleanup=not args.keep_sandboxes,
        fixture_template=selected_fixture.name,
        fixture_created=fixture_template if fixture_created else None,
        placement_probe_evidence=placement_evidence,
    )
    summary = runner.run()
    print_run_summary(
        summary,
        runner.cleanup_results,
        runner.ledger.resources,
        result_dir,
        cleanup_enabled=runner.cleanup_enabled,
    )
    return 1 if (
        summary.counts.get(CaseStatus.FAIL.value, 0)
        or summary.counts.get(CaseStatus.BLOCKED.value, 0)
    ) else 0


if __name__ == "__main__":
    raise SystemExit(execute(argparse.Namespace(all=True, case_ids=None, template=None, base_image=os.getenv("E2B_E2E_BASE_IMAGE"), result_root=PROJECT_DIR / "test-results", keep_sandboxes=False)))
