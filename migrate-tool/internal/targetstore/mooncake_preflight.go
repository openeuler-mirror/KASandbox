package targetstore

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

const mooncakePreflightTimeout = 3 * time.Second

// preflightMooncake 只检查 native Setup 必须连接的两个控制面端点。数据面的
// MOONCAKE_LOCAL_HOSTNAME 是本机对外公布的地址，不属于这里的远端连通性检查。
func preflightMooncake(ctx context.Context, metadataServer, masterAddr string) error {
	masterTCP, err := mooncakeMasterTCPAddress(masterAddr)
	if err != nil {
		return err
	}
	metadataTCP, err := mooncakeMetadataTCPAddress(metadataServer)
	if err != nil {
		return err
	}

	if err := dialMooncakeControlPlane(ctx, "MOONCAKE_MASTER_ADDR", masterTCP); err != nil {
		return err
	}
	return dialMooncakeControlPlane(ctx, "MOONCAKE_METADATA_SERVER", metadataTCP)
}

func mooncakeMasterTCPAddress(value string) (string, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || port == "" {
		return "", fmt.Errorf("MOONCAKE_MASTER_ADDR must use HOST:PORT, got %q", value)
	}
	return net.JoinHostPort(host, port), nil
}

func mooncakeMetadataTCPAddress(value string) (string, error) {
	// metadata 后端不只有 HTTP:etcd://HOST:PORT、裸 HOST:PORT 或逗号分隔列表
	// 都是 native Setup 接受的形态。预检只验证首个端点的 TCP 可达性,协议
	// 语义留给 native 层。
	first := strings.TrimSpace(strings.Split(value, ",")[0])
	if !strings.Contains(first, "://") {
		host, port, err := net.SplitHostPort(first)
		if err != nil || host == "" || port == "" {
			return "", fmt.Errorf("MOONCAKE_METADATA_SERVER must be a URL or HOST:PORT, got %q", value)
		}
		return net.JoinHostPort(host, port), nil
	}
	parsed, err := url.Parse(first)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("MOONCAKE_METADATA_SERVER must be a URL or HOST:PORT, got %q", value)
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", fmt.Errorf("MOONCAKE_METADATA_SERVER %q needs an explicit port", value)
		}
	}
	return net.JoinHostPort(parsed.Hostname(), port), nil
}

func dialMooncakeControlPlane(ctx context.Context, variable, address string) error {
	dialer := net.Dialer{Timeout: mooncakePreflightTimeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect to %s at %s: %w", variable, address, err)
	}
	_ = connection.Close()
	return nil
}
