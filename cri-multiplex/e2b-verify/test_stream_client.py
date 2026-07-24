#!/usr/bin/env python3
"""
test_stream_client.py — 通用 CRI streaming WebSocket 客户端

用于连接 Exec / Attach 返回的 URL，按 Kubernetes remotecommand
v5.channel.k8s.io 协议发送 stdin channel，并读取 stdout/stderr channel。

用法:
    python3 test_stream_client.py <URL> [timeout_seconds]

示例:
    echo "echo hello" | python3 test_stream_client.py http://10.12.0.54:35021/exec/abc123 5
"""
import base64
import hashlib
import os
import socket
import struct
import sys
import time

CHANNEL_STDIN = 0
CHANNEL_STDOUT = 1
CHANNEL_STDERR = 2
CHANNEL_ERROR = 3

OP_CONT = 0x0
OP_TEXT = 0x1
OP_BINARY = 0x2
OP_CLOSE = 0x8
OP_PING = 0x9
OP_PONG = 0xA


def parse_url(url):
    if not url.startswith("http://"):
        print(f"不支持的 URL 格式: {url}", file=sys.stderr)
        sys.exit(1)

    rest = url[len("http://"):]
    host_port = rest.split("/", 1)[0]
    path = "/" + rest.split("/", 1)[1] if "/" in rest else "/"
    host, port_text = host_port.rsplit(":", 1)
    return host, int(port_text), host_port, path


def recv_exact(sock, size):
    data = b""
    while len(data) < size:
        chunk = sock.recv(size - len(data))
        if not chunk:
            raise EOFError("connection closed")
        data += chunk
    return data


def send_ws_frame(sock, opcode, payload=b""):
    if isinstance(payload, str):
        payload = payload.encode()

    first = 0x80 | opcode
    mask_bit = 0x80
    length = len(payload)
    header = bytearray([first])

    if length < 126:
        header.append(mask_bit | length)
    elif length <= 0xFFFF:
        header.append(mask_bit | 126)
        header.extend(struct.pack("!H", length))
    else:
        header.append(mask_bit | 127)
        header.extend(struct.pack("!Q", length))

    mask = os.urandom(4)
    masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
    sock.sendall(bytes(header) + mask + masked)


def recv_ws_frame(sock):
    b1, b2 = recv_exact(sock, 2)
    opcode = b1 & 0x0F
    masked = bool(b2 & 0x80)
    length = b2 & 0x7F

    if length == 126:
        length = struct.unpack("!H", recv_exact(sock, 2))[0]
    elif length == 127:
        length = struct.unpack("!Q", recv_exact(sock, 8))[0]

    mask = recv_exact(sock, 4) if masked else b""
    payload = recv_exact(sock, length) if length else b""
    if masked:
        payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
    return opcode, payload


def websocket_handshake(sock, host_port, path, timeout):
    key = base64.b64encode(os.urandom(16)).decode()
    req = (
        f"GET {path} HTTP/1.1\r\n"
        f"Host: {host_port}\r\n"
        "Upgrade: websocket\r\n"
        "Connection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {key}\r\n"
        "Sec-WebSocket-Version: 13\r\n"
        "Sec-WebSocket-Protocol: v5.channel.k8s.io\r\n"
        "\r\n"
    )
    sock.sendall(req.encode())

    header_buf = b""
    deadline = time.time() + timeout
    while b"\r\n\r\n" not in header_buf:
        if time.time() >= deadline:
            raise TimeoutError("timeout waiting for websocket headers")
        chunk = sock.recv(1)
        if not chunk:
            raise EOFError("connection closed before headers")
        header_buf += chunk

    headers = header_buf.decode(errors="replace")
    print(f"=== Headers ===\n{headers.rstrip()}\n=== End Headers ===", file=sys.stderr, flush=True)

    status_line = headers.splitlines()[0] if headers.splitlines() else ""
    if " 101 " not in status_line:
        raise RuntimeError(f"websocket upgrade failed: {status_line}")

    accept_expected = base64.b64encode(
        hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()
    ).decode()
    if f"sec-websocket-accept: {accept_expected}".lower() not in headers.lower():
        raise RuntimeError("websocket accept header mismatch")


def main():
    if len(sys.argv) < 2:
        print("用法: python3 test_stream_client.py <URL> [timeout_seconds]", file=sys.stderr)
        sys.exit(1)

    host, port, host_port, path = parse_url(sys.argv[1])
    timeout = int(sys.argv[2]) if len(sys.argv) > 2 else 10

    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.settimeout(timeout)
    try:
        sock.connect((host, port))
        websocket_handshake(sock, host_port, path, timeout)

        if not sys.stdin.isatty():
            stdin_data = sys.stdin.buffer.read()
            if stdin_data:
                stdin_data = stdin_data.replace(b"\r", b"\n")
                send_ws_frame(sock, OP_BINARY, bytes([CHANNEL_STDIN]) + stdin_data)

        output_seen = False
        deadline = time.time() + timeout
        while time.time() < deadline:
            sock.settimeout(max(0.1, deadline - time.time()))
            try:
                opcode, payload = recv_ws_frame(sock)
            except socket.timeout:
                break
            except EOFError:
                break

            if opcode == OP_CLOSE:
                break
            if opcode == OP_PING:
                send_ws_frame(sock, OP_PONG, payload)
                continue
            if opcode not in (OP_BINARY, OP_TEXT, OP_CONT) or not payload:
                continue

            channel = payload[0]
            data = payload[1:]
            if channel in (CHANNEL_STDOUT, CHANNEL_STDERR):
                if data:
                    output_seen = True
                    sys.stdout.buffer.write(data)
                    sys.stdout.flush()
            elif channel == CHANNEL_ERROR and data:
                print(data.decode(errors="replace"), file=sys.stderr)

        try:
            send_ws_frame(sock, OP_CLOSE)
        except Exception:
            pass
        sock.close()

        if output_seen:
            sys.exit(0)
        print("无输出", file=sys.stderr)
        sys.exit(1)
    except Exception as e:
        sock.close()
        print(f"WebSocket 连接失败: {e}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
