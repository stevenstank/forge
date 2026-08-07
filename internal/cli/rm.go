package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/stevenstank/forge/internal/runtime"
)

// newRemoveCommand builds the `forge rm` subcommand (FR-6.6).
func newRemoveCommand() Command {
	return Command{
		Name:    "rm",
		Summary: "remove stopped containers",
		Exec:    execRemove,
	}
}

// rmFlags are the flags local to `forge rm`.
type rmFlags struct {
	force bool
}

// newRmFlagSet builds the flag set for `forge rm`.
func newRmFlagSet() (*flag.FlagSet, *rmFlags) {
	var local rmFlags

	fs := flag.NewFlagSet("forge rm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	fs.BoolVar(&local.force, "f", false, "stop the container first if it is still running")

	return fs, &local
}

// execRemove removes one or more stopped containers.
func execRemove(ctx context.Context, env *Env, args []string) error {
	fs, local := newRmFlagSet()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeRemoveUsage(env.Stderr)
			return nil
		}
		return fmt.Errorf("%w: rm: %w", ErrUsage, err)
	}

	ids := fs.Args()
	if len(ids) == 0 {
		writeRemoveUsage(env.Stderr)
		return fmt.Errorf("%w: rm requires a container id", ErrUsage)
	}

	runner, err := newRunner(env)
	if err != nil {
		return err
	}

	opts := runtime.RemoveOptions{Force: local.force}

	return eachContainer(ctx, env, ids, func(ctx context.Context, id string) error {
		return runner.Remove(ctx, id, opts)
	})
}

// writeRemoveUsage prints help for `forge rm`.
func writeRemoveUsage(w io.Writer) {
	fmt.Fprint(w, "Usage:\n  forge rm [-f] <container-id>...\n\n")
	fmt.Fprint(w, "Removes a stopped container: its root filesystem, its logs, and its\n")
	fmt.Fprint(w, "metadata, along with any cgroup or network resources it still holds.\n\n")
	fmt.Fprint(w, "A running container is refused rather than removed. Deleting the filesystem\n")
	fmt.Fprint(w, "out from under a process executing inside it does not stop the container,\n")
	fmt.Fprint(w, "it corrupts it. Pass -f to stop it first.\n\n")
	fmt.Fprint(w, "Flags:\n")

	fs, _ := newRmFlagSet()
	fs.SetOutput(w)
	fs.PrintDefaults()

	fmt.Fprint(w, "\nExamples:\n")
	fmt.Fprint(w, "  sudo forge rm 7f3c9a1b2d04\n")
	fmt.Fprint(w, "  sudo forge rm -f 7f3c9a1b2d04\n")
	fmt.Fprint(w, "  sudo forge rm $(sudo forge ps -a -q)\n")
}
