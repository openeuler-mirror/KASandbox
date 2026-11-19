"""验证 DT 入口装配、参数透传及失败退出码，不启动任何服务。"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class RunnerTests(unittest.TestCase):
    def run_runner(self, args, fail_module=""):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            source = root / "repo"
            verify = source / "cri-multiplex/agentenv-verify"
            verify.mkdir(parents=True)
            here = Path(__file__).resolve().parent
            shutil.copy(here.parent / "run-ut.sh", verify.parent / "run-ut.sh")
            shutil.copy(here / "prepare-tests.py", verify / "prepare-tests.py")
            for module, package in (("cri-multiplex", "pkg/engine/agentenv"), ("e2b-webhook", "providers")):
                (source / module).mkdir(exist_ok=True)
                target = verify / "testdata" / module / package
                target.mkdir(parents=True)
                (target / "sample_test.go").write_text("package test\n")
            binary = root / "bin"
            binary.mkdir()
            go = binary / "go"
            go.write_text(r"""#!/usr/bin/env python3
import json, os, pathlib, sys
cwd = pathlib.Path.cwd()
assert 'agentenv-verify' not in str(cwd)
assert list(cwd.rglob('sample_test.go')), '测试没有装配'
with open(os.environ['CALLS'], 'a') as f:
    f.write(json.dumps([cwd.name, sys.argv[1:]]) + '\n')
if '-list' in sys.argv:
    print('TestSample')
else:
    print(json.dumps({'Action':'pass','Package':cwd.name,'Test':'TestSample'}))
    sys.exit(7 if cwd.name == os.environ.get('FAIL_MODULE') else 0)
""")
            go.chmod(0o755)
            calls = root / "calls"
            env = dict(os.environ, PATH=str(binary) + os.pathsep + os.environ['PATH'],
                       CALLS=str(calls), FAIL_MODULE=fail_module, TMPDIR=str(root))
            result = subprocess.run(['bash', str(verify.parent / 'run-ut.sh'), *args],
                                    env=env, capture_output=True, text=True)
            import json
            self.assertTrue(calls.exists(), result.stdout + result.stderr)
            events = [json.loads(line) for line in calls.read_text().splitlines()]
            self.assertFalse(list(root.glob('agentenv-ut.*')), '临时目录未清理')
            return result, events

    def test_all_and_flags(self):
        result, calls = self.run_runner(['all', '-count=1'])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        runs = [(module, args) for module, args in calls if '-list' not in args]
        self.assertEqual([module for module, _ in runs], ['cri-multiplex', 'e2b-webhook'])
        self.assertTrue(all('-count=1' in args for _, args in runs))

    def test_default(self):
        result, calls = self.run_runner([])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual({module for module, _ in calls}, {'cri-multiplex', 'e2b-webhook'})

    def test_selected_package(self):
        result, calls = self.run_runner(['pkg/engine/agentenv', '-run', 'TestSample'])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual({module for module, _ in calls}, {'cri-multiplex'})
        self.assertIn('./pkg/engine/agentenv', calls[-1][1])

    def test_list_only(self):
        result, calls = self.run_runner(['--list'])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(all('-list' in args for _, args in calls))

    def test_failure_propagates(self):
        for module in ('cri-multiplex', 'e2b-webhook'):
            with self.subTest(module=module):
                result, calls = self.run_runner(['all'], module)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual({name for name, _ in calls}, {'cri-multiplex', 'e2b-webhook'})


if __name__ == '__main__':
    unittest.main()
