"""bench clean-host: host-level cleanup of leaked v35 network resources between full runs.

v35 架构的 orchestrator 为每个沙箱预建 netns（ns-*）+ veth（veth-*）+ iptables 规则，
正常 kill 后槽位进入复用池（设计保留，上限 1000），但创建失败/超时回收的沙箱会泄漏
这些资源。本命令在两次 full 压测之间做宿主级清理。池化槽位与被埋泄漏无法按名字区分，
因此一律全清。

重要：orchestrator 的网络池是进程内状态（另有限时归属标记 /run/e2b-netslots），
清空 netns 后运行中的 orchestrator 不会感知，会继续把已删除的槽位分发给新沙箱，
导致 fc-netns-exec open /run/netns/ns-N 失败、连锁创建失败。因此执行本命令后
**必须重启 template-manager**（由它在启动时回收无主槽位并按需重新 mint）。
"""

from __future__ import annotations

import subprocess
import sys

from ..e2b_common import print_json
from ..e2b_config import require_api_key
from .client import BenchClient
from .common import (
    MASQ_RULE_PATTERN,
    _host_veth_count,
    _iptables_rule_count,
    _netns_count,
    add_common_arguments,
    guarded,
)

NETNS_PREFIX = "ns-"
VETH_PREFIX = "veth-"


def register(subparsers) -> None:
    parser = subparsers.add_parser(
        "clean-host",
        help="清理宿主网络残留（ns-* netns / veth-* / 引用 veth- 的 iptables 规则）；两次 full 之间使用",
    )
    parser.add_argument("--dry-run", action="store_true", help="只打印将删除的数量，不执行")
    add_common_arguments(parser)
    parser.set_defaults(handler=guarded(execute))


def _log(message: str) -> None:
    print(f"[bench clean-host] {message}", file=sys.stderr, flush=True)


def _netns_names() -> list[str]:
    try:
        import os

        return sorted(
            entry for entry in os.listdir("/var/run/netns")
            if entry.startswith(NETNS_PREFIX)
        )
    except OSError:
        return []


def _veth_names() -> list[str]:
    try:
        import os

        return sorted(
            entry for entry in os.listdir("/sys/class/net")
            if entry.startswith(VETH_PREFIX)
        )
    except OSError:
        return []


def _iptables_save() -> str:
    completed = subprocess.run(
        ["iptables-save"], capture_output=True, text=True, timeout=60, check=False
    )
    if completed.returncode != 0:
        raise RuntimeError(f"iptables-save 失败：{completed.stderr[-300:]}")
    return completed.stdout


def _filter_veth_rules(source: str) -> tuple[str, int]:
    """删除引用 veth- 接口的规则（对齐 run_v34_compare.sh 的 remove_veth_iptables_rules）。"""
    import shlex

    kept: list[str] = []
    removed = 0
    table = ""
    for line in source.splitlines():
        if line.startswith("*"):
            table = line[1:]
        elif line == "COMMIT":
            table = ""
        if table and line.startswith("-A "):
            tokens = shlex.split(line)
            references_veth = any(
                tokens[index] in {"-i", "-o", "--in-interface", "--out-interface"}
                and index + 1 < len(tokens)
                and tokens[index + 1].startswith(VETH_PREFIX)
                for index in range(len(tokens))
            )
            if references_veth:
                removed += 1
                continue
        kept.append(line)
    return "\n".join(kept) + "\n", removed


def _delete_netns(names: list[str], *, workers: int = 16) -> int:
    """ip netns del 一次只能删一个，用 xargs 并行。"""
    if not names:
        return 0
    completed = subprocess.run(
        ["xargs", "-n1", f"-P{workers}", "ip", "netns", "del"],
        input="\n".join(names),
        capture_output=True, text=True, timeout=600, check=False,
    )
    failed = completed.stdout.count("Cannot") + completed.stderr.count("Cannot")
    return len(names) - failed


def _delete_veth(names: list[str], *, workers: int = 16) -> int:
    if not names:
        return 0
    completed = subprocess.run(
        ["xargs", "-n1", f"-P{workers}", "ip", "link", "del"],
        input="\n".join(names),
        capture_output=True, text=True, timeout=600, check=False,
    )
    failed = completed.stderr.count("Cannot")
    return len(names) - failed


def _filter_all_masq_rules(source: str) -> tuple[str, int]:
    """深度清理场景（连网络池一起清）：删除全部 10.11.* MASQUERADE POSTROUTING 规则。"""
    kept: list[str] = []
    removed = 0
    for line in source.splitlines():
        if MASQ_RULE_PATTERN.match(line):
            removed += 1
            continue
        kept.append(line)
    return "\n".join(kept) + "\n", removed


def _restore_iptables(filtered: str) -> None:
    """xtables 全局锁可能与其他清理并发，等待锁并重试（对齐参考实现的 3 次重试）。"""
    for attempt in range(1, 4):
        completed = subprocess.run(
            ["iptables-restore", "--wait=60"],
            input=filtered, capture_output=True, text=True, timeout=180, check=False,
        )
        if completed.returncode == 0:
            return
        _log(f"iptables-restore 第 {attempt}/3 次失败：{completed.stderr[-200:]}")
    raise RuntimeError("iptables-restore 连续 3 次失败")


def _preflight_check(client: BenchClient) -> str | None:
    """有沙箱或 firecracker/jailer 进程在跑时拒绝执行；返回拒绝原因。"""
    items = client.list_sandbox_items()
    if items:
        states = {}
        for item in items:
            state = str(item.get("state") or "unknown")
            states[state] = states.get(state, 0) + 1
        return f"仍存在 {len(items)} 个沙箱（{states}），请先用 bench kill-all 清理"
    from .common import _pgrep_count

    firecracker = _pgrep_count("firecracker")
    jailer = _pgrep_count("jailer")
    if firecracker or jailer:
        return f"仍存在 firecracker={firecracker} jailer={jailer} 进程，请先清理沙箱"
    return None


def execute(args: argparse.Namespace) -> int:
    require_api_key()
    client = BenchClient(timeout=60)
    refusal = _preflight_check(client)
    if refusal:
        print_json({"status": "refused", "reason": refusal})
        return 1

    netns_names = _netns_names()
    veth_names = _veth_names()
    iptables_text = _iptables_save()
    masq_total = _filter_all_masq_rules(iptables_text)[1]
    before = {
        "netns": len(netns_names),
        "netns_total": _netns_count(),
        "veth": len(veth_names),
        "iptables_rules": _iptables_rule_count(),
        "iptables_masq_rules": masq_total,
    }
    _log(f"清理前：{before}")
    if args.dry_run:
        print_json({"status": "dry-run", "before": before, "would_delete": before})
        return 0

    deleted_netns = _delete_netns(netns_names)
    deleted_veth = _delete_veth(veth_names)
    filtered, removed_rules = _filter_veth_rules(iptables_text)
    filtered, removed_masq = _filter_all_masq_rules(filtered)
    _restore_iptables(filtered)

    after = {
        "netns": _netns_count(),
        "veth": _host_veth_count(),
        "iptables_rules": _iptables_rule_count(),
    }
    _log(f"清理后：{after}")
    _log(
        "警告：网络池预建槽位已一并清空。运行中的 orchestrator 不会感知槽位被删，"
        "继续分发会导致 fc-netns-exec 报 open /run/netns/ns-N 不存在而创建失败；"
        "请在下次压测前务必重启 template-manager（本命令不做任何 nomad 操作，请手动执行），"
        "由它启动时回收无主槽位并重新 mint"
    )
    print_json({
        "status": "ok",
        "before": before,
        "after": after,
        "deleted": {
            "netns": deleted_netns,
            "veth": deleted_veth,
            "iptables_rules": removed_rules,
            "iptables_masq_rules": removed_masq,
        },
    })
    return 0
