"""Persistent ledger for resources created by one E2E run."""

from __future__ import annotations

import json
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable


CleanupResult = bool | tuple[str, str]
SUCCESSFUL_CLEANUP_STATUSES = frozenset({
    "cleaned",
    "already-stopped",
    "already-deleted",
    "already-absent",
    "expired",
})


def summarize_cleanup(
    resources: list[dict],
    cleanup: list[dict],
    *,
    enabled: bool,
) -> dict:
    """Summarize run-scoped cleanup without claiming an external server audit."""
    registered = {
        (str(item.get("kind", "")), str(item.get("id", "")))
        for item in resources
    }
    latest_by_resource: dict[tuple[str, str], str] = {}
    for item in cleanup:
        key = (str(item.get("kind", "")), str(item.get("id", "")))
        latest_by_resource[key] = str(item.get("status", "unknown"))
    recorded = set(latest_by_resource)
    statuses = Counter(str(item.get("status", "unknown")) for item in cleanup)
    final_statuses = Counter(
        status
        for key, status in latest_by_resource.items()
        if key in registered
    )
    failed = final_statuses["cleanup-failed"]
    skipped = final_statuses["cleanup-skipped"]
    unknown = sum(
        count
        for status, count in final_statuses.items()
        if status not in SUCCESSFUL_CLEANUP_STATUSES
        and status not in {"cleanup-failed", "cleanup-skipped"}
    )
    pending = len(registered - recorded)
    completed = sum(
        status in SUCCESSFUL_CLEANUP_STATUSES
        for key, status in latest_by_resource.items()
        if key in registered
    )
    unresolved = len(registered) - completed
    clean = (
        enabled
        and unresolved == 0
        and failed == 0
        and skipped == 0
        and unknown == 0
    )
    return {
        "enabled": enabled,
        "registered": len(registered),
        "recorded": len(registered & recorded),
        "completed": completed,
        "failed": failed,
        "skipped": skipped,
        "unknown": unknown,
        "pending": pending,
        "unresolved": unresolved,
        "clean": clean,
        "status_counts": dict(sorted(statuses.items())),
        "resource_counts": dict(sorted(
            Counter(kind for kind, _resource_id in registered).items()
        )),
        "scope": "run-ledger",
    }


class ResourceLedger:
    def __init__(self, path: Path, run_id: str):
        self.path = path
        self.run_id = run_id
        self.resources: list[dict] = []
        self.cleanup: list[dict] = []
        self._save()

    def _record(self, kind: str, resource_id: str, name: str | None, case_id: str) -> None:
        if name is not None and self.run_id not in name:
            raise ValueError("Named resources must contain the current run ID")
        self.resources.append({
            "kind": kind,
            "id": resource_id,
            "name": name,
            "case_id": case_id,
            "run_id": self.run_id,
            "created_at": datetime.now(timezone.utc).isoformat(),
        })
        self._save()

    def record_sandbox(self, sandbox_id: str, case_id: str) -> None:
        self._record("sandbox", sandbox_id, None, case_id)

    def record_template(self, template_id: str, name: str, case_id: str) -> None:
        self._record("template", template_id, name, case_id)

    def record_background_process(self, sandbox_id: str, pid: int, case_id: str) -> None:
        self._record("background-process", f"{sandbox_id}:{pid}", None, case_id)

    def record_pty(self, sandbox_id: str, pid: int, case_id: str) -> None:
        self._record("pty", f"{sandbox_id}:{pid}", None, case_id)

    def record_watcher(self, sandbox_id: str, label: str, case_id: str) -> None:
        self._record("watcher", f"{sandbox_id}:{label}", None, case_id)

    def record_snapshot(self, snapshot_id: str, case_id: str) -> None:
        self._record("snapshot", snapshot_id, None, case_id)

    def record_template_tag(self, template_name: str, tag: str, case_id: str) -> None:
        self._record("template-tag", f"{template_name}:{tag}", template_name, case_id)

    def record_cleanup(self, kind: str, resource_id: str, status: str, detail: str) -> None:
        self.cleanup.append({"kind": kind, "id": resource_id, "status": status, "detail": detail})
        self._save()

    def cleanup_sandboxes(self, cleaner: Callable[[str], CleanupResult]) -> list[dict]:
        seen: set[str] = set()
        for resource in self.resources:
            if resource["kind"] != "sandbox" or resource["run_id"] != self.run_id or resource["id"] in seen:
                continue
            seen.add(resource["id"])
            try:
                cleaned = cleaner(resource["id"])
                if isinstance(cleaned, tuple):
                    outcome, detail = cleaned
                else:
                    outcome = "cleaned" if cleaned else "cleanup-failed"
                    detail = "cleanup call completed" if cleaned else "cleanup call returned false"
            except Exception as exc:
                outcome = "cleanup-failed"
                detail = f"{type(exc).__name__}: {exc}"
            self.cleanup.append({"kind": "sandbox", "id": resource["id"], "status": outcome, "detail": detail})
            self._save()
        return list(self.cleanup)

    def _save(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        payload = {"run_id": self.run_id, "resources": self.resources, "cleanup": self.cleanup}
        self.path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
