"""独立准入安装脚本的 mock 测试，不连接 Kubernetes。"""
import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[3]
SPEC = importlib.util.spec_from_file_location("deploy_agentenv", ROOT / "deploy/agentenv/webhook.py")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class DeploymentTests(unittest.TestCase):
    def test_scope(self):
        doc = MODULE.manifest("Y2E=")
        self.assertEqual(doc["metadata"]["name"], "agentenv-webhook")
        self.assertEqual(len(doc["webhooks"]), 1)
        hook = doc["webhooks"][0]
        self.assertIn("runtimeClassName == 'agentenv'", hook["matchConditions"][0]["expression"])
        self.assertEqual(hook["clientConfig"]["caBundle"], "Y2E=")

    def test_install_only_applies_own_resource(self):
        responses = [json.dumps({"serverVersion": {"major": "1", "minor": "30"}}), "service", json.dumps({"data": {"tls.crt": "Y2E="}})]
        with patch("sys.argv", ["webhook.py", "install"]), patch.object(MODULE, "kubectl", side_effect=responses), patch.object(MODULE.subprocess, "run") as run:
            MODULE.main()
            run.assert_called_once()
            self.assertEqual(run.call_args.args[0], ["kubectl", "apply", "-f", "-"])
            self.assertEqual(json.loads(run.call_args.kwargs["input"])["metadata"]["name"], "agentenv-webhook")

    def test_old_cluster_stops_before_mutation(self):
        with patch("sys.argv", ["webhook.py", "install"]), patch.object(MODULE, "kubectl", return_value=json.dumps({"serverVersion": {"major": "1", "minor": "29"}})), patch.object(MODULE.subprocess, "run") as run:
            with self.assertRaises(ValueError):
                MODULE.main()
            run.assert_not_called()

    def test_missing_certificate(self):
        with self.assertRaises(ValueError):
            MODULE.manifest("")

    def test_uninstall_only_own_resource(self):
        with patch("sys.argv", ["webhook.py", "uninstall"]), patch.object(MODULE, "kubectl") as command:
            MODULE.main()
            command.assert_called_once_with("delete", "mutatingwebhookconfiguration", "agentenv-webhook", "--ignore-not-found=true")


if __name__ == "__main__":
    unittest.main()
