"""Download one file from an existing E2B sandbox."""

from __future__ import annotations

import argparse
import inspect
from pathlib import Path

from .e2b_common import connect_sandbox, non_empty, print_json
from .command_entry import run_feature
from .e2b_config import add_config_arguments, require_api_key


def register_subcommand(subparsers) -> None:
    parser = subparsers.add_parser("download-file", help="Download a file from a sandbox")
    add_config_arguments(parser)
    parser.add_argument("--sandbox-id", required=True, type=non_empty)
    parser.add_argument("--remote-path", required=True, type=non_empty)
    parser.add_argument("--local-path", required=True, type=Path)
    parser.add_argument("--overwrite", action="store_true")
    parser.set_defaults(handler=execute)


def execute(args: argparse.Namespace) -> int:
    require_api_key()
    local_path = args.local_path.expanduser().resolve()
    if local_path.exists() and not args.overwrite:
        raise ValueError(f"Local path already exists; use --overwrite to replace it: {local_path}")
    local_path.parent.mkdir(parents=True, exist_ok=True)

    sandbox = connect_sandbox(args.sandbox_id)
    read_options = {}
    try:
        if "format" in inspect.signature(sandbox.files.read).parameters:
            read_options["format"] = "bytes"
    except (TypeError, ValueError):
        pass
    content = sandbox.files.read(args.remote_path, **read_options)
    if isinstance(content, (bytes, bytearray, memoryview)):
        local_path.write_bytes(bytes(content))
    else:
        local_path.write_text(str(content), encoding="utf-8")

    print_json(
        {
            "sandbox_id": args.sandbox_id,
            "remote_path": args.remote_path,
            "local_path": local_path,
            "bytes": local_path.stat().st_size,
        }
    )
    return 0


def main() -> int:
    return run_feature("download-file", register_subcommand, __doc__ or "")


if __name__ == "__main__":
    raise SystemExit(main())
