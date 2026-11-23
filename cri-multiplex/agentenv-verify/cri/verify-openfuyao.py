#!/usr/bin/env python3
"""模拟扶摇调用，通过统一 Webhook、CRI、Admin v2 和 AgentENV 元数据 API 验证沙箱生命周期。"""
import argparse
from datetime import datetime, timezone
from http.client import HTTPConnection, HTTPSConnection
import ipaddress
import json
import os
from pathlib import Path
import re
import ssl
import subprocess
import tomllib
from urllib.parse import quote, urlencode, urlsplit
from uuid import uuid4


PROTO_DIR = Path(__file__).resolve().parents[2] / "proto"
RUNTIME_PROTO = PROTO_DIR / "agentenv/runtime/v1/runtime.proto"
ADMIN = "crimultiplex.admin.v2.SandboxAdminService"
CRI = "runtime.v1.RuntimeService"
TEMPLATE = "sandbox.cri/template"


def check(condition, message):
    if not condition:
        raise AssertionError(message)


class OpenFuyaoStub:
    def __init__(self, args):
        self.args = args
        credentials = tomllib.loads(args.credentials.read_text(encoding="utf-8"))
        self.api_url = args.api_url or credentials["url"]
        self.api_key = credentials["api_key"]
        self.admin_token = os.environ.get("AENV_INTERNAL_ADMIN_TOKEN") or self.api_key
        self.webhook_tls = ssl.create_default_context(cafile=str(args.webhook_ca))
        self.sandbox_id = str(uuid4())
        self.name = "openfuyao-probe-" + self.sandbox_id[:8]
        self.team_id = str(uuid4())
        self.team_query = urlencode({"team_id": self.team_id})
        self.catalog = f"/internal/sandboxes/{self.sandbox_id}"
        self.target = {"backend": "agentenv", "criSandboxId": self.sandbox_id}
        self.created = False
        self.pause = None
        self.checkpoint = None
        self.operations = []
        self.completed_builds = set()
        self.cases = []
        self.requests = []

    def passed(self, case):
        self.cases.append(case)
        print("PASS", case, flush=True)

    def http(self, method, url, headers, payload=None, expected=(200,), tls=None):
        parsed = urlsplit(url)
        check(parsed.scheme in {"http", "https"} and parsed.hostname, "HTTP URL 无效")
        connection = (
            HTTPSConnection(parsed.hostname, parsed.port, timeout=20, context=tls)
            if parsed.scheme == "https"
            else HTTPConnection(parsed.hostname, parsed.port, timeout=20)
        )
        path = parsed.path or "/"
        if parsed.query:
            path += "?" + parsed.query
        if payload is not None:
            headers = dict(headers, **{"Content-Type": "application/json"})
            payload = json.dumps(payload)
        try:
            connection.request(method, path, body=payload, headers=headers)
            response = connection.getresponse()
            body = response.read()
        finally:
            connection.close()
        self.requests.append({"method": method, "path": parsed.path, "status": response.status})
        check(response.status in expected, f"{method} {parsed.path}: HTTP {response.status}，预期 {expected}")
        return json.loads(body) if body else None

    def internal(self, method, path, payload=None, expected=(200,)):
        return self.http(method, self.api_url.rstrip("/") + path,
                         {"X-Admin-Token": self.admin_token}, payload, expected)

    def rpc(self, socket, proto, service, method, data, expected="OK", includes=(), timeout=30):
        cmd = ["grpcurl", "-plaintext", "-emit-defaults", "-max-time", str(timeout)]
        for entry in (proto.parent, *includes):
            cmd.extend(("-import-path", str(entry.resolve())))
        cmd.extend(("-proto", proto.name, "-d", "@", f"unix://{socket.resolve()}", f"{service}/{method}"))
        result = subprocess.run(cmd, input=json.dumps(data), capture_output=True,
                                text=True, timeout=timeout + 5, check=False)
        match = re.search(r"^\s*Code:\s*(\w+)", result.stderr, re.MULTILINE)
        status = "OK" if result.returncode == 0 else match.group(1) if match else "ClientError"
        self.requests.append({"method": f"{service}/{method}", "status": status})
        check(status == expected, f"{method}: {status}，预期 {expected}\n{result.stderr.strip()}")
        return json.loads(result.stdout) if status == "OK" and result.stdout else {}

    def cri(self, method, data, expected="OK", timeout=30):
        return self.rpc(self.args.cri_socket, self.args.cri_proto, CRI, method, data,
                        expected, self.args.cri_proto_include, timeout)

    def admin(self, method, data, expected="OK", timeout=30):
        return self.rpc(self.args.admin_socket, PROTO_DIR / "admin_v2.proto",
                        ADMIN, method, data, expected, timeout=timeout)

    def runtime(self, service, method, data):
        return self.rpc(self.args.runtime_socket, RUNTIME_PROTO,
                        f"agentenv.runtime.v1.{service}", method, data, includes=(PROTO_DIR,))

    def runtime_sandboxes(self):
        return self.runtime("SandboxLifecycleService", "List", {
            "productSandboxId": self.sandbox_id, "includePaused": True,
        }).get("sandboxes", [])

    def admit(self, template):
        annotations = {TEMPLATE: template} if template is not None else {}
        pod = {
            "apiVersion": "v1", "kind": "Pod",
            "metadata": {"name": self.name, "namespace": "agentenv-stub", "annotations": annotations},
            "spec": {"runtimeClassName": "agentenv", "containers": [{"name": "app", "image": "agentenv.dev/app:v1"}]},
        }
        uid = str(uuid4())
        review = {
            "apiVersion": "admission.k8s.io/v1", "kind": "AdmissionReview",
            "request": {"uid": uid, "operation": "CREATE", "object": pod,
                        "kind": {"version": "v1", "kind": "Pod"}},
        }
        response = self.http("POST", self.args.webhook_url, {}, review, tls=self.webhook_tls)["response"]
        check(response["uid"] == uid and response["allowed"], "Webhook 返回的准入结果不匹配")
        check(not response.get("patch"), "Webhook 不应修改已有的模板注解或 RuntimeClass")
        return annotations

    def run(self):
        annotations = self.admit(self.args.template)
        check(annotations == {TEMPLATE: self.args.template}, "Webhook 未保留模板别名")
        check(self.admit(self.args.template_id) == {TEMPLATE: self.args.template_id},
              "按模板 ID 准入的解析结果不匹配")
        check(self.admit(None) == {}, "缺少模板时不应注入模板信息")
        check(self.admit(self.name) == {TEMPLATE: self.name}, "未知模板不应被解析")
        self.passed("W1-W4 模板别名、ID、缺失和未知模板准入")

        pod = {"metadata": {"uid": self.sandbox_id, "name": self.name, "namespace": "agentenv-stub"}}
        request = {"runtimeHandler": "agentenv", "config": pod}
        self.cri("RunPodSandbox", request, "InvalidArgument")
        check(not self.runtime_sandboxes(), "无效创建产生了沙箱")
        self.passed("C1 缺少必填模板被拒绝且未创建")
        pod["annotations"] = annotations
        # 请求可能已执行但响应未返回，清理始终使用预先生成的唯一 ID。
        self.created = True
        for _ in range(2):
            response = self.cri("RunPodSandbox", request, timeout=90)
            check(response["podSandboxId"] == self.sandbox_id, "首次创建或重试未返回相同 CRI ID")
        items = self.cri("ListPodSandbox", {"filter": {"id": self.sandbox_id}})["items"]
        check(len(items) == 1 and items[0]["id"] == self.sandbox_id, "幂等重试产生多条实例记录")
        ready = self.cri("PodSandboxStatus", {"podSandboxId": self.sandbox_id})["status"]
        check(ready["state"] == "SANDBOX_READY", "沙箱未 Ready")
        check(ready["metadata"]["uid"] == self.sandbox_id and ready["annotations"][TEMPLATE] == self.args.template,
              "CRI 查询返回的 Pod UID 或模板与创建请求不匹配")
        pod_ip = ipaddress.ip_address(ready["network"]["ip"])
        check(not (pod_ip.is_unspecified or pod_ip.is_loopback), "沙箱未返回有效 Pod IP")
        self.passed("C2-C3 CRI 创建、幂等和 Ready Pod IP")

        runtime = self.admin("GetSandboxRuntime", self.target)["runtime"]
        check(runtime["runtimeState"] == "Running" and runtime["backend"] == "agentenv",
              "Admin 运行事实与 CRI 实例不匹配")
        check(runtime["criSandboxId"] == runtime["sandboxId"] == self.sandbox_id,
              "Admin 返回的标识与创建请求不匹配")
        native = runtime["extra"]["native_sandbox_id"]
        self.passed("A1 Admin v2 查询运行事实和原生 ID")
        self.admin("PauseSandbox", self.target, "InvalidArgument")
        check(self.admin("GetSandboxRuntime", self.target)["runtime"]["runtimeState"] == "Running",
              "缺少操作 ID 的暂停请求改变了运行状态")
        self.passed("A2 暂停缺少操作 ID 被拒绝")

        # 模拟业务侧透传的元数据，使用本次沙箱和模板的实际身份。
        metadata = {
            "team_id": self.team_id, "base_template_id": self.args.template_id,
            "source_pod_uid": self.sandbox_id, "source_node_name": runtime["nodeName"],
            "sandbox_started_at": datetime.fromtimestamp(int(ready["createdAt"]) // 10**9, timezone.utc).isoformat(),
            "vcpu": self.args.vcpu, "ram_mb": self.args.ram_mb, "total_disk_size_mb": 0,
            "kernel_version": "stub", "firecracker_version": "stub", "envd_version": "stub", "secure": False,
        }
        self.snapshot("snapshot-templates", "CheckpointSandbox", metadata)
        checkpoint_id = self.checkpoint["snapshot_id"]
        resolve = f"/internal/snapshots/{quote(checkpoint_id, safe='')}/resolve?{self.team_query}"
        check(self.internal("GET", resolve)["build_id"] == self.checkpoint["build_id"],
              "检查点元数据解析到了不同构建")
        base = quote(self.args.template_id, safe="")
        self.internal("DELETE", f"/internal/templates/{base}?{self.team_query}", expected=(409,))
        self.passed("S1 检查点预分配、执行、回调和解析")

        self.snapshot("pause-snapshot", "PauseSandbox", metadata)
        state = self.internal("GET", self.catalog + "/snapshot-state")
        last = self.internal("GET", self.catalog + "/last-snapshot")
        check(state["paused"] and state["build_id"] == last["build_id"] == self.pause["build_id"],
              "暂停状态与快照元数据不匹配")
        self.passed("S2 暂停预分配、执行、回调和查询")
        return {"cri_sandbox_id": self.sandbox_id, "native_sandbox_id": native, "pod_ip": str(pod_ip)}

    def snapshot(self, endpoint, method, metadata):
        operation = str(uuid4())
        payload = dict(metadata, operation_id=operation)
        path = f"{self.catalog}/{endpoint}"
        record = self.internal("POST", path, payload)
        if method == "CheckpointSandbox":
            self.checkpoint = record
        else:
            self.pause = record
        self.operations.append((operation, method, record["build_id"]))
        check(self.internal("POST", path, payload) == record, f"{endpoint} 重试产生新分配")
        rpc_data = dict(self.target, operationId=operation, timeoutSeconds="90")
        for _ in range(2):
            result = self.admin(method, rpc_data, timeout=100)
            check(result.get("snapshotPersisted"), f"{method} 未持久化")
            if method == "CheckpointSandbox":
                check(result.get("vmResumed"), "检查点结束后源 VM 未恢复运行")
        callback = f"/internal/builds/{quote(record['build_id'], safe='')}/status"
        self.internal("POST", callback, {"operation_id": operation, "status": "success"})
        self.completed_builds.add(record["build_id"])
        state = self.admin("GetSandboxRuntime", self.target)["runtime"]["runtimeState"]
        check(state == ("Running" if method == "CheckpointSandbox" else "Paused"), f"{method} 后状态为 {state}")

    def cleanup(self):
        errors = []

        def attempt(action, *args, **kwargs):
            try:
                return action(*args, **kwargs)
            except Exception as error:
                errors.append(str(error))
                return None

        if self.created:
            pod = {"podSandboxId": self.sandbox_id}
            attempt(self.cri, "StopPodSandbox", pod)
            attempt(self.cri, "StopPodSandbox", pod)
            attempt(self.cri, "RemovePodSandbox", pod)
            attempt(self.admin, "GetSandboxRuntime", self.target, "NotFound")
            if attempt(self.runtime_sandboxes):
                errors.append("清理后仍有本次 Runtime Sandbox")
        for operation, method, build in self.operations:
            callback = f"/internal/builds/{quote(build, safe='')}/status"
            if build not in self.completed_builds:
                attempt(self.internal, "POST", callback, {"operation_id": operation, "status": "failed"}, (200, 409))
            if method == "CheckpointSandbox":
                # 通过本次唯一操作查询真实快照引用，只清理该快照。
                info = attempt(self.runtime, "SandboxAdminService", "GetOperation", {"operationId": operation})
                ref = (info or {}).get("result", {}).get("snapshot_ref")
                if ref:
                    attempt(self.http, "DELETE", self.api_url.rstrip("/") + f"/snapshots/{quote(ref, safe='')}",
                            {"X-API-Key": self.api_key}, expected=(204, 404))
                elif build in self.completed_builds:
                    errors.append("已完成的 Checkpoint 缺少原生快照引用")
        if self.pause:
            attempt(self.internal, "DELETE", self.catalog + "/snapshot", expected=(204, 404))
            attempt(self.internal, "GET", self.catalog + "/last-snapshot", expected=(404,))
        if self.checkpoint:
            ref = quote(self.checkpoint["snapshot_id"], safe="")
            attempt(self.internal, "DELETE", f"/internal/snapshot-templates/{ref}?{self.team_query}", expected=(204, 404))
            attempt(self.internal, "GET", f"/internal/snapshots/{ref}/resolve?{self.team_query}", expected=(404,))
        if self.created and not errors:
            self.passed("D1 CRI 停止幂等、删除和资源清理")
        return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for arg in ("cri-socket", "admin-socket", "runtime-socket", "cri-proto", "webhook-ca", "output"):
        parser.add_argument("--" + arg, type=Path, required=True)
    for arg in ("webhook-url", "template", "template-id"):
        parser.add_argument("--" + arg, required=True)
    parser.add_argument("--cri-proto-include", type=Path, action="append", default=[])
    parser.add_argument("--credentials", type=Path, default=Path.home() / ".config/aenv/credentials")
    parser.add_argument("--api-url")
    parser.add_argument("--vcpu", type=int, default=2)
    parser.add_argument("--ram-mb", type=int, default=1024)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    stub = OpenFuyaoStub(args)
    result = {}
    try:
        result["sandbox"] = stub.run()
    except Exception as error:
        result["failure"] = str(error)
        print("FAIL", error, flush=True)
    finally:
        result["cleanup_errors"] = stub.cleanup()
        result["cases"] = stub.cases
        result["requests"] = stub.requests
        result["status"] = "FAIL" if result.get("failure") or result["cleanup_errors"] else "PASS"
        (args.output / "result.json").write_text(json.dumps(result, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    return 0 if result["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
