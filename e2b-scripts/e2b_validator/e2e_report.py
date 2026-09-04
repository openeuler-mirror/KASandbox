"""Incremental JSON, command-log, and Chinese Markdown reports."""

from __future__ import annotations

import json
import re
from datetime import datetime
from pathlib import Path
from typing import Any

from .e2e_models import Business, CaseStatus, RunSummary
from .e2e_objectives import detailed_objective
from .e2e_resources import summarize_cleanup


SECRET_PATTERNS = (
    re.compile(r"(?i)(E2B_API_KEY\s*=\s*)([^\s]+)"),
    re.compile(r'(?i)((?:access[_-]?token|teamApiKey|password)\s*[:=]\s*["\']?)([^"\'\s,}]+)'),
    re.compile(r"(?i)(Authorization\s*:\s*Bearer\s+)([^\s]+)"),
    re.compile(r'(?i)(["\']?(?:api[_-]?key|token|access[_-]?token|password)["\']?\s*:\s*["\'])([^"\']+)(["\'])'),
    re.compile(
        r"(?i)([?&](?:api[_-]?key|token|access[_-]?token|signature|credential|"
        r"x-amz-signature|x-amz-credential|x-amz-security-token)=)([^&\s]+)"
    ),
)

SENSITIVE_KEY_MARKERS = (
    "apikey",
    "accesstoken",
    "authorization",
    "password",
    "credential",
    "signature",
    "secret",
    "hashseed",
)


def redact(value: Any) -> str:
    text = str(value)
    for pattern in SECRET_PATTERNS:
        replacement = r"\1[REDACTED]\3" if pattern.groups >= 3 else r"\1[REDACTED]"
        text = pattern.sub(replacement, text)
    return text


def redact_data(value: Any) -> Any:
    """Recursively redact structured output while preserving its JSON shape."""
    if isinstance(value, dict):
        redacted: dict[str, Any] = {}
        for key, item in value.items():
            rendered_key = str(key)
            normalized_key = re.sub(r"[^a-z0-9]", "", rendered_key.lower())
            if normalized_key == "token" or any(marker in normalized_key for marker in SENSITIVE_KEY_MARKERS):
                redacted[rendered_key] = "[REDACTED]"
            else:
                redacted[rendered_key] = redact_data(item)
        return redacted
    if isinstance(value, list):
        return [redact_data(item) for item in value]
    if isinstance(value, tuple):
        return [redact_data(item) for item in value]
    if isinstance(value, str):
        return redact(value)
    return value


class E2EReporter:
    def __init__(self, result_dir: Path, markdown_path: Path):
        self.result_dir = result_dir
        self.markdown_path = markdown_path
        self.result_dir.mkdir(parents=True, exist_ok=True)
        self.markdown_path.parent.mkdir(parents=True, exist_ok=True)
        self.command_log = self.result_dir / "commands.log"

    def log_command(self, command: list[str], returncode: int, stdout: str, stderr: str, duration: float) -> None:
        entry = (
            f"$ {redact(' '.join(command))}\n"
            f"duration={duration:.3f}s returncode={returncode}\n"
            f"stdout:\n{redact(stdout)}\n"
            f"stderr:\n{redact(stderr)}\n{'=' * 72}\n"
        )
        with self.command_log.open("a", encoding="utf-8") as stream:
            stream.write(entry)

    def save(
        self,
        summary: RunSummary,
        cleanup: list[dict] | None = None,
        *,
        resources: list[dict] | None = None,
        cleanup_enabled: bool = True,
    ) -> None:
        cleanup_items = cleanup or []
        cleanup_summary = (
            summarize_cleanup(resources, cleanup_items, enabled=cleanup_enabled)
            if resources is not None
            else None
        )
        payload = redact_data(summary.to_dict())
        payload["cleanup"] = redact_data(cleanup_items)
        if cleanup_summary is not None:
            payload["cleanup_summary"] = redact_data(cleanup_summary)
        (self.result_dir / "result.json").write_text(
            json.dumps(payload, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        self.markdown_path.write_text(
            self._markdown(summary, cleanup_items, cleanup_summary),
            encoding="utf-8",
        )

    def _markdown(
        self,
        summary: RunSummary,
        cleanup: list[dict],
        cleanup_summary: dict | None,
    ) -> str:
        counts = summary.counts
        total = len(summary.results)
        pass_rate = counts["PASS"] * 100 / total if total else 0.0
        negative_count = sum(
            "expected-error" in item.case.tags
            for item in summary.results
        )
        boundary_count = sum(
            "boundary" in item.case.tags
            for item in summary.results
        )
        duration = None
        if summary.finished_at:
            try:
                duration = (
                    datetime.fromisoformat(summary.finished_at)
                    - datetime.fromisoformat(summary.started_at)
                ).total_seconds()
            except ValueError:
                duration = None
        lines = [
            "# 测试用例结果集合",
            "",
            "## 1. 执行结论",
            "",
            f"- 运行编号：`{summary.run_id}`",
            f"- 用例总数：{total}",
            f"- PASS：{counts['PASS']}",
            f"- FAIL：{counts['FAIL']}",
            f"- BLOCKED：{counts['BLOCKED']}",
            f"- SKIPPED：{counts['SKIPPED']}",
            f"- 通过率：{pass_rate:.2f}%",
            f"- 正向用例：{total - negative_count}",
            f"- 负向用例：{negative_count}",
            f"- 边界用例：{boundary_count}（可能同时属于负向用例）",
            f"- 开始时间：{summary.started_at}",
            f"- 完成时间：{summary.finished_at or '执行中'}",
            f"- 执行耗时：{f'{duration:.3f} 秒' if duration is not None else '执行中'}",
            f"- 业务覆盖：{len({item.case.business for item in summary.results})} 类",
            "",
            "## 2. 业务汇总",
            "",
            "| 业务 | 总数 | PASS | FAIL | BLOCKED | SKIPPED |",
            "|---|---:|---:|---:|---:|---:|",
        ]
        for business in Business:
            results = [item for item in summary.results if item.case.business == business]
            values = [
                sum(item.status.value == status for item in results)
                for status in ("PASS", "FAIL", "BLOCKED", "SKIPPED")
            ]
            lines.append(
                f"| `{business.value}` | {len(results)} | "
                f"{values[0]} | {values[1]} | {values[2]} | {values[3]} |"
            )

        lines.extend(["", "## 3. 用例进展与详细结果", ""])
        for index, result in enumerate(summary.results, 1):
            case = result.case
            lines.extend([
                f"### 3.{index} {case.case_id} {case.title}",
                "",
                f"- 用例编号：`{case.case_id}`",
                f"- 所属业务：`{case.business.value}`",
                f"- 测试目的：{redact(detailed_objective(case))}",
                f"- 前置条件：{'；'.join(redact(item) for item in case.preconditions)}",
                f"- 参数组合：`{redact(json.dumps(case.parameters, ensure_ascii=False, sort_keys=True))}`",
                f"- 测试步骤：{'；'.join(redact(item) for item in case.steps)}",
                f"- 预期结果：{redact(case.expected)}",
                f"- 实际结果：{redact(result.actual)}",
                f"- 耗时：{result.duration_seconds:.3f} 秒",
                f"- 结论：**{result.status.value}**",
                f"- 证据：{'；'.join(redact(item) for item in result.evidence) or '无'}",
            ])
            if result.status in {CaseStatus.FAIL, CaseStatus.BLOCKED}:
                diagnostic = result.diagnostics
                lines.extend([
                    f"- 失败阶段：`{redact(diagnostic.get('failure_stage', 'unknown'))}`",
                    "- 证据摘要：",
                ])
                evidence_summary = diagnostic.get("evidence_summary", [])
                lines.extend(f"  - {redact(item)}" for item in evidence_summary)
                lines.extend([
                    "- 可能原因：",
                ])
                causes = diagnostic.get("possible_causes", [])
                lines.extend(f"  - {redact(item)}" for item in causes)
                lines.append("- 只读定位命令：")
                lines.append("")
                lines.append("```bash")
                lines.extend(redact(item) for item in diagnostic.get("next_commands", []))
                lines.append("```")
                lines.extend([
                    "",
                    f"- 结果文件：{'；'.join(redact(item) for item in diagnostic.get('artifacts', []))}",
                    "- AI/Agent 定位提示：",
                    "",
                    "```text",
                    redact(diagnostic.get("analysis_prompt", "")),
                    "```",
                ])
            lines.append("")

        lines.extend(["## 4. 资源清理", ""])
        if cleanup_summary is not None:
            lines.extend([
                f"- 登记资源：{cleanup_summary['registered']}",
                f"- 完成清理：{cleanup_summary['completed']}",
                f"- 清理失败：{cleanup_summary['failed']}",
                f"- 清理跳过：{cleanup_summary['skipped']}",
                f"- 未知状态：{cleanup_summary['unknown']}",
                f"- 未记录结果：{cleanup_summary['pending']}",
                f"- 未完成资源：{cleanup_summary['unresolved']}",
                (
                    "- 账本判定：本轮登记资源均已处理"
                    if cleanup_summary["clean"]
                    else "- 账本判定：清理不完整，需检查下表"
                ),
                "- 核验范围：仅限本轮 run_id 资源账本，不处理其他业务资源",
                "",
            ])
        if cleanup:
            lines.extend([
                "| 类型 | ID | 状态 | 说明 |",
                "|---|---|---|---|",
            ])
            for item in cleanup:
                lines.append(
                    f"| {item.get('kind', '')} | `{item.get('id', '')}` | "
                    f"{item.get('status', '')} | {redact(item.get('detail', ''))} |"
                )
        else:
            lines.append("本次尚无资源清理记录。")

        lines.extend([
            "",
            "## 5. 判定说明",
            "",
            "- `PASS`：实际行为符合预期；负向用例收到预期错误也属于通过。",
            "- `FAIL`：脚本、接口返回或独立核验与预期不一致。",
            "- `BLOCKED`：镜像仓库、调度、网络、服务状态或版本能力阻断了有效结论。",
            "- `SKIPPED`：依赖用例未通过，继续执行无法产生有效结论。",
            "",
        ])
        return "\n".join(lines)
