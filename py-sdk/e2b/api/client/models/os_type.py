from enum import Enum


class OsType(str, Enum):
    LINUX = "linux"
    WINDOWS = "windows"
    ANDROID = "android"

    def __str__(self) -> str:
        return str(self.value)
