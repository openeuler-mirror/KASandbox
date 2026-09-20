"""bench replay-mixgen: 从 swerebench-offline-replays 的 manifest.csv 生成 mix 配置。

数据源模型：每条轨迹对应一个独立镜像（374 条 SWE-rebench，每实例一镜像）。
按语言均衡抽样（等名额，剩余按池子大小分配），每个选中实例生成一个 workload：
template 名 swreb-<instance-slug>，image 取自 manifest（默认保留 SWR 地址，
可用 --registry-rewrite 改写到内网 registry），trajectory_dir 指向单个轨迹文件。
TerminalBench（tbench-*）默认剔除。
"""

from __future__ import annotations

import argparse
import csv
import json
import random
import sys
from collections import defaultdict
from pathlib import Path

from ..e2b_common import positive_int, print_json
from .trajectory import slugify

TEMPLATE_PREFIX = "swreb"


def register(subparsers) -> None:
    parser = subparsers.add_parser(
        "replay-mixgen",
        help="从 manifest.csv 按语言均衡抽样生成 replay mix 配置（每条轨迹一个镜像的场景）",
    )
    parser.add_argument("--manifest", type=Path, required=True, help="swerebench-offline-replays 的 manifest.csv")
    parser.add_argument("--replays-dir", type=Path, required=True, help="轨迹文件所在目录（manifest 的 replay_file 相对此目录）")
    parser.add_argument("--count", type=positive_int, default=60, help="抽样实例数（默认 60）")
    parser.add_argument("--total-target", type=positive_int, help="总回放次数，按实例均分 vm_count（缺省 = 实例数）")
    parser.add_argument("--seed", type=int, default=42, help="抽样随机种子（默认 42，可复现）")
    parser.add_argument(
        "--registry-rewrite",
        help="镜像地址改写，格式 old_prefix=new_prefix（如 swr.cn-north-4.myhuaweicloud.com/kunpeng-ai=10.175.0.89:30443/e2b-orchestration）",
    )
    parser.add_argument("--include-tbench", action="store_true", help="包含 TerminalBench 轨迹（默认剔除）")
    parser.add_argument("-o", "--output", type=Path, help="mix 配置输出路径（缺省打印到 stdout）")
    parser.set_defaults(handler=execute)


def execute(args: argparse.Namespace) -> int:
    rows: list[dict] = []
    with args.manifest.open(encoding="utf-8") as stream:
        for row in csv.DictReader(stream):
            if not args.include_tbench and row["replay_file"].startswith("tbench-"):
                continue
            replay_path = args.replays_dir / row["replay_file"]
            if not replay_path.is_file():
                print(f"[mixgen] 轨迹缺失，跳过 {row['instance_id']}：{replay_path}", file=sys.stderr)
                continue
            rows.append(row)
    if not rows:
        raise ValueError("manifest 中没有可用轨迹行")

    # 语言均衡：每语言等名额，剩余名额按池子大小从大到小补
    by_lang: dict[str, list[dict]] = defaultdict(list)
    for row in rows:
        by_lang[row["language"]].append(row)
    rng = random.Random(args.seed)
    for pool in by_lang.values():
        rng.shuffle(pool)

    languages = sorted(by_lang, key=lambda lang: -len(by_lang[lang]))
    base = args.count // len(languages)
    remainder = args.count % len(languages)
    quota = {lang: base + (1 if idx < remainder else 0) for idx, lang in enumerate(languages)}
    # 小池子（如 c/cpp）用不完的名额二次分配给还有余量的语言，保证总数凑齐
    leftover = sum(quota[lang] - min(quota[lang], len(by_lang[lang])) for lang in languages)
    for lang in languages:
        if leftover <= 0:
            break
        capacity = len(by_lang[lang]) - min(quota[lang], len(by_lang[lang]))
        take = min(capacity, leftover)
        quota[lang] += take
        leftover -= take

    picked: list[dict] = []
    for lang in languages:
        picked.extend(by_lang[lang][: min(quota[lang], len(by_lang[lang]))])
    if len(picked) < args.count:
        print(
            f"[mixgen] 警告：池子不足，实际选中 {len(picked)} 个实例（目标 {args.count}）",
            file=sys.stderr,
        )

    total_target = args.total_target or len(picked)
    vm_base = total_target // len(picked)
    vm_remainder = total_target % len(picked)

    def _rewrite(image: str) -> str:
        if not args.registry_rewrite:
            return image
        old, sep, new = args.registry_rewrite.partition("=")
        if not sep:
            raise ValueError("--registry-rewrite 格式必须是 old_prefix=new_prefix")
        return new + image[len(old):] if image.startswith(old) else image

    workloads = []
    for index, row in enumerate(sorted(picked, key=lambda r: r["instance_id"])):
        name = f"{TEMPLATE_PREFIX}-{slugify(row['instance_id'], limit=56)}"
        workloads.append({
            "name": name,
            "template": name,
            "image": _rewrite(row["image"]),
            "trajectory_dir": str((args.replays_dir / row["replay_file"]).resolve()),
            "vm_count": vm_base + (1 if index < vm_remainder else 0),
            "workdir": "/testbed",
        })

    config = {"workloads": workloads}
    text = json.dumps(config, ensure_ascii=False, indent=2)
    if args.output:
        args.output.write_text(text + "\n", encoding="utf-8")
    print_json({
        "output": str(args.output) if args.output else None,
        "instances": len(workloads),
        "total_target": sum(w["vm_count"] for w in workloads),
        "languages": {lang: min(quota[lang], len(by_lang[lang])) for lang in languages},
    })
    if not args.output:
        print(text)
    return 0
