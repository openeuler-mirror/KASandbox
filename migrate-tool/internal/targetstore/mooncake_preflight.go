package targetstore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

const mooncakePreflightTimeout = 3 * time.Second

// preflightMooncake 只检查 native Setup 必须连接的两个控制面端点。数据面的
// MOONCAKE_LOCAL_HOSTNAME 是本机对外公布的地址,不属于这里的远端连通性检查。
//
// 两个变量都可能是端点列表:高可用部署下 master 通过 etcd 发现,写成
// etcd://HOST:PORT;HOST:PORT(分号或逗号分隔),metadata 也可以是同样的 etcd
// 列表。运行时把整串原样交给 native 客户端,这里同样不限定形态:列表只要求
// 至少一个端点 TCP 可达,协议语义留给 native 层。
func preflightMooncake(ctx context.Context, metadataServer, masterAddr string) error {
	masterEndpoints, err := mooncakeControlPlaneEndpoints("MOONCAKE_MASTER_ADDR", masterAddr)
	if err != nil {
		return err
	}
	metadataEndpoints, err := mooncakeControlPlaneEndpoints("MOONCAKE_METADATA_SERVER", metadataServer)
	if err != nil {
		return err
	}

	if err := dialMooncakeControlPlane(ctx, "MOONCAKE_MASTER_ADDR", masterEndpoints); err != nil {
		return err
	}
	return dialMooncakeControlPlane(ctx, "MOONCAKE_METADATA_SERVER", metadataEndpoints)
}

// mooncakeControlPlaneEndpoints 把控制面变量解析为 TCP 地址列表。接受的形态:
//
//   - HOST:PORT
//   - http://HOST[:PORT]/path、https://...(缺省端口 80/443)
//   - etcd://HOST:PORT;HOST:PORT[/prefix](分号或逗号分隔的多端点,HA 模式)
//   - 不带 scheme 的 HOST:PORT;HOST:PORT 列表
func mooncakeControlPlaneEndpoints(variable, value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("%s must not be empty", variable)
	}
	invalid := func() error {
		return fmt.Errorf("%s must be a URL, HOST:PORT, or etcd://HOST:PORT;HOST:PORT list, got %q", variable, value)
	}

	scheme, rest, hasScheme := strings.Cut(value, "://")
	if hasScheme && (scheme == "http" || scheme == "https") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Hostname() == "" {
			return nil, invalid()
		}
		port := parsed.Port()
		if port == "" {
			port = "80"
			if scheme == "https" {
				port = "443"
			}
		}
		return []string{net.JoinHostPort(parsed.Hostname(), port)}, nil
	}

	list := value
	if hasScheme {
		if scheme == "" {
			return nil, invalid()
		}
		// etcd://a:2379;b:2379/prefix:端点列表止于第一个路径分隔符。
		list = rest
		if index := strings.IndexByte(list, '/'); index >= 0 {
			list = list[:index]
		}
	}

	var endpoints []string
	for _, item := range strings.FieldsFunc(list, func(r rune) bool { return r == ';' || r == ',' }) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		host, port, err := net.SplitHostPort(item)
		if err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("%s endpoint %q must use HOST:PORT (full value %q)", variable, item, value)
		}
		endpoints = append(endpoints, net.JoinHostPort(host, port))
	}
	if len(endpoints) == 0 {
		return nil, invalid()
	}
	return endpoints, nil
}

// dialMooncakeControlPlane 依次尝试各端点,任一可达即通过;全部失败时汇总
// 每个端点的错误,便于分辨是地址写错还是整个集群不可达。
func dialMooncakeControlPlane(ctx context.Context, variable string, addresses []string) error {
	dialer := net.Dialer{Timeout: mooncakePreflightTimeout}
	var failures []error
	for _, address := range addresses {
		connection, err := dialer.DialContext(ctx, "tcp", address)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		failures = append(failures, fmt.Errorf("connect to %s at %s: %w", variable, address, err))
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(failures...)
}
