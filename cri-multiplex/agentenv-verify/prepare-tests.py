#!/usr/bin/env python3
"""在独立目录装配同包测试；不修改生产源码、不启动服务。"""
import argparse
from pathlib import Path
import shutil
import re


def prepare(kind, source, output):
    source, output = Path(source).resolve(), Path(output).resolve()
    if output == source or source in output.parents:
        raise ValueError("测试目录必须位于源码目录之外")
    tests = Path(__file__).resolve().parent
    if output.exists():
        raise FileExistsError(output)
    output.mkdir(parents=True)
    if kind == 'go':
        for module in ('cri-multiplex', 'e2b-webhook'):
            shutil.copytree(source / module, output / module,
                            ignore=shutil.ignore_patterns('.git', 'agentenv-verify', '__pycache__'))
        shutil.copytree(tests / 'testdata', output, dirs_exist_ok=True)
    else:
        shutil.copytree(source, output, dirs_exist_ok=True,
                        ignore=shutil.ignore_patterns('.git', 'target', 'vendor', '.cargo-home', '__pycache__'))
        # vendor 只读复用；Cargo 产物写入本次测试目录或调用方指定的 CARGO_TARGET_DIR。
        if (source / 'vendor').exists():
            (output / 'vendor').symlink_to(source / 'vendor', target_is_directory=True)
        for path in (tests / 'runtime/rust').rglob('*.rs'):
            dest = output / path.relative_to(tests / 'runtime/rust')
            dest.parent.mkdir(parents=True, exist_ok=True)
            content = path.read_text(encoding='utf-8')
            marker = '// agentenv-verify: insert-tests\n'
            if content.startswith(marker):
                original = dest.read_text(encoding='utf-8')
                start = original.index('mod tests {')
                end = re.search(r'(?m)^}', original[start:])
                if end is None:
                    raise ValueError(f'找不到测试模块结尾: {dest}')
                index = start + end.start()
                dest.write_text(original[:index] + content[len(marker):] + '\n' + original[index:],
                                encoding='utf-8', newline='\n')
                continue
            with dest.open('a', encoding='utf-8', newline='\n') as stream:
                stream.write('\n' + content)
    return output


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kind', choices=['go', 'runtime'], required=True)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    print(prepare(args.kind, args.source, args.output))
