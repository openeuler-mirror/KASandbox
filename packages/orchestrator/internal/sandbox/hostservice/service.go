package hostservice

import (
	"context"
	"io"
	"os"
)

type RestartPolicy int

const (
	RestartNever RestartPolicy = iota
	RestartOnCrash
)

type ReadyCheck interface {
	Check(ctx context.Context) error
	String() string
}

type Service struct {
	Name       string
	Binary     string
	Args       []string
	Env        []string
	NetNSName  string
	ExtraFiles []*os.File
	// ParentFiles are retained only by the orchestrator. They keep socketpair
	// and pipe peers alive but are not inherited by the child process.
	ParentFiles []*os.File
	// LogWriter, when non-nil, receives the process's stdout/stderr instead of
	// the default zapio piping into the orchestrator log. Ownership stays with
	// the caller (closed via Cleanup); it is deliberately not closed per
	// (re)start so RestartOnCrash restarts keep appending to it.
	LogWriter     io.Writer
	Cleanup       func()
	RestartPolicy RestartPolicy
	ReadyCheck    ReadyCheck
}

// CloseParentResources releases all descriptors and filesystem resources
// owned by the parent side of a service definition.
func (s Service) CloseParentResources() {
	for _, file := range s.ExtraFiles {
		if file != nil {
			_ = file.Close()
		}
	}
	for _, file := range s.ParentFiles {
		if file != nil {
			_ = file.Close()
		}
	}
	if s.Cleanup != nil {
		s.Cleanup()
	}
}
