"""Generate concise, consistent objectives for terminal output and reports."""

from __future__ import annotations

import re

from .e2e_models import Business, TestCase


SCENARIO_ACTIONS = {
    "expect_cli_error": "通过 CLI 提交边界或错误参数并检查客户端前置校验",
    "create_sandbox": "通过业务入口创建并连接 Sandbox",
    "sandbox_lifecycle": "创建短生命周期 Sandbox 并持续轮询控制面状态",
    "create_template": "发起 Template 构建并跟踪调度及最终状态",
    "run_command": "在共享 Sandbox 内执行命令并采集退出码和输出",
    "upload_file": "上传测试文件并在 Sandbox 内独立计算摘要",
    "download_file": "准备远端文件后下载到本地并逐字节核验",
    "list_sandboxes": "查询 Sandbox 列表并交叉核对本轮资源",
    "list_templates": "查询 Template 列表并交叉核对构建状态",
    "extended-command": "通过 Commands SDK 执行后台或流式命令操作",
    "extended-filesystem": "通过 Filesystem SDK 执行文件与目录操作",
    "extended-sandbox": "通过 Sandbox SDK 调用状态与生命周期能力",
    "extended-network": "通过 Sandbox Network API 生成并检查端口路由",
    "extended-snapshot": "通过 Snapshot SDK 执行快照生命周期操作",
    "extended-checkpoint": "创建 Checkpoint 并从独立 Sandbox 核验恢复状态",
    "extended-pause-resume": "调用 pause、resume 或 connect 并轮询状态",
    "extended-pty": "通过 PTY SDK 建立终端会话并执行交互操作",
    "extended-template": "通过 Template SDK 执行构建与资源管理操作",
}


BUSINESS_RISKS = {
    Business.CREATE_SANDBOX: "无效配置、调度失败或生命周期异常",
    Business.CREATE_TEMPLATE: "构建参数、基础镜像或调度链路异常",
    Business.RUN_COMMAND: "命令执行语义或结果回传偏差",
    Business.UPLOAD_FILE: "上传内容损坏、权限冲突或路径处理错误",
    Business.DOWNLOAD_FILE: "下载内容损坏或覆盖保护失效",
    Business.LIST_SANDBOXES: "控制面资源可见性或分页结果偏差",
    Business.LIST_TEMPLATES: "模板可见性、状态或分页结果偏差",
    Business.BACKGROUND_COMMAND: "后台进程控制或流式输出异常",
    Business.FILESYSTEM: "文件内容、路径或目录操作异常",
    Business.FILESYSTEM_WATCH: "文件事件遗漏或递归监听异常",
    Business.SIGNED_FILE_URL: "签名 URL 路径、身份或时效异常",
    Business.SANDBOX_INSPECTION: "Sandbox 状态读取或连接信息偏差",
    Business.SANDBOX_LIFECYCLE: "暂停、恢复或连接状态迁移异常",
    Business.METRICS: "指标采集缺失、延迟或字段异常",
    Business.NETWORK: "端口映射或 Sandbox 路由异常",
    Business.SNAPSHOT: "快照创建、恢复或删除状态异常",
    Business.CHECKPOINT_RESTORE: "恢复状态失真或实例间数据污染",
    Business.PAUSE_RESUME: "生命周期接口状态码或状态迁移异常",
    Business.PTY: "终端输入、尺寸调整或会话回收异常",
    Business.TEMPLATE_SDK: "SDK 构建状态或模板资源管理异常",
}


FOCUS_PATTERNS = {
    "expect_cli_error": "{title}的拒绝时机、错误类型与资源副作用",
    "create_sandbox": "{title}的创建结果、连接能力与可观察状态",
    "sandbox_lifecycle": "{title}的过期时点、可见性与连接状态",
    "create_template": "{title}的构建状态、资源参数与错误归因",
    "run_command": "{title}的退出码、标准输出与执行语义",
    "upload_file": "{title}的远端内容、权限处理与 SHA-256",
    "download_file": "{title}的本地内容、覆盖行为与 SHA-256",
    "list_sandboxes": "{title}的返回结构、资源可见性与分页行为",
    "list_templates": "{title}的返回结构、模板状态与分页行为",
    "extended-command": "{title}的进程状态、输出与控制结果",
    "extended-filesystem": "{title}的内容、路径状态与返回结果",
    "extended-sandbox": "{title}的接口响应、状态变化与数据一致性",
    "extended-network": "{title}的域名结构、端口信息与 Sandbox 标识",
    "extended-snapshot": "{title}的资源状态、数据内容与清理结果",
    "extended-checkpoint": "{title}的恢复内容、实例隔离与资源残留",
    "extended-pause-resume": "{title}的状态码、状态迁移与数据连续性",
    "extended-pty": "{title}的会话状态、交互输出与终止结果",
    "extended-template": "{title}的 SDK 返回、构建状态与资源清理",
}


def _normalize_mixed_text(text: str) -> str:
    normalized = re.sub(r"\s+", " ", text).strip()
    normalized = re.sub(r"(?<=[\u4e00-\u9fff])(?=[A-Za-z0-9])", " ", normalized)
    normalized = re.sub(r"(?<=[A-Za-z0-9])(?=[\u4e00-\u9fff])", " ", normalized)
    return normalized


def _clean_title(title: str) -> str:
    return _normalize_mixed_text(title) or "当前用例"


def _focus(case: TestCase, *, compact: bool = False) -> str:
    title = _clean_title(case.title)
    if compact and len(title) > 18:
        title = f"{title[:17]}…"
    pattern = FOCUS_PATTERNS.get(
        case.scenario,
        "{title}的关键状态、返回结果与资源副作用",
    )
    return pattern.format(title=title)


def detailed_objective(case: TestCase) -> str:
    """Return one 50-90 character objective without repeating full parameters."""
    action = SCENARIO_ACTIONS.get(
        case.scenario,
        "通过公开业务入口执行目标操作并采集响应",
    )
    risk = BUSINESS_RISKS.get(
        case.business,
        "接口行为、状态变化或资源处理异常",
    )

    objective = _normalize_mixed_text(
        f"{action}，重点核对{_focus(case)}，用于识别{risk}。"
    )
    if len(objective) > 90:
        objective = _normalize_mixed_text(
            f"{action}，重点核对{_focus(case, compact=True)}，用于识别{risk}。"
        )
    if len(objective) > 90:
        objective = _normalize_mixed_text(
            f"{action}，核对{_focus(case, compact=True)}，识别{risk}。"
        )
    if len(objective) < 50:
        objective = objective[:-1] + "，并确认结果可稳定复现。"

    if not 50 <= len(objective) <= 90:
        raise ValueError(
            f"{case.case_id} objective length must be 50-90 characters, got {len(objective)}"
        )
    return objective
