"""Build an E2B template from a Dockerfile or base image."""

from __future__ import annotations

import argparse
import inspect
from pathlib import Path

from .e2b_common import non_empty, positive_int, print_json, resource_name
from .command_entry import run_feature
from .e2b_config import add_config_arguments, require_api_key


def register_subcommand(subparsers) -> None:
    parser = subparsers.add_parser("create-template", help="Build a template")
    add_config_arguments(parser)
    parser.add_argument("--name", required=True, type=resource_name, help="Template name")
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--dockerfile", type=Path, help="Path to a Dockerfile")
    source.add_argument("--dockerfile-content", help="Inline Dockerfile content")
    source.add_argument("--base-image", type=non_empty, help="Container base image")
    parser.add_argument("--cpu-count", type=positive_int, default=1)
    parser.add_argument("--memory-mb", type=positive_int, default=1024)
    parser.add_argument("--skip-cache", action="store_true")
    parser.set_defaults(handler=execute)


def _build_definition(template_class, args: argparse.Namespace):
    template = template_class()
    if args.dockerfile is not None:
        dockerfile = args.dockerfile.expanduser().resolve()
        if not dockerfile.is_file():
            raise FileNotFoundError(f"Dockerfile does not exist: {dockerfile}")
        return template.from_dockerfile(str(dockerfile))
    if args.dockerfile_content is not None:
        return template.from_dockerfile(args.dockerfile_content)
    from_image = getattr(template, "from_image", None)
    if callable(from_image):
        return from_image(args.base_image)
    return template.from_dockerfile(f"FROM {args.base_image}")


def _supported_kwargs(callable_object, values: dict) -> dict:
    try:
        parameters = inspect.signature(callable_object).parameters
    except (TypeError, ValueError):
        return values
    accepts_kwargs = any(
        parameter.kind == inspect.Parameter.VAR_KEYWORD
        for parameter in parameters.values()
    )
    return values if accepts_kwargs else {key: value for key, value in values.items() if key in parameters}


def execute(args: argparse.Namespace) -> int:
    require_api_key()
    from e2b import Template, default_build_logger

    definition = _build_definition(Template, args)
    build_options = _supported_kwargs(
        Template.build,
        {
            "cpu_count": args.cpu_count,
            "memory_mb": args.memory_mb,
            "skip_cache": args.skip_cache,
            "on_build_logs": default_build_logger(),
        },
    )
    parameters = inspect.signature(Template.build).parameters
    if "alias" in parameters and "name" not in parameters:
        build_info = Template.build(definition, alias=args.name, **build_options)
    else:
        build_info = Template.build(definition, args.name, **build_options)
    print_json(build_info)
    return 0


def main() -> int:
    return run_feature("create-template", register_subcommand, __doc__ or "")


if __name__ == "__main__":
    raise SystemExit(main())
