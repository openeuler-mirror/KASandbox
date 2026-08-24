"""Unified command-line entry point for the E2B environment tests."""

from __future__ import annotations

import argparse
import sys

from .e2b_config import configure_environment


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="E2B sandbox and template test tools",
    )
    subparsers = parser.add_subparsers(dest="action", required=True)

    from .create_sandbox import register_subcommand as register_create_sandbox
    from .create_template import register_subcommand as register_create_template
    from .download_file import register_subcommand as register_download_file
    from .e2e_test_runner import register_subcommand as register_e2e_tests
    from .list_sandboxes import register_subcommand as register_list_sandboxes
    from .list_templates import register_subcommand as register_list_templates
    from .run_command import register_subcommand as register_run_command
    from .upload_file import register_subcommand as register_upload_file

    register_create_sandbox(subparsers)
    register_create_template(subparsers)
    register_run_command(subparsers)
    register_upload_file(subparsers)
    register_download_file(subparsers)
    register_list_sandboxes(subparsers)
    register_list_templates(subparsers)
    register_e2e_tests(subparsers)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)

    try:
        configure_environment(args.env_file)
        return int(args.handler(args) or 0)
    except (FileNotFoundError, ValueError) as exc:
        parser.error(str(exc))
    except KeyboardInterrupt:
        print("Operation cancelled.", file=sys.stderr)
        return 130
    except Exception as exc:
        print(f"E2B operation failed: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
