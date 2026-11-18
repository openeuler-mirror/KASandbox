#!/usr/bin/env python3
"""核对独立 Runtime RPM 的提交输入；不连接服务或修改源码。"""
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
    root = args.root.resolve()
    # 当前输入指纹独立于历史验证报告，避免元数据变更后误用旧 spec 哈希。
    evidence = json.loads((Path(__file__).with_name('srpm-inputs.json')).read_text(encoding='utf-8'))
    for name, expected in evidence['srpm_inputs'].items():
        assert hashlib.sha256((root / name).read_bytes()).hexdigest() == expected, name
    archives = {}
    for name in evidence['srpm_inputs']:
        if not name.endswith('.tar.gz'):
            continue
        with tarfile.open(root / name) as archive:
            members = archive.getmembers()
            assert len({m.name for m in members}) == len(members), name
            for member in members:
                path = PurePosixPath(member.name)
                assert not path.is_absolute() and '..' not in path.parts, member.name
                assert not member.isdev() and not member.isfifo(), member.name
                if member.issym() or member.islnk():
                    target = PurePosixPath(member.linkname)
                    assert not target.is_absolute(), member.name
                    # 链接逐段归一化后不能逃出归档根。
                    parts = list(path.parent.parts) if member.issym() else []
                    for part in target.parts:
                        if part == '..':
                            assert parts, member.name
                            parts.pop()
                        elif part != '.':
                            parts.append(part)
            archives[name] = len(members)
    patterns = [r'-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----',
                r'openEuler\d+[#\$]', r'123\.60\.114\.33',
                r'(?i)(?:access_token|password)\s*[:=]\s*[\"\x27][A-Za-z0-9_/-]{20,}']
    findings = []
    for path in root.rglob('*'):
        relative = path.relative_to(root)
        if not path.is_file() or any(x in ('.git', '__pycache__') for x in relative.parts):
            continue
        if path.suffix in ('.gz', '.pyc') or path.name == 'verify-submission.py':
            continue
        text = path.read_text(encoding='utf-8')
        if path.suffix == '.patch':
            text = '\n'.join(line[1:] for line in text.splitlines()
                             if line.startswith('+') and not line.startswith('+++'))
        for index, pattern in enumerate(patterns):
            if re.search(pattern, text):
                findings.append({'path': relative.as_posix(), 'rule': index + 1})
    assert not findings, findings
    print(json.dumps({'status': 'PASS', 'inputs': len(evidence['srpm_inputs']),
                      'archives': archives, 'sensitive_pattern_findings': findings}, ensure_ascii=False))


if __name__ == '__main__':
    main()
