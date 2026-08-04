from dataclasses import dataclass
from typing import Dict, List, Optional, Tuple

from e2b.sandbox.os_type import OsType


def _get_command_shell(os_type: Optional[OsType], cmd: str) -> Tuple[str, List[str]]:
    """
    Resolve the shell command and args used to run a user command for a given
    guest OS. Internal helper.
    """
    if os_type == "windows":
        return "powershell.exe", ["-NoLogo", "-NonInteractive", "-Command", cmd]
    if os_type == "android":
        return "/system/bin/sh", ["-c", cmd]

    return "/bin/bash", ["-l", "-c", cmd]


@dataclass
class ProcessInfo:
    """
    Information about a command, PTY session or start command running in the sandbox as process.
    """

    pid: int
    """
    Process ID.
    """

    tag: Optional[str]
    """
    Custom tag used for identifying special commands like start command in the custom template.
    """

    cmd: str
    """
    Command that was executed.
    """

    args: List[str]
    """
    Command arguments.
    """

    envs: Dict[str, str]
    """
    Environment variables used for the command.
    """

    cwd: Optional[str]
    """
    Executed command working directory.
    """
