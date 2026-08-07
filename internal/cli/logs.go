package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/stevenstank/forge/internal/runtime"
)

// newLogsCommand builds the `forge logs` subcommand (FR-6.4).
func newLogsCommand() Command {
	return Command{
		Name:    "logs",
		Summary: "show a container's output",
		Exec:    execLogs,
	}
}

// logsFlags are the flags local to `forge logs`.
type logsFlags struct {
	follow     bool
	tail       int
	timestamps bool
}

// newLogsFlagSet builds the flag set for `forge logs`.
func newLogsFlagSet() (*flag.FlagSet, *logsFlags) {
	var local logsFlags

	fs := flag.NewFlagSet("forge logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	fs.BoolVar(&local.follow, "f", false, "keep printing until the container exits")
	fs.IntVar(&local.tail, "n", 0, "print only the last `count` entries (default: all)")
	fs.BoolVar(&local.timestamps, "t", false, "prefix each entry with the time forge received it")

	return fs, &local
}

// execLogs prints a container's captured output.
//
// The container's stdout goes to forge's stdout and its stderr to forge's
// stderr, so a caller redirecting one of them gets what the container actually
// wrote to it — the same thing they would have got from the run itself.
func execLogs(ctx context.Context, env *Env, args []string) error {
	fs, local := newLogsFlagSet()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeLogsUsage(env.Stderr)
			return nil
		}
		return fmt.Errorf("%w: logs: %w", ErrUsage, err)
	}

	ids := fs.Args()
	switch {
	case len(ids) == 0:
		writeLogsUsage(env.Stderr)
		return fmt.Errorf("%w: logs requires a container id", ErrUsage)
	case len(ids) > 1:
		// Unlike stop and rm, this one cannot sensibly take several: two
		// containers' output interleaved in one terminal, with no way to tell
		// which line came from which, is worse than useless.
		return fmt.Errorf("%w: logs takes one container id, got %d", ErrUsage, len(ids))
	}
	if local.tail < 0 {
		return fmt.Errorf("%w: -n must not be negative, got %d", ErrUsage, local.tail)
	}

	runner, err := newRunner(env)
	if err != nil {
		return err
	}

	opts := runtime.LogOptions{
		Follow:     local.follow,
		Tail:       local.tail,
		Timestamps: local.timestamps,
	}

	if err := runner.Logs(ctx, ids[0], opts, env.Stdout, env.Stderr); err != nil {
		if errors.Is(err, context.Canceled) {
			// A user pressing Ctrl-C out of `forge logs -f` got what they
			// asked for. Reporting it as a failure would put an error on the
			// end of every follow anyone ever ends.
			return nil
		}
		if isContainerUserError(err) {
			return &ExitError{Code: ExitUsage, Err: err}
		}
		return err
	}

	return nil
}

// writeLogsUsage prints help for `forge logs`.
func writeLogsUsage(w io.Writer) {
	fmt.Fprint(w, "Usage:\n  forge logs [-f] [-n count] [-t] <container-id>\n\n")
	fmt.Fprint(w, "Prints what the container wrote to stdout and stderr, in the order it\n")
	fmt.Fprint(w, "wrote it. The container's stdout goes to forge's stdout and its stderr to\n")
	fmt.Fprint(w, "forge's stderr, so redirecting either gives you that stream alone.\n\n")
	fmt.Fprint(w, "Output is captured for every container, including attached runs, so this\n")
	fmt.Fprint(w, "works while a container is still running as well as after it has stopped.\n")
	fmt.Fprint(w, "A container's log is removed with the container: it survives the run only\n")
	fmt.Fprint(w, "if the run was given -keep, and forge rm takes it.\n\n")
	fmt.Fprint(w, "-f keeps printing until the container exits. It ends on its own when the\n")
	fmt.Fprint(w, "container does, and can be interrupted at any time.\n\n")
	fmt.Fprint(w, "Flags:\n")

	fs, _ := newLogsFlagSet()
	fs.SetOutput(w)
	fs.PrintDefaults()

	fmt.Fprint(w, "\nExamples:\n")
	fmt.Fprint(w, "  sudo forge logs 7f3c9a1b2d04\n")
	fmt.Fprint(w, "  sudo forge logs -f 7f3c9a1b2d04\n")
	fmt.Fprint(w, "  sudo forge logs -n 20 -t 7f3c9a1b2d04\n")
	fmt.Fprint(w, "  sudo forge logs 7f3c9a1b2d04 2>/dev/null   # stdout only\n")
}
