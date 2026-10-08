package discovery

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// startHealthServer 启动一个真实的 gRPC 健康服务（对齐 orchestrator 的注册方式）
func startHealthServer(t *testing.T) (ip string, port int, stop func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := grpc.NewServer()
	hs := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, hs)
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	go func() { _ = srv.Serve(lis) }()

	return "127.0.0.1", lis.Addr().(*net.TCPAddr).Port, srv.Stop
}

// reservePort 占用后立刻释放一个端口，用于拿到一个"几乎必然不可达"的地址
func reservePort(t *testing.T) int {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := lis.Addr().(*net.TCPAddr).Port
	require.NoError(t, lis.Close())

	return port
}

func newTestProber() *healthProber {
	return newHealthProber(time.Hour, 300*time.Millisecond, 2)
}

func TestParseStaticEntries(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []staticEntry
	}{
		{
			name: "empty",
			raw:  "",
			want: []staticEntry{},
		},
		{
			name: "node with ip",
			raw:  "node1=10.0.0.1",
			want: []staticEntry{{NodeID: "node1", IP: "10.0.0.1"}},
		},
		{
			name: "node with ip and port",
			raw:  "node1=10.0.0.1:5009",
			want: []staticEntry{{NodeID: "node1", IP: "10.0.0.1", Port: 5009}},
		},
		{
			name: "node without ip falls back to nodeID as address",
			raw:  "node1",
			want: []staticEntry{{NodeID: "node1", IP: "node1"}},
		},
		{
			name: "multiple entries with spaces and empty items",
			raw:  " node1=10.0.0.1 , ,node2=10.0.0.2:5009,, ",
			want: []staticEntry{
				{NodeID: "node1", IP: "10.0.0.1"},
				{NodeID: "node2", IP: "10.0.0.2", Port: 5009},
			},
		},
		{
			name: "invalid port ignored",
			raw:  "node1=10.0.0.1:abc",
			want: []staticEntry{{NodeID: "node1", IP: "10.0.0.1"}},
		},
		{
			name: "entry without node id skipped",
			raw:  "=10.0.0.1,node2=10.0.0.2",
			want: []staticEntry{{NodeID: "node2", IP: "10.0.0.2"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseStaticEntries(tt.raw))
		})
	}
}

func TestStaticDiscovery_ListWithoutHealthCheck(t *testing.T) {
	entries := []staticEntry{
		{NodeID: "node1", IP: "10.0.0.1"},
		{NodeID: "node2", IP: "10.0.0.2"},
	}
	sd := NewStaticDiscovery(context.Background(), entries, false)

	allocs, err := sd.ListOrchestratorAndTemplateBuilderAllocations(context.Background())
	require.NoError(t, err)
	require.Len(t, allocs, 2)
	assert.Equal(t, "node1", allocs[0].NodeID)
	assert.Equal(t, "10.0.0.1", allocs[0].AllocationIP)
	assert.Equal(t, "static-10.0.0.1", allocs[0].AllocationID)
}

func TestStaticDiscovery_HealthyNodeReturned(t *testing.T) {
	ip, port, stop := startHealthServer(t)
	defer stop()

	entries := []staticEntry{{NodeID: "node1", IP: ip, Port: port}}
	sd := NewStaticDiscovery(context.Background(), entries, true,
		WithStaticProber(newTestProber()),
		WithStaticProbePort(port),
	)

	allocs, err := sd.ListOrchestratorAndTemplateBuilderAllocations(context.Background())
	require.NoError(t, err)
	require.Len(t, allocs, 1)
	assert.Equal(t, "node1", allocs[0].NodeID)
}

func TestStaticDiscovery_UnreachableNodeFiltered(t *testing.T) {
	entries := []staticEntry{{NodeID: "node1", IP: "127.0.0.1", Port: reservePort(t)}}
	sd := NewStaticDiscovery(context.Background(), entries, true,
		WithStaticProber(newTestProber()),
	)

	allocs, err := sd.ListOrchestratorAndTemplateBuilderAllocations(context.Background())
	require.NoError(t, err)
	assert.Empty(t, allocs)
}

func TestStaticDiscovery_NodeGoesDownIsRemovedAfterThreshold(t *testing.T) {
	ip, port, stop := startHealthServer(t)

	prober := newTestProber()
	entries := []staticEntry{{NodeID: "node1", IP: ip, Port: port}}
	sd := NewStaticDiscovery(context.Background(), entries, true,
		WithStaticProber(prober),
	)

	// 节点健康，出现在列表中
	allocs, err := sd.ListOrchestratorAndTemplateBuilderAllocations(context.Background())
	require.NoError(t, err)
	require.Len(t, allocs, 1)

	// 停掉服务模拟 systemd 服务宕机
	stop()

	// 第一次失败：未到阈值，仍在列表
	prober.probeOnce(context.Background(), entries, int(sd.probePort))
	allocs, err = sd.ListOrchestratorAndTemplateBuilderAllocations(context.Background())
	require.NoError(t, err)
	assert.Len(t, allocs, 1, "single failure should not remove the node")

	// 第二次失败：达到阈值，摘除
	prober.probeOnce(context.Background(), entries, int(sd.probePort))
	allocs, err = sd.ListOrchestratorAndTemplateBuilderAllocations(context.Background())
	require.NoError(t, err)
	assert.Empty(t, allocs, "node should be removed after failThreshold consecutive failures")
}

func TestStaticDiscovery_NodeRecoversImmediately(t *testing.T) {
	ip, port, stop := startHealthServer(t)

	prober := newTestProber()
	entries := []staticEntry{{NodeID: "node1", IP: ip, Port: port}}
	sd := NewStaticDiscovery(context.Background(), entries, true,
		WithStaticProber(prober),
	)

	// 摘除节点
	stop()
	prober.probeOnce(context.Background(), entries, int(sd.probePort))
	prober.probeOnce(context.Background(), entries, int(sd.probePort))
	assert.False(t, prober.isServing("node1"))

	// 服务恢复（同地址重新监听）
	lis, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	srv := grpc.NewServer()
	hs := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, hs)
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	// 一次成功探测即加回
	prober.probeOnce(context.Background(), entries, int(sd.probePort))
	allocs, err := sd.ListOrchestratorAndTemplateBuilderAllocations(context.Background())
	require.NoError(t, err)
	assert.Len(t, allocs, 1, "node should be added back after a single successful probe")
}

func TestStaticDiscovery_FileReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allocations.json")

	writeFile := func(content string, mtime time.Time) {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		require.NoError(t, os.Chtimes(path, mtime, mtime))
	}

	base := time.Now()
	writeFile(`[{"nodeID":"node1","ip":"10.0.0.1"}]`, base)

	t.Setenv("E2B_STATIC_ALLOCATIONS_FILE", path)
	t.Setenv("E2B_STATIC_DISCOVERY_HEALTHCHECK", "false")
	sd := NewStaticDiscoveryFromEnv(context.Background())

	allocs, err := sd.ListOrchestratorAndTemplateBuilderAllocations(context.Background())
	require.NoError(t, err)
	require.Len(t, allocs, 1)
	assert.Equal(t, "node1", allocs[0].NodeID)

	// 改写文件（mtime 推后），List 时应热加载
	writeFile(`[{"nodeID":"node1","ip":"10.0.0.1"},{"nodeID":"node2","ip":"10.0.0.2"}]`, base.Add(time.Minute))
	allocs, err = sd.ListOrchestratorAndTemplateBuilderAllocations(context.Background())
	require.NoError(t, err)
	require.Len(t, allocs, 2)
	assert.Equal(t, "node2", allocs[1].NodeID)
}

func TestStaticDiscovery_EnvTakesPrecedenceOverFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allocations.json")
	require.NoError(t, os.WriteFile(path, []byte(`[{"nodeID":"file-node","ip":"10.0.0.9"}]`), 0o644))

	t.Setenv("E2B_STATIC_ALLOCATIONS", "env-node=10.0.0.1")
	t.Setenv("E2B_STATIC_ALLOCATIONS_FILE", path)
	t.Setenv("E2B_STATIC_DISCOVERY_HEALTHCHECK", "false")
	sd := NewStaticDiscoveryFromEnv(context.Background())

	allocs, err := sd.ListOrchestratorAndTemplateBuilderAllocations(context.Background())
	require.NoError(t, err)
	require.Len(t, allocs, 1)
	assert.Equal(t, "env-node", allocs[0].NodeID)

	// env 优先时不启用热加载：改文件不应生效
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.WriteFile(path, []byte(`[{"nodeID":"file-node2","ip":"10.0.0.10"}]`), 0o644))
	require.NoError(t, os.Chtimes(path, future, future))
	allocs, err = sd.ListOrchestratorAndTemplateBuilderAllocations(context.Background())
	require.NoError(t, err)
	require.Len(t, allocs, 1)
	assert.Equal(t, "env-node", allocs[0].NodeID)
}

func TestHealthProber_FailThresholdStateMachine(t *testing.T) {
	p := newHealthProber(1, time.Second, 2)
	entry := staticEntry{NodeID: "node1", IP: "10.0.0.1"}
	ctx := context.Background()

	// 初始：未知节点不存活
	assert.False(t, p.isServing("node1"))

	// 首次成功：立即存活
	p.update(ctx, entry, true)
	assert.True(t, p.isServing("node1"))

	// 一次失败：未达阈值，保持存活
	p.update(ctx, entry, false)
	assert.True(t, p.isServing("node1"))

	// 连续第二次失败：摘除
	p.update(ctx, entry, false)
	assert.False(t, p.isServing("node1"))

	// 再次失败：保持摘除
	p.update(ctx, entry, false)
	assert.False(t, p.isServing("node1"))

	// 一次成功：立即恢复
	p.update(ctx, entry, true)
	assert.True(t, p.isServing("node1"))
}

func TestLoadStaticEntriesFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid file filters empty nodeID", func(t *testing.T) {
		path := filepath.Join(dir, "valid.json")
		require.NoError(t, os.WriteFile(path, []byte(
			`[{"nodeID":"node1","ip":"10.0.0.1"},{"nodeID":"","ip":"10.0.0.2"},{"nodeID":"node2","ip":"10.0.0.3","port":5009}]`), 0o644))

		entries, mtime, err := loadStaticEntriesFile(path)
		require.NoError(t, err)
		assert.False(t, mtime.IsZero())
		assert.Equal(t, []staticEntry{
			{NodeID: "node1", IP: "10.0.0.1"},
			{NodeID: "node2", IP: "10.0.0.3", Port: 5009},
		}, entries)
	})

	t.Run("missing file", func(t *testing.T) {
		_, _, err := loadStaticEntriesFile(filepath.Join(dir, "missing.json"))
		require.Error(t, err)
	})

	t.Run("invalid json", func(t *testing.T) {
		path := filepath.Join(dir, "invalid.json")
		require.NoError(t, os.WriteFile(path, []byte(`not-json`), 0o644))
		_, _, err := loadStaticEntriesFile(path)
		require.Error(t, err)
	})
}
