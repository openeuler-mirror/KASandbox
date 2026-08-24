"""Upload one local file to an existing E2B sandbox."""

from __future__ import annotations

import argparse
from pathlib import Path

from .e2b_common import connect_sandbox, non_empty, print_json
from .command_entry import run_feature
from .e2b_config import add_config_arguments, require_api_key


def register_subcommand(subparsers) -> None:
    parser = subparsers.add_parser("upload-file", help="Upload a file to a sandbox")
    add_config_arguments(parser)
    parser.add_argument("--sandbox-id", required=True, type=non_empty)
    parser.add_argument("--local-path", required=True, type=Path)
    parser.add_argument("--remote-path", required=True, type=non_empty)
    parser.add_argument("--user", help="Sandbox user used by the Files API write operation")
    parser.set_defaults(handler=execute)


def execute(args: argparse.Namespace) -> int:
    require_api_key()
    local_path = args.local_path.expanduser().resolve()
    if not local_path.is_file():
        raise FileNotFoundError(f"Local file does not exist: {local_path}")

    sandbox = connect_sandbox(args.sandbox_id)
    options = {"user": args.user} if args.user else {}
    with local_path.open("rb") as local_file:
        result = sandbox.files.write(args.remote_path, local_file, **options)
    print_json(
        {
            "sandbox_id": args.sandbox_id,
            "local_path": local_path,
            "remote_path": args.remote_path,
            "user": args.user,
            "bytes": local_path.stat().st_size,
            "result": result,
        }
    )
    return 0


def main() -> int:
    return run_feature("upload-file", register_subcommand, __doc__ or "")


if __name__ == "__main__":
    raise SystemExit(main())
