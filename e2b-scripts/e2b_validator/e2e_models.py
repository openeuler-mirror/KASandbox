"""Data contracts for the real E2B end-to-end test runner."""

from __future__ import annotations

import dataclasses
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Any


class Business(str, Enum):
    CREATE_SANDBOX = "create-sandbox"
    CREATE_TEMPLATE = "create-template"
    RUN_COMMAND = "run-command"
    UPLOAD_FILE = "upload-file"
    DOWNLOAD_FILE = "download-file"
    LIST_SANDBOXES = "list-sandboxes"
    LIST_TEMPLATES = "list-templates"


class CaseStatus(str, Enum):
    PASS = "PASS"
    FAIL = "FAIL"
    BLOCKED = "BLOCKED"
    SKIPPED = "SKIPPED"


@dataclass(frozen=True)
class TestCase:
    case_id: str
    business: Business
    title: str
    purpose: str
    preconditions: list[str]
    parameters: dict[str, Any]
    steps: list[str]
    expected: str
    scenario: str
    depends_on: tuple[str, ...] = ()
    tags: tuple[str, ...] = ()


@dataclass
class CaseResult:
    case: TestCase
    status: CaseStatus
    duration_seconds: float
    actual: str
    evidence: list[str] = field(default_factory=list)
    started_at: str = field(default_factory=lambda: datetime.now(timezone.utc).isoformat())
    error_type: str | None = None

    def to_dict(self) -> dict[str, Any]:
        data = dataclasses.asdict(self)
        data["case"]["business"] = self.case.business.value
        data["status"] = self.status.value
        return data


@dataclass
class RunSummary:
    run_id: str
    results: list[CaseResult] = field(default_factory=list)
    started_at: str = field(default_factory=lambda: datetime.now(timezone.utc).isoformat())
    finished_at: str | None = None
    environment: dict[str, Any] = field(default_factory=dict)

    @property
    def counts(self) -> dict[str, int]:
        return {
            status.value: sum(result.status == status for result in self.results)
            for status in CaseStatus
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            "run_id": self.run_id,
            "started_at": self.started_at,
            "finished_at": self.finished_at,
            "environment": self.environment,
            "counts": self.counts,
            "results": [result.to_dict() for result in self.results],
        }
