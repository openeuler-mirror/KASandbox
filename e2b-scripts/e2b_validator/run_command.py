"""Run one foreground command in an existing E2B sandbox."""

from __future__ import annotations

import argparse

from .e2b_common import connect_sandbox, non_empty, parse_json_object, positive_int, print_json
from .command_entry import run_feature
from .e2b_config import add_config_arguments, require_api_key


def _command_exit_exception_type():
    from e2b.sandbox.commands.command_handle import CommandExitException

    return CommandExitException


def register_subcommand(subparsers) -> None:
    parser = subparsers.add_parser("run-command", help="Run a command in a sandbox")
    add_config_arguments(parser)
    parser.add_argument("--sandbox-id", required=True, type=non_empty)
    parser.add_argument("--command", required=True, type=non_empty)
    parser.add_argument("--cwd")
    parser.add_argument("--user")
    parser.add_argument("--envs", help="Command environment variables as a JSON object")
    parser.add_argument("--timeout", type=positive_int, help="Command timeout in seconds")
    parser.set_defaults(handler=execute)


def execute(args: argparse.Namespace) -> int:
    require_api_key()
    sandbox = connect_sandbox(args.sandbox_id)
    options = {}
    envs = parse_json_object(args.envs, "--envs")
    if envs is not None:
        options["envs"] = envs
    for key in ("cwd", "user", "timeout"):
        value = getattr(args, key)
        if value is not None:
            options[key] = value
    try:
        result = sandbox.commands.run(args.command, **options)
    except _command_exit_exception_type() as exc:
        print_json(exc)
        return int(exc.exit_code)
    print_json(result)
    exit_code = getattr(result, "exit_code", 0)
    return int(exit_code or 0)


def main() -> int:
    return run_feature("run-command", register_subcommand, __doc__ or "")


if __name__ == "__main__":
    raise SystemExit(main())
