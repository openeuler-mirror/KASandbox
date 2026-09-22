"""bench 子命令路由：create / kill-all / scale / density / snapshot-* / rollback / clone / pause-resume / replay / replay-nolifecycle / replay-matrix / replay-mixgen / all."""

from __future__ import annotations

import argparse

from ..e2b_common import print_json
from ..e2b_config import add_config_arguments, require_api_key
from . import (
    clean_host,
    clone,
    create,
    create_from_snapshot,
    density,
    pause_resume,
    replay,
    replay_matrix,
    replay_mixgen,
    replay_nolifecycle,
    rollback,
    runner,
    scale,
    snapshot_concurrency,
    snapshot_dirty,
)
from .client import BenchClient
from .common import add_common_arguments, cleanup_marked_sandboxes, guarded


def _register_kill_all(subparsers) -> None:
    parser = subparsers.add_parser("kill-all", help="清理 bench 创建的沙箱（默认仅清理带 bench 标记的）")
    parser.add_argument(
        "--all",
        action="store_true",
        help="删除全部沙箱（危险：包含非 bench 创建的沙箱）",
    )
    parser.add_argument("--run-id", help="仅清理指定 run_id 的 bench 沙箱")
    add_common_arguments(parser)
    parser.set_defaults(handler=guarded(execute_kill_all))


def execute_kill_all(args: argparse.Namespace) -> int:
    require_api_key()
    client = BenchClient(timeout=60)
    outcome = cleanup_marked_sandboxes(client, run_id=args.run_id, include_all=args.all)
    print_json(outcome)
    return 0 if outcome.get("failed", 0) == 0 else 1


def register_subcommand(subparsers) -> None:
    parser = subparsers.add_parser("bench", help="性能基准测试（对标 CubeSandbox 口径）")
    add_config_arguments(parser)
    bench_subparsers = parser.add_subparsers(dest="bench_action", required=True)

    create.register(bench_subparsers)
    _register_kill_all(bench_subparsers)
    clean_host.register(bench_subparsers)
    scale.register(bench_subparsers)
    density.register(bench_subparsers)
    snapshot_concurrency.register(bench_subparsers)
    snapshot_dirty.register(bench_subparsers)
    create_from_snapshot.register(bench_subparsers)
    rollback.register(bench_subparsers)
    clone.register(bench_subparsers)
    pause_resume.register(bench_subparsers)
    replay.register(bench_subparsers)
    replay_nolifecycle.register(bench_subparsers)
    replay_matrix.register(bench_subparsers)
    replay_mixgen.register(bench_subparsers)
    runner.register(bench_subparsers)
