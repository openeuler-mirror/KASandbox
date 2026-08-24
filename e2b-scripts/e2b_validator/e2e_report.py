"""Incremental JSON, command-log, and Chinese Markdown reports."""

from __future__ import annotations

import json
import re
from pathlib import Path
from typing import Any

from .e2e_models import Business, RunSummary


SECRET_PATTERNS = (
    re.compile(r"(?i)(E2B_API_KEY\s*=\s*)([^\s]+)"),
    re.compile(r'(?i)((?:access[_-]?token|teamApiKey|password)\s*[:=]\s*["\']?)([^"\'\s,}]+)'),
    re.compile(r"(?i)(Authorization\s*:\s*Bearer\s+)([^\s]+)"),
    re.compile(r'(?i)(["\']?(?:api[_-]?key|token|access[_-]?token|password)["\']?\s*:\s*["\'])([^"\']+)(["\'])'),
)


def redact(value: Any) -> str:
    text = str(value)
    for pattern in SECRET_PATTERNS:
        replacement = r"\1[REDACTED]\3" if pattern.groups >= 3 else r"\1[REDACTED]"
        text = pattern.sub(replacement, text)
    return text


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

    def save(self, summary: RunSummary, cleanup: list[dict] | None = None) -> None:
        payload = summary.to_dict()
        payload["cleanup"] = cleanup or []
        (self.result_dir / "result.json").write_text(
            json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8"
        )
        self.markdown_path.write_text(self._markdown(summary, cleanup or []), encoding="utf-8")

    def _markdown(self, summary: RunSummary, cleanup: list[dict]) -> str:
        counts = summary.counts
        lines = [
            "# 测试用例结果集合",
            "",
            "## 1. 执行结论",
            "",
            f"- 运行编号：`{summary.run_id}`",
            f"- 用例总数：{len(summary.results)}",
            f"- PASS：{counts['PASS']}",
            f"- FAIL：{counts['FAIL']}",
            f"- BLOCKED：{counts['BLOCKED']}",
            f"- SKIPPED：{counts['SKIPPED']}",
            f"- 开始时间：{summary.started_at}",
            f"- 完成时间：{summary.finished_at or '执行中'}",
            "",
            "## 2. 七项业务汇总",
            "",
            "| 业务 | 总数 | PASS | FAIL | BLOCKED | SKIPPED |",
            "|---|---:|---:|---:|---:|---:|",
        ]
        for business in Business:
            results = [item for item in summary.results if item.case.business == business]
            values = [sum(item.status.value == status for item in results) for status in ("PASS", "FAIL", "BLOCKED", "SKIPPED")]
            lines.append(f"| `{business.value}` | {len(results)} | {values[0]} | {values[1]} | {values[2]} | {values[3]} |")
        lines.extend(["", "## 3. 用例进展与详细结果", ""])
        for index, result in enumerate(summary.results, 1):
            case = result.case
            lines.extend([
                f"### 3.{index} {case.case_id} {case.title}",
                "",
                f"- 用例编号：`{case.case_id}`",
                f"- 所属业务：`{case.business.value}`",
                f"- 测试目的：{case.purpose}",
                f"- 前置条件：{'；'.join(case.preconditions)}",
                f"- 参数组合：`{redact(json.dumps(case.parameters, ensure_ascii=False, sort_keys=True))}`",
                f"- 测试步骤：{'；'.join(case.steps)}",
                f"- 预期结果：{case.expected}",
                f"- 实际结果：{redact(result.actual)}",
                f"- 耗时：{result.duration_seconds:.3f} 秒",
                f"- 结论：**{result.status.value}**",
                f"- 证据：{'；'.join(redact(item) for item in result.evidence) or '无'}",
                "",
            ])
        lines.extend(["## 4. 资源清理", ""])
        if cleanup:
            lines.extend(["| 类型 | ID | 状态 | 说明 |", "|---|---|---|---|"])
            for item in cleanup:
                lines.append(f"| {item.get('kind', '')} | `{item.get('id', '')}` | {item.get('status', '')} | {redact(item.get('detail', ''))} |")
        else:
            lines.append("本次尚无资源清理记录。")
        lines.extend([
            "",
            "## 5. 判定说明",
            "",
            "- `PASS`：实际行为符合预期；预期错误被正确拒绝也属于通过。",
            "- `FAIL`：脚本、接口返回或独立核验与预期不一致。",
            "- `BLOCKED`：镜像仓库、网络、服务状态或环境能力阻断了有效结论。",
            "- `SKIPPED`：依赖资源未就绪，继续执行无法产生有效结论。",
            "",
        ])
        return "\n".join(lines)
