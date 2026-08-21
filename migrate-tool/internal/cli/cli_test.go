package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestSubcommandHelpSucceedsWithoutRequiredArguments(t *testing.T) {
	commands := [][]string{
		{"list", "--help"},
		{"export", "--help"},
		{"inspect", "--help"},
		{"verify", "--help"},
		{"import", "--help"},
	}
	for _, args := range commands {
		t.Run(args[0], func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := (CLI{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), args)
			if err != nil {
				t.Fatalf("Run(%v) returned %v", args, err)
			}
		})
	}
}

func TestInvalidFormatIsRejectedBeforeIO(t *testing.T) {
	tests := [][]string{
		{"list", "--format", "yaml"},
		{"import", "missing.bundle", "--target-team", "slug:runtime", "--format", "yaml", "--apply"},
	}
	for _, args := range tests {
		t.Run(args[0], func(t *testing.T) {
			err := (CLI{}).Run(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), "format must be table or json") {
				t.Fatalf("Run(%v) error = %v", args, err)
			}
		})
	}
}

func TestCommandsRejectUnexpectedPositionals(t *testing.T) {
	tests := [][]string{
		{"version", "unexpected"},
		{"list", "unexpected"},
	}
	for _, args := range tests {
		t.Run(args[0], func(t *testing.T) {
			err := (CLI{}).Run(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
				t.Fatalf("Run(%v) error = %v", args, err)
			}
		})
	}
}
