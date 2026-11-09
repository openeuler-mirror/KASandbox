#!/usr/bin/env python3
"""mock_api.py — 可控的 e2b transform API，用于降级与边界用例。

两个监听端口：
  data    (--port，默认 18080)：webhook 访问的 /sandboxes/transform
  control (--control-port，默认 port+1)：测试脚本切行为的控制面

数据面端点：
  POST /sandboxes/transform   按当前 mode 返回
  GET  /healthz               探活
  GET  /count                 已收到的 transform 请求数
  GET  /last-headers          最后一次请求的 header（JSON，小写 key）
  GET  /last-body             最后一次请求的 body
  GET  /last-response         最后一次响应的 status/bytes/delay（验证故障真的生效）

控制面端点：
  POST /mode   切换行为；body 形如 {"mode":"status","value":500}
  GET  /mode   读取当前行为
  GET  /healthz

模式（运行时切换，不需要重启 mock、也不需要改 webhook 的 env）：
  normal               用启动参数（--status/--delay/--body/--body-file/--size）
  status:<code>        固定状态码 + 错误 body
  body:<text>          固定响应体
  body_file:<path>     从文件读响应体
  delay:<seconds>      延迟后按 normal 响应
  size:<bytes>         返回指定大小的 JSON（用于触发 200KB 合并上限）
  refuse               关闭数据面监听 → 客户端得到 connection refused

用法示例：
  python3 mock_api.py --port 18080
  # 之后由测试脚本按用例切换（控制面默认在 18081）：
  curl -X POST -d '{"mode":"status","value":500}' http://127.0.0.1:18081/mode
  curl -X POST -d '{"mode":"refuse"}'              http://127.0.0.1:18081/mode
  curl -X POST -d '{"mode":"normal"}'              http://127.0.0.1:18081/mode
"""
import argparse
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DEFAULT_BODY = json.dumps(
    {
        "sandbox": {
            "alias": "mock-template",
            "templateId": "mock-template-id",
            "baseTemplateId": "mock-template-id",
            "buildId": "mock-build-id",
            "teamId": "mock-team-id",
            "vcpu": 2,
            "ramMb": 2048,
            "totalDiskSizeMb": 1024,
            "maxSandboxLength": 3600,
            "hugePages": False,
            "autoPause": False,
            "snapshot": False,
            "envdVersion": "mock-envd",
            "kernelVersion": "mock-kernel",
            "firecrackerVersion": "mock-fc",
            "executionId": "mock-execution-id",
            "sandboxId": "mock-sandbox-id",
        }
    }
)

VALID_MODES = {"normal", "status", "body", "body_file", "delay", "size", "refuse"}


class State:
    """跨请求共享的状态：计数、最后一次请求、最后一次响应、当前模式。"""

    def __init__(self):
        self.lock = threading.RLock()
        self.count = 0
        self.last_headers = {}
        self.last_body = ""
        self.last_response = {}
        self.mode = {"mode": "normal"}


STATE = State()
# 启动参数定义的 normal 行为（由 main 填充）
DEFAULTS = {"status": 200, "delay": 0.0, "body": DEFAULT_BODY}


class DataServer:
    """数据面监听器，可在运行时关闭/重开（refuse 模式）。

    关闭后再监听用的是同一个 host:port，因此 webhook 的 E2B_API_URL 不需要变，
    也就不会触发 Deployment 滚动。
    """

    def __init__(self, host, port):
        self.host = host
        self.port = port
        self.httpd = None
        self.thread = None
        self.lock = threading.Lock()

    def running(self):
        with self.lock:
            return self.httpd is not None

    def start(self):
        with self.lock:
            if self.httpd is not None:
                return True
            try:
                httpd = ThreadingHTTPServer((self.host, self.port), Handler)
            except OSError as exc:
                sys.stderr.write("[mock] 无法监听 %s:%d: %s\n" % (self.host, self.port, exc))
                sys.stderr.flush()
                return False
            httpd.daemon_threads = True
            self.httpd = httpd
            self.thread = threading.Thread(target=httpd.serve_forever, daemon=True)
            self.thread.start()
            sys.stderr.write("[mock] data listening on %s:%d\n" % (self.host, self.port))
            sys.stderr.flush()
            return True

    def stop(self):
        with self.lock:
            if self.httpd is None:
                return True
            httpd = self.httpd
            self.httpd = None
            self.thread = None
        # shutdown() 等 serve_forever 退出；放在锁外，避免阻塞控制面的其它请求。
        httpd.shutdown()
        httpd.server_close()
        sys.stderr.write("[mock] data listener closed (refuse)\n")
        sys.stderr.flush()
        return True


DATA = DataServer("0.0.0.0", 18080)


def current_mode():
    with STATE.lock:
        return dict(STATE.mode)


def apply_mode(payload):
    """切换模式；返回 (ok, message)。"""
    mode = str(payload.get("mode", ""))
    if mode not in VALID_MODES:
        return False, "未知 mode: %r" % mode
    if mode == "refuse":
        with STATE.lock:
            STATE.mode = {"mode": "refuse"}
            STATE.last_response = {}
        DATA.stop()
        return True, "refuse"
    # 非 refuse：确保数据面在监听（从 refuse 切回来时重开）
    if not DATA.running() and not DATA.start():
        return False, "数据面重新监听失败"
    with STATE.lock:
        entry = {"mode": mode}
        if "value" in payload:
            entry["value"] = payload["value"]
        STATE.mode = entry
        # 清空上一条响应，避免用例没真正打到 mock 时读到旧证据。
        STATE.last_response = {}
    return True, mode


def resolve_response():
    """按当前模式返回 (status, body, delay)。"""
    mode = current_mode()
    kind = mode.get("mode", "normal")
    value = mode.get("value")

    if kind == "status":
        code = int(value)
        return code, json.dumps({"code": code, "message": "mock error"}), 0.0
    if kind == "body":
        return 200, str(value), 0.0
    if kind == "body_file":
        with open(str(value), encoding="utf-8") as handle:
            return 200, handle.read(), 0.0
    if kind == "delay":
        return int(DEFAULTS["status"]), DEFAULTS["body"], float(value)
    if kind == "size":
        return 200, json.dumps({"sandbox": {"teamId": "x" * int(value)}}), 0.0
    return int(DEFAULTS["status"]), DEFAULTS["body"], float(DEFAULTS["delay"])


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "webhook-verify-mock/2.0"

    # ---- 输出 ----
    def _send(self, code, body, content_type="application/json"):
        payload = body.encode("utf-8") if isinstance(body, str) else body
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        try:
            self.wfile.write(payload)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _json(self, code, obj):
        self._send(code, json.dumps(obj))

    def _read_body(self):
        length = int(self.headers.get("Content-Length") or 0)
        if not length:
            return ""
        return self.rfile.read(length).decode("utf-8", "replace")

    # ---- 路由 ----
    def do_GET(self):  # noqa: N802
        path = self.path.split("?")[0]
        if path in ("/healthz", "/health"):
            self._send(200, "ok", "text/plain")
        elif path == "/count":
            with STATE.lock:
                self._json(200, {"count": STATE.count})
        elif path == "/last-headers":
            with STATE.lock:
                self._json(200, STATE.last_headers)
        elif path == "/last-body":
            with STATE.lock:
                self._send(200, STATE.last_body)
        elif path == "/last-response":
            with STATE.lock:
                self._json(200, STATE.last_response)
        elif path == "/mode":
            self._json(200, current_mode())
        else:
            self._json(404, {"code": 404, "message": "not found"})

    def do_POST(self):  # noqa: N802
        path = self.path.split("?")[0]
        body = self._read_body()

        if path == "/mode":
            try:
                payload = json.loads(body or "{}")
            except ValueError as exc:
                self._json(400, {"ok": False, "error": "invalid json: %s" % exc})
                return
            ok, message = apply_mode(payload)
            self._json(200 if ok else 400, {"ok": ok, "mode": message})
            return

        if path != "/sandboxes/transform":
            self._json(404, {"code": 404, "message": "not found"})
            return

        with STATE.lock:
            STATE.last_headers = {k.lower(): v for k, v in self.headers.items()}
            STATE.last_body = body
            STATE.count += 1

        code, payload, delay = resolve_response()
        encoded = payload.encode("utf-8")
        # 先记录再延迟/发送：D2 的超时用例里客户端会先断开，服务端仍在 sleep，
        # 只有提前落证据，测试才能在准入返回后立即确认“延迟确实生效”。
        with STATE.lock:
            STATE.last_response = {
                "status": int(code),
                "bytes": len(encoded),
                "delay": float(delay),
                "mode": current_mode().get("mode", ""),
                "body_head": payload[:128],
            }
        if delay > 0:
            time.sleep(delay)
        self._send(code, payload)

    def log_message(self, fmt, *args):  # 日志写 stderr，stdout 留给启动提示
        sys.stderr.write("[mock] %s\n" % (fmt % args))
        sys.stderr.flush()


def build_body(args):
    if args.body_file:
        with open(args.body_file, encoding="utf-8") as handle:
            return handle.read()
    if args.body is not None:
        return args.body
    if args.size > 0:
        # 合法 JSON：把一个超大字符串放进注解来源字段，用于触发 200KB 合并上限。
        return json.dumps({"sandbox": {"teamId": "x" * args.size}})
    return DEFAULT_BODY


def main():
    parser = argparse.ArgumentParser(description="可控 e2b transform API")
    parser.add_argument("--port", type=int, default=18080)
    parser.add_argument("--control-port", type=int, default=0)
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--status", type=int, default=200)
    parser.add_argument("--delay", type=float, default=0.0)
    parser.add_argument("--body", default=None)
    parser.add_argument("--body-file", default=None)
    parser.add_argument("--size", type=int, default=0)
    args = parser.parse_args()

    control_port = args.control_port or (args.port + 1)
    DATA.host = args.host
    DATA.port = args.port
    DEFAULTS["status"] = args.status
    DEFAULTS["delay"] = args.delay
    DEFAULTS["body"] = build_body(args)

    if not DATA.start():
        sys.exit(1)

    control = ThreadingHTTPServer((args.host, control_port), Handler)
    control.daemon_threads = True

    sys.stderr.write(
        "[mock] control listening on %s:%d (data %d, status=%d delay=%ss body=%dB)\n"
        % (args.host, control_port, args.port, args.status, args.delay, len(DEFAULTS["body"]))
    )
    sys.stderr.flush()
    sys.stdout.write("READY port=%d control=%d\n" % (args.port, control_port))
    sys.stdout.flush()

    try:
        control.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        control.server_close()
        DATA.stop()


if __name__ == "__main__":
    main()
