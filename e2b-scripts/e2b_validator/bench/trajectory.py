"""bench replay 的轨迹加载与合成。

轨迹文件（.replay.json）记录 agent 的操作序列：
{"trajectory": [{"action": "shell命令", "delay_time": 秒, ...}, ...]}，
delay_time 模拟 LLM 推理间隔（执行下一条命令前的暂停时长）。
加载语义对齐 replay-aenv agent/replay_agent_no_lifecycle.py。
"""

from __future__ import annotations

import json
import random
import shlex
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


# --- mix 多模板混合回放（移植自 replay-aenv agent/replay_agent.py，语义 1:1） ---


@dataclass(frozen=True)
class WorkloadSpec:
    """mix 配置中的单个负载：一组轨迹 + 模板 + 总回放次数。"""

    name: str
    template: str
    trajectory_dir: Path
    vm_count: int


@dataclass(frozen=True)
class MixTask:
    """调度产物：一次轨迹回放任务（模板/轨迹/步序列已解析）。"""

    workload: str
    template: str
    trajectory: str
    steps: tuple[ReplayStep, ...]


def slugify(value: str, limit: int = 72) -> str:
    """workload 名规整：小写、非字母数字折叠为 '-'，对齐 replay-aenv。"""
    import re

    slug = re.sub(r"[^a-z0-9]+", "-", value.strip().lower()).strip("-")
    return (slug or "workload")[:limit].strip("-") or "workload"


def load_mix_config(path: str | Path) -> tuple[int | None, list[WorkloadSpec]]:
    """加载 mix 配置：{"concurrency": int?, "workloads": [{name?, template, trajectory_dir, vm_count}]}。

    校验语义对齐 replay-aenv load_mix_config：name 缺省取 template、slug 后必须唯一；
    vm_count 是该负载的总回放次数（不是并发数）；trajectory_dir 相对路径以配置文件所在目录为基准。
    返回 (concurrency, workloads)。
    """
    config_path = Path(path).expanduser().resolve()
    if not config_path.is_file():
        raise FileNotFoundError(f"mix 配置不存在：{config_path}")
    try:
        raw: Any = json.loads(config_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise ValueError(f"mix 配置 JSON 解析失败：{config_path}: {exc}") from exc
    if not isinstance(raw, dict):
        raise ValueError("mix 配置根节点必须是 JSON object")

    concurrency = raw.get("concurrency")
    if concurrency is not None:
        if isinstance(concurrency, bool) or not isinstance(concurrency, int) or concurrency < 1:
            raise ValueError("concurrency 必须是正整数")

    raw_workloads = raw.get("workloads")
    if not isinstance(raw_workloads, list) or not raw_workloads:
        raise ValueError("workloads 必须是非空数组")

    workloads: list[WorkloadSpec] = []
    names: set[str] = set()
    for index, raw_workload in enumerate(raw_workloads):
        field = f"workloads[{index}]"
        if not isinstance(raw_workload, dict):
            raise ValueError(f"{field} 必须是 JSON object")
        template = raw_workload.get("template")
        if not isinstance(template, str) or not template.strip():
            raise ValueError(f"{field}.template 必须是非空字符串")
        template = template.strip()
        raw_name = raw_workload.get("name", template)
        if not isinstance(raw_name, str) or not raw_name.strip():
            raise ValueError(f"{field}.name 必须是非空字符串")
        name = slugify(raw_name)
        if name in names:
            raise ValueError(f"workload 名称重复：{name}")
        names.add(name)
        vm_count = raw_workload.get("vm_count")
        if isinstance(vm_count, bool) or not isinstance(vm_count, int) or vm_count < 1:
            raise ValueError(f"{field}.vm_count 必须是正整数")
        raw_dir = raw_workload.get("trajectory_dir")
        if not isinstance(raw_dir, str) or not raw_dir.strip():
            raise ValueError(f"{field}.trajectory_dir 必须是非空字符串")
        trajectory_dir = Path(raw_dir.strip()).expanduser()
        if not trajectory_dir.is_absolute():
            trajectory_dir = config_path.parent / trajectory_dir
        trajectory_dir = trajectory_dir.resolve()
        if not find_trajectories(trajectory_dir):
            raise ValueError(f"{field}.trajectory_dir 中没有 .json/.traj 文件：{trajectory_dir}")
        workloads.append(
            WorkloadSpec(
                name=name,
                template=template,
                trajectory_dir=trajectory_dir,
                vm_count=vm_count,
            )
        )
    return concurrency, workloads


def build_mix_schedule(workloads: list[WorkloadSpec]) -> list[MixTask]:
    """平滑加权轮询（SWRR）交错各负载，单负载内轨迹循环复用。

    对齐 replay-aenv build_mixed_schedule：按 vm_count 加权交错，保证任意时刻
    各负载的已发射比例贴近其 vm_count 占比。
    """
    remaining = {w.name: w.vm_count for w in workloads}
    current = {w.name: 0 for w in workloads}
    emitted = {w.name: 0 for w in workloads}
    by_name = {w.name: w for w in workloads}
    total = sum(remaining.values())
    steps_cache: dict[Path, list[tuple[str, list[ReplayStep]]]] = {}
    schedule: list[MixTask] = []
    while len(schedule) < total:
        active = [w for w in workloads if remaining[w.name]]
        active_weight = sum(w.vm_count for w in active)
        for w in active:
            current[w.name] += w.vm_count
        selected = max(active, key=lambda w: current[w.name])
        current[selected.name] -= active_weight

        if selected.trajectory_dir not in steps_cache:
            steps_cache[selected.trajectory_dir] = [
                (path.name, load_trajectory(path))
                for path in find_trajectories(selected.trajectory_dir)
            ]
        pool = steps_cache[selected.trajectory_dir]
        name, steps = pool[emitted[selected.name] % len(pool)]
        schedule.append(
            MixTask(
                workload=selected.name,
                template=by_name[selected.name].template,
                trajectory=name,
                steps=tuple(steps),
            )
        )
        emitted[selected.name] += 1
        remaining[selected.name] -= 1
    return schedule


# --- action 包装与归一化（移植自 replay-aenv agent/replay_agent.py，语义 1:1） ---


def _remove_one_boundary_newline(value: str) -> str:
    """去掉工具参数首尾各一个展示用换行。"""
    if value.startswith("\r\n"):
        value = value[2:]
    elif value.startswith("\n"):
        value = value[1:]
    if value.endswith("\r\n"):
        value = value[:-2]
    elif value.endswith("\n"):
        value = value[:-1]
    return value


def normalize_str_replace_editor_action(action: str) -> str:
    """对齐原始 SWE 结构化工具对多行编辑的处理。

    轨迹里 str_replace_editor 的 old_str/new_str 在渲染成 CLI 时首尾各多一个
    展示换行，原始结构化调用会先剥掉再匹配；直接按字面执行 CLI 会匹配失败。
    只对 str_replace 子命令做边界归一化，其余 action 原样返回。
    """
    try:
        arguments = shlex.split(action)
    except ValueError:
        return action
    if (
        len(arguments) < 3
        or arguments[0] != "str_replace_editor"
        or arguments[1] != "str_replace"
    ):
        return action

    flag_indexes: dict[str, int] = {}
    for flag in ("--old_str", "--new_str"):
        try:
            index = arguments.index(flag)
        except ValueError:
            return action
        if index + 1 >= len(arguments):
            return action
        flag_indexes[flag] = index + 1

    normalized_old_str = _remove_one_boundary_newline(arguments[flag_indexes["--old_str"]])
    if not normalized_old_str:
        return action
    arguments[flag_indexes["--old_str"]] = normalized_old_str
    arguments[flag_indexes["--new_str"]] = _remove_one_boundary_newline(
        arguments[flag_indexes["--new_str"]]
    )
    return shlex.join(arguments)


def wrap_action(action: str, workdir: str) -> str:
    """把轨迹 action 包装成 bash -lc 调用：对齐 SWE bash 工具行为（末段管道状态码、
    关闭分页器、切到 workdir）。str_replace_editor 先做边界归一化。"""
    action = normalize_str_replace_editor_action(action)
    commands = [
        "set +o pipefail",
        "export PAGER=cat",
        "export MANPAGER=cat",
        "export GIT_PAGER=cat",
        "export LESS=-FRX",
        f"cd {shlex.quote(workdir)}",
        action,
    ]
    return f"bash -lc {shlex.quote('; '.join(commands))}"


# --- writeTxt 写入模式改写（移植自 replay-aenv，测快照/pause 增长） ---

WRITE_MODES = ("buffered", "tmpfs", "directio")


def rewrite_write_txt_action(action: str, write_mode: str, workdir: str) -> str:
    """把 `writeTxt N`（写 N 个 1 MiB 高熵文件）按批次写入模式改写。

    buffered：不改写（走工具自身的缓冲写）；tmpfs：改写为 dd 写 /dev/shm；
    directio：改写为 dd oflag=direct 直写 workdir。非 writeTxt 的 action 原样返回。
    """
    if write_mode not in WRITE_MODES:
        raise ValueError(f"unsupported write mode: {write_mode}")
    try:
        arguments = shlex.split(action)
    except ValueError:
        return action
    if len(arguments) != 2 or arguments[0] != "writeTxt":
        return action
    try:
        size_mib = int(arguments[1])
    except ValueError:
        return action
    if size_mib < 0:
        return action
    if write_mode == "buffered":
        return action

    if write_mode == "tmpfs":
        target = "/dev/shm/pause-growth-tmpfs.bin"
        return (
            'test "$(findmnt -n -o FSTYPE -T /dev/shm)" = tmpfs && '
            f"dd if=/dev/urandom of={shlex.quote(target)} bs=1048576 "
            f"count={size_mib} status=none"
        )
    target = f"{workdir.rstrip('/')}/pause-growth-directio.bin"
    return (
        f"fstype=$(findmnt -n -o FSTYPE -T {shlex.quote(workdir)}) && "
        'case "$fstype" in tmpfs|ramfs) exit 1;; esac && '
        f"dd if=/dev/urandom of={shlex.quote(target)} bs=1048576 "
        f"count={size_mib} oflag=direct conv=fsync status=none"
    )
