package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"sync"
	"syscall"

	"connectrpc.com/connect"
	"github.com/creack/pty"
	"github.com/rs/zerolog"

	"github.com/e2b-dev/infra/packages/envd/internal/execcontext"
	"github.com/e2b-dev/infra/packages/envd/internal/logs"
	"github.com/e2b-dev/infra/packages/envd/internal/permissions"
	"github.com/e2b-dev/infra/packages/envd/internal/platform"
	"github.com/e2b-dev/infra/packages/envd/internal/services/cgroups"
	rpc "github.com/e2b-dev/infra/packages/envd/internal/services/spec/process"
)

const (
	defaultOomScore  = 100
	outputBufferSize = 64
	stdChunkSize     = 2 << 14
	ptyChunkSize     = 2 << 13
)

type ProcessExit struct {
	Error  *string
	Status string
	Exited bool
	Code   int32
}

type Handler struct {
	Config *rpc.ProcessConfig

	logger *zerolog.Logger

	Tag *string
	cmd *exec.Cmd
	tty *os.File

	cancel context.CancelFunc

	outCtx    context.Context //nolint:containedctx // todo: refactor so this can be removed
	outCancel context.CancelFunc

	stdinMu sync.Mutex
	stdin   io.WriteCloser

	DataEvent *MultiplexedChannel[rpc.ProcessEvent_Data]
	EndEvent  *MultiplexedChannel[rpc.ProcessEvent_End]
}

// This method must be called only after the process has been started
func (p *Handler) Pid() uint32 {
	return uint32(p.cmd.Process.Pid)
}

func New(
	ctx context.Context,
	user *user.User,
	req *rpc.StartRequest,
	logger *zerolog.Logger,
	defaults *execcontext.Defaults,
	cgroupManager cgroups.Manager,
	cancel context.CancelFunc,
) (*Handler, error) {
	if err := platform.ValidateProcessRequest(req); err != nil {
		if errors.Is(err, platform.ErrUnsupportedPTY) {
			return nil, connect.NewError(connect.CodeUnimplemented, err)
		}

		return nil, err
	}

	cmd := platform.CommandContext(ctx, req.GetProcess())

	if err := platform.ConfigureProcessCredentials(cmd, user, cgroupManager, getProcType(req), logger); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resolvedPath, err := permissions.ExpandAndResolve(req.GetProcess().GetCwd(), user, defaults.Workdir)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	// Check if the cwd resolved path exists
	if _, err := os.Stat(resolvedPath); errors.Is(err, os.ErrNotExist) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("cwd '%s' does not exist", resolvedPath))
	}

	cmd.Dir = resolvedPath

	platform.ConfigureProcessEnv(cmd, user, req.GetProcess(), defaults)

	outMultiplex := NewMultiplexedChannel[rpc.ProcessEvent_Data](outputBufferSize)

	var outWg sync.WaitGroup

	// Create a context for waiting for and cancelling output pipes.
	// Cancellation of the process via timeout will propagate and cancel this context too.
	outCtx, outCancel := context.WithCancel(ctx)

	h := &Handler{
		Config:    req.GetProcess(),
		cmd:       cmd,
		Tag:       req.Tag,
		DataEvent: outMultiplex,
		cancel:    cancel,
		outCtx:    outCtx,
		outCancel: outCancel,
		EndEvent:  NewMultiplexedChannel[rpc.ProcessEvent_End](0),
		logger:    logger,
	}

	if req.GetPty() != nil {
		// The pty should ideally start only in the Start method, but the package does not support that and we would have to code it manually.
		// The output of the pty should correctly be passed though.
		tty, err := pty.StartWithSize(cmd, &pty.Winsize{
			Cols: uint16(req.GetPty().GetSize().GetCols()),
			Rows: uint16(req.GetPty().GetSize().GetRows()),
		})
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("error starting pty with command '%s' in dir '%s' with '%d' cols and '%d' rows: %w", cmd, cmd.Dir, req.GetPty().GetSize().GetCols(), req.GetPty().GetSize().GetRows(), err))
		}

		outWg.Go(func() {
			for {
				buf := make([]byte, ptyChunkSize)

				n, readErr := tty.Read(buf)

				if n > 0 {
					outMultiplex.Source <- rpc.ProcessEvent_Data{
						Data: &rpc.ProcessEvent_DataEvent{
							Output: &rpc.ProcessEvent_DataEvent_Pty{
								Pty: buf[:n],
							},
						},
					}
				}

				if errors.Is(readErr, io.EOF) {
					break
				}

				if readErr != nil {
					fmt.Fprintf(os.Stderr, "error reading from pty: %s\n", readErr)

					break
				}
			}
		})

		h.tty = tty
	} else {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error creating stdout pipe for command '%s': %w", cmd, err))
		}

		outWg.Go(func() {
			stdoutLogs := make(chan []byte, outputBufferSize)
			defer close(stdoutLogs)

			stdoutLogger := logger.With().Str("event_type", "stdout").Logger()

			go logs.LogBufferedDataEvents(stdoutLogs, &stdoutLogger, "data")

			for {
				buf := make([]byte, stdChunkSize)

				n, readErr := stdout.Read(buf)

				if n > 0 {
					outMultiplex.Source <- rpc.ProcessEvent_Data{
						Data: &rpc.ProcessEvent_DataEvent{
							Output: &rpc.ProcessEvent_DataEvent_Stdout{
								Stdout: buf[:n],
							},
						},
					}

					stdoutLogs <- buf[:n]
				}

				if errors.Is(readErr, io.EOF) {
					break
				}

				if readErr != nil {
					fmt.Fprintf(os.Stderr, "error reading from stdout: %s\n", readErr)

					break
				}
			}
		})

		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error creating stderr pipe for command '%s': %w", cmd, err))
		}

		outWg.Go(func() {
			stderrLogs := make(chan []byte, outputBufferSize)
			defer close(stderrLogs)

			stderrLogger := logger.With().Str("event_type", "stderr").Logger()

			go logs.LogBufferedDataEvents(stderrLogs, &stderrLogger, "data")

			for {
				buf := make([]byte, stdChunkSize)

				n, readErr := stderr.Read(buf)

				if n > 0 {
					outMultiplex.Source <- rpc.ProcessEvent_Data{
						Data: &rpc.ProcessEvent_DataEvent{
							Output: &rpc.ProcessEvent_DataEvent_Stderr{
								Stderr: buf[:n],
							},
						},
					}

					stderrLogs <- buf[:n]
				}

				if errors.Is(readErr, io.EOF) {
					break
				}

				if readErr != nil {
					fmt.Fprintf(os.Stderr, "error reading from stderr: %s\n", readErr)

					break
				}
			}
		})

		// For backwards compatibility we still set the stdin if not explicitly disabled
		// If stdin is disabled, the process will use /dev/null as stdin
		if req.Stdin == nil || req.GetStdin() == true {
			stdin, err := cmd.StdinPipe()
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error creating stdin pipe for command '%s': %w", cmd, err))
			}

			h.stdin = stdin
		}
	}

	go func() {
		outWg.Wait()

		close(outMultiplex.Source)

		outCancel()
	}()

	return h, nil
}

func getProcType(req *rpc.StartRequest) cgroups.ProcessType {
	if req != nil && req.GetPty() != nil {
		return cgroups.ProcessTypePTY
	}

	return cgroups.ProcessTypeUser
}

func (p *Handler) SendSignal(signal syscall.Signal) error {
	if p.cmd.Process == nil {
		return fmt.Errorf("process not started")
	}

	if signal == syscall.SIGKILL || signal == syscall.SIGTERM {
		p.outCancel()
	}

	return platform.SendProcessSignal(p.cmd, signal)
}

func (p *Handler) ResizeTty(size *pty.Winsize) error {
	if p.tty == nil {
		return fmt.Errorf("tty not assigned to process")
	}

	return pty.Setsize(p.tty, size)
}

func (p *Handler) WriteStdin(data []byte) error {
	if p.tty != nil {
		return fmt.Errorf("tty assigned to process — input should be written to the pty, not the stdin")
	}

	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()

	if p.stdin == nil {
		return fmt.Errorf("stdin not enabled or closed")
	}

	_, err := p.stdin.Write(data)
	if err != nil {
		return fmt.Errorf("error writing to stdin of process '%d': %w", p.cmd.Process.Pid, err)
	}

	return nil
}

// CloseStdin closes the stdin pipe to signal EOF to the process.
// Only works for non-PTY processes.
func (p *Handler) CloseStdin() error {
	if p.tty != nil {
		return fmt.Errorf("cannot close stdin for PTY process — send Ctrl+D (0x04) instead")
	}

	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()

	if p.stdin == nil {
		return nil
	}

	err := p.stdin.Close()
	// We still set the stdin to nil even on error as there are no errors,
	// for which it is really safe to retry close across all distributions.
	p.stdin = nil

	return err
}

func (p *Handler) WriteTty(data []byte) error {
	if p.tty == nil {
		return fmt.Errorf("tty not assigned to process — input should be written to the stdin, not the tty")
	}

	_, err := p.tty.Write(data)
	if err != nil {
		return fmt.Errorf("error writing to tty of process '%d': %w", p.cmd.Process.Pid, err)
	}

	return nil
}

func (p *Handler) Start() (uint32, error) {
	// Pty is already started in the New method
	if p.tty == nil {
		err := p.cmd.Start()
		if err != nil {
			return 0, fmt.Errorf("error starting process '%s': %w", p.cmd, err)
		}
	}

	if runtime.GOOS == "linux" {
		if adjustErr := adjustOomScore(p.cmd.Process.Pid, defaultOomScore); adjustErr != nil {
			fmt.Fprintf(os.Stderr, "error adjusting oom score for process '%s': %s\n", p.cmd, adjustErr)
		}
	}

	p.logger.
		Info().
		Str("event_type", "process_start").
		Int("pid", p.cmd.Process.Pid).
		Str("command", p.cmd.String()).
		Msg(fmt.Sprintf("Process with pid %d started", p.cmd.Process.Pid))

	return uint32(p.cmd.Process.Pid), nil
}

func (p *Handler) Wait() {
	// Wait for the output pipes to be closed or cancelled.
	<-p.outCtx.Done()

	err := p.cmd.Wait()

	if p.tty != nil {
		p.tty.Close()
	}

	var errMsg *string

	if err != nil {
		msg := err.Error()
		errMsg = &msg
	}

	endEvent := &rpc.ProcessEvent_EndEvent{
		Error:    errMsg,
		ExitCode: int32(p.cmd.ProcessState.ExitCode()),
		Exited:   p.cmd.ProcessState.Exited(),
		Status:   p.cmd.ProcessState.String(),
	}

	event := rpc.ProcessEvent_End{
		End: endEvent,
	}

	p.EndEvent.Source <- event

	p.logger.
		Info().
		Str("event_type", "process_end").
		Interface("process_result", endEvent).
		Msg(fmt.Sprintf("Process with pid %d ended", p.cmd.Process.Pid))

	// Ensure the process cancel is called to cleanup resources.
	// As it is called after end event and Wait, it should not affect command execution or returned events.
	p.cancel()
}
