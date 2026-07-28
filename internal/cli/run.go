package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/stevenstank/forge/internal/runtime"
)

// newRunCommand builds the `forge run` subcommand.
func newRunCommand() Command {
	return Command{
		Name:    "run",
		Summary: "run a command in an isolated container",
		Exec:    execRun,
	}
}

// runFlags are the flags local to `forge run`.
type runFlags struct {
	hostname string
}

// execRun parses `forge run` arguments and hands a Spec to the runtime.
//
// Per SSOT §13.6 there is no logic here beyond translating arguments into a
// Spec and a Status into an exit code.
func execRun(ctx context.Context, env *Env, args []string) error {
	var local runFlags

	fs := flag.NewFlagSet("forge run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.StringVar(&local.hostname, "hostname", "", "hostname inside the container (default: the container ID)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeRunUsage(env.Stderr, fs)
			return nil
		}
		return fmt.Errorf("%w: run: %w", ErrUsage, err)
	}

	command := fs.Args()
	if len(command) == 0 {
		writeRunUsage(env.Stderr, fs)
		return fmt.Errorf("%w: run requires a command to execute", ErrUsage)
	}

	spec := runtime.Spec{
		Command:  command,
		Hostname: local.hostname,
		Stdin:    env.Stdin,
		Stdout:   env.Stdout,
		Stderr:   env.Stderr,
		// Stage 1 runs a host binary and has no image config to draw an
		// environment from, so the container gets a minimal, explicit one.
		Env: defaultContainerEnv(),
	}

	status, err := runtime.NewRunner(env.Logger).Run(ctx, spec)
	if err != nil {
		if errors.Is(err, runtime.ErrNoCommand) || errors.Is(err, runtime.ErrNotAPath) {
			return fmt.Errorf("%w: %w", ErrUsage, err)
		}
		return err
	}

	// FR-1.4: report the container's exit code. Forge exits with the
	// container's status, as Docker does. See ADR-0009.
	if status.Code != 0 {
		return &ExitError{Code: status.Code}
	}

	return nil
}

// defaultContainerEnv is the environment Stage 1 gives a container.
//
// It is deliberately minimal and explicit: nothing is inherited from the host.
// PATH is included because almost every program expects one to exist.
func defaultContainerEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
}

// writeRunUsage prints help for `forge run`.
func writeRunUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, "Usage:\n  forge run [flags] <path> [args...]\n\n")
	fmt.Fprint(w, "Runs <path> as PID 1 inside new PID, UTS and mount namespaces.\n")
	fmt.Fprint(w, "<path> is a path on the host filesystem; forge does not search PATH.\n\n")
	fmt.Fprint(w, "Flags:\n")

	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)

	fmt.Fprint(w, "\nExample:\n  sudo forge run /bin/echo hello from forge\n")
}

// newInitCommand builds Forge's internal re-exec entry point.
//
// It is hidden because it is not a user-facing verb: `forge run` starts it, it
// runs inside the container's namespaces, and it replaces itself with the
// container's binary. See ADR-0008.
func newInitCommand() Command {
	return Command{
		Name:   runtime.InitCommandName,
		Hidden: true,
		Exec:   execInit,
	}
}

// execInit runs the container's init. It returns only on failure; on success
// execve has already replaced the process.
func execInit(_ context.Context, _ *Env, _ []string) error {
	if err := runtime.Init(); err != nil {
		return &ExitError{Code: runtime.InitExitCode, Err: err}
	}

	// Unreachable in practice: Init either fails or never returns.
	return &ExitError{Code: runtime.InitExitCode, Err: os.ErrInvalid}
}
