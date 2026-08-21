package targetstore

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestMooncakeMetadataTCPAddress(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: "http://metadata.internal:8080/metadata", want: "metadata.internal:8080"},
		{value: "http://metadata.internal/metadata", want: "metadata.internal:80"},
		{value: "https://metadata.internal/metadata", want: "metadata.internal:443"},
		{value: "https://[::1]/metadata", want: "[::1]:443"},
		{value: "etcd://etcd.internal:2379", want: "etcd.internal:2379"},
		{value: "10.0.0.5:8500", want: "10.0.0.5:8500"},
		{value: "etcd1.internal:2379,etcd2.internal:2379", want: "etcd1.internal:2379"},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			got, err := mooncakeMetadataTCPAddress(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("mooncakeMetadataTCPAddress() = %q, want %q", got, test.want)
			}
		})
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
