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
    parser.add_argument("--build-id", required=True)
    args = parser.parse_args()

    prefix = "agentenv-admission-" + uuid.uuid4().hex[:10]
    for case, template, build in (
        ("alias", args.alias, None),
        ("native-id", args.template_id, None),
        ("pinned-build", args.alias, args.build_id),
        ("missing-template", None, None),
        ("unknown-template", prefix, None),
    ):
        annotations = {}
        if template is not None:
            annotations["sandbox.cri/template"] = template
        if build is not None:
            annotations["sandbox.cri/build"] = build
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
        resolved = case in {"alias", "native-id", "pinned-build"}
        expected = (args.template_id, args.build_id) if resolved else (None, None)
        observed = (actual.get("aenv.dev/template-id"), actual.get("sandbox.cri/build"))
        if observed != expected:
            raise RuntimeError(f"{case}: 模板注解预期 {expected!r}，实际 {observed!r}")
        print(f"PASS {case}")


if __name__ == "__main__":
    main()
