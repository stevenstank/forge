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
	"github.com/stevenstank/forge/internal/image"
	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/network"
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

	network string
	mtu     int

	keep bool
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

	// Networking (FR-4.1 to FR-4.4). Empty is passed straight through for the
	// same reason the limits are: which network a container gets when nobody
	// asks is the runtime's decision, not the CLI's (SSOT §2). The runtime's
	// answer is bridge, which the usage text below states so it is not a
	// secret kept in a struct tag.
	fs.StringVar(&local.network, "network", "", "network `mode`: bridge, none or host (default: bridge)")
	fs.IntVar(&local.mtu, "mtu", 0, "MTU of the container's interface (default: the kernel's)")

	// Retention (FR-6.1, FR-6.6). The default is to leave nothing behind,
	// which is what every stage so far has done and what PRD §10.4 asks of the
	// test suite. -keep is how a user asks for a container that outlives its
	// run, so that forge ps -a can list it and forge rm removes it.
	fs.BoolVar(&local.keep, "keep", false, "keep the container's record and filesystem after it exits, for forge ps -a and forge rm")

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

// splitImageAndCommand decides whether the first positional argument names an
// image or is the command itself.
//
// Stage 5 adds a positional `<image>` to a verb whose first positional has meant
// "the command" since Stage 1, and both grammars have to keep working:
//
//	forge run [flags] <image> [cmd] [args...]      new: the command is optional
//	forge run [flags] -rootfs <dir> <cmd> [args…]  Stage 2–4, unchanged
//	forge run [flags] <cmd-path> [args...]         Stage 1, unchanged
//
// Three rules, applied in order, decide it without a lookahead and without a
// new flag:
//
//  1. -rootfs was given, so the container's filesystem is already named and the
//     positionals are the command. Exactly Stages 2 to 4.
//  2. The first positional begins with "/", "./" or "../", so it is a path.
//     Exactly Stage 1.
//  3. Otherwise it is an image reference, and the rest is the command.
//
// The two namespaces cannot overlap, which is what makes this unambiguous
// rather than merely conventional: a command Forge accepts without an image
// must be an absolute path (runtime.ErrNotAPath, Stage 1), and a registry
// reference can never begin with "/" — "docker.io/library/alpine" has slashes,
// but not a leading one.
func splitImageAndCommand(rootfsFlag string, positional []string) (imageRef string, command []string) {
	if rootfsFlag != "" || isCommandPath(positional[0]) {
		return "", positional
	}
	return positional[0], positional[1:]
}

// isCommandPath reports whether an argument is written as a path rather than as
// an image reference.
func isCommandPath(arg string) bool {
	return strings.HasPrefix(arg, "/") ||
		strings.HasPrefix(arg, "./") ||
		strings.HasPrefix(arg, "../")
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

	positional := fs.Args()
	if len(positional) == 0 {
		return runtime.Spec{}, errNoCommandGiven
	}

	imageRef, command := splitImageAndCommand(local.rootfs, positional)

	limits, err := parseLimits(local)
	if err != nil {
		return runtime.Spec{}, err
	}

	spec := runtime.Spec{
		Command:      command,
		Image:        imageRef,
		Hostname:     local.hostname,
		Rootfs:       local.rootfs,
		Mounts:       local.mounts,
		ReadonlyRoot: local.readOnly,
		WorkingDir:   local.workdir,
		Limits:       limits,
		Network:      network.Mode(local.network),
		NetworkMTU:   local.mtu,
		Keep:         local.keep,
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

	// A container with no image has no environment to inherit, so Forge
	// supplies the minimal explicit one Stages 1 to 4 have always used. A
	// container *with* an image gets the image's, merged by the runtime — which
	// is the only place that knows what the image declared, and so the only
	// place that can decide (SSOT §2, §13.6).
	if spec.Image == "" {
		spec.Env = defaultContainerEnv()
	}

	runner, err := newRunner(env)
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
		runtime.ErrMTUWithoutInterface,
		runtime.ErrInvalidMTU,
		// A mode that is not one of the three, or an MTU the kernel refuses.
		// The environment sentinels next door stay absent for the reason given
		// above: a host with no nf_tables or no CAP_NET_ADMIN is not a typo.
		network.ErrInvalidInterface,
		rootfs.ErrSourceNotFound,
		rootfs.ErrSourceNotADirectory,
		rootfs.ErrSourceIsHostRoot,
		runtime.ErrImageAndRootfs,

		// The image sentinels that are the caller's fault: a reference that is
		// malformed, one that names nothing, one that needs credentials Forge
		// does not have, and one with no build for this machine. Each of them
		// is answered by typing something different.
		//
		// Deliberately absent, for the same reason the cgroup and network
		// environment sentinels are: ErrRegistryUnavailable is a registry that
		// is down, and ErrDigestMismatch, ErrCorruptLayer and ErrEscapesRoot are
		// bad content. None of those is something the user typed wrong, and
		// reporting them as usage errors would send them looking at their
		// command line instead of at their network or their image.
		image.ErrInvalidReference,
		image.ErrNotFound,
		image.ErrUnauthorized,
		image.ErrNoMatchingPlatform,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// defaultContainerEnv is the environment Forge gives a container that has no
// image to take one from.
//
// The value itself belongs to the runtime, which also uses it as the fallback
// for an image that declares no environment of its own. Two definitions of "the
// default environment" would be exactly the kind of divergence SSOT §13.6
// exists to prevent.
func defaultContainerEnv() []string {
	return runtime.DefaultEnv()
}

// writeRunUsage prints help for `forge run`.
func writeRunUsage(w io.Writer) {
	fmt.Fprint(w, "Usage:\n")
	fmt.Fprint(w, "  forge run [flags] <image> [cmd] [args...]\n")
	fmt.Fprint(w, "  forge run [flags] -rootfs <dir> <path> [args...]\n")
	fmt.Fprint(w, "  forge run [flags] <path> [args...]\n\n")
	fmt.Fprint(w, "Runs a command as PID 1 inside new PID, UTS, mount and network namespaces.\n\n")
	fmt.Fprint(w, "The first argument is an image reference, such as alpine:3.20, unless it\n")
	fmt.Fprint(w, "begins with / ./ or ../ or -rootfs was given, in which case it is the\n")
	fmt.Fprint(w, "command. Forge pulls the image, verifies every layer against its digest,\n")
	fmt.Fprint(w, "caches it under -image-root, and unpacks it into the container's own root\n")
	fmt.Fprint(w, "filesystem. With no command, the image's entrypoint and cmd are used;\n")
	fmt.Fprint(w, "arguments given here replace its cmd and keep its entrypoint.\n\n")
	fmt.Fprint(w, "A bare command name is resolved against the container's own PATH, but only\n")
	fmt.Fprint(w, "when running from an image. Without one forge does not search PATH at all —\n")
	fmt.Fprint(w, "there would be no filesystem to search but the host's — so give a <path>.\n\n")
	fmt.Fprint(w, "Without an image and without -rootfs the container shares the host's\n")
	fmt.Fprint(w, "filesystem. With either, it gets its own root filesystem via pivot_root, and\n")
	fmt.Fprint(w, "host directories can be bind-mounted in with -mount.\n\n")
	fmt.Fprint(w, "Every container gets a cgroup v2 leaf for accounting. -memory, -cpus,\n")
	fmt.Fprint(w, "-cpu-weight and -pids constrain it; a limit left unset is inherited rather\n")
	fmt.Fprint(w, "than capped. Pass \"max\" to ask for no limit explicitly.\n\n")
	fmt.Fprint(w, "By default the container gets its own network namespace, an address on the\n")
	fmt.Fprint(w, "forge0 bridge, and NAT to the outside world. -network none gives it an\n")
	fmt.Fprint(w, "isolated namespace with only loopback; -network host leaves it in the host's\n")
	fmt.Fprint(w, "network namespace with no isolation at all.\n\n")
	fmt.Fprint(w, "Flags:\n")

	fs, _ := newRunFlagSet()
	fs.SetOutput(w)
	fs.PrintDefaults()

	fmt.Fprint(w, "\nExamples:\n")
	fmt.Fprint(w, "  sudo forge run alpine:3.20\n")
	fmt.Fprint(w, "  sudo forge run alpine:3.20 ls /etc\n")
	fmt.Fprint(w, "  sudo forge run -memory 128m ghcr.io/org/image:latest /bin/sh\n")
	fmt.Fprint(w, "  sudo forge run /bin/echo hello from forge\n")
	fmt.Fprint(w, "  sudo forge run -rootfs /srv/alpine /bin/sh\n")
	fmt.Fprint(w, "  sudo forge run -rootfs /srv/alpine -mount /srv/data:/data:ro /bin/ls /data\n")
	fmt.Fprint(w, "  sudo forge run -memory 128m -pids 64 /bin/sh\n")
	fmt.Fprint(w, "  sudo forge run -cpus 1.5 -cpu-weight 512 /bin/sh\n")
	fmt.Fprint(w, "  sudo forge run -network none /bin/sh\n")
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
