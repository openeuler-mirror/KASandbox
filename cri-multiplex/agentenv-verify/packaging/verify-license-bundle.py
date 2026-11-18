#!/usr/bin/env python3
"""核对许可归档、来源与 spec 一致性；不访问运行服务。"""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import tarfile


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--root', type=Path, required=True)
    args = parser.parse_args()
    root = args.root
    bundle = root/'AgentENV-licenses-0.2.4.tar.gz'
    before = hashlib.sha256(bundle.read_bytes()).hexdigest()
    subprocess.run([sys.executable, str(root/'tools/build-license-bundle.py'), '--root', str(root)], check=True)
    assert before == hashlib.sha256(bundle.read_bytes()).hexdigest(), '归档不具备确定性'
    with tarfile.open(bundle) as archive:
        members = archive.getmembers()
        assert len({m.name for m in members}) == len(members), '重复路径'
        for member in members:
            path = PurePosixPath(member.name)
            assert member.isfile() and not path.is_absolute() and '..' not in path.parts
            assert member.mode == 0o644 and member.uid == member.gid == member.mtime == 0
        files = {m.name: archive.extractfile(m).read() for m in members}
    sums = files['licenses/SHA256SUMS'].decode().splitlines()
    expected = set(files) - {'licenses/SHA256SUMS'}
    checked = set()
    for line in sums:
        digest, name = line.split('  ', 1)
        name = 'licenses/'+name
        assert hashlib.sha256(files[name]).hexdigest() == digest, name
        checked.add(name)
    assert checked == expected
    manifest = json.loads(files['licenses/manifest.json'])
    expression = re.search(r'^License:\s*(.+)$', (root/'agentenv.spec').read_text(encoding='utf-8'), re.M)[1]
    assert manifest['license_expression'] == expression
    for row in manifest['components'].values():
        assert row['texts']
        for name in row['texts']:
            prefix = 'licenses/' if row['workspace'] else 'licenses/dependencies/'
            assert prefix+name in files, name
    for item in manifest['supplement_provenance']:
        name = 'licenses/dependencies/'+item['crate']+'/'+item['file']
        assert hashlib.sha256(files[name]).hexdigest() == item['sha256']
    for item in manifest['embedded_components']:
        assert 'licenses/dependencies/'+item['text'] in files
    spec = importlib.util.spec_from_file_location('bundle_builder', root/'tools/build-license-bundle.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    # 避免把双许可误当 AND，或丢掉子库附加许可。
    cases = {'MIT OR Apache-2.0': {'MIT'}, 'MIT/Apache-2.0': {'MIT'},
             'Apache-2.0 OR GPL-2.0-only': {'Apache-2.0'},
             '(MIT OR Apache-2.0) AND BSD-3-Clause': {'MIT', 'BSD-3-Clause'},
             'Apache 2.0': {'Apache-2.0'},
             'GPL-2.0-only WITH Classpath-exception-2.0': {'GPL-2.0-only WITH Classpath-exception-2.0'}}
    for expression, expected in cases.items():
        assert module.select(expression) == expected, expression
    for expression in ['', '(MIT', 'MIT AND', 'MIT OR', 'MIT garbage']:
        try:
            module.select(expression)
        except (AssertionError, IndexError):
            pass
        else:
            raise AssertionError('未拒绝非法表达式: '+expression)
    result = {'status': 'PASS', 'sha256': before, 'components': len(manifest['components']),
              'embedded_components': len(manifest['embedded_components']), 'files': len(files),
              'checks': ['deterministic archive', 'all file hashes', 'safe archive paths and modes',
                         'spec expression', 'manifest references', 'fixed-source provenance', 'SPDX parser 11 cases']}
    print(json.dumps(result, ensure_ascii=False))


if __name__ == '__main__':
    main()
