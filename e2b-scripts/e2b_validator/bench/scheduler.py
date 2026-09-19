"""bench replay 的调度原语（threading 同步版）。

1:1 移植自 replay-aenv 的 asyncio 实现（agent/run_slot_scheduler.py、
agent/control_plane_scheduler.py），语义保持一致：
- RunningSlotScheduler：延时堆（ready_at，time.monotonic）+ FIFO 准入，
  lease 持有期间计入 active，硬上限 maximum——客户端准入永不超限；
- SmoothRateLimiter：全局 FIFO 队列，按 1/qps 间隔平滑分发（从实际分发时刻
  排下一个，唤醒晚了不补发追突发），inflight_cap 限制在途操作数。
"""

from __future__ import annotations

import contextlib
import heapq
import itertools
import threading
import time
from collections import deque
from enum import IntEnum
from typing import Any, Iterator


def is_transient_sandbox_error(exc: BaseException) -> bool:
    """判断一次失败的基础设施请求是否可安全重试（瞬断错误）。"""

    text = f"{type(exc).__name__}: {exc}".lower()
    return any(
        marker in text
        for marker in (
            "502:",
            "http 502",
            "bad gateway",
            "503:",
            "http 503",
            "service unavailable",
            "504:",
            "http 504",
            "gateway timeout",
            "429:",
            "http 429",
            "too many requests",
            "rate limit",
            "timed out",
            "timeout",
            "connection reset",
            "connection refused",
            "connection aborted",
            "temporarily unavailable",
        )
    )


class OperationType(IntEnum):
    RESUME = 0
    CLEANUP = 10
    PAUSE = 20
    CREATE = 30
    COMMAND = 40


class _RunRequest:
    """延时堆元素：按 (ready_at, sequence) 排序，同 ready_at 先到先服务。"""

    __slots__ = (
        "ready_at",
        "sequence",
        "task_id",
        "queued_at",
        "event",
        "lease",
        "error",
        "cancelled",
    )

    def __init__(self, *, ready_at: float, sequence: int, task_id: str, queued_at: float) -> None:
        self.ready_at = ready_at
        self.sequence = sequence
        self.task_id = task_id
        self.queued_at = queued_at
        self.event = threading.Event()
        self.lease: RunningLease | None = None
        self.error: BaseException | None = None
        self.cancelled = False

    def __lt__(self, other: "_RunRequest") -> bool:
        return (self.ready_at, self.sequence) < (other.ready_at, other.sequence)


class RunningLease:
    """一个已预约的 RUNNING 沙箱名额。

    预约在 resume 之前完成，必须持有到沙箱确认 paused / stopped / deleted 为止。
    """

    __slots__ = (
        "_scheduler",
        "lease_id",
        "task_id",
        "acquired_at",
        "queue_wait_sec",
        "_released",
    )

    def __init__(
        self,
        scheduler: "RunningSlotScheduler",
        *,
        lease_id: int,
        task_id: str,
        acquired_at: float,
        queue_wait_sec: float,
    ) -> None:
        self._scheduler = scheduler
        self.lease_id = lease_id
        self.task_id = task_id
        self.acquired_at = acquired_at
        self.queue_wait_sec = queue_wait_sec
        self._released = False

    @property
    def released(self) -> bool:
        return self._released

    def release(self) -> None:
        if self._released:
            raise RuntimeError(f"Running lease {self.lease_id} released twice")
        self._released = True
        self._scheduler._release(self)


class RunningSlotScheduler:
    """完整 ``resume -> command -> pause`` 切片的 FIFO 准入调度器。

    请求在 monotonic 延时堆中等待到 ready_at，到期请求按 ready 时间/到达顺序准入。
    active 从预约（而非 resume 开始）起算，因此即使生命周期调用在途，
    客户端准入也永远不会超过 maximum。
    """

    def __init__(self, maximum: int) -> None:
        if maximum < 1:
            raise ValueError("running slot maximum must be positive")
        self.maximum = maximum
        self._condition = threading.Condition()
        self._sequence = itertools.count(1)
        self._lease_ids = itertools.count(1)
        self._delayed: list[_RunRequest] = []
        self._ready: deque[_RunRequest] = deque()
        self._active: dict[int, RunningLease] = {}
        self._peak_active = 0
        self._granted = 0
        self._total_queue_wait_sec = 0.0
        self._dispatcher: threading.Thread | None = None
        self._closed = False

    @classmethod
    def single_task(cls) -> "RunningSlotScheduler":
        return cls(1)

    def _ensure_dispatcher_locked(self) -> None:
        if self._dispatcher is None or not self._dispatcher.is_alive():
            self._dispatcher = threading.Thread(
                target=self._dispatch_loop,
                name="bench-running-slot-dispatcher",
                daemon=True,
            )
            self._dispatcher.start()

    def acquire(
        self,
        task_id: str,
        *,
        ready_at: float | None = None,
    ) -> RunningLease:
        if not task_id:
            raise ValueError("task_id must not be empty")
        now = time.monotonic()
        request = _RunRequest(
            ready_at=now if ready_at is None else max(now, ready_at),
            sequence=next(self._sequence),
            task_id=task_id,
            queued_at=now,
        )
        with self._condition:
            if self._closed:
                raise RuntimeError("running slot scheduler is closed")
            heapq.heappush(self._delayed, request)
            self._ensure_dispatcher_locked()
            self._condition.notify_all()

        try:
            request.event.wait()
        except BaseException:
            # 等待被异常打断（如 KeyboardInterrupt）：已发放的 lease 立即归还，
            # 未发放的请求标记取消，由分发线程跳过
            granted_lease: RunningLease | None = None
            with self._condition:
                if request.lease is not None:
                    granted_lease = request.lease
                else:
                    request.cancelled = True
                self._condition.notify_all()
            if granted_lease is not None and not granted_lease.released:
                granted_lease.release()
            raise
        if request.error is not None:
            raise request.error
        assert request.lease is not None
        return request.lease

    def _dispatch_loop(self) -> None:
        while True:
            with self._condition:
                now = time.monotonic()
                while self._delayed and self._delayed[0].ready_at <= now:
                    request = heapq.heappop(self._delayed)
                    if not request.cancelled:
                        self._ready.append(request)

                while self._ready and len(self._active) < self.maximum:
                    request = self._ready.popleft()
                    if request.cancelled:
                        continue
                    acquired_at = time.monotonic()
                    lease = RunningLease(
                        self,
                        lease_id=next(self._lease_ids),
                        task_id=request.task_id,
                        acquired_at=acquired_at,
                        queue_wait_sec=acquired_at - request.queued_at,
                    )
                    self._active[lease.lease_id] = lease
                    self._peak_active = max(self._peak_active, len(self._active))
                    self._granted += 1
                    self._total_queue_wait_sec += lease.queue_wait_sec
                    request.lease = lease
                    request.event.set()

                if self._closed:
                    pending = list(self._ready)
                    pending.extend(self._delayed)
                    self._ready.clear()
                    self._delayed.clear()
                    for request in pending:
                        if request.lease is None and request.error is None:
                            request.error = RuntimeError("running slot scheduler closed")
                            request.event.set()
                    return

                timeout: float | None = None
                if self._delayed:
                    timeout = max(0.0, self._delayed[0].ready_at - time.monotonic())
                self._condition.wait(timeout=timeout)

    def _release(self, lease: RunningLease) -> None:
        with self._condition:
            active = self._active.pop(lease.lease_id, None)
            if active is None:
                raise RuntimeError(f"Unknown running lease {lease.lease_id}")
            self._condition.notify_all()

    def snapshot(self) -> dict[str, Any]:
        with self._condition:
            waiting = len(self._ready) + sum(
                1 for request in self._delayed if not request.cancelled
            )
            return {
                "maximum": self.maximum,
                "active": len(self._active),
                "peak_active": self._peak_active,
                "waiting": waiting,
                "granted": self._granted,
                "average_queue_wait_sec": (
                    self._total_queue_wait_sec / self._granted if self._granted else 0.0
                ),
            }

    def close(self) -> None:
        with self._condition:
            if self._closed:
                return
            self._closed = True
            self._condition.notify_all()
            dispatcher = self._dispatcher
        if dispatcher is not None and dispatcher is not threading.current_thread():
            dispatcher.join()


class _RateWaiter:
    __slots__ = ("operation", "event", "enqueued_at", "reserved", "granted", "cancelled")

    def __init__(self, *, operation: OperationType, enqueued_at: float) -> None:
        self.operation = operation
        self.event = threading.Event()
        self.enqueued_at = enqueued_at
        self.reserved = False
        self.granted = False
        self.cancelled = False


class SmoothRateLimiter:
    """单一平滑 FIFO 请求流 + 在途上限。

    每个新请求和每次重试都进同一个队列。操作类型只用于统计：create / pause /
    resume / cleanup / command 准入优先级完全相同，一律按到达顺序分发。
    """

    def __init__(self, *, qps: float, inflight_cap: int) -> None:
        if qps <= 0:
            raise ValueError("control-plane-qps must be positive")
        if inflight_cap <= 0:
            raise ValueError("control-plane in-flight cap must be positive")

        self.qps = qps
        self.inflight_cap = inflight_cap
        self._interval_sec = 1.0 / qps
        self._condition = threading.Condition()
        self._queue: deque[_RateWaiter] = deque()
        self._next_dispatch_at = 0.0
        self._in_flight = 0
        self._dispatched = 0
        self._dispatcher: threading.Thread | None = None
        self._wait_total_sec = 0.0
        self._wait_max_sec = 0.0
        self._dispatched_by_operation: dict[OperationType, int] = {
            operation: 0 for operation in OperationType
        }

    def _waiting(self) -> int:
        return len(self._queue)

    def snapshot(self) -> dict[str, Any]:
        with self._condition:
            return {
                "mode": "rate",
                "qps": self.qps,
                "inflight_cap": self.inflight_cap,
                "in_flight": self._in_flight,
                "waiting": self._waiting(),
                "dispatched": self._dispatched,
                "average_wait_sec": (
                    self._wait_total_sec / self._dispatched if self._dispatched else 0.0
                ),
                "max_wait_sec": self._wait_max_sec,
                "dispatched_by_operation": {
                    operation.name.lower(): count
                    for operation, count in self._dispatched_by_operation.items()
                },
                "waiting_by_operation": {
                    operation.name.lower(): sum(
                        1 for waiter in self._queue if waiter.operation == operation
                    )
                    for operation in OperationType
                },
            }

    def _select_waiter(self) -> _RateWaiter | None:
        while self._queue and self._queue[0].cancelled:
            self._queue.popleft()
        return self._queue.popleft() if self._queue else None

    def _ensure_dispatcher_locked(self) -> None:
        if self._dispatcher is None or not self._dispatcher.is_alive():
            self._dispatcher = threading.Thread(
                target=self._dispatch_loop,
                name="bench-rate-limiter-dispatcher",
                daemon=True,
            )
            self._dispatcher.start()

    def _dispatch_loop(self) -> None:
        while True:
            with self._condition:
                self._condition.wait_for(
                    lambda: self._waiting() == 0 or self._in_flight < self.inflight_cap
                )
                if self._waiting() == 0:
                    self._dispatcher = None
                    return

                waiter = self._select_waiter()
                if waiter is None:
                    continue
                waiter.reserved = True
                self._in_flight += 1
                now = time.monotonic()
                dispatch_at = max(now, self._next_dispatch_at)
                wait_sec = max(0.0, dispatch_at - now)

            if wait_sec:
                time.sleep(wait_sec)

            # 从实际分发时刻排下一个：唤醒晚了不补发追突发
            with self._condition:
                self._next_dispatch_at = time.monotonic() + self._interval_sec

            if waiter.cancelled:
                with self._condition:
                    self._in_flight -= 1
                    self._condition.notify_all()
                continue

            queue_wait_sec = time.monotonic() - waiter.enqueued_at
            self._dispatched += 1
            self._wait_total_sec += queue_wait_sec
            self._wait_max_sec = max(self._wait_max_sec, queue_wait_sec)
            self._dispatched_by_operation[waiter.operation] += 1
            waiter.granted = True
            waiter.event.set()

    def _acquire(self, operation: OperationType) -> None:
        waiter = _RateWaiter(operation=operation, enqueued_at=time.monotonic())
        with self._condition:
            self._queue.append(waiter)
            self._ensure_dispatcher_locked()
            self._condition.notify_all()

        try:
            waiter.event.wait()
        except BaseException:
            with self._condition:
                if not waiter.reserved:
                    waiter.cancelled = True
                    try:
                        self._queue.remove(waiter)
                    except ValueError:
                        pass
                elif waiter.granted:
                    # 准入后、进入上下文体之前被打断：_release 永远不会被调用
                    self._in_flight -= 1
                else:
                    # 已预约未分发：标记取消，由分发线程观察到后释放
                    waiter.cancelled = True
                self._condition.notify_all()
            raise

    def _release(self) -> None:
        with self._condition:
            self._in_flight -= 1
            if self._in_flight < 0:
                raise RuntimeError("global rate limiter released without acquire")
            self._condition.notify_all()

    @contextlib.contextmanager
    def slot(self, operation: OperationType) -> Iterator[None]:
        acquired = False
        try:
            self._acquire(operation)
            acquired = True
            yield
        finally:
            if acquired:
                self._release()
