#!/usr/bin/env python3
"""e2b_sdk.py — E2B SDK 侧验证（E8 与模板构建）。

写法参考 deploy/create_template.py 的 setup_e2b_env() 与 deploy/create_sandbox.py：
先按候选顺序解析 API 地址并探活，再设置 E2B_* 环境变量，最后才 import e2b。

约定：
  - stdout 只输出 `K=V`，供 bash 侧 eval；
  - 日志一律 stderr；
  - 退出码 0=成功 / 1=失败 / 77=当前环境不适用（SKIP）。

子命令：
  probe    自建沙箱跑通 create→exec→文件→pause→resume→info→kill（E8）
  template 现场构建模板（默认不跑）

说明：Pod 通道创建的沙箱不会在 API 落库（POST /sandboxes/transform 只返回创建参数），
无法用 SDK connect/cross/pause，因此不再提供 connect/cross/pause 子命令；
pause/resume 能力改由 probe 对 SDK 自建沙箱验证。
"""
import argparse
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

DEFAULT_CONFIG = "/root/.e2b/config.json"
DEFAULT_TEMPLATE = "ubuntu-22-04-custom-1"


def log(message):
    print(message, file=sys.stderr, flush=True)


def emit(key, value):
    print("%s=%s" % (key, value), flush=True)


def skip(reason):
    emit("REASON", reason)
    log("[skip] %s" % reason)
    sys.exit(77)


def fail(reason):
    emit("REASON", reason)
    log("[fail] %s" % reason)
    sys.exit(1)


# --------------------------------------------------------------------------
# 环境
# --------------------------------------------------------------------------
def health_ok(url):
    try:
        with urllib.request.urlopen("%s/health" % url.rstrip("/"), timeout=3) as resp:
            return 200 <= resp.status < 300
    except Exception:
        return False


def kubectl_api_url():
    """用 API Service 的 ClusterIP 兜底（宿主机经常连不上 deploy 里的 SERVER_IP）。"""
    try:
        out = subprocess.run(
            ["kubectl", "-n", os.environ.get("WV_NAMESPACE", "e2b"),
             "get", "svc", "api", "-o", "jsonpath={.spec.clusterIP}"],
            capture_output=True, text=True, timeout=20,
        )
        ip = out.stdout.strip()
        if out.returncode == 0 and ip and ip != "None":
            return "http://%s:3000" % ip
    except Exception as exc:
        log("[warn] kubectl 解析 api Service 失败: %s" % exc)
    return None


def resolve_api_url(explicit=None):
    candidates = [
        explicit or os.environ.get("WV_SDK_API_URL"),
        os.environ.get("E2B_API_URL"),
        kubectl_api_url(),
        "http://localhost:3000",
    ]
    tried = []
    for candidate in candidates:
        if not candidate or candidate in tried:
            continue
        tried.append(candidate)
        if health_ok(candidate):
            log("[env] E2B API: %s" % candidate)
            return candidate
        log("[env] API 不可达: %s" % candidate)
    return None


def load_config(path):
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        log("[warn] 读取 %s 失败: %s" % (path, exc))
        return {}


def setup_env(api_url, config_path):
    os.environ["E2B_API_URL"] = api_url
    os.environ["E2B_HTTP_SSL"] = "false"
    os.environ["E2B_DOMAIN"] = "e2b.app"

    config = load_config(config_path)
    team_api_key = os.environ.get("E2B_API_KEY") or config.get("teamApiKey")
    if not team_api_key:
        skip("missing_credentials: %s 中没有 teamApiKey" % config_path)
    access_token = os.environ.get("E2B_ACCESS_TOKEN") or config.get("accessToken")
    if access_token:
        os.environ["E2B_ACCESS_TOKEN"] = access_token
    os.environ["E2B_API_KEY"] = team_api_key


def sdk_available():
    try:
        import e2b  # noqa: F401
        return True
    except Exception as exc:
        log("[warn] e2b SDK 不可用: %s" % exc)
        return False


# --------------------------------------------------------------------------
# 子命令
# --------------------------------------------------------------------------
def cmd_probe(args):
    from e2b import Sandbox, SandboxNotFoundException

    template = args.template or os.environ.get("WV_E2E_TEMPLATE") or DEFAULT_TEMPLATE

    sbx = Sandbox.create(template, timeout=args.timeout)
    sandbox_id = sbx.sandbox_id
    emit("SDK_SANDBOX_ID", sandbox_id)
    log("[probe] created %s from %s" % (sandbox_id, template))

    result = sbx.commands.run("whoami", timeout=60)
    emit("WHOAMI_RC", result.exit_code)
    emit("WHOAMI_STDOUT", (result.stdout or "").strip())
    if result.exit_code != 0:
        fail("whoami_exit_%s: %s" % (result.exit_code, (result.stderr or "").strip()))

    path = "/tmp/webhook-verify-sdk.txt"
    sbx.files.write(path, "sdk_ok")
    content = sbx.files.read(path)
    emit("FILE_MATCH", 1 if content == "sdk_ok" else 0)
    if content != "sdk_ok":
        fail("file_roundtrip_mismatch")

    # pause/resume：确认能进入 paused，再靠 connect 自动 resume 执行命令。
    try:
        sbx.pause()
    except Exception as exc:
        fail("pause_error: %s" % exc)

    info = Sandbox.get_info(sandbox_id)
    paused_state = getattr(getattr(info, "state", None), "value", None) or str(getattr(info, "state", ""))
    emit("PAUSED_STATE", paused_state)
    if paused_state != "paused":
        fail("state_after_pause: %s" % paused_state)

    resumed = Sandbox.connect(sandbox_id)
    result = resumed.commands.run("echo resume_ok", timeout=60)
    emit("RESUME_RC", result.exit_code)
    emit("RESUME_STDOUT", (result.stdout or "").strip())
    if result.exit_code != 0 or (result.stdout or "").strip() != "resume_ok":
        fail("resume_command_failed")

    info = Sandbox.get_info(sandbox_id)
    state = getattr(getattr(info, "state", None), "value", None) or str(getattr(info, "state", ""))
    emit("STATE", state)

    killed = sbx.kill()
    emit("KILLED", bool(killed))

    gone = 1
    try:
        Sandbox.get_info(sandbox_id)
        gone = 0
    except SandboxNotFoundException:
        gone = 1
    except Exception as exc:  # 其它错误不能算“已回收”
        log("[warn] 回收确认异常: %s" % exc)
        gone = 0
    emit("GONE", gone)
    if not gone:
        fail("sandbox_still_visible_after_kill")


def cmd_template(args):
    from e2b import Template

    dockerfile = args.dockerfile or "FROM %s\n" % args.image
    alias = args.alias
    log("[template] building %s (skip_cache=%s)" % (alias, not args.use_cache))

    build = Template.build(
        Template().from_dockerfile(dockerfile),
        alias=alias,
        cpu_count=args.cpu,
        memory_mb=args.memory,
        on_build_logs=lambda entry: log("[build] %s" % getattr(entry, "message", entry)),
        skip_cache=not args.use_cache,
    )
    emit("TEMPLATE_ALIAS", alias)
    for attr in ("template_id", "build_id"):
        value = getattr(build, attr, None)
        if value:
            emit(attr.upper(), value)


# --------------------------------------------------------------------------
# 入口
# --------------------------------------------------------------------------
def main():
    parser = argparse.ArgumentParser(description="E2B SDK 侧验证")
    parser.add_argument("--config", default=os.environ.get("WV_E2B_CONFIG", DEFAULT_CONFIG))
    parser.add_argument("--api-url", default=None)
    sub = parser.add_subparsers(dest="command", required=True)

    probe = sub.add_parser("probe", help="自建沙箱并跑通数据面")
    probe.add_argument("--template", default=None)
    probe.add_argument("--timeout", type=int, default=300)
    probe.set_defaults(func=cmd_probe)

    template = sub.add_parser("template", help="构建模板（默认不跑）")
    template.add_argument("--alias", required=True)
    template.add_argument("--image", default="ubuntu:22.04-custom")
    template.add_argument("--dockerfile", default=None)
    template.add_argument("--cpu", type=int, default=2)
    template.add_argument("--memory", type=int, default=2048)
    template.add_argument("--use-cache", action="store_true")
    template.set_defaults(func=cmd_template)

    args = parser.parse_args()

    if not sdk_available():
        skip("e2b_sdk_not_installed")

    api_url = resolve_api_url(args.api_url)
    if not api_url:
        skip("e2b_api_unreachable")
    setup_env(api_url, args.config)

    try:
        args.func(args)
    except SystemExit:
        raise
    except Exception as exc:
        fail("%s: %s" % (type(exc).__name__, exc))


if __name__ == "__main__":
    main()
