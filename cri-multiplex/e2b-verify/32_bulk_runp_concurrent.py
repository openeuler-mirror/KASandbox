#!/usr/bin/env python3
"""
32_bulk_runp_concurrent.py — 并发启动 N 个 direct sandbox（纯创建，不采集数据）

逻辑与 28_bulk_concurrency_standalone.sh 的创建部分一致：
串行预生成 Pod JSON（不计时）→ 并发栅栏对齐 → 所有沙箱同一时刻发起
RunPodSandbox。不含任何 CNI/PerfTrace 日志采集与统计。

依赖：crictl、目标机上 cri-multiplex 已在运行（CNI 模式，建议带
      -hide-sandbox-label=flux-sandbox.io/direct=true 防 kubelet GC）。

用法：
  sudo python3 32_bulk_runp_concurrent.py 100                 # 并发 100 个
  sudo python3 32_bulk_runp_concurrent.py 100 --cleanup       # 创建完成后立即清理

  # 多节点同时开火（要求节点间时钟同步）：先在其中一台取未来时刻
  #   python3 -c 'import time; print(int(time.time()*1000)+45000)'
  # 然后各节点同一时刻启动：
  #   sudo python3 32_bulk_runp_concurrent.py 200 \
  #       --socket /run/cri-multiplex.sock --start-at-ms <上面的值>

  # 并发异常时排查：换进程模式（与 28 号 bash 脚本 fork 模型一致，排除 GIL 影响）
  # 并打印每个沙箱的栅栏偏差/每次尝试耗时，定位耗时在客户端还是服务端：
  #   sudo python3 32_bulk_runp_concurrent.py 2 --mode process --debug

参数：
  COUNT                并发沙箱数（位置参数，必填）
  --pod-json PATH      基础 Pod JSON 模板（默认 /tmp/e2b-pod.json，含 e2b.dev/*
                       注解，可从已跑过 00/01 用例的机器复制）
  --socket PATH        cri-multiplex socket（默认 /tmp/cri-multiplex.sock；
                       8 节点环境为 /run/cri-multiplex.sock）
  --prefix STR         本批次沙箱名前缀（默认 c<COUNT>-<时分秒>）
  --start-at-ms MS     并发栅栏绝对时刻（epoch 毫秒）。设置后所有沙箱等到
                       该时刻同时发起；不设置则 worker 全部就绪后 1.5s 开火
  --mode thread|process  worker 并发模型（默认 thread；process 与 28 号
                       bash 脚本的 fork 模型一致，用于排除 GIL 干扰）
  --timeout DUR        crictl runp 超时（默认 900s）。不重试：单次失败即记为该
                       沙箱的最终结果，保证端到端耗时是"一次并发调用"的真实测量
  --debug              结束后打印每个沙箱的明细：栅栏偏差/每次 attempt 耗时/
                       失败输出。用于判断慢在客户端发起还是服务端处理
  --cleanup            创建完成后立即 rmp -f 清理全部沙箱（默认保留运行）
  --ids-file PATH      pod id 清单输出路径（默认 /tmp/<prefix>-pod-ids.txt，
                       保留模式下可用于事后清理: xargs -a <file> crictl rmp -f）
"""

import argparse
import glob
import json
import multiprocessing
import os
import random
import re
import shutil
import subprocess
import sys
import threading
import time
from datetime import datetime


def now_ms() -> int:
    return time.time_ns() // 1_000_000


def log_info(msg: str):
    print(f"[INFO]  {msg}", file=sys.stderr, flush=True)


def log_warn(msg: str):
    print(f"[WARN]  {msg}", file=sys.stderr, flush=True)


def log_fail(msg: str):
    print(f"[FAIL]  {msg}", file=sys.stderr, flush=True)


def log_pass(msg: str):
    print(f"[PASS]  {msg}", file=sys.stderr, flush=True)


# ───────────────────────────────────────────────
# Pod JSON 生成（与 28 号脚本一致：独立 uid/name + direct label）
# ───────────────────────────────────────────────

def prepare_pod_json(template_path: str, prefix: str, workdir: str) -> str:
    uid = f"e2b{prefix}{int(time.time())}{random.randint(0, 32767)}"
    with open(template_path, encoding="utf-8") as f:
        text = f.read()
    # 复刻 bash 版 sed 行为：替换所有 "uid"，替换第一处 "name"
    text = re.sub(r'"uid":\s*"[^"]*"', f'"uid": "{uid}"', text)
    text = re.sub(r'"name":\s*"[^"]*"', f'"name": "test-e2b-{prefix}-{uid}"', text, count=1)
    pod = json.loads(text)
    labels = pod.setdefault("labels", {})
    labels["flux-sandbox.io/direct"] = "true"
    labels["flux-sandbox.io/runtime"] = "e2b"
    out_path = os.path.join(workdir, f"pod-{prefix}.json")
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(pod, f, indent=2, ensure_ascii=True)
        f.write("\n")
    return out_path


# ───────────────────────────────────────────────
# 单个沙箱 RunPodSandbox（并发栅栏：自旋等到 target_ms 再发起）
# 结果写 workdir/<idx>.result（JSON 一行），thread/process 模式通用
# ───────────────────────────────────────────────

def worker_body(idx, pod_json, target_ms, socket, timeout, workdir):
    rec = {"idx": idx, "ok": False, "pod_id": None, "err": "",
           "fire_offset_ms": None, "start_ms": None, "end_ms": None,
           "attempt_durs_ms": []}
    try:
        # 粗睡到栅栏前 50ms，再自旋对齐，兼顾精度与 CPU 开销
        delta = target_ms - now_ms()
        if delta > 60:
            time.sleep((delta - 50) / 1000)
        while now_ms() < target_ms:
            pass

        start_ms = now_ms()
        rec["fire_offset_ms"] = start_ms - target_ms
        rec["start_ms"] = start_ms
        # 只调一次，不重试：脚本的意义是测 N 并发的端到端耗时，重试会破坏测量语义
        crictl = ["crictl", "--runtime-endpoint", f"unix://{socket}",
                  "runp", "-T", timeout, "-r", "e2b", pod_json]
        proc = subprocess.run(crictl, capture_output=True, text=True)
        rec["attempt_durs_ms"].append(now_ms() - start_ms)
        output = (proc.stdout + proc.stderr).strip()
        first = output.splitlines()[0].strip() if output else ""
        if (re.fullmatch(r"[a-z0-9-]+", first)
                and "error" not in output.lower() and "fata" not in output.lower()):
            rec["ok"] = True
            rec["pod_id"] = first
        else:
            rec["err"] = output[:500]
        rec["end_ms"] = now_ms()
    except Exception as e:  # worker 异常也要落盘，避免主进程误判为"还在跑"
        rec["err"] = f"worker exception: {e!r}"
        rec["end_ms"] = now_ms()
    with open(os.path.join(workdir, f"{idx}.result"), "w", encoding="utf-8") as f:
        f.write(json.dumps(rec, ensure_ascii=False) + "\n")


# ───────────────────────────────────────────────
# 主流程
# ───────────────────────────────────────────────

def main():
    ap = argparse.ArgumentParser(description="并发启动 N 个 direct sandbox（仅创建，不采集）")
    ap.add_argument("count", type=int, help="并发沙箱数")
    ap.add_argument("--pod-json", default="/tmp/e2b-pod.json")
    ap.add_argument("--socket", default="/tmp/cri-multiplex.sock")
    ap.add_argument("--prefix", default=None)
    ap.add_argument("--start-at-ms", type=int, default=0)
    ap.add_argument("--mode", choices=["thread", "process"], default="process",
                    help="worker 并发模型（默认 process，与 28 号 bash 脚本的 fork 模型一致；"
                         "thread 模式在某些环境上受 GIL 影响可能异常，仅作对照用）")
    ap.add_argument("--timeout", default="900s")
    ap.add_argument("--debug", action="store_true")
    ap.add_argument("--cleanup", action="store_true")
    ap.add_argument("--ids-file", default=None)
    args = ap.parse_args()

    if args.count < 1:
        log_fail(f"COUNT 必须是正整数: {args.count}")
        sys.exit(1)
    prefix = args.prefix or f"c{args.count}-{datetime.now().strftime('%H%M%S')}"
    ids_file = args.ids_file or f"/tmp/{prefix}-pod-ids.txt"

    # ── 前置检查 ──
    if not shutil.which("crictl"):
        log_fail("未找到 crictl")
        sys.exit(1)
    if not os.path.isfile(args.pod_json):
        log_fail(f"基础 Pod JSON 不存在: {args.pod_json}（用 --pod-json 指定）")
        sys.exit(1)
    if not os.path.exists(args.socket):
        log_fail(f"cri-multiplex socket 不存在: {args.socket}")
        sys.exit(1)
    probe = subprocess.run(["crictl", "--runtime-endpoint", f"unix://{args.socket}", "version"],
                           capture_output=True, text=True)
    if probe.returncode != 0:
        log_fail(f"cri-multiplex 无响应: crictl version 失败: {probe.stderr.strip()}")
        sys.exit(1)
    log_pass(f"crictl/socket 就绪，cri-multiplex 运行中（socket={args.socket}）")

    # hide-sandbox-label 检查（防 kubelet GC direct 沙箱）
    out = subprocess.run(["sh", "-c",
                          "tr '\\0' ' ' < /proc/$(pgrep -f 'cmd/cri-multiplex|/cri-multiplex ' | head -1)/cmdline"],
                         capture_output=True, text=True)
    if out.stdout and "-hide-sandbox-label" not in out.stdout:
        log_warn("cri-multiplex 未配置 -hide-sandbox-label，kubelet 可能把 direct 沙箱当孤儿 GC")

    # ── 预生成 Pod JSON（不计时）──
    workdir = f"/tmp/e2b-bulk-py-{prefix}-{os.getpid()}"
    os.makedirs(workdir, exist_ok=True)
    log_info(f"预生成 {args.count} 个 Pod JSON（不计入耗时）...")
    pod_jsons = [prepare_pod_json(args.pod_json, f"{prefix}-{i}", workdir)
                 for i in range(1, args.count + 1)]
    log_pass(f"{args.count} 个 Pod JSON 已生成")

    # ── 并发栅栏 ──
    if args.start_at_ms > 0:
        target_ms = args.start_at_ms
        if now_ms() >= target_ms:
            log_warn(f"--start-at-ms={target_ms} 已过（节点时钟不同步或预生成过慢），将立即发起")
    else:
        target_ms = now_ms() + 1500
    fire_at = datetime.fromtimestamp(target_ms / 1000).strftime("%H:%M:%S") + f".{target_ms % 1000:03d}"
    log_info(f"并发栅栏: target={fire_at}，{args.count} 个 worker({args.mode}) 等待同刻发起")

    batch_start_ms = now_ms()
    if args.mode == "process":
        # 与 28 号 bash 脚本一致的 fork 模型：进程先就位，自旋等栅栏
        ctx = multiprocessing.get_context("fork")
        workers = [ctx.Process(target=worker_body,
                               args=(i, pod_jsons[i - 1], target_ms, args.socket,
                                     args.timeout, workdir),
                               daemon=True)
                   for i in range(1, args.count + 1)]
        for w in workers:
            w.start()
        for w in workers:
            w.join()
    else:
        workers = [threading.Thread(target=worker_body,
                                    args=(i, pod_jsons[i - 1], target_ms, args.socket,
                                          args.timeout, workdir),
                                    daemon=True)
                   for i in range(1, args.count + 1)]
        for w in workers:
            w.start()
        for w in workers:
            w.join()
    batch_end_ms = now_ms()

    # ── 结果汇总（仅成功/失败与耗时明细，不做服务端日志采集）──
    records = []
    for path in glob.glob(os.path.join(workdir, "*.result")):
        try:
            with open(path, encoding="utf-8") as f:
                records.append(json.loads(f.read().strip()))
        except (OSError, ValueError) as e:
            log_warn(f"读取结果文件失败 {path}: {e}")
    got = {r["idx"] for r in records}
    for i in range(1, args.count + 1):
        if i not in got:
            records.append({"idx": i, "ok": False, "pod_id": None,
                            "err": "worker 未产生结果（进程/线程异常退出）",
                            "fire_offset_ms": None, "start_ms": None,
                            "end_ms": None, "attempt_durs_ms": []})
    records.sort(key=lambda r: r["idx"])

    pod_ids = [r["pod_id"] for r in records if r["ok"]]
    failed = [r for r in records if not r["ok"]]

    if args.debug or failed:
        log_info("per-sandbox 明细: idx | ok | 栅栏偏差ms | 总耗时ms | 各attempt耗时ms | err")
        for r in records:
            total = (r["end_ms"] - r["start_ms"]) if r["start_ms"] and r["end_ms"] else -1
            line = (f"  #{r['idx']:>4} | {'OK ' if r['ok'] else 'ERR'} | "
                    f"{r['fire_offset_ms'] if r['fire_offset_ms'] is not None else '-':>5} | "
                    f"{total:>7} | {r['attempt_durs_ms']}")
            if r["err"]:
                line += f" | {r['err'][:200]}"
            print(line, file=sys.stderr, flush=True)

    with open(ids_file, "w", encoding="utf-8") as f:
        for pid in pod_ids:
            f.write(pid + "\n")
    log_info(f"pod id 清单已写入: {ids_file}")

    starts = [r["start_ms"] for r in records if r["start_ms"]]
    ends = [r["end_ms"] for r in records if r["end_ms"]]
    if failed:
        log_fail(f"并发创建 {args.count} 个 direct sandbox：{len(failed)} 个失败")
    else:
        log_pass(f"{args.count} 个 direct sandbox 创建完成")
    if starts:
        log_info(f"端到端耗时(首个RunPodSandbox开始→最后一个结束): "
                 f"{(max(ends) - min(starts)) / 1000:.3f}s；"
                 f"发起离散度(首个→最后发起): {max(starts) - min(starts)}ms")

    # ── 单沙箱耗时分位数（仅成功沙箱；max 尾部不再掩盖整体分布）──
    ok_durs = sorted(r["end_ms"] - r["start_ms"]
                     for r in records
                     if r["ok"] and r["start_ms"] and r["end_ms"])

    def pct(p: int) -> int:
        if not ok_durs:
            return -1
        k = (p * len(ok_durs) + 99) // 100 - 1
        return ok_durs[max(0, min(len(ok_durs) - 1, k))]

    if ok_durs:
        log_info(f"单沙箱耗时分布(成功 {len(ok_durs)} 只): "
                 f"min={ok_durs[0]}ms p50={pct(50)}ms p90={pct(90)}ms "
                 f"p95={pct(95)}ms p99={pct(99)}ms max={ok_durs[-1]}ms "
                 f"avg={sum(ok_durs) // len(ok_durs)}ms")

    # 机器可读汇总行（供 33_multi_round_benchmark.sh 逐轮采集对比）
    if starts:
        print(f"[BulkResult] count={args.count} ok={len(pod_ids)} fail={len(failed)} "
              f"e2e_ms={max(ends) - min(starts)} p50_ms={pct(50)} p90_ms={pct(90)} "
              f"p95_ms={pct(95)} p99_ms={pct(99)} max_ms={ok_durs[-1] if ok_durs else -1}",
              file=sys.stderr, flush=True)

    # ── 可选清理 ──
    if args.cleanup and pod_ids:
        log_info(f"清理 {len(pod_ids)} 个 direct sandbox...")
        clean_start = now_ms()
        crictl_base = ["crictl", "--runtime-endpoint", f"unix://{args.socket}"]
        for pid in pod_ids:
            # rmp 需要 -f：cri-multiplex 的 RemovePodSandbox 本身做完整清理，
            # 但 crictl 客户端对 running 状态的沙箱不加 -f 会直接拒绝
            subprocess.run(crictl_base + ["rmp", "-f", pid],
                           capture_output=True, text=True)
        deadline = time.time() + 300
        while True:
            remaining = 0
            for pid in pod_ids:
                chk = subprocess.run(crictl_base + ["inspectp", pid],
                                     capture_output=True, text=True)
                if chk.returncode == 0:
                    remaining += 1
            if remaining == 0:
                break
            if time.time() >= deadline:
                log_fail(f"清理超时，仍有 {remaining} 个沙箱未消失（id 清单: {ids_file}）")
                sys.exit(1)
            time.sleep(1)
        log_pass(f"批量清理完成，耗时 {(now_ms() - clean_start) / 1000:.3f}s")
    elif pod_ids:
        log_info(f"保留 {len(pod_ids)} 个沙箱运行；事后清理: "
                 f"xargs -a {ids_file} crictl --runtime-endpoint unix://{args.socket} rmp -f")

    # 清理临时目录（result 文件已读完）
    shutil.rmtree(workdir, ignore_errors=True)

    sys.exit(0 if not failed else 1)


if __name__ == "__main__":
    main()
