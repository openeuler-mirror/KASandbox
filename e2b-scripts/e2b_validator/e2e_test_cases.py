"""Detailed data-driven catalog for the seven real E2B business operations."""

from __future__ import annotations

from typing import Any

from .e2e_models import Business, TestCase


def build_cases(
    run_id: str,
    *,
    template: str = "openclaw",
    base_image: str = "registry.example.com/e2b/ubuntu:22.04-custom",
) -> list[TestCase]:
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
        preconditions: list[str] | None = None,
        steps: list[str] | None = None,
        depends_on: tuple[str, ...] = (),
        tags: tuple[str, ...] = (),
    ) -> None:
        cases.append(TestCase(
            case_id, business, title, purpose,
            preconditions or ["E2B API、template-manager 和目标模板可用"],
            parameters,
            steps or ["通过 start.sh 业务入口发起请求", "解析返回并使用独立操作核验实际状态"],
            expected, scenario, depends_on, tags,
        ))

    # Create sandbox: valid combinations, client boundaries, server error, and lifecycle.
    add("SB-001", Business.CREATE_SANDBOX, "指定模板创建共享沙箱", "验证 openclaw 模板可创建并返回真实 sandbox ID", {"template": template, "timeout": 3600, "context_key": "main"}, "创建成功且可执行命令", "create_sandbox", tags=("smoke", "cross-business"))
    add("SB-002", Business.CREATE_SANDBOX, "metadata 与 envs 组合", "验证 metadata 和环境变量组合传递", {"template": template, "timeout": 120, "metadata": {"env": "test", "owner": "automation", "run_id": run_id}, "envs": {"APP_ENV": "test", "E2E_RUN_ID": run_id}}, "创建成功，环境变量可在沙箱内读取", "create_sandbox", tags=("pairwise",))
    add("SB-003", Business.CREATE_SANDBOX, "secure 模式安全语义", "验证 secure 参数不仅透传，而且返回或访问链路暴露可观察的安全属性", {"template": template, "timeout": 120, "secure": True}, "创建成功，并能观察到 secure 属性或安全访问凭据", "create_sandbox", tags=("pairwise", "capability"))
    add("SB-004", Business.CREATE_SANDBOX, "最短生命周期 10 秒", "验证 timeout=10 的真实过期行为", {"template": template, "timeout": 10, "grace_seconds": 35}, "创建后短暂可用，随后在宽限期内不可连接", "sandbox_lifecycle", tags=("boundary", "lifecycle"))
    add("SB-005", Business.CREATE_SANDBOX, "timeout 为 0", "验证客户端拒绝非正 timeout", {"argv": ["create-sandbox", "--template", template, "--timeout", "0"]}, "请求在客户端边界被拒绝", "expect_cli_error", tags=("boundary", "expected-error"))
    add("SB-006", Business.CREATE_SANDBOX, "timeout 为负数", "验证客户端拒绝负 timeout", {"argv": ["create-sandbox", "--template", template, "--timeout", "-1"]}, "请求在客户端边界被拒绝", "expect_cli_error", tags=("boundary", "expected-error"))
    add("SB-007", Business.CREATE_SANDBOX, "metadata 非法 JSON", "验证 metadata 语法错误不会到达服务端", {"argv": ["create-sandbox", "--template", template, "--metadata", "{bad"]}, "返回明确 JSON 错误且不创建资源", "expect_cli_error", tags=("expected-error",))
    add("SB-008", Business.CREATE_SANDBOX, "envs 使用数组", "验证 envs 必须是 JSON object", {"argv": ["create-sandbox", "--template", template, "--envs", "[]"]}, "拒绝非 object 的 envs", "expect_cli_error", tags=("expected-error",))
    add("SB-009", Business.CREATE_SANDBOX, "不存在的模板", "验证服务端对未知模板的错误响应", {"template": f"missing-{run_id}", "timeout": 30, "expect_error": True}, "服务端拒绝创建且无 sandbox ID", "create_sandbox", tags=("expected-error", "server-boundary"))
    add("SB-010", Business.CREATE_SANDBOX, "空模板名称", "验证资源名称空值边界", {"argv": ["create-sandbox", "--template", " "]}, "客户端拒绝空模板名称", "expect_cli_error", tags=("boundary", "expected-error"))

    # Template creation. One real build plus source and resource boundaries.
    template_name = f"e2e-{run_id}-base"
    add("TP-001", Business.CREATE_TEMPLATE, "真实 base image 模板构建", "验证 template-manager 接收、调度并返回 Harbor Ubuntu 构建结果", {"name": template_name, "base_image": base_image, "cpu_count": 1, "memory_mb": 1024, "timeout": 600}, "构建成功，或明确记录镜像/基础设施阻塞", "create_template", tags=("smoke", "infrastructure"))
    add("TP-002", Business.CREATE_TEMPLATE, "inline Dockerfile 构建", "验证 Dockerfile content 请求路径和唯一名称", {"name": f"e2e-{run_id}-inline", "dockerfile_content": f"FROM {base_image}\nRUN printf e2e >/e2e-marker\n", "cpu_count": 1, "memory_mb": 512, "timeout": 600}, "构建成功，或明确记录基础设施阻塞", "create_template", tags=("pairwise", "infrastructure"))
    add("TP-003", Business.CREATE_TEMPLATE, "skip-cache 参数", "验证 skip-cache、CPU 和内存组合", {"name": f"e2e-{run_id}-nocache", "base_image": base_image, "cpu_count": 2, "memory_mb": 1024, "skip_cache": True, "timeout": 600}, "参数被接收并形成可追踪构建结果", "create_template", tags=("pairwise", "infrastructure"))
    add("TP-004", Business.CREATE_TEMPLATE, "不存在的 Dockerfile", "验证本地源文件边界", {"argv": ["create-template", "--name", f"e2e-{run_id}-missing-file", "--dockerfile", f"/tmp/e2e-{run_id}-missing-Dockerfile"]}, "构建请求发出前返回文件不存在", "expect_cli_error", tags=("expected-error",))
    add("TP-005", Business.CREATE_TEMPLATE, "CPU 为 0", "验证 CPU 正数边界", {"argv": ["create-template", "--name", f"e2e-{run_id}-cpu0", "--base-image", "alpine", "--cpu-count", "0"]}, "客户端拒绝 CPU=0", "expect_cli_error", tags=("boundary", "expected-error"))
    add("TP-006", Business.CREATE_TEMPLATE, "内存为负数", "验证 memory 正数边界", {"argv": ["create-template", "--name", f"e2e-{run_id}-mem", "--base-image", "alpine", "--memory-mb", "-1"]}, "客户端拒绝负内存", "expect_cli_error", tags=("boundary", "expected-error"))
    add("TP-007", Business.CREATE_TEMPLATE, "无效镜像名称", "验证 template-manager 构建失败状态", {"name": f"e2e-{run_id}-badimage", "base_image": f"invalid.invalid/{run_id}:missing", "cpu_count": 1, "memory_mb": 512, "timeout": 180, "expect_error": True}, "请求不产生成功模板，错误可归因", "create_template", tags=("expected-error", "server-boundary"))
    add("TP-008", Business.CREATE_TEMPLATE, "多个构建源冲突", "验证 source mutually-exclusive 约束", {"argv": ["create-template", "--name", f"e2e-{run_id}-multi", "--base-image", "alpine", "--dockerfile-content", "FROM alpine"]}, "客户端拒绝多个构建源", "expect_cli_error", tags=("expected-error",))

    # Command execution against the shared sandbox.
    command_specs = [
        ("CMD-001", "正常退出", "printf 'command-ok'", 0, "command-ok", {}),
        ("CMD-002", "非零退出码", "printf 'expected-error' >&2; exit 7", 7, "expected-error", {}),
        ("CMD-003", "stdout 与 stderr", "printf 'out-value'; printf 'err-value' >&2", 0, "err-value", {}),
        ("CMD-004", "环境变量注入", 'printf "%s" "$CASE_VALUE"', 0, "env-ok", {"envs": {"CASE_VALUE": "env-ok"}}),
        ("CMD-005", "工作目录", "pwd", 0, "/tmp", {"cwd": "/tmp"}),
        ("CMD-006", "默认 user 用户", "whoami", 0, "user", {}),
        ("CMD-007", "显式 root 用户", "id -u && whoami", 0, "0", {"user": "root"}),
        ("CMD-008", "特殊字符", "printf '%s' 'a b;中文;$HOME'", 0, "中文", {}),
        ("CMD-009", "多行输出", "printf 'line1\\nline2\\nline3\\n'", 0, "line3", {}),
        ("CMD-010", "长输出", "head -c 32768 /dev/zero | tr '\\0' x | wc -c", 0, "32768", {}),
        ("CMD-011", "命令 timeout", "sleep 3", None, "", {"timeout": 1, "expect_error": True}),
        ("CMD-012", "命令为空", "", None, "", {"client_error": True}),
        ("CMD-013", "不存在的工作目录", "pwd", None, "", {"cwd": f"/missing/{run_id}", "expect_error": True}),
        ("CMD-014", "不存在的 sandbox ID", "true", None, "", {"sandbox_id": f"missing-{run_id}", "expect_error": True}),
    ]
    for case_id, title, command, exit_code, contains, options in command_specs:
        params = {"command": command, "expected_exit": exit_code, "contains": contains, **options}
        add(case_id, Business.RUN_COMMAND, title, f"验证 run-command 的{title}行为", params, "退出码、输出和错误类型符合预期", "run_command", depends_on=("SB-001",) if "sandbox_id" not in options else (), tags=(("expected-error",) if options.get("expect_error") or options.get("client_error") or exit_code == 7 else ()))

    # Upload variants, verified inside the sandbox.
    upload_specs = [
        ("UP-001", "文本文件", "text", "hello-e2b", "/tmp/e2e-text.txt"),
        ("UP-002", "二进制文件", "hex", "0001feff", "/tmp/e2e-binary.bin"),
        ("UP-003", "空文件", "hex", "", "/tmp/e2e-empty"),
        ("UP-004", "中文 UTF-8", "text", "沙箱文件一致性", "/tmp/e2e-cn.txt"),
        ("UP-005", "嵌套路径", "text", "nested", "/tmp/e2e/nested/value.txt"),
        ("UP-006", "较大文件", "repeat", "4096", "/tmp/e2e-large.bin"),
        ("UP-007", "覆盖同名远端文件", "text", "replacement", "/tmp/e2e-overwrite.txt"),
    ]
    for case_id, title, content_type, content, remote in upload_specs:
        parameters = {"content_type": content_type, "content": content, "remote_path": remote}
        if case_id == "UP-007":
            parameters["initial_content"] = "old-value"
            parameters["user"] = "root"
        add(case_id, Business.UPLOAD_FILE, title, f"验证上传{title}并在沙箱内核验 SHA-256", parameters, "远端逐字节摘要与本地一致", "upload_file", depends_on=("SB-001",), tags=("cross-business",))
    add("UP-008", Business.UPLOAD_FILE, "本地文件不存在", "验证不存在源文件不会写入沙箱", {"missing_local": True, "remote_path": "/tmp/never-created"}, "返回文件不存在错误", "upload_file", depends_on=("SB-001",), tags=("expected-error",))

    # Download variants include setup, overwrite guard, and byte comparison.
    download_specs = [
        ("DL-001", "文本文件", "printf 'download-text' > /tmp/dl-text", "/tmp/dl-text", "download-text"),
        ("DL-002", "二进制文件", "printf '\\000\\001\\376\\377' > /tmp/dl-bin", "/tmp/dl-bin", None),
        ("DL-003", "空文件", ": > /tmp/dl-empty", "/tmp/dl-empty", ""),
        ("DL-004", "中文文件", "printf '下载一致性' > /tmp/dl-cn", "/tmp/dl-cn", "下载一致性"),
        ("DL-005", "自动创建本地父目录", "printf 'nested-download' > /tmp/dl-nested", "/tmp/dl-nested", "nested-download"),
        ("DL-006", "覆盖本地文件", "printf 'new-value' > /tmp/dl-overwrite", "/tmp/dl-overwrite", "new-value"),
    ]
    for case_id, title, setup, remote, expected_content in download_specs:
        add(case_id, Business.DOWNLOAD_FILE, title, f"验证下载{title}并比较本地内容", {"setup_command": setup, "remote_path": remote, "expected_content": expected_content, "overwrite": case_id == "DL-006"}, "本地文件内容与远端逐字节一致", "download_file", depends_on=("SB-001",), tags=("cross-business",))
    add("DL-007", Business.DOWNLOAD_FILE, "本地覆盖保护", "验证未指定 overwrite 时拒绝覆盖", {"setup_command": "printf guard > /tmp/dl-guard", "remote_path": "/tmp/dl-guard", "local_exists": True}, "已有本地文件保持不变", "download_file", depends_on=("SB-001",), tags=("expected-error",))
    add("DL-008", Business.DOWNLOAD_FILE, "远端文件不存在", "验证不存在的远端路径返回错误", {"remote_path": f"/tmp/missing-{run_id}", "expect_error": True}, "下载失败且不产生有效本地文件", "download_file", depends_on=("SB-001",), tags=("expected-error",))

    # Read-only listing behavior before/after resources exist and pagination boundaries.
    for case_id, title, max_pages, assertion in [
        ("LSB-001", "基础查询", None, "json"),
        ("LSB-002", "创建后可见", None, "main-visible"),
        ("LSB-003", "max-pages=1", 1, "json"),
        ("LSB-004", "多个本轮沙箱可见", None, "run-visible"),
        ("LSB-005", "metadata 精确查询链路", None, "metadata-visible"),
    ]:
        dependencies = ("SB-002",) if case_id == "LSB-005" else (("SB-001",) if case_id != "LSB-001" else ())
        add(case_id, Business.LIST_SANDBOXES, title, f"验证 list-sandboxes {title}", {"max_pages": max_pages, "assertion": assertion}, "返回合法 JSON 且满足资源可见性预期", "list_sandboxes", depends_on=dependencies, tags=("read-only",))
    add("LSB-006", Business.LIST_SANDBOXES, "max-pages=0", "验证分页下界", {"argv": ["list-sandboxes", "--max-pages", "0"]}, "客户端拒绝分页值 0", "expect_cli_error", tags=("boundary", "expected-error"))

    for case_id, title, max_pages, assertion in [
        ("LTP-001", "基础查询", None, "json"),
        ("LTP-002", "本轮 fixture 模板可见", None, "fixture-visible"),
        ("LTP-003", "max-pages=1", 1, "json"),
        ("LTP-004", "构建后状态查询", None, "run-visible"),
        ("LTP-005", "重复查询稳定性", None, "json"),
    ]:
        parameters = {"max_pages": max_pages, "assertion": assertion}
        if assertion == "fixture-visible":
            parameters["fixture_template"] = template
        add(case_id, Business.LIST_TEMPLATES, title, f"验证 list-templates {title}", parameters, "返回合法 JSON 且模板状态可解释", "list_templates", tags=("read-only",))
    add("LTP-006", Business.LIST_TEMPLATES, "max-pages=-1", "验证模板分页下界", {"argv": ["list-templates", "--max-pages", "-1"]}, "客户端拒绝负分页值", "expect_cli_error", tags=("boundary", "expected-error"))

    return cases
