"""Create one E2B sandbox and leave it running."""

from __future__ import annotations

import argparse

from .e2b_common import parse_json_object, positive_int, print_json, resource_name
from .command_entry import run_feature
from .e2b_config import add_config_arguments, require_api_key


def _secure_observable(sandbox) -> dict[str, object]:
    """Expose only secure-related field names and presence, never secret values."""
    fields: list[str] = []
    values: dict[str, object] = {}
    for name in ("secure", "access_token", "envd_access_token", "_envd_access_token", "sandbox_url"):
        try:
            value = getattr(sandbox, name)
        except AttributeError:
            continue
        fields.append(name)
        values[name] = value
    observable = any(value not in (None, "", False, [], {}) for value in values.values())
    return {"observable": observable, "fields": sorted(set(fields))}


def register_subcommand(subparsers) -> None:
    parser = subparsers.add_parser("create-sandbox", help="Create a sandbox")
    add_config_arguments(parser)
    parser.add_argument("--template", type=resource_name, help="Template name or ID; omit for the SDK default")
    parser.add_argument("--timeout", type=positive_int, help="Sandbox lifetime timeout in seconds")
    parser.add_argument("--metadata", help='Metadata JSON object, for example {"env":"test"}')
    parser.add_argument("--envs", help='Sandbox environment variables as a JSON object')
    parser.add_argument("--secure", action="store_true", help="Require secured sandbox access")
    parser.set_defaults(handler=execute)


def execute(args: argparse.Namespace) -> int:
    require_api_key()
    from e2b import Sandbox

    options = {}
    metadata = parse_json_object(args.metadata, "--metadata")
    envs = parse_json_object(args.envs, "--envs")
    if metadata is not None:
        options["metadata"] = metadata
    if envs is not None:
        options["envs"] = envs
    if args.timeout is not None:
        options["timeout"] = args.timeout
    if args.secure:
        options["secure"] = True

    sandbox = Sandbox.create(args.template, **options) if args.template else Sandbox.create(**options)
    result = {
        "sandbox_id": sandbox.sandbox_id,
        "template": args.template or "default",
        "status": "created",
    }
    if args.secure:
        result["secure_observable"] = _secure_observable(sandbox)
    print_json(result)
    return 0


def main() -> int:
    return run_feature("create-sandbox", register_subcommand, __doc__ or "")


if __name__ == "__main__":
    raise SystemExit(main())
