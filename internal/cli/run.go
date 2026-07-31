package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/rootfs"
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

// errNoCommandGiven reports `forge run` with no command after its flags. It is
// separate from a flag parse error because it prints usage rather than a
// complaint about a flag.
var errNoCommandGiven = errors.New("run requires a command to execute")

// runFlags are the flags local to `forge run`.
type runFlags struct {
	hostname string
	rootfs   string
	mounts   mountList
	readOnly bool
	workdir  string
}

// mountList collects repeated -mount flags. Parsing each one immediately means
// a malformed spec is reported with the flag that carried it, rather than as a
// failure much later with no clue which of several mounts was wrong.
type mountList []mount.Mount

// String implements flag.Value.
func (l *mountList) String() string {
	if l == nil || len(*l) == 0 {
		return ""
	}
	specs := make([]string, 0, len(*l))
	for _, m := range *l {
		specs = append(specs, m.Source+":"+m.Destination)
	}
	return strings.Join(specs, " ")
}

// Set implements flag.Value.
func (l *mountList) Set(spec string) error {
	m, err := mount.ParseMountSpec(spec)
	if err != nil {
		return err
	}
	*l = append(*l, m)
	return nil
}

// newRunFlagSet builds the flag set for `forge run`. Errors and usage are
// silenced because execRun formats both itself.
func newRunFlagSet() (*flag.FlagSet, *runFlags) {
	var local runFlags

	fs := flag.NewFlagSet("forge run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	fs.StringVar(&local.hostname, "hostname", "", "hostname inside the container (default: the container ID)")
	fs.StringVar(&local.rootfs, "rootfs", "", "host directory to use as the container's root filesystem")
	fs.Var(&local.mounts, "mount", "bind mount `src:dst[:ro,nosuid,nodev,noexec]`, repeatable")
	fs.BoolVar(&local.readOnly, "read-only", false, "mount the container's root filesystem read-only")
	fs.StringVar(&local.workdir, "workdir", "", "working directory inside the container (default: /)")

	return fs, &local
}

// parseRunSpec turns `forge run` arguments into a Spec.
//
// It is separate from execRun so the whole of the CLI's actual work — mapping
// arguments onto a Spec — is testable without starting a container, and so
// without root (SSOT §13.6). The returned Spec has no streams attached; that is
// execRun's job.
func parseRunSpec(args []string) (runtime.Spec, error) {
	fs, local := newRunFlagSet()

	if err := fs.Parse(args); err != nil {
		return runtime.Spec{}, err
	}

	command := fs.Args()
	if len(command) == 0 {
		return runtime.Spec{}, errNoCommandGiven
	}

	spec := runtime.Spec{
		Command:      command,
		Hostname:     local.hostname,
		Rootfs:       local.rootfs,
		Mounts:       local.mounts,
		ReadonlyRoot: local.readOnly,
		WorkingDir:   local.workdir,
	}
	if err := spec.Validate(); err != nil {
		return runtime.Spec{}, err
	}

	return spec, nil
}

// execRun parses `forge run` arguments and hands a Spec to the runtime.
//
// Per SSOT §13.6 there is no logic here beyond translating arguments into a
// Spec and a Status into an exit code.
func execRun(ctx context.Context, env *Env, args []string) error {
	spec, err := parseRunSpec(args)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			writeRunUsage(env.Stderr)
			return nil
		case errors.Is(err, errNoCommandGiven):
			writeRunUsage(env.Stderr)
			return fmt.Errorf("%w: %w", ErrUsage, err)
		default:
			return fmt.Errorf("%w: run: %w", ErrUsage, err)
		}
	}

	spec.Stdin, spec.Stdout, spec.Stderr = env.Stdin, env.Stdout, env.Stderr
	// Stage 2 runs a binary from a root filesystem that carries no image
	// config, so the container's environment is still minimal and explicit.
	spec.Env = defaultContainerEnv()

	runner, err := runtime.NewRunner(env.Logger, runtime.Config{Root: env.Opts.Root})
	if err != nil {
		return err
	}

	status, err := runner.Run(ctx, spec)
	if err != nil {
		if isUserError(err) {
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

// isUserError reports whether an error is the caller's fault rather than
// Forge's, and so should exit 1 rather than 2 (SSOT §9).
//
// Most of what can go wrong once a container is starting is Forge's problem.
// These are the exceptions: a root filesystem the user named that cannot be
// used is a bad argument, however late Forge discovers it.
func isUserError(err error) bool {
	for _, sentinel := range []error{
		runtime.ErrNoCommand,
		runtime.ErrNotAPath,
		runtime.ErrRootfsNotAbsolute,
		runtime.ErrWorkingDirNotAbsolute,
		runtime.ErrMountWithoutRootfs,
		rootfs.ErrSourceNotFound,
		rootfs.ErrSourceNotADirectory,
		rootfs.ErrSourceIsHostRoot,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// defaultContainerEnv is the environment Forge gives a container.
//
// It is deliberately minimal and explicit: nothing is inherited from the host.
// PATH is included because almost every program expects one to exist.
func defaultContainerEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
}

// writeRunUsage prints help for `forge run`.
func writeRunUsage(w io.Writer) {
	fmt.Fprint(w, "Usage:\n  forge run [flags] <path> [args...]\n\n")
	fmt.Fprint(w, "Runs <path> as PID 1 inside new PID, UTS and mount namespaces.\n")
	fmt.Fprint(w, "<path> is resolved inside the container; forge does not search PATH.\n\n")
	fmt.Fprint(w, "Without -rootfs the container shares the host's filesystem. With it, the\n")
	fmt.Fprint(w, "container gets its own root filesystem via pivot_root, and host directories\n")
	fmt.Fprint(w, "can be bind-mounted in with -mount.\n\n")
	fmt.Fprint(w, "Flags:\n")

	fs, _ := newRunFlagSet()
	fs.SetOutput(w)
	fs.PrintDefaults()

	fmt.Fprint(w, "\nExamples:\n")
	fmt.Fprint(w, "  sudo forge run /bin/echo hello from forge\n")
	fmt.Fprint(w, "  sudo forge run -rootfs /srv/alpine /bin/sh\n")
	fmt.Fprint(w, "  sudo forge run -rootfs /srv/alpine -mount /srv/data:/data:ro /bin/ls /data\n")
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
