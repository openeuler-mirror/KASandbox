"""e2b-verify 凭证/配置加载（Python 侧，优先级与 lib/e2b_env.sh 一致）。

优先级（高 -> 低）：
  1. 进程已存在的环境变量；
  2. 仓库外凭证文件 ~/.config/e2b-verify/secrets.env（可用 E2B_VERIFY_SECRETS_FILE 覆盖）；
  3. 仓库内 .env（可选、已 gitignore，只放非敏感配置）；
  4. /root/.e2b/config.json 的 live 凭证（最终覆盖，与 shell 侧行为一致）。

仓库内不再保存明文 E2B_API_KEY / E2B_ACCESS_TOKEN。
"""

from __future__ import annotations

import ipaddress
import json
import os
import re
import shutil
import socket
import stat
import subprocess
import sys
from pathlib import Path

VERIFY_DIR = Path(__file__).resolve().parent.parent
SECRETS_FILE = Path(
    os.environ.get("E2B_VERIFY_SECRETS_FILE")
    or (Path.home() / ".config" / "e2b-verify" / "secrets.env")
)
CONFIG_JSON = Path(os.environ.get("E2B_CONFIG_JSON") or "/root/.e2b/config.json")

_DEFAULTS = {
    "E2B_API_URL": "http://127.0.0.1:3000",
    "E2B_DOMAIN": "e2b.app",
    "E2B_HTTP_SSL": "false",
    "E2B_TEMPLATE_ALIAS": "ubuntu-22-04-custom-1",
}

# 模板构建镜像仓库（Harbor）：默认用当前环境节点 IP 拼接，不写死地址。
TEMPLATE_IMAGE_REGISTRY_ENV = "E2B_TEMPLATE_IMAGE_REGISTRY"
TEMPLATE_IMAGE_REGISTRY_PORT = "30443"
TEMPLATE_IMAGE_REGISTRY_PROJECT = "e2b-orchestration"


def _warn(msg: str) -> None:
    print(f"[python_env] WARN {msg}", file=sys.stderr)


def _parse_env_file(path: Path) -> dict[str, str]:
    """解析 KEY=VALUE 文件；文件不存在时返回空字典（不抛异常）。"""
    try:
        text = path.read_text(encoding="utf-8")
    except OSError:
        return {}
    out: dict[str, str] = {}
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        out[key.strip()] = value.strip().strip("'").strip('"')
    return out


def _check_secrets_mode(path: Path) -> None:
    try:
        mode = stat.S_IMODE(path.stat().st_mode)
    except OSError:
        return
    if mode not in (0o600, 0o400):
        _warn(f"凭证文件 {path} 权限为 {oct(mode)[2:]}，建议 chmod 600")


def _is_usable_ipv4(value: str) -> bool:
    """判断是否为可用作节点地址的 IPv4（排除回环/链路本地/未指定/组播）。"""
    try:
        addr = ipaddress.IPv4Address(value.strip())
    except ValueError:
        return False
    return not (
        addr.is_loopback
        or addr.is_link_local
        or addr.is_unspecified
        or addr.is_multicast
    )


def _run_quiet(cmd: list[str], timeout: float = 3.0) -> str:
    """执行探测命令；命令缺失、失败或超时都返回空字符串。"""
    try:
        proc = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return ""
    return proc.stdout.strip() if proc.returncode == 0 else ""


def detect_node_ip() -> str | None:
    """探测当前环境节点 IP。

    优先级：
      1. 显式环境变量 NODE_IP（验证脚本约定）/ SERVER_IP（部署约定）；
      2. Kubernetes Node 的 InternalIP（节点名取 NODE_NAME 或本机 hostname）；
      3. 默认路由的源地址（ip route get）；
      4. hostname -i 解析出的 IPv4。
    """
    for key in ("NODE_IP", "SERVER_IP"):
        value = os.environ.get(key, "").strip()
        if _is_usable_ipv4(value):
            return value

    if shutil.which("kubectl"):
        node_name = os.environ.get("NODE_NAME", "").strip() or socket.gethostname()
        node_ip = _run_quiet(
            [
                "kubectl",
                "get",
                "node",
                node_name,
                "-o",
                'jsonpath={.status.addresses[?(@.type=="InternalIP")].address}',
            ]
        )
        if _is_usable_ipv4(node_ip):
            return node_ip

    route = _run_quiet(["ip", "-4", "route", "get", "1.1.1.1"])
    match = re.search(r"\bsrc\s+(\d{1,3}(?:\.\d{1,3}){3})\b", route)
    if match and _is_usable_ipv4(match.group(1)):
        return match.group(1)

    for candidate in _run_quiet(["hostname", "-i"]).split():
        if _is_usable_ipv4(candidate):
            return candidate

    return None


def resolve_template_image_registry() -> str:
    """返回模板构建用的镜像仓库前缀，默认取当前环境节点 IP。

    显式设置 E2B_TEMPLATE_IMAGE_REGISTRY 时原样使用；否则拼成
    <node-ip>:30443/e2b-orchestration，避免在脚本里写死 IP。
    """
    explicit = os.environ.get(TEMPLATE_IMAGE_REGISTRY_ENV, "").strip()
    if explicit:
        return explicit

    node_ip = detect_node_ip()
    if not node_ip:
        raise RuntimeError(
            "无法探测当前环境节点 IP；请显式设置 "
            f"{TEMPLATE_IMAGE_REGISTRY_ENV}=<节点IP>:"
            f"{TEMPLATE_IMAGE_REGISTRY_PORT}/{TEMPLATE_IMAGE_REGISTRY_PROJECT}"
        )
    return (
        f"{node_ip}:{TEMPLATE_IMAGE_REGISTRY_PORT}/"
        f"{TEMPLATE_IMAGE_REGISTRY_PROJECT}"
    )


def load_verify_env() -> dict[str, str]:
    """按优先级加载凭证与配置，返回 {变量名: 来源文件}。"""
    sources: dict[str, str] = {}

    if SECRETS_FILE.is_file():
        _check_secrets_mode(SECRETS_FILE)

    for path in (SECRETS_FILE, VERIFY_DIR / ".env"):
        for key, value in _parse_env_file(path).items():
            if value and not os.environ.get(key):
                os.environ[key] = value
                sources[key] = str(path)

    cfg: dict[str, object] = {}
    if CONFIG_JSON.is_file():
        try:
            cfg = json.loads(CONFIG_JSON.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            cfg = {}
    for env_key, cfg_key in (
        ("E2B_API_KEY", "teamApiKey"),
        ("E2B_ACCESS_TOKEN", "accessToken"),
        ("E2B_TEAM_ID", "teamId"),
    ):
        value = cfg.get(cfg_key)
        if value:
            os.environ[env_key] = str(value)
            sources[env_key] = str(CONFIG_JSON)

    for key, value in _DEFAULTS.items():
        os.environ.setdefault(key, value)

    return sources


__all__ = [
    "load_verify_env",
    "detect_node_ip",
    "resolve_template_image_registry",
    "SECRETS_FILE",
    "CONFIG_JSON",
    "VERIFY_DIR",
]
