"""One-command setup and launcher for the E2B test scripts."""

from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path

from e2b_validator.bootstrap import check_python_version, compile_sources, ensure_dependencies
from e2b_validator.e2b_config import DEFAULT_ENV_FILE
from e2b_validator.self_hosted_runtime import (
    DEFAULT_CLIENT_CONFIG,
    DEFAULT_INFRA_ENV,
    load_env_runtime_settings,
    load_runtime_settings,
)


READINESS_COMMANDS = (["list-sandboxes"], ["list-templates"])


def print_next_steps() -> None:
    print("E2B_STARTUP_OK")
    print("Use this launcher for every self-hosted operation:")
    print("  bash start.sh list-sandboxes")
    print("  bash start.sh create-sandbox --template openclaw --timeout 300")
    print("Use start.sh as the only public command entry point.")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=(
            "Install missing dependencies, configure the E2B runtime, and run a command. "
            "With no command, compile the source and run read-only readiness checks."
        )
    )
    parser.add_argument("--skip-install", action="store_true", help="Do not install a missing E2B SDK")
    parser.add_argument("--skip-checks", action="store_true", help="Skip source compilation in readiness mode")
    parser.add_argument("--env-file", type=Path, help="Use a normal env file instead of deployment auto-discovery")
    parser.add_argument("--infra-env", type=Path, default=DEFAULT_INFRA_ENV, help=argparse.SUPPRESS)
    parser.add_argument("--client-config", type=Path, default=DEFAULT_CLIENT_CONFIG, help=argparse.SUPPRESS)
    parser.add_argument("command", nargs=argparse.REMAINDER, help="E2B subcommand and arguments")
    return parser


def _configure_runtime(args: argparse.Namespace, command: list[str]) -> list[str]:
    if args.env_file is not None:
        env_file = args.env_file.expanduser().resolve()
        settings = load_env_runtime_settings(command, env_file)
        for key, value in settings.environment.items():
            os.environ.setdefault(key, value)
        return settings.command_args
    if DEFAULT_ENV_FILE.is_file():
        settings = load_env_runtime_settings(command, DEFAULT_ENV_FILE)
        for key, value in settings.environment.items():
            os.environ.setdefault(key, value)
        return settings.command_args
    settings = load_runtime_settings(
        command,
        infra_env=args.infra_env.expanduser().resolve(),
        client_config=args.client_config.expanduser().resolve(),
    )
    for key, value in settings.environment.items():
        os.environ.setdefault(key, value)
    return settings.command_args


def _run_command(args: argparse.Namespace, command: list[str]) -> int:
    from e2b_validator.build_prod import main as run_build_prod

    return run_build_prod(_configure_runtime(args, command))


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        check_python_version()
        if args.command and any(value in {"-h", "--help"} for value in args.command):
            from e2b_validator.build_prod import main as run_build_prod

            return run_build_prod(args.command)
        ensure_dependencies(skip_install=args.skip_install)
        if args.command:
            return _run_command(args, args.command)

        if not args.skip_checks:
            compile_sources()
        for command in READINESS_COMMANDS:
            result = _run_command(args, list(command))
            if result != 0:
                return result
        print_next_steps()
        return 0
    except (FileNotFoundError, RuntimeError, ValueError) as exc:
        print(f"Startup failed: {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("Startup cancelled.", file=sys.stderr)
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
