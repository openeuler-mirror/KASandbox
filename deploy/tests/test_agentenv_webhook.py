#!/usr/bin/env python3
"""离线测试 Helm 渲染与部署命令桩，不连接 Kubernetes。需要 Helm、Bash 和 PyYAML。"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]

KUBECTL = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
entry = {"args": args}
for arg in args:
    if arg.startswith("--patch-file="):
        entry["patch"] = json.loads(pathlib.Path(arg.split("=", 1)[1]).read_text())
    if arg.startswith("--from-file="):
        entry["mode"] = oct(pathlib.Path(arg.split("=", 2)[2]).stat().st_mode & 0o777)
with open(os.environ["CALLS"], "a") as out:
    out.write(json.dumps(entry) + "\n")
if "apply" in args:
    sys.stdin.read()
elif "create" in args:
    print("{}")
elif "mutatingwebhookconfiguration" in args and "get" in args:
    print(json.dumps({"webhooks": [{"clientConfig": {}} for _ in range(int(os.environ["HOOKS"]))]}))
elif "secret" in args and "get" in args:
    print("Y2E=")
elif "patch" in args and os.environ.get("PATCH_FAIL") == "1":
    sys.exit(1)
'''

OPENSSL = r'''#!/usr/bin/env python3
import pathlib, sys
for option in ("-keyout", "-out"):
    pathlib.Path(sys.argv[sys.argv.index(option) + 1]).write_text("test-certificate")
'''


class DeploymentTests(unittest.TestCase):
    def run_setup(self, enabled="true", agentenv="true", key="test-only-key", hooks=3, fail=False):
        with tempfile.TemporaryDirectory(prefix="ka-webhook-") as directory:
            root = Path(directory)
            binaries = root / "bin"
            binaries.mkdir()
            tmp = root / "tmp"
            tmp.mkdir()
            for name, body in (("kubectl", KUBECTL), ("openssl", OPENSSL)):
                target = binaries / name
                target.write_text(body)
                target.chmod(0o755)
            calls = root / "calls.jsonl"
            env = dict(os.environ, PATH=f"{binaries}:{os.environ['PATH']}", TMPDIR=str(tmp),
                       CALLS=str(calls), HOOKS=str(hooks), PATCH_FAIL="1" if fail else "0",
                       ENABLE_WEBHOOK=enabled, ENABLE_AGENTENV_WEBHOOK=agentenv,
                       E2B_WEBHOOK_API_KEY=key, E2B_WEBHOOK_API_CONFIG=str(root / "absent"))
            result = subprocess.run(["bash", "-c", 'source "$1"; setup_webhook_certificates',
                                     "test", str(ROOT / "deploy/deploy.sh")], env=env, capture_output=True, text=True)
            rows = [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []
            self.assertEqual(list(tmp.iterdir()), [], "临时私钥未清理")
            self.assertNotIn("test-only-key", result.stdout + result.stderr + json.dumps(rows))
            return result, rows

    def test_disabled_preserves_resources(self):
        result, rows = self.run_setup(enabled="false", agentenv="false")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(rows, [])

    def test_missing_agentenv_key_fails_before_mutation(self):
        result, rows = self.run_setup(key="")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("API key", result.stdout)
        self.assertEqual(rows, [])

    def test_legacy_empty_key_remains_supported(self):
        result, rows = self.run_setup(agentenv="false", key="", hooks=2)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any("generic" in row["args"] for row in rows))

    def test_variable_entry_count_and_atomic_secret_update(self):
        for count in (2, 3, 4):
            with self.subTest(count=count):
                result, rows = self.run_setup(hooks=count)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertFalse(any("delete" in row["args"] for row in rows))
                patches = [row["patch"] for row in rows if "patch" in row]
                self.assertEqual(len(patches), 1)
                self.assertEqual([p["path"] for p in patches[0]],
                                 [f"/webhooks/{i}/clientConfig/caBundle" for i in range(count)])
                self.assertTrue(all(p["value"] == "Y2E=" for p in patches[0]))
                self.assertEqual([r["mode"] for r in rows if "mode" in r], ["0o600"])

    def test_patch_failure_cleans_files_without_restart(self):
        result, rows = self.run_setup(fail=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(any("patch" in row for row in rows))
        self.assertFalse(any("rollout" in row["args"] for row in rows))


class HelmTests(unittest.TestCase):
    def render(self, agentenv=False, version="1.30.0", enabled=True):
        with tempfile.TemporaryDirectory(prefix="ka-helm-") as directory:
            chart = Path(directory)
            (chart / "templates").mkdir()
            shutil.copyfile(ROOT / "helm/templates/e2b-webhook.yaml", chart / "templates/webhook.yaml")
            (chart / "Chart.yaml").write_text("apiVersion: v2\nname: webhook-test\nversion: 0.1.0\n")
            values = {"webhook": {"enabled": enabled, "agentenv": {"enabled": agentenv}},
                      "api": {"pool": "default"}, "apiPort": 3000, "registry": "example.invalid",
                      "image": {"webhook": "webhook:test"}, "pullPolicy": "IfNotPresent"}
            (chart / "values.yaml").write_text(yaml.safe_dump(values))
            result = subprocess.run(["helm", "template", "test", str(chart), "--kube-version", version],
                                    capture_output=True, text=True)
            return result, list(yaml.safe_load_all(result.stdout)) if result.returncode == 0 else []

    def test_default_preserves_two_entries(self):
        result, docs = self.render(version="1.29.0")
        self.assertEqual(result.returncode, 0, result.stderr)
        hooks = next(d["webhooks"] for d in docs if d and d["kind"] == "MutatingWebhookConfiguration")
        self.assertEqual(len(hooks), 2)
        deployment = next(d for d in docs if d and d["kind"] == "Deployment")
        env = deployment["spec"]["template"]["spec"]["containers"][0]["env"]
        key = next(e for e in env if e["name"] == "E2B_API_KEY")
        self.assertTrue(key["valueFrom"]["secretKeyRef"]["optional"])

    def test_agentenv_scoped_and_nonroot(self):
        result, docs = self.render(agentenv=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        hooks = next(d["webhooks"] for d in docs if d and d["kind"] == "MutatingWebhookConfiguration")
        self.assertEqual(len(hooks), 3)
        hook = hooks[-1]
        self.assertEqual(hook["failurePolicy"], "Ignore")
        expression = hook["matchConditions"][0]["expression"]
        self.assertIn("object.spec.runtimeClassName == 'agentenv'", expression)
        self.assertIn("batch-sandbox.sandbox.opensandbox.io/pod-index", expression)
        pod = next(d for d in docs if d and d["kind"] == "Deployment")["spec"]["template"]["spec"]
        self.assertFalse(pod["automountServiceAccountToken"])
        self.assertEqual(pod["securityContext"]["runAsUser"], 65532)
        key = next(e for e in pod["containers"][0]["env"] if e["name"] == "E2B_API_KEY")
        self.assertFalse(key["valueFrom"]["secretKeyRef"]["optional"])

    def test_unsupported_cluster_rejected_only_when_enabled(self):
        result, _ = self.render(agentenv=True, version="1.29.0")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Kubernetes >= 1.30", result.stderr)

    def test_disabled_chart_empty(self):
        result, docs = self.render(enabled=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any(docs))


if __name__ == "__main__":
    unittest.main(verbosity=2)
