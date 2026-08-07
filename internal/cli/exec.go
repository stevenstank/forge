package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/stevenstank/forge/internal/runtime"
)

// newExecCommand builds the `forge exec` subcommand (FR-6.2).
func newExecCommand() Command {
	return Command{
		Name:    "exec",
		Summary: "run a command inside a running container",
		Exec:    execExec,
	}
}

// execFlags are the flags local to `forge exec`.
type execFlags struct {
	workdir string
	env     envList
}

// envList collects repeated -env flags.
type envList []string

// String implements flag.Value.
func (l *envList) String() string {
	if l == nil {
		return ""
	}
	return fmt.Sprint(len(*l), " variables")
}

// Set implements flag.Value.
func (l *envList) Set(assignment string) error {
	if assignment == "" {
		return errors.New("an environment entry must be NAME=VALUE")
	}
	*l = append(*l, assignment)
	return nil
}

// newExecFlagSet builds the flag set for `forge exec`.
func newExecFlagSet() (*flag.FlagSet, *execFlags) {
	var local execFlags

	fs := flag.NewFlagSet("forge exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	fs.StringVar(&local.workdir, "workdir", "", "working `directory` inside the container (default: /)")
	fs.Var(&local.env, "env", "environment entry `NAME=VALUE`, repeatable")

	return fs, &local
}

// execExec parses `forge exec` arguments and hands them to the runtime.
//
// Flag parsing stops at the first non-flag argument, which is what makes
// `forge exec <id> ls -l` work: the -l belongs to ls, not to forge. Go's flag
// package does this by default, and it is the behaviour the command needs
// rather than an accident.
func execExec(ctx context.Context, env *Env, args []string) error {
	fs, local := newExecFlagSet()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeExecUsage(env.Stderr)
			return nil
		}
		return fmt.Errorf("%w: exec: %w", ErrUsage, err)
	}

	positional := fs.Args()
	if len(positional) < 2 {
		writeExecUsage(env.Stderr)
		return fmt.Errorf("%w: exec requires a container id and a command", ErrUsage)
	}

	runner, err := newRunner(env)
	if err != nil {
		return err
	}

	spec := runtime.ExecSpec{
		ID:         positional[0],
		Command:    positional[1:],
		WorkingDir: local.workdir,
		Stdin:      env.Stdin,
		Stdout:     env.Stdout,
		Stderr:     env.Stderr,
	}
	if len(local.env) > 0 {
		spec.Env = append(runtime.DefaultEnv(), local.env...)
	}

	status, err := runner.Exec(ctx, spec)
	if err != nil {
		if isExecUserError(err) {
			return &ExitError{Code: ExitUsage, Err: err}
		}
		return err
	}

	// The command's own exit status is propagated, as `forge run` propagates a
	// container's (ADR-0009). A caller scripting `forge exec ... test -f /x`
	// needs the answer in $?, not in a message.
	if status.Code != 0 {
		return &ExitError{Code: status.Code}
	}

	return nil
}

// isExecUserError reports whether an exec failure is the caller's fault.
//
// An unknown container, a stopped one, and a command that is not in the
// container are all things the user can see and fix from what they typed. A
// failure to join a namespace is not.
func isExecUserError(err error) bool {
	if isContainerUserError(err) {
		return true
	}

	for _, sentinel := range []error{runtime.ErrNoCommand, runtime.ErrCommandNotFound} {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	return false
}

// writeExecUsage prints help for `forge exec`.
func writeExecUsage(w io.Writer) {
	fmt.Fprint(w, "Usage:\n  forge exec [flags] <container-id> <cmd> [args...]\n\n")
	fmt.Fprint(w, "Runs a command inside a container that is already running, in the\n")
	fmt.Fprint(w, "container's mount, PID, network, UTS and IPC namespaces and in its cgroup.\n")
	fmt.Fprint(w, "The command sees the container's filesystem, its processes and its network,\n")
	fmt.Fprint(w, "and counts against its resource limits.\n\n")
	fmt.Fprint(w, "The command is resolved inside the container: a path is used as given, and\n")
	fmt.Fprint(w, "a bare name is searched for on PATH — the container's PATH, not the host's.\n\n")
	fmt.Fprint(w, "The container must be running. A container that has stopped is refused\n")
	fmt.Fprint(w, "rather than started, and the exec'd command dies with the container if the\n")
	fmt.Fprint(w, "container is stopped while it is running.\n\n")
	fmt.Fprint(w, "forge exits with the command's own exit status.\n\n")
	fmt.Fprint(w, "Flags stop at the container id, so flags after it belong to the command.\n\n")
	fmt.Fprint(w, "Flags:\n")

	fs, _ := newExecFlagSet()
	fs.SetOutput(w)
	fs.PrintDefaults()

	fmt.Fprint(w, "\nExamples:\n")
	fmt.Fprint(w, "  sudo forge exec 7f3c9a1b2d04 /bin/ls /etc\n")
	fmt.Fprint(w, "  sudo forge exec 7f3c9a1b2d04 ps -ef\n")
	fmt.Fprint(w, "  sudo forge exec -workdir /tmp 7f3c9a1b2d04 /bin/sh\n")
	fmt.Fprint(w, "  sudo forge exec -env DEBUG=1 7f3c9a1b2d04 /bin/env\n")
}
