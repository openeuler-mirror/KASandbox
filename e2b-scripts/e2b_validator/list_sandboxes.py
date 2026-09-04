"""List all sandboxes visible to the configured E2B team."""

from __future__ import annotations

import argparse

from .e2b_common import collect_pages, positive_int, print_json
from .command_entry import run_feature
from .e2b_config import add_config_arguments, require_api_key


def register_subcommand(subparsers) -> None:
    parser = subparsers.add_parser("list-sandboxes", help="List all sandboxes")
    add_config_arguments(parser)
    parser.add_argument("--max-pages", type=positive_int, help="Optional pagination safety limit")
    parser.set_defaults(handler=execute)


def execute(args: argparse.Namespace) -> int:
    require_api_key()
    from e2b import Sandbox

    sandboxes = collect_pages(Sandbox.list(), args.max_pages)
    print_json({"count": len(sandboxes), "sandboxes": sandboxes})
    return 0


def main() -> int:
    return run_feature("list-sandboxes", register_subcommand, __doc__ or "")


if __name__ == "__main__":
    raise SystemExit(main())
