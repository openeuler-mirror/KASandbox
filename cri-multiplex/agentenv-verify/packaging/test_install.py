#!/usr/bin/env python3
"""在临时目录运行安装脚本副本和命令桩，不接触节点服务或系统路径。"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SOURCE = None


class InstallTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.payload = self.root / "payload"
        self.payload.mkdir()
        self.state = self.root / "state"
        self.server = self.root / "server"
        self.server.write_text("native\n")
        self.server.chmod(0o755)
        self.dropin = self.root / "service.d/runtime.conf"
        self.log = self.root / "calls"
        binaries = self.root / "bin"
        binaries.mkdir()
        stub = '''#!/usr/bin/env python3
import json, os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name
with open(os.environ['CALL_LOG'], 'a') as f: f.write(name + ' ' + ' '.join(sys.argv[1:]) + '\\n')
if name == 'systemctl':
 if sys.argv[1] == 'is-active': sys.exit(0 if os.environ.get('RUNNING') == '1' else 3)
 if sys.argv[1] == 'start' and os.environ.get('FAIL_START_ONCE'):
  marker = pathlib.Path(os.environ['FAIL_START_ONCE'])
  if not marker.exists():
   marker.touch()
   sys.exit(1)
elif name == 'pgrep': sys.exit(1)
elif name == 'aenv': print('[{}]' if os.environ.get('BUSY') == '1' else '[]')
elif name == 'id':
 if os.environ.get('FAIL_ID') == '1': sys.exit(1)
 print('1000')
elif name == 'curl': sys.exit(0)
'''
        for name in ("systemctl", "pgrep", "aenv", "id", "curl"):
            target = binaries / name
            target.write_text(stub)
            target.chmod(0o755)
        self.env = dict(os.environ, PATH=str(binaries) + os.pathsep + os.environ["PATH"], CALL_LOG=str(self.log))
        script = SOURCE.read_text(encoding="utf-8")
        for original, target in (("/usr/libexec/agentenv-cri-runtime", self.payload), ("/var/lib/agentenv-runtime-rpm", self.state), ("/usr/local/bin/server", self.server), ("/etc/systemd/system/aenv.service.d/90-agentenv-runtime.conf", self.dropin)):
            script = script.replace(original, str(target))
        # 只对临时副本绕过 root 检查，生产脚本不提供测试绕过参数。
        script = script.replace("[[ $EUID == 0 ]]", "[[ 1 == 1 ]]")
        self.script = self.root / "control.sh"
        self.script.write_text(script)
        self.candidate("candidate")

    def candidate(self, version):
        (self.payload / "server").write_text(version + "\n")
        (self.payload / "manifest.json").write_text(json.dumps({"patch_version": version}))
        sums = "".join(hashlib.sha256((self.payload / name).read_bytes()).hexdigest() + "  " + name + "\n" for name in ("server", "manifest.json"))
        (self.payload / "SHA256SUMS").write_text(sums)

    def run_action(self, action, success=True, drained=True, **env):
        args = ["bash", str(self.script), action] + (["--drained"] if drained else [])
        result = subprocess.run(args, env=dict(self.env, **env), capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def test_install_repeat_uninstall_preserves_stopped_state(self):
        self.run_action("activate")
        self.run_action("activate")
        self.assertEqual((self.state / "native-server").read_text(), "native\n")
        self.run_action("deactivate")
        self.run_action("deactivate")
        self.assertEqual(self.server.read_text(), "native\n")
        self.assertFalse(self.dropin.exists())
        calls = self.log.read_text()
        self.assertNotIn("systemctl start", calls)
        self.assertNotIn("systemctl stop", calls)

    def test_running_service_restarted_and_original_restored(self):
        self.run_action("activate", RUNNING="1")
        self.run_action("deactivate", RUNNING="1")
        self.assertEqual(self.server.read_text(), "native\n")
        calls = self.log.read_text()
        self.assertEqual(calls.count("systemctl start aenv.service"), 2)
        self.assertNotIn("cri-multiplex", calls)

    def test_busy_refused_before_mutation(self):
        self.run_action("activate", success=False, RUNNING="1", BUSY="1")
        self.assertEqual(self.server.read_text(), "native\n")
        self.assertNotIn("systemctl stop", self.log.read_text())

    def test_explicit_drain_required(self):
        self.run_action("activate", success=False, drained=False)
        self.assertFalse(self.log.exists())

    def test_corrupt_payload_refused(self):
        (self.payload / "server").write_text("corrupt")
        self.run_action("activate", success=False)
        self.assertEqual(self.server.read_text(), "native\n")

    def test_existing_dropin_preserved(self):
        self.dropin.parent.mkdir()
        self.dropin.write_text("colleague configuration")
        self.run_action("activate", success=False)
        self.assertEqual(self.dropin.read_text(), "colleague configuration")

    def test_upgrade_requires_uninstall(self):
        self.run_action("activate")
        self.candidate("next")
        self.run_action("activate", success=False)
        self.assertEqual(self.server.read_text(), "candidate\n")

    def test_failure_restores_and_allows_retry(self):
        self.run_action("activate", success=False, FAIL_ID="1")
        self.assertEqual(self.server.read_text(), "native\n")
        self.assertFalse(self.dropin.exists())
        self.assertFalse((self.state / "active-version").exists())
        self.run_action("activate")
        self.run_action("deactivate")

    def test_failed_deactivation_restores_runtime_and_backup(self):
        self.run_action("activate")
        self.run_action("deactivate", success=False, RUNNING="1", FAIL_START_ONCE=str(self.root / "failed-start"))
        self.assertEqual(self.server.read_text(), "candidate\n")
        self.assertTrue(self.dropin.exists())
        self.assertEqual((self.state / "active-version").read_text(), "candidate\n")
        self.assertEqual((self.state / "native-server").read_text(), "native\n")
        self.run_action("deactivate", RUNNING="1")
        self.assertEqual(self.server.read_text(), "native\n")

    def test_corrupt_native_backup_refused(self):
        self.run_action("activate")
        (self.state / "native-server").write_text("corrupt")
        self.run_action("deactivate", success=False)
        self.assertEqual(self.server.read_text(), "candidate\n")
        self.assertTrue(self.dropin.exists())


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--script", type=Path, required=True)
    args, remaining = parser.parse_known_args()
    SOURCE = args.script
    unittest.main(argv=[__file__, *remaining])
