package agentenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimeapi "github.com/cri-multiplex/pkg/agentenv/runtime/v1"
	"github.com/cri-multiplex/pkg/engine"
)

const (
	containerProcessPollInterval = 200 * time.Millisecond
	containerProcessStopGrace    = 10 * time.Second
	containerProcessAPITimeout   = 10 * time.Second
)

// startContainerProcess 把 CRI 容器的 entrypoint/args 作为 guest 进程启动。
// 只有 Runtime 返回真实 guest PID 后，调用方才允许把容器投影为 running。
func (e *Engine) startContainerProcess(ctx context.Context, record containerRecord) (uint32, error) {
	command := append([]string(nil), record.Command...)
	command = append(command, record.Args...)
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return 0, status.Error(codes.InvalidArgument, "container command is required to start a real process")
	}
	mapping, err := e.resolveMapping(engine.SandboxRef{CRISandboxID: record.CRISandboxID})
	if err != nil {
		return 0, err
	}
	if err := e.ensureClients(); err != nil {
		return 0, e.mapRuntimeError(err)
	}
	if e.exec == nil {
		return 0, status.Error(codes.Internal, "AgentENV exec client is not initialized")
	}
	response, err := e.exec.StartProcess(ctx, &runtimeapi.SandboxProcessStartRequest{
		NativeSandboxId: mapping.NativeSandboxID,
		Command:         command,
		Envs:            cloneStringMap(record.Env),
		Cwd:             stringPointer(record.WorkingDir),
		Tty:             record.Tty,
		Stdin:           record.Stdin,
		TerminalSize:    nil,
	})
	if err != nil {
		return 0, e.mapRuntimeError(err)
	}
	if response.GetPid() == 0 {
		return 0, status.Error(codes.Internal, "AgentENV Runtime returned an empty guest process pid")
	}
	return response.GetPid(), nil
}

// stopContainerProcess 先 SIGTERM，超过宽限期后由 Runtime 发送 SIGKILL。
func (e *Engine) stopContainerProcess(ctx context.Context, record containerRecord) (int32, error) {
	if record.NativeProcessID == 0 {
		return 0, status.Errorf(codes.FailedPrecondition, "container %s has no native process", record.ContainerID)
	}
	mapping, err := e.resolveMapping(engine.SandboxRef{CRISandboxID: record.CRISandboxID})
	if err != nil {
		return 0, err
	}
	if err := e.ensureClients(); err != nil {
		return 0, e.mapRuntimeError(err)
	}
	if e.exec == nil {
		return 0, status.Error(codes.Internal, "AgentENV exec client is not initialized")
	}
	response, err := e.exec.StopProcess(ctx, &runtimeapi.SandboxProcessStopRequest{
		NativeSandboxId:    mapping.NativeSandboxID,
		Pid:                record.NativeProcessID,
		GracePeriodSeconds: uint64(containerProcessStopGrace / time.Second),
	})
	if err != nil {
		if status.Code(e.mapRuntimeError(err)) == codes.NotFound {
			return 0, nil
		}
		return 0, e.mapRuntimeError(err)
	}
	return response.GetExitCode(), nil
}

// monitorContainerProcess 增量消费 Runtime 输出，写入 CRI 日志并维护真实退出状态。
func (e *Engine) monitorContainerProcess(containerID string) {
	record, ok := e.getContainer(containerID)
	if !ok || record.NativeProcessID == 0 {
		return
	}
	mapping, err := e.resolveMapping(engine.SandboxRef{CRISandboxID: record.CRISandboxID})
	if err != nil {
		e.setContainerProcessError(containerID, err)
		return
	}
	var writer *criLogWriter
	openWriter := func() {
		next, err := newCRILogWriter(record.LogPath)
		if err != nil {
			e.setContainerProcessError(containerID, err)
			writer = nil
			return
		}
		writer = next
	}
	logGeneration := record.LogGeneration
	openWriter()
	defer func() {
		if writer != nil {
			_ = writer.Close()
		}
	}()

	after := record.ProcessLogSequence
	ticker := time.NewTicker(containerProcessPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		current, exists := e.getContainer(containerID)
		if !exists || current.State == containerStateRemoved || current.State == containerStateExited {
			return
		}
		if current.LogGeneration != logGeneration {
			if writer != nil {
				_ = writer.Close()
				writer = nil
			}
			record.LogPath = current.LogPath
			openWriter()
			logGeneration = current.LogGeneration
		}
		ctx, cancel := context.WithTimeout(context.Background(), containerProcessAPITimeout)
		response, err := e.exec.GetProcess(ctx, &runtimeapi.SandboxProcessGetRequest{
			NativeSandboxId: mapping.NativeSandboxID,
			Pid:             record.NativeProcessID,
			AfterSequence:   after,
		})
		cancel()
		if err != nil {
			if status.Code(e.mapRuntimeError(err)) == codes.NotFound {
				e.markContainerProcessExited(containerID, -1, "guest process disappeared")
				return
			}
			e.setContainerProcessError(containerID, e.mapRuntimeError(err))
			continue
		}

		next := after
		for _, chunk := range response.GetOutputs() {
			if chunk.GetSequence() <= next {
				continue
			}
			if writer != nil {
				if err := writer.WriteChunk(chunk.GetStream(), chunk.GetData()); err != nil {
					e.setContainerProcessError(containerID, err)
					break
				}
			}
			next = chunk.GetSequence()
		}
		if next != after {
			after = next
			_, _, _ = e.mutateContainer(containerID, func(current *containerRecord) {
				current.ProcessLogSequence = next
			})
		}
		if response.GetOutputTruncated() {
			e.setContainerProcessError(containerID, fmt.Errorf("Runtime process output buffer was truncated"))
		}
		if !response.GetRunning() {
			if writer != nil {
				if err := writer.Flush(); err != nil {
					e.setContainerProcessError(containerID, err)
				}
			}
			e.markContainerProcessExited(containerID, response.GetExitCode(), response.GetErrorMessage())
			return
		}
	}
}

func (e *Engine) markContainerProcessExited(containerID string, exitCode int32, message string) {
	now := time.Now()
	_, _, _ = e.mutateContainer(containerID, func(record *containerRecord) {
		if record.State == containerStateRemoved || record.State == containerStateExited {
			return
		}
		record.State = containerStateExited
		record.ExitCode = exitCode
		record.FinishedAt = now
		if message != "" {
			record.LastError = message
		}
	})
}

func (e *Engine) setContainerProcessError(containerID string, err error) {
	if err == nil {
		return
	}
	message := err.Error()
	if len(message) > 512 {
		message = message[:512]
	}
	_, _, _ = e.mutateContainer(containerID, func(record *containerRecord) {
		record.LastError = message
	})
}

type criLogWriter struct {
	file    *os.File
	partial map[string][]byte
}

func newCRILogWriter(path string) (*criLogWriter, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create container log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open container log: %w", err)
	}
	return &criLogWriter{file: file, partial: make(map[string][]byte)}, nil
}

func (w *criLogWriter) WriteChunk(stream runtimeapi.SandboxProcessOutputStream, data []byte) error {
	if w == nil || w.file == nil || len(data) == 0 {
		return nil
	}
	streamName := "stdout"
	if stream == runtimeapi.SandboxProcessOutputStream_SANDBOX_PROCESS_OUTPUT_STREAM_STDERR {
		streamName = "stderr"
	}
	combined := append(append([]byte(nil), w.partial[streamName]...), data...)
	start := 0
	for index, byteValue := range combined {
		if byteValue != '\n' {
			continue
		}
		if err := w.writeLine(streamName, combined[start:index], "F"); err != nil {
			return err
		}
		start = index + 1
	}
	if start < len(combined) {
		w.partial[streamName] = append([]byte(nil), combined[start:]...)
	} else {
		delete(w.partial, streamName)
	}
	return nil
}

func (w *criLogWriter) Flush() error {
	if w == nil || w.file == nil {
		return nil
	}
	for streamName, data := range w.partial {
		if len(data) == 0 {
			continue
		}
		if err := w.writeLine(streamName, data, "P"); err != nil {
			return err
		}
		delete(w.partial, streamName)
	}
	return nil
}

func (w *criLogWriter) writeLine(streamName string, data []byte, tag string) error {
	_, err := fmt.Fprintf(w.file, "%s %s %s %s\n", time.Now().UTC().Format(time.RFC3339Nano), streamName, tag, string(data))
	return err
}

func (w *criLogWriter) Close() error {
	if w == nil || w.file == nil {
		return nil
	}
	if err := w.Flush(); err != nil {
		_ = w.file.Close()
		return err
	}
	return w.file.Close()
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
