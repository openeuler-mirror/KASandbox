#!/usr/bin/env python3
"""用 Kubernetes 服务端 dry-run 验证统一 Webhook 的 AgentENV Pod 准入。"""
import argparse
import json
import subprocess
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--alias", required=True)
    parser.add_argument("--template-id", required=True)
    args = parser.parse_args()

    prefix = "agentenv-admission-" + uuid.uuid4().hex[:10]
    for case, template, expected in (
        ("alias", args.alias, args.template_id),
        ("native-id", args.template_id, args.template_id),
        ("missing-template", None, None),
        ("unknown-template", prefix, prefix),
    ):
        annotations = {"sandbox.cri/template": template} if template is not None else {}
        pod = {
            "apiVersion": "v1",
            "kind": "Pod",
            "metadata": {
                "namespace": args.namespace,
                "name": f"{prefix}-{case}",
                "annotations": annotations,
            },
            "spec": {
                "runtimeClassName": "agentenv",
                "containers": [{"name": "test", "image": "agentenv.dev/app:v1"}],
            },
        }
        result = subprocess.run(
            ["kubectl", "create", "--dry-run=server", "-f", "-", "-o", "json"],
            input=json.dumps(pod), text=True, stdout=subprocess.PIPE, check=True, timeout=30,
        )
        admitted = json.loads(result.stdout)
        if admitted["spec"]["runtimeClassName"] != "agentenv":
            raise RuntimeError(f"{case}: runtimeClassName 发生变化")
        actual = admitted["metadata"].get("annotations", {})
        expected_annotations = {"sandbox.cri/template": expected} if expected is not None else {}
        actual_template = {
            name: value for name, value in actual.items()
            if name.startswith(("aenv.dev/", "sandbox.cri/"))
        }
        if actual_template != expected_annotations:
            raise RuntimeError(f"{case}: 模板注解预期 {expected_annotations!r}，实际 {actual_template!r}")
        print(f"PASS {case}")


if __name__ == "__main__":
    main()
