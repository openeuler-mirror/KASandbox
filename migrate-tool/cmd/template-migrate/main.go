package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	command := cli.CLI{Stdout: os.Stdout, Stderr: os.Stderr}
	if err := command.Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if cli.IsConflict(err) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
