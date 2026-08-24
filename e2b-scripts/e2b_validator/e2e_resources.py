"""Persistent ledger for resources created by one E2E run."""

from __future__ import annotations

import json
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable


CleanupResult = bool | tuple[str, str]


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
