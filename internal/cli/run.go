package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/stevenstank/forge/internal/cgroup"
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
//
// The resource limits are captured as strings and parsed together in
// parseLimits rather than through flag.Value, so every limit is reported by
// parseRunSpec — the one unit-testable seam this package has for argument
// handling (SSOT §13.6).
type runFlags struct {
	hostname string
	rootfs   string
	mounts   mountList
	readOnly bool
	workdir  string

	memory    string
	cpus      string
	cpuWeight string
	pids      string
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

	// Resource limits (FR-3.2 to FR-3.4). Every default is empty rather than a
	// number: an unset flag means "the caller asked for nothing", and what a
	// container gets when nobody asks is the runtime's decision, not the CLI's
	// (SSOT §2). "max" is how a caller says "explicitly unlimited".
	fs.StringVar(&local.memory, "memory", "", "memory limit, such as `128m`, 1g or max (default: unlimited)")
	fs.StringVar(&local.cpus, "cpus", "", "CPU limit in cores, such as `1.5` or max (default: unlimited)")
	fs.StringVar(&local.cpuWeight, "cpu-weight", "", "relative CPU share from 1 to 10000, such as `512` (default: the kernel's 100)")
	fs.StringVar(&local.pids, "pids", "", "maximum number of processes, such as `64` or max (default: unlimited)")

	return fs, &local
}

// parseLimits turns the resource-limit flags into the typed value the runtime
// and internal/cgroup share.
//
// The unit arithmetic is not here: each flag hands its string to the parser
// that lives next to the type defining the unit (SSOT §13.6). All this function
// contributes is which flag carried the value, so a rejected limit names the
// flag the user actually typed instead of failing anonymously.
//
// A flag left unset leaves its field nil, which is how "the caller asked for
// nothing" survives all the way down to internal/cgroup writing no file at all.
// Zero is a real value for memory.max and pids.max and cannot carry that
// meaning, which is why Limits uses pointers.
func parseLimits(local *runFlags) (cgroup.Limits, error) {
	var limits cgroup.Limits

	if local.memory != "" {
		memory, err := cgroup.ParseBytes(local.memory)
		if err != nil {
			return cgroup.Limits{}, fmt.Errorf("-memory: %w", err)
		}
		limits.MemoryMax = &memory
	}
	if local.cpus != "" {
		quota, err := cgroup.ParseCPUs(local.cpus)
		if err != nil {
			return cgroup.Limits{}, fmt.Errorf("-cpus: %w", err)
		}
		limits.CPU = &quota
	}
	if local.cpuWeight != "" {
		weight, err := cgroup.ParseWeight(local.cpuWeight)
		if err != nil {
			return cgroup.Limits{}, fmt.Errorf("-cpu-weight: %w", err)
		}
		limits.CPUWeight = &weight
	}
	if local.pids != "" {
		pids, err := cgroup.ParsePIDs(local.pids)
		if err != nil {
			return cgroup.Limits{}, fmt.Errorf("-pids: %w", err)
		}
		limits.PIDsMax = &pids
	}

	return limits, nil
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

	limits, err := parseLimits(local)
	if err != nil {
		return runtime.Spec{}, err
	}

	spec := runtime.Spec{
		Command:      command,
		Hostname:     local.hostname,
		Rootfs:       local.rootfs,
		Mounts:       local.mounts,
		ReadonlyRoot: local.readOnly,
		WorkingDir:   local.workdir,
		Limits:       limits,
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
// used is a bad argument, however late Forge discovers it. So is a limit the
// kernel would refuse — parseLimits catches every value it can judge on its
// own, but a combination only Limits.Validate rejects still reaches here as
// cgroup.ErrInvalidLimit, and it is no less the caller's fault for arriving
// late.
//
// The environment sentinels are deliberately absent: a host without a cgroup v2
// hierarchy, or a kernel not offering a controller, is not something the user
// typed wrong, and reporting it as a usage error would send them looking at
// their command line instead of their machine.
func isUserError(err error) bool {
	for _, sentinel := range []error{
		cgroup.ErrInvalidLimit,
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
	fmt.Fprint(w, "Every container gets a cgroup v2 leaf for accounting. -memory, -cpus,\n")
	fmt.Fprint(w, "-cpu-weight and -pids constrain it; a limit left unset is inherited rather\n")
	fmt.Fprint(w, "than capped. Pass \"max\" to ask for no limit explicitly.\n\n")
	fmt.Fprint(w, "Flags:\n")

	fs, _ := newRunFlagSet()
	fs.SetOutput(w)
	fs.PrintDefaults()

	fmt.Fprint(w, "\nExamples:\n")
	fmt.Fprint(w, "  sudo forge run /bin/echo hello from forge\n")
	fmt.Fprint(w, "  sudo forge run -rootfs /srv/alpine /bin/sh\n")
	fmt.Fprint(w, "  sudo forge run -rootfs /srv/alpine -mount /srv/data:/data:ro /bin/ls /data\n")
	fmt.Fprint(w, "  sudo forge run -memory 128m -pids 64 /bin/sh\n")
	fmt.Fprint(w, "  sudo forge run -cpus 1.5 -cpu-weight 512 /bin/sh\n")
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
