package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/stevenstank/forge/internal/runtime"
)

// newPsCommand builds the `forge ps` subcommand (FR-6.1).
func newPsCommand() Command {
	return Command{
		Name:    "ps",
		Summary: "list containers",
		Exec:    execPs,
	}
}

// psFlags are the flags local to `forge ps`.
type psFlags struct {
	all   bool
	quiet bool
}

// newPsFlagSet builds the flag set for `forge ps`.
func newPsFlagSet() (*flag.FlagSet, *psFlags) {
	var local psFlags

	fs := flag.NewFlagSet("forge ps", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	fs.BoolVar(&local.all, "a", false, "include containers that have stopped")
	fs.BoolVar(&local.quiet, "q", false, "print only container IDs")

	return fs, &local
}

// execPs lists containers.
func execPs(_ context.Context, env *Env, args []string) error {
	fs, local := newPsFlagSet()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writePsUsage(env.Stderr)
			return nil
		}
		return fmt.Errorf("%w: ps: %w", ErrUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: ps takes no arguments, got %q", ErrUsage, fs.Arg(0))
	}

	runner, err := newRunner(env)
	if err != nil {
		return err
	}

	containers, stateErrs := runner.List(local.all)

	// Unreadable records are reported without hiding the containers that are
	// fine. A user running ps because something is wrong is the last person
	// who should be shown nothing at all (SSOT §13.7).
	for _, err := range stateErrs {
		fmt.Fprintf(env.Stderr, "forge: %v\n", err)
	}

	if local.quiet {
		for _, c := range containers {
			fmt.Fprintln(env.Stdout, c.ID)
		}
		return nil
	}

	return writeContainerTable(env.Stdout, containers)
}

// writeContainerTable prints the ps table.
//
// tabwriter rather than fixed widths because a container ID is 12 characters
// but an image reference is whatever the user typed, and a table that wraps is
// harder to read than one that is wide.
//
// The flush is the only write here that can fail in a way worth reporting: it
// is where the buffered table actually reaches the terminal, so a failure means
// the user saw nothing rather than saw it badly.
func writeContainerTable(w io.Writer, containers []runtime.Container) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)

	fmt.Fprintln(tw, "CONTAINER ID\tIMAGE\tCOMMAND\tSTATUS\tCREATED\tPID")
	for _, c := range containers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			c.ID,
			orNone(c.Image),
			truncate(strings.Join(c.Command, " "), 24),
			describeStatus(c),
			humaniseAge(time.Since(c.Created)),
			describePID(c),
		)
	}

	return tw.Flush()
}

// describeStatus renders a container's status the way a user reads it: the
// state, and for a container that has finished, how it finished.
func describeStatus(c runtime.Container) string {
	if c.Running() || c.ExitCode == nil {
		return c.Status
	}

	return fmt.Sprintf("%s (%d)", c.Status, *c.ExitCode)
}

// describePID renders the container's PID, or a dash for a container that has
// no process — one that never started, or one that has finished.
func describePID(c runtime.Container) string {
	if c.PID == 0 || !c.Running() {
		return "-"
	}
	return fmt.Sprintf("%d", c.PID)
}

// orNone renders an empty field as a dash, so a column is never blank in a way
// that looks like a formatting bug.
func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// truncate shortens s to at most limit characters, marking what it cut with an
// ellipsis. A container's command is frequently a shell one-liner, and printing
// all of it would push every column after it off the screen.
func truncate(s string, limit int) string {
	if s == "" {
		return "-"
	}

	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}

	return string(runes[:limit-1]) + "…"
}

// humaniseAge renders a duration the way Docker does, because this column is
// read at a glance rather than measured.
func humaniseAge(d time.Duration) string {
	switch {
	case d < 0:
		// A record created in the future: a clock that moved backwards, which
		// is worth showing rather than rendering as a huge negative age.
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds ago", int(d.Seconds()))
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Hours()/24), "day")
	}
}

// plural renders "1 minute ago" and "2 minutes ago".
func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s ago", unit)
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

// writePsUsage prints help for `forge ps`.
func writePsUsage(w io.Writer) {
	fmt.Fprint(w, "Usage:\n  forge ps [-a] [-q]\n\n")
	fmt.Fprint(w, "Lists containers. By default only those that are still live — created,\n")
	fmt.Fprint(w, "running, or stopping — are shown; -a includes the ones that have finished\n")
	fmt.Fprint(w, "and are waiting for forge rm.\n\n")
	fmt.Fprint(w, "A container appears here only if it was started with -keep, or is still\n")
	fmt.Fprint(w, "running: an attached forge run removes its own record when it exits.\n\n")
	fmt.Fprint(w, "Flags:\n")

	fs, _ := newPsFlagSet()
	fs.SetOutput(w)
	fs.PrintDefaults()

	fmt.Fprint(w, "\nExamples:\n")
	fmt.Fprint(w, "  sudo forge ps\n")
	fmt.Fprint(w, "  sudo forge ps -a\n")
	fmt.Fprint(w, "  sudo forge ps -a -q\n")
}
