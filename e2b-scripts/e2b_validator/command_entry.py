"""Shared standalone entry point for individual feature modules."""

from __future__ import annotations

import argparse
import sys
from collections.abc import Callable

from .e2b_config import configure_environment


def run_feature(
    action: str,
    register_subcommand: Callable,
    description: str,
    argv: list[str] | None = None,
) -> int:
    parser = argparse.ArgumentParser(description=description)
    subparsers = parser.add_subparsers(dest="action", required=True)
    register_subcommand(subparsers)
    args = parser.parse_args([action, *(sys.argv[1:] if argv is None else argv)])
    configure_environment(args.env_file)
    return int(args.handler(args) or 0)
