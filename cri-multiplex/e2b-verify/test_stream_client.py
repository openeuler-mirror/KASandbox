#!/usr/bin/env python3
"""
test_stream_client.py — 通用 streaming URL 连接客户端

用于连接 Exec / Attach 返回的 URL，进行交互式操作。
自动化验证时通过 stdin 发送命令，读取 stdout 输出。

用法:
    python3 test_stream_client.py <URL> [timeout_seconds] [input_commands]

示例:
    # 交互模式（手动输入）
    python3 test_stream_client.py http://10.12.0.54:35021/exec/abc123

    # 自动化模式（发送命令后读取输出）
    echo "echo hello" | python3 test_stream_client.py http://10.12.0.54:35021/exec/abc123 5
"""
import socket
import sys
import threading
import os
import select
import time


def main():
    if len(sys.argv) < 2:
        print("用法: python3 test_stream_client.py <URL> [timeout_seconds]", file=sys.stderr)
        sys.exit(1)

    url = sys.argv[1]
    timeout = int(sys.argv[2]) if len(sys.argv) > 2 else 10

    # 解析 URL
    if not url.startswith("http://"):
        print(f"不支持的 URL 格式: {url}", file=sys.stderr)
        sys.exit(1)

    host_port = url.replace("http://", "").split("/")[0]
    path = "/" + "/".join(url.replace("http://", "").split("/")[1:])

    host, port = host_port.split(":")
    port = int(port)

    # 连接
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.settimeout(timeout)
    try:
        sock.connect((host, port))
    except Exception as e:
        print(f"连接失败: {e}", file=sys.stderr)
        sys.exit(1)

    # 发送 HTTP GET 请求
    req = f"GET {path} HTTP/1.1\r\nHost: {host_port}\r\n\r\n"
    sock.send(req.encode())

    # 读取 HTTP 响应头
    header_buf = b""
    while b"\r\n\r\n" not in header_buf:
        chunk = sock.recv(1)
        if not chunk:
            print("Connection closed before headers", file=sys.stderr)
            sock.close()
            sys.exit(1)
        header_buf += chunk

    headers, remainder = header_buf.split(b"\r\n\r\n", 1)
    print(f"=== Headers ===\n{headers.decode()}\n=== End Headers ===", file=sys.stderr, flush=True)

    if remainder:
        sys.stdout.buffer.write(remainder)
        sys.stdout.flush()

    # 自动化模式：从 stdin 读取并发送，收集输出
    output_lines = []

    def read_from_sock():
        while True:
            try:
                data = sock.recv(4096)
                if not data:
                    break
                sys.stdout.buffer.write(data)
                sys.stdout.flush()
                output_lines.append(data)
            except socket.timeout:
                break
            except:
                break

    t = threading.Thread(target=read_from_sock, daemon=True)
    t.start()

    # 如果有 stdin 输入，发送它
    if not sys.stdin.isatty():
        # 从 stdin 读取所有输入
        stdin_data = sys.stdin.buffer.read()
        if stdin_data:
            # 把 \r 转换为 \n，兼容无 PTY 的 pipe 模式
            stdin_data = stdin_data.replace(b'\r', b'\n')
            try:
                sock.send(stdin_data)
            except:
                pass
            # 等待输出
            time.sleep(1)

    # 等待读取线程完成或超时
    t.join(timeout=timeout)

    sock.close()

    # 检查是否有输出
    if output_lines:
        sys.exit(0)
    else:
        print("无输出", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
