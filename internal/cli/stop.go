package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/stevenstank/forge/internal/runtime"
)

// newStopCommand builds the `forge stop` subcommand (FR-6.3).
func newStopCommand() Command {
	return Command{
		Name:    "stop",
		Summary: "stop running containers",
		Exec:    execStop,
	}
}

// stopFlags are the flags local to `forge stop`.
type stopFlags struct {
	timeout int
	remove  bool
}

// newStopFlagSet builds the flag set for `forge stop`.
func newStopFlagSet() (*flag.FlagSet, *stopFlags) {
	var local stopFlags

	fs := flag.NewFlagSet("forge stop", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	fs.IntVar(&local.timeout, "t", int(runtime.DefaultStopTimeout.Seconds()),
		"`seconds` to wait after SIGTERM before sending SIGKILL")
	fs.BoolVar(&local.remove, "rm", false, "remove the container once it has stopped")

	return fs, &local
}

// execStop stops one or more containers.
//
// Each ID is stopped in turn and the failures are collected rather than
// short-circuited: a user stopping four containers should not have the last
// three left running because the first one was already gone.
func execStop(ctx context.Context, env *Env, args []string) error {
	fs, local := newStopFlagSet()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeStopUsage(env.Stderr)
			return nil
		}
		return fmt.Errorf("%w: stop: %w", ErrUsage, err)
	}

	ids := fs.Args()
	if len(ids) == 0 {
		writeStopUsage(env.Stderr)
		return fmt.Errorf("%w: stop requires a container id", ErrUsage)
	}
	if local.timeout < 0 {
		return fmt.Errorf("%w: -t must not be negative, got %d", ErrUsage, local.timeout)
	}

	runner, err := newRunner(env)
	if err != nil {
		return err
	}

	opts := runtime.StopOptions{
		// A -t of 0 means "do not wait", which is a legitimate request and is
		// not the same as "unset". The flag defaults to the runtime's own
		// timeout so that zero can carry that meaning.
		Timeout: time.Duration(local.timeout) * time.Second,
		Remove:  local.remove,
	}
	if local.timeout == 0 {
		// The runtime reads a zero timeout as "use the default", so a
		// deliberate zero is expressed as the smallest wait there is.
		opts.Timeout = time.Nanosecond
	}

	return eachContainer(ctx, env, ids, func(ctx context.Context, id string) error {
		return runner.Stop(ctx, id, opts)
	})
}

// eachContainer applies op to every id, printing the ones that succeed and
// collecting the errors of the ones that do not.
//
// Printing the ID on success is SSOT §9: every command that mutates state
// writes the affected container ID to stdout, so a shell can pipe it onward.
func eachContainer(ctx context.Context, env *Env, ids []string, op func(context.Context, string) error) error {
	var errs []error

	for _, id := range ids {
		if err := op(ctx, id); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintln(env.Stdout, id)
	}

	if len(errs) == 0 {
		return nil
	}

	joined := errors.Join(errs...)
	if isContainerUserError(joined) {
		// Exit 1, as SSOT §9 requires of a user error — but through ExitError
		// rather than ErrUsage, which would print the whole of forge's usage
		// text. Nothing about the command line was wrong: the user typed a
		// well-formed `forge stop <id>` and the container was not there. Help
		// they did not ask for buries the one line they need.
		return &ExitError{Code: ExitUsage, Err: joined}
	}

	return joined
}

// isContainerUserError reports whether a Stage 6 failure is the caller's fault
// rather than Forge's, and so should exit 1 rather than 2 (SSOT §9).
//
// An unknown container and a container that is still running are both things
// the user can see and fix from what they typed. A container that would not die
// after SIGKILL is not: that is a kernel-level problem, and telling the user to
// check their command line would send them looking in the wrong place.
func isContainerUserError(err error) bool {
	for _, sentinel := range []error{
		runtime.ErrNotFound,
		runtime.ErrRunning,
		runtime.ErrNotRunning,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// writeStopUsage prints help for `forge stop`.
func writeStopUsage(w io.Writer) {
	fmt.Fprint(w, "Usage:\n  forge stop [-t seconds] [-rm] <container-id>...\n\n")
	fmt.Fprint(w, "Sends SIGTERM to the container's init process, waits, and sends SIGKILL if\n")
	fmt.Fprint(w, "it is still running. The container's network and cgroup are released as it\n")
	fmt.Fprint(w, "goes; its filesystem and its record are kept for forge ps -a and forge rm,\n")
	fmt.Fprint(w, "unless -rm is given.\n\n")
	fmt.Fprint(w, "Note that a container's init is PID 1 of its own PID namespace, and the\n")
	fmt.Fprint(w, "kernel discards a signal from outside that namespace unless the process\n")
	fmt.Fprint(w, "installed a handler for it. A shell, or anything that does not handle\n")
	fmt.Fprint(w, "SIGTERM, will therefore always wait out -t and then be killed.\n\n")
	fmt.Fprint(w, "Stopping a container that has already stopped is not an error.\n\n")
	fmt.Fprint(w, "Flags:\n")

	fs, _ := newStopFlagSet()
	fs.SetOutput(w)
	fs.PrintDefaults()

	fmt.Fprint(w, "\nExamples:\n")
	fmt.Fprint(w, "  sudo forge stop 7f3c9a1b2d04\n")
	fmt.Fprint(w, "  sudo forge stop -t 30 7f3c9a1b2d04\n")
	fmt.Fprint(w, "  sudo forge stop -rm 7f3c9a1b2d04\n")
}
