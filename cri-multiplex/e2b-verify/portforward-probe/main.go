// 本工具只验证指定测试 Pod 的 PortForward 数据流，不创建或删除资源。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	cri "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func run() error {
	socket := flag.String("socket", "", "隔离 mux Unix socket")
	pod := flag.String("pod", "", "本次测试 Pod ID")
	port := flag.Int("port", 18080, "guest HTTP 端口")
	expected := flag.String("expected", "", "预期响应正文")
	nodeIP := flag.String("node-ip", "", "允许访问的本节点 IP")
	flag.Parse()
	if *socket == "" || *pod == "" || *expected == "" || *port < 1 || *port > 65535 {
		return fmt.Errorf("required arguments missing or invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := grpc.NewClient("unix://"+*socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer c.Close()
	response, err := cri.NewRuntimeServiceClient(c).PortForward(ctx, &cri.PortForwardRequest{PodSandboxId: *pod, Port: []int32{int32(*port)}})
	if err != nil {
		return err
	}
	u, err := url.Parse(response.GetUrl())
	if err != nil {
		return err
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || ip == nil || !(ip.IsLoopback() || ip.String() == *nodeIP) {
		return fmt.Errorf("unexpected streaming endpoint")
	}
	transport := spdy.NewRoundTripperWithProxy(nil, func(*http.Request) (*url.URL, error) { return nil, nil })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Add(httpstream.HeaderProtocolVersion, "portforward.k8s.io")
	httpResponse, err := transport.RoundTrip(req)
	if err != nil {
		return err
	}
	conn, err := transport.NewConnection(httpResponse)
	if err != nil {
		httpResponse.Body.Close()
		return err
	}
	defer conn.Close()
	// SPDY stream 没有 ReadDeadline；在总超时后关闭连接以解除所有阻塞读取。
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set("port", strconv.Itoa(*port))
	headers.Set("requestID", "0")
	errorStream, err := conn.CreateStream(headers)
	if err != nil {
		return err
	}
	defer errorStream.Reset()
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	stream, err := conn.CreateStream(headers)
	if err != nil {
		return err
	}
	defer stream.Reset()
	if _, err = io.WriteString(stream, "GET / HTTP/1.1\r\nHost: probe\r\nConnection: close\r\n\r\n"); err != nil {
		return err
	}
	r, err := http.ReadResponse(bufio.NewReader(stream), nil)
	if err != nil {
		return fmt.Errorf("read forwarded HTTP: %w", err)
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		return err
	}
	if r.StatusCode != http.StatusOK || string(body) != *expected {
		return fmt.Errorf("forwarded HTTP status/body mismatch: status=%d bytes=%d", r.StatusCode, len(body))
	}
	fmt.Println("PASS PortForward real HTTP bytes")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
