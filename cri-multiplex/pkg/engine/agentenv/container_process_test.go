package agentenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
)

func TestConcurrentStartContainerStartsOneGuestProcess(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	created, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "concurrent-start", "agentenv.dev/app:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	var starts int32
	instance.exec = &fakeExec{startProcess: func(context.Context, *runtimeapi.SandboxProcessStartRequest) (*runtimeapi.SandboxProcessStartResponse, error) {
		atomic.AddInt32(&starts, 1)
		return &runtimeapi.SandboxProcessStartResponse{Pid: 5151}, nil
	}}
	var wait sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := instance.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: created.GetContainerId()})
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("StartContainer: %v", err)
		}
	}
	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("startProcess calls = %d, want 1", got)
	}
}

func TestReopenContainerLogIncrementsGeneration(t *testing.T) {
	instance, containerID := startStreamingTestContainer(t)
	if _, err := instance.ReopenContainerLog(context.Background(), &runtime.ReopenContainerLogRequest{ContainerId: containerID}); err != nil {
		t.Fatalf("ReopenContainerLog: %v", err)
	}
	record, ok := instance.getContainer(containerID)
	if !ok || record.LogGeneration != 1 {
		t.Fatalf("log generation = %d, want 1", record.LogGeneration)
	}
}

func TestStartContainerStartsNativeProcessAndStopUsesPid(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	var startRequest *runtimeapi.SandboxProcessStartRequest
	var stopRequest *runtimeapi.SandboxProcessStopRequest
	instance.exec = &fakeExec{
		startProcess: func(_ context.Context, req *runtimeapi.SandboxProcessStartRequest) (*runtimeapi.SandboxProcessStartResponse, error) {
			startRequest = req
			return &runtimeapi.SandboxProcessStartResponse{Pid: 5151}, nil
		},
		getProcess: func(_ context.Context, req *runtimeapi.SandboxProcessGetRequest) (*runtimeapi.SandboxProcessGetResponse, error) {
			return &runtimeapi.SandboxProcessGetResponse{Pid: req.GetPid(), Running: true}, nil
		},
		stopProcess: func(_ context.Context, req *runtimeapi.SandboxProcessStopRequest) (*runtimeapi.SandboxProcessStopResponse, error) {
			stopRequest = req
			return &runtimeapi.SandboxProcessStopResponse{Stopped: true, ExitCode: 23}, nil
		},
	}

	request := criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0)
	request.Config.Envs = []*runtime.KeyValue{{Key: "APP_MODE", Value: "test"}}
	request.Config.WorkingDir = "/workspace"
	created, err := instance.CreateContainer(context.Background(), request)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if _, err := instance.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: created.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if startRequest.GetNativeSandboxId() != "019a0000-0000-7000-8000-000000000201" {
		t.Fatalf("native sandbox id = %q", startRequest.GetNativeSandboxId())
	}
	if got := strings.Join(startRequest.GetCommand(), " "); got != "/bin/sh -c sleep 3600" {
		t.Fatalf("start command = %q", got)
	}
	if startRequest.GetEnvs()["APP_MODE"] != "test" || startRequest.GetCwd() != "/workspace" {
		t.Fatalf("start environment mismatch: %+v", startRequest)
	}
	record, ok := instance.getContainer(created.GetContainerId())
	if !ok || record.NativeProcessID != 5151 || record.State != containerStateRunning {
		t.Fatalf("container process projection = %+v, found=%v", record, ok)
	}

	if _, err := instance.StopContainer(context.Background(), &runtime.StopContainerRequest{ContainerId: created.GetContainerId()}); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	if stopRequest.GetPid() != 5151 || stopRequest.GetNativeSandboxId() != startRequest.GetNativeSandboxId() {
		t.Fatalf("stop request does not target guest pid: %+v", stopRequest)
	}
	record, _ = instance.getContainer(created.GetContainerId())
	if record.State != containerStateExited || record.ExitCode != 23 {
		t.Fatalf("stopped container projection = %+v", record)
	}
}

func TestContainerProcessExitWritesCRILogAndExitCode(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	instance.exec = &fakeExec{
		startProcess: func(context.Context, *runtimeapi.SandboxProcessStartRequest) (*runtimeapi.SandboxProcessStartResponse, error) {
			return &runtimeapi.SandboxProcessStartResponse{Pid: 6161}, nil
		},
		getProcess: func(_ context.Context, req *runtimeapi.SandboxProcessGetRequest) (*runtimeapi.SandboxProcessGetResponse, error) {
			return &runtimeapi.SandboxProcessGetResponse{
				Pid:      req.GetPid(),
				Running:  false,
				ExitCode: 7,
				Outputs: []*runtimeapi.SandboxProcessOutputChunk{{
					Sequence: 1,
					Stream:   runtimeapi.SandboxProcessOutputStream_SANDBOX_PROCESS_OUTPUT_STREAM_STDOUT,
					Data:     []byte("workload finished\n"),
				}},
			}, nil
		},
	}
	created, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	record, _ := instance.getContainer(created.GetContainerId())
	if _, err := instance.StartContainer(context.Background(), &runtime.StartContainerRequest{ContainerId: created.GetContainerId()}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		record, _ = instance.getContainer(created.GetContainerId())
		if record.State == containerStateExited {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if record.State != containerStateExited || record.ExitCode != 7 {
		t.Fatalf("process exit was not projected: %+v", record)
	}
	data, err := os.ReadFile(record.LogPath)
	if err != nil {
		t.Fatalf("read CRI log: %v", err)
	}
	if !strings.Contains(string(data), " stdout F workload finished") {
		t.Fatalf("CRI log does not contain stdout line: %q", string(data))
	}
}

func TestCRILogWriterPreservesPartialLinesByStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.log")
	writer, err := newCRILogWriter(path)
	if err != nil {
		t.Fatalf("newCRILogWriter: %v", err)
	}
	if err := writer.WriteChunk(runtimeapi.SandboxProcessOutputStream_SANDBOX_PROCESS_OUTPUT_STREAM_STDOUT, []byte("first\npartial")); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	if err := writer.WriteChunk(runtimeapi.SandboxProcessOutputStream_SANDBOX_PROCESS_OUTPUT_STREAM_STDERR, []byte("error\n")); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(data)
	for _, want := range []string{" stdout F first", " stderr F error", " stdout P partial"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log %q does not contain %q", log, want)
		}
	}
}
func TestUpdateContainerResourcesIsExplicitlyUnimplemented(t *testing.T) {
	instance, _, _ := newReadyContainerTestEngine(t)
	created, err := instance.CreateContainer(context.Background(), criCreateContainerRequest("cri-1", "app", "agentenv.dev/app:v1", 0))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	_, err = instance.UpdateContainerResources(context.Background(), &runtime.UpdateContainerResourcesRequest{ContainerId: created.GetContainerId()})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("update resources code = %v, want Unimplemented", status.Code(err))
	}
}
