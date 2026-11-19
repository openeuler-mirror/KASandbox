#!/usr/bin/env python3
"""离线核对两仓九接口的入参与必填要求，不发送 HTTP 请求。依赖 PyYAML。"""
import argparse
import json
from pathlib import Path


def expand(document, value):
    if isinstance(value, list):
        return [expand(document, item) for item in value]
    if not isinstance(value, dict):
        return value
    if "$ref" in value:
        target = document
        for part in value["$ref"].split("/")[1:]:
            target = target[part.replace("~1", "/").replace("~0", "~")]
        return expand(document, target)
    return {key: expand(document, item) for key, item in value.items()
            if key not in ("description", "summary", "example", "examples")}


def check_user_fields(value, location="request"):
    if isinstance(value, list):
        for item in value:
            check_user_fields(item, location)
    elif isinstance(value, dict):
        for name in value.get("properties", {}):
            if name.lower().replace("_", "").replace("-", "") in {"user", "userid", "username"}:
                raise ValueError(f"不需要的用户字段: {location}.{name}")
        name = value.get("name", "")
        if value.get("in") and name.lower().replace("_", "").replace("-", "") in {"user", "userid", "username"}:
            raise ValueError(f"不需要的用户参数: {location}.{name}")
        for key, item in value.items():
            check_user_fields(item, f"{location}.{key}")


def check(agentenv_path, kasandbox_path):
    import yaml

    agentenv = yaml.safe_load(agentenv_path.read_text(encoding="utf-8"))
    kasandbox = yaml.safe_load(kasandbox_path.read_text(encoding="utf-8"))
    checked = []
    for path, item in agentenv["paths"].items():
        if not path.startswith("/internal/"):
            continue
        for method, operation in item.items():
            if method not in ("get", "post", "delete", "put", "patch"):
                continue
            other = kasandbox["paths"][path][method]
            for field in ("parameters", "requestBody"):
                check_user_fields(expand(agentenv, operation.get(field)), f"{method} {path}")
                if expand(agentenv, operation.get(field)) != expand(kasandbox, other.get(field)):
                    raise ValueError(f"请求契约不一致: {method} {path} {field}")
            checked.append(f"{method.upper()} {path}")
    if len(checked) != 9:
        raise ValueError(f"预期九接口，实际 {len(checked)}")
    return {"result": "PASS", "routes": checked, "request_contracts_equal": True}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agentenv", type=Path, required=True, help="AgentENV src/api/openapi.yml")
    parser.add_argument("--kasandbox", type=Path, default=Path(__file__).resolve().parents[3] / "spec/openapi.yml")
    args = parser.parse_args()
    print(json.dumps(check(args.agentenv, args.kasandbox), ensure_ascii=False, indent=2))
