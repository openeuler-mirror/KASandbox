"""bench replay 的轨迹加载与合成。

轨迹文件（.replay.json）记录 agent 的操作序列：
{"trajectory": [{"action": "shell命令", "delay_time": 秒, ...}, ...]}，
delay_time 模拟 LLM 推理间隔（执行下一条命令前的暂停时长）。
加载语义对齐 replay-aenv agent/replay_agent_no_lifecycle.py。
"""

from __future__ import annotations

import json
import random
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any

# 合成轨迹 delay_time 的采样来源：replay-aenv 真实轨迹分布；目录缺失时兜底均匀分布
DELAY_SAMPLE_DIR = Path("/opt/fqy/replay-aenv-main/delay_time_trajectories")
FALLBACK_DELAY_MIN_SEC = 0.5
FALLBACK_DELAY_MAX_SEC = 8.0

# 合成轨迹用通用只读命令轮换（不依赖特定模板内容）
SYNTHETIC_COMMANDS = (
    'find / -maxdepth 3 -type f -name "*.conf" 2>/dev/null | head -5',
    "ls -la /usr/bin | wc -l",
    "cat /etc/os-release",
    "echo bench-replay-probe",
)


@dataclass(frozen=True)
class ReplayStep:
    action: str
    delay_time_sec: float


def load_trajectory(path: str | Path) -> list[ReplayStep]:
    """读取一个 .replay.json，提取每步的 action 与 delay_time_sec。

    缺省/非法 delay_time（非数值、负数、bool）→ 0.0 并告警；无 action 的步骤跳过；
    全部步骤不可执行时抛 ValueError。
    """
    trajectory_path = Path(path).expanduser().resolve()
    if not trajectory_path.is_file():
        raise FileNotFoundError(f"轨迹文件不存在：{trajectory_path}")
    try:
        data: Any = json.loads(trajectory_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise ValueError(f"轨迹 JSON 解析失败：{trajectory_path}: {exc}") from exc
    if not isinstance(data, dict):
        raise ValueError(f"轨迹根节点必须是 JSON object：{trajectory_path}")
    raw_steps = data.get("trajectory")
    if not isinstance(raw_steps, list):
        raise ValueError(f"轨迹 trajectory 字段必须是数组：{trajectory_path}")

    steps: list[ReplayStep] = []
    for raw_step in raw_steps:
        if not isinstance(raw_step, dict):
            continue
        action = raw_step.get("action")
        if not isinstance(action, str) or not action.strip():
            continue
        raw_delay = raw_step.get("delay_time", 0.0)
        if isinstance(raw_delay, bool) or not isinstance(raw_delay, (int, float)) or raw_delay < 0:
            print(
                f"[replay] 非法 delay_time={raw_delay!r}（action={action.strip()!r}，"
                f"{trajectory_path.name}），按 0 秒处理",
                file=sys.stderr,
            )
            delay_time_sec = 0.0
        else:
            delay_time_sec = float(raw_delay)
        steps.append(ReplayStep(action=action.strip(), delay_time_sec=delay_time_sec))
    if not steps:
        raise ValueError(f"轨迹中无可执行 action：{trajectory_path}")
    return steps


def find_trajectories(directory: str | Path) -> list[Path]:
    """目录第一层的 .json/.traj 轨迹文件（按文件名排序）。"""
    directory = Path(directory).expanduser().resolve()
    if not directory.is_dir():
        raise NotADirectoryError(f"轨迹目录不存在：{directory}")
    return sorted(
        path
        for path in directory.iterdir()
        if path.is_file() and (path.name.endswith(".json") or path.name.endswith(".traj"))
    )


def collect_delay_samples(directory: str | Path = DELAY_SAMPLE_DIR) -> list[float]:
    """统计目录下全部真实轨迹的 delay_time 分布样本；目录缺失/无样本返回空列表。"""
    directory = Path(directory).expanduser()
    if not directory.is_dir():
        return []
    samples: list[float] = []
    for path in find_trajectories(directory):
        try:
            samples.extend(step.delay_time_sec for step in load_trajectory(path))
        except ValueError as exc:
            print(f"[replay] 跳过不可用轨迹 {path.name}：{exc}", file=sys.stderr)
    return samples


def generate_synthetic_trajectories(
    count: int,
    steps: int,
    rng_seed: int | None = None,
    *,
    delay_sample_dir: str | Path = DELAY_SAMPLE_DIR,
) -> list[list[ReplayStep]]:
    """合成 count 条各 steps 步的轨迹。

    命令从 SYNTHETIC_COMMANDS 轮换；delay_time 从真实轨迹统计分布随机采样，
    样本为空时兜底为 0.5~8s 均匀分布（均值约 4s，与真实轨迹量级一致）。
    """
    if count < 1 or steps < 1:
        raise ValueError("合成轨迹的 count 与 steps 必须为正整数")
    samples = collect_delay_samples(delay_sample_dir)
    rng = random.Random(rng_seed)
    trajectories: list[list[ReplayStep]] = []
    for _ in range(count):
        trajectory: list[ReplayStep] = []
        for index in range(steps):
            if samples:
                delay = rng.choice(samples)
            else:
                delay = rng.uniform(FALLBACK_DELAY_MIN_SEC, FALLBACK_DELAY_MAX_SEC)
            trajectory.append(
                ReplayStep(
                    action=SYNTHETIC_COMMANDS[index % len(SYNTHETIC_COMMANDS)],
                    delay_time_sec=round(delay, 3),
                )
            )
        trajectories.append(trajectory)
    return trajectories
