"""List all templates visible to the configured E2B team."""

from __future__ import annotations

import argparse
import json
import urllib.request

from .e2b_common import collect_pages, positive_int, print_json
from .command_entry import run_feature
from .e2b_config import add_config_arguments, api_base_url, require_api_key


def register_subcommand(subparsers) -> None:
    parser = subparsers.add_parser("list-templates", help="List all templates")
    add_config_arguments(parser)
    parser.add_argument("--max-pages", type=positive_int, help="Used when the SDK returns a paginator")
    parser.set_defaults(handler=execute)


def _list_with_rest_api(api_key: str):
    request = urllib.request.Request(
        f"{api_base_url()}/templates",
        headers={"X-API-Key": api_key, "Accept": "application/json"},
        method="GET",
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def execute(args: argparse.Namespace) -> int:
    api_key = require_api_key()
    from e2b import Template

    list_method = getattr(Template, "list", None)
    if callable(list_method):
        templates = collect_pages(list_method(), args.max_pages)
        source = "sdk"
    else:
        templates = _list_with_rest_api(api_key)
        source = "rest-api"
    print_json({"count": len(templates), "source": source, "templates": templates})
    return 0


def main() -> int:
    return run_feature("list-templates", register_subcommand, __doc__ or "")


if __name__ == "__main__":
    raise SystemExit(main())
