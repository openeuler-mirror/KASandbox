#!/usr/bin/env python3
"""核对许可归档、来源与 spec 一致性；不访问运行服务。"""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import tarfile


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--root', type=Path, required=True)
    args = parser.parse_args()
    root = args.root
    bundle = root/'AgentENV-licenses-0.2.4.tar.gz'
    before = hashlib.sha256(bundle.read_bytes()).hexdigest()
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
    result = {'status': 'PASS', 'sha256': before, 'components': len(manifest['components']),
              'embedded_components': len(manifest['embedded_components']), 'files': len(files),
              'checks': ['all file hashes', 'safe archive paths and modes',
                         'spec expression', 'manifest references', 'fixed-source provenance']}
    print(json.dumps(result, ensure_ascii=False))


if __name__ == '__main__':
    main()
