from typing import Optional


class CheckpointInfo:
    def __init__(
        self,
        checkpoint_id: str,
        name: Optional[str] = None,
        created_at: Optional[int] = None,
    ):
        self.checkpoint_id = checkpoint_id
        self.name = name
        self.created_at = created_at

    def __repr__(self) -> str:
        return f"CheckpointInfo(id={self.checkpoint_id}, name={self.name})"