from typing import Literal, cast

from e2b.api.client.types import Unset
from e2b.exceptions import SandboxException

OsType = Literal["linux", "windows", "android"]
"""
Guest operating system of a sandbox.

The value is authoritatively determined by the backend (based on the template /
raw image) and read from the REST response. When absent, the SDK defaults to
``"linux"`` for backward compatibility.
"""


def normalize_os_type(value: object) -> OsType:
    """Normalize a backend OS value without hiding unsupported values."""
    if value is None or isinstance(value, Unset):
        return "linux"

    normalized = str(value).strip().lower()
    if normalized in ("linux", "windows", "android"):
        return cast(OsType, normalized)

    raise SandboxException(f"Unsupported sandbox osType returned by backend: {value!r}")
