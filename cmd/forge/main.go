// Command forge is a Docker-inspired container runtime built from scratch.
//
// This package is wiring only: it builds a cancellable context, hands the
// arguments to internal/cli, and translates the result into an exit status.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/stevenstank/forge/internal/cli"
)

func main() {
	// Cancelling on SIGINT/SIGTERM gives long-running operations a chance to
	// run their cleanup paths rather than leaking kernel resources (PRD NFR-5).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)

	// os.Exit skips deferred calls, so release the signal handler explicitly.
	stop()
	os.Exit(code)
}
