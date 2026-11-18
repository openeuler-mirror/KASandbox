#!/usr/bin/env python3
"""注册 AgentENV Pod 准入；复用已部署的 Webhook 服务，不重启服务。依赖 PyYAML。"""
import argparse
import json
from pathlib import Path
import subprocess

import yaml


def kubectl(*args):
    return subprocess.check_output(["kubectl", *args], text=True)


def manifest(ca):
    if not ca:
        raise ValueError("Webhook TLS Secret 缺少 tls.crt")
    value = yaml.safe_load(Path(__file__).with_name("webhook.yaml").read_text(encoding="utf-8"))
    value["webhooks"][0]["clientConfig"]["caBundle"] = ca
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("install", "uninstall"))
    parser.add_argument("--dry-run", action="store_true", help="仅输出将应用的资源")
    args = parser.parse_args()
    if args.action == "uninstall":
        if args.dry_run:
            print("kubectl delete mutatingwebhookconfiguration agentenv-webhook --ignore-not-found=true")
        else:
            kubectl("delete", "mutatingwebhookconfiguration", "agentenv-webhook", "--ignore-not-found=true")
        return
    version = json.loads(kubectl("version", "-o", "json"))["serverVersion"]
    major = int(version["major"])
    minor = int(version["minor"].rstrip("+"))
    if (major, minor) < (1, 30):
        raise ValueError("AgentENV 准入需要 Kubernetes >= 1.30")
    kubectl("-n", "e2b", "get", "service", "e2b-webhook")
    secret = json.loads(kubectl("-n", "e2b", "get", "secret", "e2b-webhook-tls", "-o", "json"))
    value = manifest(secret.get("data", {}).get("tls.crt", ""))
    data = json.dumps(value)
    if args.dry_run:
        print(data)
    else:
        subprocess.run(["kubectl", "apply", "-f", "-"], input=data, text=True, check=True)


if __name__ == "__main__":
    main()
