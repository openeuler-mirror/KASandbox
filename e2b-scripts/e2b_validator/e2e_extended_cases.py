"""Extended E2B SDK case catalog for capabilities beyond the original seven operations."""

from __future__ import annotations

from typing import Any

from .e2e_models import Business, TestCase


def build_extended_cases(run_id: str, *, template: str, base_image: str) -> list[TestCase]:
    cases: list[TestCase] = []

    def add(
        case_id: str,
        business: Business,
        title: str,
        purpose: str,
        parameters: dict[str, Any],
        expected: str,
        scenario: str,
        *,
        depends_on: tuple[str, ...] = ("SB-001",),
        preconditions: list[str] | None = None,
        tags: tuple[str, ...] = (),
    ) -> None:
        cases.append(TestCase(
            case_id=case_id,
            business=business,
            title=title,
            purpose=purpose,
            preconditions=preconditions or ["共享 Sandbox 可连接", "E2B Python SDK 2.19 或更高版本"],
            parameters=parameters,
            steps=["通过 SDK 调用目标能力", "使用独立查询或状态读取核验结果", "登记并清理本轮资源"],
            expected=expected,
            scenario=scenario,
            depends_on=depends_on,
            tags=tags,
        ))

    command_cases = [
        ("BG-001", "后台命令等待", "background run 返回 PID，wait 得到完整结果", {"mode": "wait"}),
        ("BG-002", "后台进程查询", "启动的 PID 出现在 commands.list 结果中", {"mode": "list"}),
        ("BG-003", "后台命令标准输入", "send_stdin 输入可被后台命令读取", {"mode": "stdin"}),
        ("BG-004", "断开并重新连接", "disconnect 不终止进程，connect 可继续等待", {"mode": "reconnect"}),
        ("BG-005", "CommandHandle 终止", "handle.kill 终止本轮后台进程", {"mode": "handle-kill"}),
        ("BG-006", "不存在的 PID", "终止未知 PID 返回 false 或明确 not found", {"mode": "missing-pid"}),
        ("STR-001", "stdout/stderr 流式回调", "两个 callback 分别收到对应输出", {"mode": "stream"}),
        ("STR-002", "流式非零退出", "callback 保留 stderr，异常保留退出码 9", {"mode": "stream-error"}),
    ]
    for case_id, title, expected, parameters in command_cases:
        add(case_id, Business.BACKGROUND_COMMAND, title, f"验证 Commands SDK 的{title}行为", parameters, expected, "extended-command")

    filesystem_cases = [
        ("FS-001", Business.FILESYSTEM, "三种读取格式", "text、bytes、stream 返回相同字节", {"mode": "read-formats"}),
        ("FS-002", Business.FILESYSTEM, "写入并覆盖", "write 创建文件并完整覆盖原内容", {"mode": "overwrite"}),
        ("FS-003", Business.FILESYSTEM, "批量写入", "write_files 一次写入两个文件", {"mode": "write-files"}),
        ("FS-004", Business.FILESYSTEM, "空批量写入", "write_files([]) 返回空列表且无副作用", {"mode": "empty-write-files"}),
        ("FS-005", Business.FILESYSTEM, "目录创建与深度查询", "make_dir 创建嵌套目录，list(depth=2) 可见第二层目录但不越界返回第三层文件", {"mode": "directory-list"}),
        ("FS-006", Business.FILESYSTEM, "存在性与文件信息", "exists 与 get_info 返回一致的文件状态", {"mode": "info"}),
        ("FS-007", Business.FILESYSTEM, "重命名与删除", "rename 后旧路径消失，remove 后新路径消失", {"mode": "rename-remove"}),
        ("FS-008", Business.FILESYSTEM, "目录深度下界", "list(depth=0) 被客户端明确拒绝", {"mode": "invalid-depth"}),
        ("WAT-001", Business.FILESYSTEM_WATCH, "目录事件监听", "watcher 能读取本轮文件创建事件并停止", {"mode": "watch"}),
        ("WAT-002", Business.FILESYSTEM_WATCH, "递归目录监听", "recursive watcher 捕获子目录事件；旧 envd 记为 BLOCKED", {"mode": "watch-recursive"}),
        ("URL-001", Business.SIGNED_FILE_URL, "文件直连 URL", "upload_url/download_url 的 query 包含解码后的目标路径、用户、签名和过期时间", {"mode": "signed-urls"}),
    ]
    for case_id, business, title, expected, parameters in filesystem_cases:
        add(case_id, business, title, f"验证 Filesystem SDK 的{title}行为", parameters, expected, "extended-filesystem")

    sandbox_cases = [
        ("SI-001", Business.SANDBOX_INSPECTION, "运行状态与详情", "is_running=true，get_info 返回当前 Sandbox ID", {"mode": "inspect"}),
        ("SI-002", Business.SANDBOX_INSPECTION, "动态延长 timeout", "set_timeout 成功且 Sandbox 继续运行", {"mode": "set-timeout", "timeout": 900}),
        ("SI-003", Business.SANDBOX_INSPECTION, "按 ID 重新连接", "新 SDK 对象连接同一 Sandbox 并执行命令", {"mode": "reconnect"}),
        ("LC-001", Business.SANDBOX_LIFECYCLE, "手动暂停与恢复", "pause 后 connect 恢复同一独立 Sandbox", {"mode": "pause-resume"}),
    ]
    for case_id, business, title, expected, parameters in sandbox_cases:
        dependencies = () if case_id == "LC-001" else ("SB-001",)
        add(
            case_id,
            business,
            title,
            f"验证 Sandbox SDK 的{title}行为",
            parameters,
            expected,
            "extended-sandbox",
            depends_on=dependencies,
        )

    network_cases = [
        ("NET-001", "端口 Host 路由", "get_host(8080) 返回包含端口和 Sandbox ID 的路由", {"mode": "host"}),
    ]
    for case_id, title, expected, parameters in network_cases:
        add(case_id, Business.NETWORK, title, f"验证 Sandbox Network 的{title}行为", parameters, expected, "extended-network")

    snapshot_cases = [
        ("SNP-001", "创建并查询 Snapshot", "Snapshot ID 出现在本轮独立 Sandbox 的列表中", {"mode": "create-list"}, ()),
        ("SNP-002", "从 Snapshot 恢复", "恢复的新 Sandbox 保留快照文件内容", {"mode": "restore"}, ("SNP-001",)),
        ("SNP-003", "删除 Snapshot", "本轮 Snapshot 删除成功", {"mode": "delete"}, ("SNP-001",)),
        ("SNP-004", "重复删除 Snapshot", "已删除 Snapshot 再次删除返回 false", {"mode": "delete-missing"}, ("SNP-003",)),
    ]
    for case_id, title, expected, parameters, dependencies in snapshot_cases:
        add(case_id, Business.SNAPSHOT, title, f"验证 Snapshot SDK 的{title}行为", parameters, expected, "extended-snapshot", depends_on=dependencies)

    checkpoint_cases = [
        (
            "CPR-001",
            "创建并查询 Checkpoint",
            "写入普通路径、/home/user-sandbox 和 /etc 后创建 Checkpoint，并按 source Sandbox 查询",
            "Checkpoint ID 出现在 source Sandbox 的 Snapshot 列表中",
            {"mode": "create-list"},
            (),
        ),
        (
            "CPR-002",
            "从 Checkpoint 恢复",
            "使用 Checkpoint ID 创建新 Sandbox，并读取快照前写入的基线文件",
            "恢复成功、文件一致，且新 Sandbox ID 与 source 不同",
            {"mode": "restore-new-id"},
            ("CPR-001",),
        ),
        (
            "CPR-003",
            "两阶段 Checkpoint 精确回滚",
            "依次保存 state-1、state-2，再写入 dirty-state 并分别恢复两个 Checkpoint",
            "两个恢复实例分别得到 state-1 和 state-2",
            {"mode": "two-stage-rollback"},
            (),
        ),
        (
            "CPR-004",
            "source 与 restored 状态隔离",
            "分别修改 source 和 restored 的同一路径",
            "两个 Sandbox 的文件内容相互独立",
            {"mode": "source-restore-isolation"},
            ("CPR-002",),
        ),
        (
            "CPR-005",
            "Checkpoint 后 source 继续可用",
            "重新连接 source 并执行独立命令",
            "source Sandbox 在 Checkpoint 后仍可运行命令",
            {"mode": "source-continues"},
            ("CPR-001",),
        ),
        (
            "CPR-006",
            "full-memory Checkpoint 恢复后台进程",
            "先创建心跳目录，启动后台进程后创建 Checkpoint，并在恢复实例中观察文件持续增长",
            "恢复后后台进程继续运行，心跳字节数递增",
            {"mode": "memory-process"},
            (),
        ),
        (
            "CPR-007",
            "系统路径恢复",
            "读取 Checkpoint 前写入 /home/user-sandbox 和 /etc 的标记文件",
            "两个系统路径的内容均被恢复",
            {"mode": "system-paths"},
            ("CPR-002",),
        ),
        (
            "CPR-008",
            "无效 Checkpoint 拒绝恢复",
            "删除本轮 Checkpoint 后，分别使用已删除 ID 和不存在 ID 发起恢复",
            "两个恢复请求均被拒绝，且不遗留未登记 Sandbox",
            {"mode": "missing-deleted"},
            (),
        ),
    ]
    for case_id, title, purpose, expected, parameters, dependencies in checkpoint_cases:
        add(
            case_id,
            Business.CHECKPOINT_RESTORE,
            title,
            purpose,
            parameters,
            expected,
            "extended-checkpoint",
            depends_on=dependencies,
            preconditions=["E2B API、Snapshot 存储和目标 Template 可用", "E2B Python SDK 支持 Snapshot"],
            tags=("checkpoint", "lifecycle"),
        )

    pause_resume_cases = [
        (
            "PRC-001",
            "full-memory pause",
            "以 memory=true 暂停独立 Sandbox，并轮询控制面状态",
            "pause 返回 204，Sandbox 状态变为 paused",
            {"mode": "full-pause", "memory": True},
            (),
        ),
        (
            "PRC-002",
            "显式 resume",
            "对 PRC-001 的 paused Sandbox 调用兼容性 /resume 接口",
            "resume 返回 201、状态恢复 running，Sandbox ID 不变",
            {"mode": "explicit-resume", "timeout": 300},
            ("PRC-001",),
        ),
        (
            "PRC-003",
            "connect 恢复 paused Sandbox",
            "full-memory pause 后调用 /connect",
            "connect 返回 201、状态恢复 running，Sandbox ID 不变",
            {"mode": "connect-resume", "timeout": 300},
            (),
        ),
        (
            "PRC-004",
            "full-memory 保留进程",
            "启动后台进程后 full-memory pause，再显式 resume",
            "恢复后原 PID 仍出现在 commands.list 中",
            {"mode": "full-process", "memory": True},
            (),
        ),
        (
            "PRC-005",
            "full-memory 保留 filesystem",
            "写入文件后 full-memory pause/resume",
            "恢复后文件内容保持不变",
            {"mode": "full-filesystem", "memory": True},
            (),
        ),
        (
            "PRC-006",
            "resume/connect 更新 timeout",
            "分别用 resume=420 秒和 connect=480 秒恢复两个 Sandbox",
            "控制面 endAt 剩余时间落在允许误差范围",
            {"mode": "timeout-refresh", "resume_timeout": 420, "connect_timeout": 480},
            (),
        ),
        (
            "PRC-007",
            "重复 pause 冲突",
            "同一 Sandbox 连续执行两次 full-memory pause",
            "第一次返回 204，状态稳定后第二次返回 409",
            {"mode": "repeat-pause", "memory": True},
            (),
        ),
        (
            "PRC-008",
            "running Sandbox connect",
            "对 running Sandbox 调用 /connect",
            "返回 200，Sandbox ID 不变",
            {"mode": "running-connect", "timeout": 300},
            (),
        ),
        (
            "PRC-009",
            "不存在 Sandbox 生命周期接口",
            "使用格式合法但不存在的 ID 分别调用 pause、resume、connect",
            "三个接口均返回 404",
            {"mode": "missing-lifecycle"},
            (),
        ),
        (
            "PRC-010",
            "memory=false resume 兼容性",
            "以 memory=false 暂停后调用 /resume，并记录服务端实际恢复行为",
            "resume 返回 201，Sandbox ID 不变；不将该结果解释为 filesystem-only 能力",
            {"mode": "filesystem-resume", "memory": False},
            (),
        ),
        (
            "PRC-011",
            "memory=false connect 兼容性",
            "以 memory=false 暂停后调用 /connect，并验证基本数据可用性",
            "connect 返回 201，且恢复后的文件内容保持不变；不验证冷启动语义",
            {"mode": "filesystem-connect", "memory": False},
            (),
        ),
        (
            "PRC-014",
            "autoPause 到期",
            "创建 timeout=10、autoPause=true 的 Sandbox 并轮询状态",
            "宽限期内从 running 自动转为 paused",
            {"mode": "auto-pause", "timeout": 10, "grace_seconds": 35},
            (),
        ),
        (
            "PRC-015",
            "data-plane 自动恢复",
            "协商 autoResume payload 版本，暂停后使用暂停前保存的 data-plane 对象执行命令",
            "请求自动恢复 Sandbox，命令成功且状态回到 running",
            {"mode": "auto-resume", "auto_pause": True, "auto_resume": True},
            (),
        ),
    ]
    for case_id, title, purpose, expected, parameters, dependencies in pause_resume_cases:
        add(
            case_id,
            Business.PAUSE_RESUME,
            title,
            purpose,
            parameters,
            expected,
            "extended-pause-resume",
            depends_on=dependencies,
            preconditions=["E2B API、orchestrator 和目标 Template 可用", "部署版本提供 pause/connect 生命周期接口"],
            tags=("pause-resume", "lifecycle"),
        )

    pty_cases = [
        ("PTY-001", "创建 PTY 并输入", "PTY 接收命令并输出环境变量", {"mode": "create-input"}),
        ("PTY-002", "调整终端尺寸", "resize 更新 rows/cols 后 PTY 保持可用", {"mode": "resize"}),
        ("PTY-003", "重新连接并终止", "connect 可连接本轮 PTY，kill 可终止", {"mode": "connect-kill"}),
    ]
    for case_id, title, expected, parameters in pty_cases:
        add(case_id, Business.PTY, title, f"验证 PTY SDK 的{title}行为", parameters, expected, "extended-pty")

    template_cases = [
        ("TSDK-001", "Template 序列化", "to_json 与 to_dockerfile 保留基础镜像、命令、目录和用户", {"mode": "serialize", "base_image": base_image}, ()),
        ("TSDK-002", "后台构建与状态查询", "build_in_background 返回 template/build ID，status 可查询", {"mode": "background-build", "base_image": base_image, "name": f"e2e-{run_id}-sdk"}, ()),
        ("TSDK-003", "Template 存在性", "exists 能查询本轮 SDK Template", {"mode": "exists", "name": f"e2e-{run_id}-sdk"}, ("TSDK-002",)),
        ("TSDK-004", "Template Tag 生命周期", "仅为本轮 Template 添加、查询并删除本轮 tag", {"mode": "tags", "name": f"e2e-{run_id}-sdk", "tag": f"run-{run_id}"}, ("TSDK-002",)),
    ]
    for case_id, title, expected, parameters, dependencies in template_cases:
        add(
            case_id, Business.TEMPLATE_SDK, title, f"验证 Template SDK 的{title}行为",
            parameters, expected, "extended-template", depends_on=dependencies,
            preconditions=["E2B Python SDK 2.19 或更高版本", "Template API 可用"],
        )

    return cases
