#!/usr/bin/env python3
"""离线核对两仓九接口的入参与必填要求，不发送 HTTP 请求。依赖 PyYAML。"""
import argparse
import json
from pathlib import Path

import yaml


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


def check(agentenv_path, kasandbox_path):
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
                if expand(agentenv, operation.get(field)) != expand(kasandbox, other.get(field)):
                    raise ValueError(f"请求契约不一致: {method} {path} {field}")
            if method == "post":
                if expand(agentenv, operation["responses"]["503"]) != expand(kasandbox, other["responses"]["503"]):
                    raise ValueError(f"维护响应不一致: {method} {path}")
            checked.append(f"{method.upper()} {path}")
    if len(checked) != 9:
        raise ValueError(f"预期九接口，实际 {len(checked)}")
    return {"result": "PASS", "routes": checked, "request_contracts_equal": True}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agentenv", type=Path, required=True, help="AgentENV src/api/openapi.yml")
    parser.add_argument("--kasandbox", type=Path, default=Path(__file__).resolve().parents[1] / "spec/openapi.yml")
    args = parser.parse_args()
    print(json.dumps(check(args.agentenv, args.kasandbox), ensure_ascii=False, indent=2))
