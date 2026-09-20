package sandbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/network"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
)

// fakeProcessService 记录经 connect-RPC 发来的命令并按需返回非零退出码。
type fakeProcessService struct {
	processconnect.UnimplementedProcessHandler

	mu       sync.Mutex
	commands []string
	// failExit 按命令子串匹配返回非零退出码。
	failExit map[string]int32
}

func (f *fakeProcessService) Start(
	_ context.Context,
	req *connect.Request[process.StartRequest],
	stream *connect.ServerStream[process.StartResponse],
) error {
	cmd := strings.Join(req.Msg.GetProcess().GetArgs(), " ")

	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	exitCode := int32(0)
	for sub, code := range f.failExit {
		if strings.Contains(cmd, sub) {
			exitCode = code
		}
	}
	f.mu.Unlock()

	end := &process.ProcessEvent_EndEvent{ExitCode: exitCode}
	if exitCode != 0 {
		end.Status = "process exited with code 1"
	}

	return stream.Send(&process.StartResponse{
		Event: &process.ProcessEvent{
			Event: &process.ProcessEvent_End{End: end},
		},
	})
}

func (f *fakeProcessService) ranCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.commands...)
}

// fakeEnvd 是注入 UT 用的假 envd：REST /files（GET/POST）+ process 服务。
type fakeEnvd struct {
	proc *fakeProcessService

	mu         sync.Mutex
	files      map[string][]byte
	failUpload bool
	uploads    int
}

func (f *fakeEnvd) handleFiles(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")

	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		content, ok := f.files[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}
		_, _ = w.Write(content)
	case http.MethodPost:
		if f.failUpload {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		defer file.Close()
		content, err := io.ReadAll(file)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		f.files[path] = content
		f.uploads++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func newFakeEnvd(t *testing.T) (*fakeEnvd, *httptest.Server) {
	t.Helper()

	fake := &fakeEnvd{
		proc:  &fakeProcessService{failExit: map[string]int32{}},
		files: map[string][]byte{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/files", fake.handleFiles)
	path, handler := processconnect.NewProcessHandler(fake.proc)
	mux.Handle(path, handler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return fake, srv
}

var testCAPEM = []byte("-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIRAKNn5oZ4fakefakefakefake\n-----END CERTIFICATE-----\n")

func TestInjectEgressProxyCASuccess(t *testing.T) {
	t.Parallel()

	fake, srv := newFakeEnvd(t)

	err := injectEgressProxyCA(t.Context(), srv.URL, "token", testCAPEM)
	require.NoError(t, err)

	fake.mu.Lock()
	stored := fake.files[egressCAGuestCertPath]
	uploads := fake.uploads
	fake.mu.Unlock()

	assert.Equal(t, testCAPEM, stored, "guest 内 cert 必须与宿主 cert 逐字节一致")
	assert.Equal(t, 1, uploads)

	commands := fake.proc.ranCommands()
	require.Len(t, commands, 2)
	assert.Contains(t, commands[0], "update-ca-certificates")
	assert.Contains(t, commands[1], "openssl x509 -in "+egressCAGuestTrustedPath+" -noout -subject")
}

func TestInjectEgressProxyCAWriteFailureRollsBack(t *testing.T) {
	t.Parallel()

	fake, srv := newFakeEnvd(t)
	fake.failUpload = true

	err := injectEgressProxyCA(t.Context(), srv.URL, "token", testCAPEM)
	require.Error(t, err)
	assert.Empty(t, fake.proc.ranCommands(), "写文件失败后不得执行任何 guest 命令")
}

func TestInjectEgressProxyCAUpdateCertsFailureRollsBack(t *testing.T) {
	t.Parallel()

	fake, srv := newFakeEnvd(t)
	fake.proc.failExit["update-ca-certificates"] = 1

	err := injectEgressProxyCA(t.Context(), srv.URL, "token", testCAPEM)
	require.Error(t, err)

	commands := fake.proc.ranCommands()
	require.Len(t, commands, 1, "update-ca-certificates 失败后不得再执行校验命令")
	assert.Contains(t, commands[0], "update-ca-certificates")
}

func TestInjectEgressProxyCAVerifyFailureRollsBack(t *testing.T) {
	t.Parallel()

	fake, srv := newFakeEnvd(t)
	fake.proc.failExit["openssl x509"] = 1

	err := injectEgressProxyCA(t.Context(), srv.URL, "token", testCAPEM)
	require.Error(t, err)
	assert.Len(t, fake.proc.ranCommands(), 2)
}

func TestInjectEgressProxyCAFingerprintIdempotent(t *testing.T) {
	t.Parallel()

	fake, srv := newFakeEnvd(t)
	fake.files[egressCAGuestCertPath] = testCAPEM

	err := injectEgressProxyCA(t.Context(), srv.URL, "token", testCAPEM)
	require.NoError(t, err)

	fake.mu.Lock()
	uploads := fake.uploads
	fake.mu.Unlock()
	assert.Equal(t, 0, uploads, "指纹一致时不得重复写文件")
	assert.Empty(t, fake.proc.ranCommands(), "指纹一致时不得重复执行 update-ca-certificates")
}

func TestInjectEgressProxyCAChangedFingerprintReinjects(t *testing.T) {
	t.Parallel()

	fake, srv := newFakeEnvd(t)
	fake.files[egressCAGuestCertPath] = []byte("old-ca")

	err := injectEgressProxyCA(t.Context(), srv.URL, "token", testCAPEM)
	require.NoError(t, err)

	fake.mu.Lock()
	stored := fake.files[egressCAGuestCertPath]
	fake.mu.Unlock()
	assert.Equal(t, testCAPEM, stored, "指纹不一致（CA 轮换）时必须重写并刷新信任库")
	assert.Len(t, fake.proc.ranCommands(), 2)
}

func TestEgressProxyCANeedsInjection(t *testing.T) {
	t.Parallel()

	cfgAuto := network.Config{SandboxProxyCAAuto: "true"}
	cfgOff := network.Config{SandboxProxyCAAuto: "false"}

	assert.True(t, egressProxyCANeedsInjection(cfgAuto, network.EgressModePerSandbox, nil),
		"CA auto + per-sandbox + mitm 缺省（默认 true）→ 注入")
	assert.False(t, egressProxyCANeedsInjection(cfgAuto, network.EgressModePerSandbox,
		map[string]string{network.MetadataKeyEgressMitm: "false"}),
		"egress-mitm=false 整体短路，不注入")
	assert.False(t, egressProxyCANeedsInjection(cfgOff, network.EgressModePerSandbox, nil),
		"开关关闭零行为")
	assert.False(t, egressProxyCANeedsInjection(cfgAuto, network.EgressModeOff, nil),
		"非 per-sandbox 沙箱不注入")
}
