package targetstore

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
)

func TestMooncakeControlPlaneEndpoints(t *testing.T) {
	tests := []struct {
		value string
		want  []string
	}{
		{value: "http://metadata.internal:8080/metadata", want: []string{"metadata.internal:8080"}},
		{value: "http://metadata.internal/metadata", want: []string{"metadata.internal:80"}},
		{value: "https://metadata.internal/metadata", want: []string{"metadata.internal:443"}},
		{value: "https://[::1]/metadata", want: []string{"[::1]:443"}},
		{value: "etcd://etcd.internal:2379", want: []string{"etcd.internal:2379"}},
		{value: "10.0.0.5:8500", want: []string{"10.0.0.5:8500"}},
		{value: "localhost:50051", want: []string{"localhost:50051"}},
		{value: "etcd1.internal:2379,etcd2.internal:2379", want: []string{"etcd1.internal:2379", "etcd2.internal:2379"}},
		// 高可用部署:master 与 metadata 都通过 etcd 集群发现,分号分隔多端点。
		{value: "etcd://10.42.74.35:2379;10.42.74.22:2379", want: []string{"10.42.74.35:2379", "10.42.74.22:2379"}},
		{value: "etcd://10.42.74.35:2379;10.42.74.22:2379/mooncake", want: []string{"10.42.74.35:2379", "10.42.74.22:2379"}},
		{value: " etcd://a:2379; b:2379 ", want: []string{"a:2379", "b:2379"}},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			got, err := mooncakeControlPlaneEndpoints("MOONCAKE_METADATA_SERVER", test.value)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("mooncakeControlPlaneEndpoints() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMooncakeControlPlaneEndpointsRejectsMalformedLists(t *testing.T) {
	for _, value := range []string{"", "etcd://", "etcd://a;b", "a:2379;missing-port", "://a:2379", "etcd://;"} {
		t.Run(value, func(t *testing.T) {
			if _, err := mooncakeControlPlaneEndpoints("MOONCAKE_MASTER_ADDR", value); err == nil || !strings.Contains(err.Error(), "MOONCAKE_MASTER_ADDR") {
				t.Fatalf("mooncakeControlPlaneEndpoints(%q) error = %v, want MOONCAKE_MASTER_ADDR error", value, err)
			}
		})
	}
}

func TestMooncakePreflightAcceptsListWithOneReachableEndpoint(t *testing.T) {
	master := listenMooncakeTestEndpoint(t)
	metadata := listenMooncakeTestEndpoint(t)
	closed := closedMooncakeTestAddress(t)

	err := preflightMooncake(
		context.Background(),
		"etcd://"+closed+";"+metadata.Addr().String(),
		"etcd://"+closed+","+master.Addr().String(),
	)
	if err != nil {
		t.Fatal(err)
	}

	err = preflightMooncake(context.Background(), "etcd://"+closed+";"+closed, master.Addr().String())
	if err == nil || strings.Count(err.Error(), "MOONCAKE_METADATA_SERVER") != 2 {
		t.Fatalf("preflightMooncake() error = %v, want one failure per unreachable metadata endpoint", err)
	}
}

func TestMooncakePreflightRejectsInvalidEndpointConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
		master   string
		want     string
	}{
		{name: "master", metadata: "http://metadata:8080/metadata", master: "missing-port", want: "MOONCAKE_MASTER_ADDR"},
		{name: "metadata scheme without port", metadata: "ftp://metadata", master: "master:50051", want: "MOONCAKE_METADATA_SERVER"},
		{name: "metadata bare host without port", metadata: "metadata.internal", master: "master:50051", want: "MOONCAKE_METADATA_SERVER"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := preflightMooncake(context.Background(), test.metadata, test.master)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("preflightMooncake() error = %v", err)
			}
		})
	}
}

func TestMooncakePreflightConnectsToBothControlPlaneEndpoints(t *testing.T) {
	master := listenMooncakeTestEndpoint(t)
	metadata := listenMooncakeTestEndpoint(t)

	err := preflightMooncake(
		context.Background(),
		"http://"+metadata.Addr().String()+"/metadata",
		master.Addr().String(),
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestMooncakePreflightReportsTheUnreachableEndpoint(t *testing.T) {
	t.Run("master", func(t *testing.T) {
		masterAddress := closedMooncakeTestAddress(t)
		metadata := listenMooncakeTestEndpoint(t)

		err := preflightMooncake(context.Background(), "http://"+metadata.Addr().String()+"/metadata", masterAddress)
		if err == nil || !strings.Contains(err.Error(), "MOONCAKE_MASTER_ADDR") {
			t.Fatalf("preflightMooncake() error = %v", err)
		}
	})

	t.Run("metadata", func(t *testing.T) {
		master := listenMooncakeTestEndpoint(t)
		metadataAddress := closedMooncakeTestAddress(t)

		err := preflightMooncake(context.Background(), "http://"+metadataAddress+"/metadata", master.Addr().String())
		if err == nil || !strings.Contains(err.Error(), "MOONCAKE_METADATA_SERVER") {
			t.Fatalf("preflightMooncake() error = %v", err)
		}
	})
}

func TestMooncakePreflightUsesCallerContext(t *testing.T) {
	master := listenMooncakeTestEndpoint(t)
	metadata := listenMooncakeTestEndpoint(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := preflightMooncake(ctx, "http://"+metadata.Addr().String()+"/metadata", master.Addr().String())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("preflightMooncake() error = %v, want context.Canceled", err)
	}
}

func listenMooncakeTestEndpoint(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func closedMooncakeTestAddress(t *testing.T) string {
	t.Helper()
	listener := listenMooncakeTestEndpoint(t)
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
