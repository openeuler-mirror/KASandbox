#!/usr/bin/env python3
"""运行指定的 AgentENV 回归套件；默认不连接服务，HTTP 测试须显式选择。"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--suite', action='append', choices=('unit', 'runtime-unit', 'http', 'contract', 'rpm-mock'), required=True)
    parser.add_argument('--runtime-source', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--kasandbox-spec', type=Path)
    parser.add_argument('--http-results', type=Path)
    parser.add_argument('--skip-fault-injection', action='store_true', help='跳过终止测试子进程的耐久性用例')
    args = parser.parse_args()
    repo = args.runtime_source.resolve()
    tests = Path(__file__).resolve().parent
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    commands = {
        'unit': ['cargo', 'test', '--frozen', '--release', '-p', 'agentenv', '--lib', 'api::internal::tests'],
        'runtime-unit': ['cargo', 'test', '--frozen', '--release', '-p', 'agentenv', '--lib', 'runtime_server::tests'],
        'http': [sys.executable, str(tests / 'internal-api-regression.py')],
        'rpm-mock': [sys.executable, str(tests.parent / 'packaging/test_install.py'), '--script', str(repo / 'scripts/release/agentenv-cri-runtime-patch/rpm-control.sh')],
    }
    if args.skip_fault_injection:
        commands['unit'] += ['--', '--skip', 'catalog_acknowledged_writes_survive_process_kill']
    if 'contract' in args.suite:
        if not args.kasandbox_spec or not args.http_results:
            parser.error('contract 需要 --kasandbox-spec 和 --http-results')
        commands['contract'] = [sys.executable, str(tests / 'validate-internal-contract.py'), '--agentenv', str(repo / 'src/api/openapi.yml'), '--kasandbox', str(args.kasandbox_spec.resolve()), '--results', str(args.http_results.resolve()), '--output', str(out / 'contract.json')]
    results = {}
    for suite in args.suite:
        with (out / (suite + '.log')).open('w') as log:
            env = dict(os.environ, HTTP_RESULT=str(out / 'http.json'))
            result = subprocess.run(commands[suite], cwd=repo, env=env, stdout=log, stderr=subprocess.STDOUT)
        results[suite] = result.returncode
        (out / 'results.json').write_text(json.dumps(results, indent=2), encoding='utf-8')
        if result.returncode:
            raise SystemExit(result.returncode)


if __name__ == '__main__':
    main()
