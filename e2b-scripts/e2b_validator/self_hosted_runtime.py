"""Resolve self-hosted E2B credentials and endpoints without running commands."""

from __future__ import annotations

import ipaddress
import json
import os
import socket
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urlparse

from .e2b_config import parse_env_file


DEFAULT_INFRA_ENV = Path("/opt/e2b-infra/dep/.env")
DEFAULT_CLIENT_CONFIG = Path("/root/.e2b/config.json")
DEFAULT_CONSUL_URL = "http://127.0.0.1:8500/v1/catalog/service/client-proxy"
DEFAULT_LOCAL_API_URL = "http://127.0.0.1:3000"


@dataclass(frozen=True)
class ServiceEndpoint:
    host: str
    port: int


@dataclass(frozen=True)
class RuntimeSettings:
    environment: dict[str, str]
    source: str
    command_args: list[str]


def expand_infra_arguments(command_args: list[str], infra: dict[str, str]) -> list[str]:
    expanded: list[str] = []
    for argument in command_args:
        if not argument.startswith("@INFRA:"):
            expanded.append(argument)
            continue
        name = argument.removeprefix("@INFRA:")
        value = infra.get(name)
        if not value:
            raise ValueError(f"The deployment variable {name or '<empty>'} is missing")
        expanded.append(value)
    return expanded


def discover_client_proxy(consul_url: str = DEFAULT_CONSUL_URL) -> ServiceEndpoint | None:
    try:
        with urllib.request.urlopen(consul_url, timeout=3) as response:
            services = json.load(response)
    except (OSError, ValueError):
        return None
    if not isinstance(services, list) or not services:
        return None
    service = services[0]
    host = service.get("ServiceAddress") or service.get("Address")
    port = service.get("ServicePort") or 3002
    if not host:
        return None
    try:
        return ServiceEndpoint(str(host), int(port))
    except (TypeError, ValueError):
        return None


def local_ipv4() -> str | None:
    try:
        addresses = socket.gethostbyname_ex(socket.gethostname())[2]
    except OSError:
        return None
    return next((address for address in addresses if not address.startswith("127.")), None)


def _is_ip_address(value: str) -> bool:
    try:
        ipaddress.ip_address(value)
        return True
    except ValueError:
        return False


def sandbox_url_for(
    sandbox_id: str,
    domain: str,
    *,
    port: int = 3002,
    use_ssl: bool = False,
) -> str:
    if not sandbox_id.strip():
        raise ValueError("sandbox_id must not be empty")
    if not domain.strip():
        raise ValueError("domain must not be empty")
    route_domain = f"{domain}.nip.io" if _is_ip_address(domain) else domain.rstrip(".")
    scheme = "https" if use_ssl else "http"
    default_port = 443 if use_ssl else 80
    port_suffix = "" if port == default_port else f":{port}"
    return f"{scheme}://49983-{sandbox_id}.{route_domain}{port_suffix}"


def _is_placeholder(value: str) -> bool:
    normalized = value.strip().lower()
    return not normalized or normalized.startswith("replace_") or (normalized.startswith("<") and normalized.endswith(">"))


def _api_host_and_ssl(api_url: str) -> tuple[str, bool]:
    """Return the API host and transport setting used by the default data-plane route."""
    parsed = urlparse(api_url)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        raise ValueError("E2B_API_URL must be an absolute http:// or https:// URL")
    return parsed.hostname, parsed.scheme == "https"


def load_env_runtime_settings(command_args: list[str], env_file: Path) -> RuntimeSettings:
    """Load a portable self-hosted configuration without deployment-path discovery."""
    configured = parse_env_file(env_file)
    environment = {
        key: os.environ.get(key, value)
        for key, value in configured.items()
    }
    for key in ("E2B_API_KEY", "E2B_API_URL", "E2B_DOMAIN", "E2B_HTTP_SSL", "E2B_PROXY_PORT"):
        if key not in environment and os.getenv(key):
            environment[key] = os.environ[key]

    api_key = environment.get("E2B_API_KEY", "")
    if _is_placeholder(api_key):
        raise ValueError(f"Set E2B_API_KEY in {env_file} before running the script")
    api_url = environment.get("E2B_API_URL", DEFAULT_LOCAL_API_URL)
    if _is_placeholder(api_url):
        raise ValueError(f"Set E2B_API_URL in {env_file} before running the script")
    environment.setdefault("E2B_API_URL", api_url)
    api_host, api_uses_ssl = _api_host_and_ssl(api_url)

    # A standard self-hosted deployment exposes client-proxy on the API host at port 3002.
    # Keep explicit values untouched for installations with a separate data-plane endpoint.
    environment.setdefault("E2B_DOMAIN", api_host)
    environment.setdefault("E2B_HTTP_SSL", "true" if api_uses_ssl else "false")
    environment.setdefault("E2B_PROXY_PORT", "3002")

    resolved_args = expand_infra_arguments(command_args, environment)
    sandbox_url = environment.get("E2B_SANDBOX_URL", "")
    if "--sandbox-id" in resolved_args and not sandbox_url:
        index = resolved_args.index("--sandbox-id") + 1
        if index >= len(resolved_args):
            raise ValueError("--sandbox-id requires a value")
        domain = environment.get("E2B_DOMAIN", "")
        if _is_placeholder(domain):
            raise ValueError("Cannot derive the sandbox route from E2B_API_URL; set E2B_DOMAIN explicitly")
        try:
            proxy_port = int(environment.get("E2B_PROXY_PORT", "3002"))
        except ValueError as exc:
            raise ValueError("E2B_PROXY_PORT must be an integer") from exc
        if proxy_port <= 0 or proxy_port > 65535:
            raise ValueError("E2B_PROXY_PORT must be between 1 and 65535")
        use_ssl = environment.get("E2B_HTTP_SSL", "false").strip().lower() in {"1", "true", "yes", "on"}
        environment["E2B_SANDBOX_URL"] = sandbox_url_for(
            resolved_args[index], domain, port=proxy_port, use_ssl=use_ssl
        )
    return RuntimeSettings(environment=environment, source="env-file", command_args=resolved_args)


def load_runtime_settings(
    command_args: list[str],
    *,
    infra_env: Path = DEFAULT_INFRA_ENV,
    client_config: Path = DEFAULT_CLIENT_CONFIG,
) -> RuntimeSettings:
    infra = parse_env_file(infra_env)
    resolved_args = expand_infra_arguments(command_args, infra)
    if not client_config.is_file():
        raise FileNotFoundError(f"E2B client configuration does not exist: {client_config}")
    try:
        client = json.loads(client_config.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise ValueError(f"Invalid E2B client configuration: {client_config}: {exc}") from exc
    if not isinstance(client, dict):
        raise ValueError(f"E2B client configuration must contain a JSON object: {client_config}")

    api_key = client.get("teamApiKey")
    if not isinstance(api_key, str) or not api_key.strip():
        raise ValueError("The deployment client configuration does not contain teamApiKey")

    proxy = discover_client_proxy()
    domain = infra.get("DOMAIN_NAME") or (proxy.host if proxy else None) or local_ipv4() or infra.get("SERVER_IP")
    if not domain:
        raise ValueError("Unable to determine E2B domain or client-proxy address")
    use_ssl = infra.get("E2B_HTTP_SSL", "false").strip().lower() in {"1", "true", "yes", "on"}
    environment = {
        "E2B_DOMAIN": domain,
        "E2B_API_URL": infra.get("E2B_API_URL", "http://127.0.0.1:3000").rstrip("/"),
        "E2B_HTTP_SSL": "true" if use_ssl else "false",
        "E2B_API_KEY": api_key,
        "E2B_PROXY_PORT": str(proxy.port if proxy else 3002),
    }
    access_token = client.get("accessToken")
    if isinstance(access_token, str) and access_token:
        environment["E2B_ACCESS_TOKEN"] = access_token

    configured_sandbox_url = infra.get("E2B_SANDBOX_URL")
    if configured_sandbox_url:
        environment["E2B_SANDBOX_URL"] = configured_sandbox_url
    elif "--sandbox-id" in resolved_args:
        index = resolved_args.index("--sandbox-id") + 1
        if index >= len(resolved_args):
            raise ValueError("--sandbox-id requires a value")
        environment["E2B_SANDBOX_URL"] = sandbox_url_for(
            resolved_args[index],
            domain,
            port=proxy.port if proxy else 3002,
            use_ssl=use_ssl,
        )
    return RuntimeSettings(environment=environment, source="self-hosted", command_args=resolved_args)
