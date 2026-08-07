// Package runtime orchestrates Forge's primitive packages into a container
// lifecycle.
//
// Per SSOT §13.2 it is the only orchestrator: the primitive packages never call
// one another, and every cross-package sequencing decision lives here. As of
// Stage 5 it composes seven — internal/namespace, internal/process,
// internal/rootfs, internal/mount, internal/cgroup, internal/network and
// internal/image.
//
// # The lifecycle
//
// Every container follows the same twelve steps, and the order is forced rather
// than chosen. Each column says what makes the step impossible any earlier.
//
//	 1  resolve reference    parent    parse the image name; pure, no I/O
//	 2  fetch manifest       parent    resolve the tag to an immutable digest
//	 3  download layers      parent    only what the blob cache is missing
//	 4  verify digests       parent    on the wire, on the write, and at use
//	 -- nothing on the host has been created; the cleanup stack is empty ------
//	 5  construct rootfs     parent    the container directory, and the image's
//	                                   layers unpacked into it, base first
//	 6  prepare filesystem   parent    the mount plan its init will apply
//	 7  prepare cgroup       parent    limits written before anything can join
//	 8  prepare network      parent    bridge, NAT and an address; no container
//	                                   needed, so it is claimed before there is
//	                                   one to leak it
//	 -- clone(2) ------------------------------------------------------------
//	 9  start process        parent    the namespaces exist; the child blocks
//	                                   on the payload pipe and does nothing
//	                                   — and in the same window: attach the pid
//	                                   to the cgroup, move the interface into
//	                                   the namespace, plug the host end in
//	10  send payload         parent    releases the child, which configures its
//	                                   own interface, mounts, pivots, resolves
//	                                   the command and execs
//	11  wait                 parent    supervise until the container exits
//	12  cleanup              parent    unwind the stack, in reverse
//
// Steps 1 to 4 come first because they are the only ones that create nothing.
// They touch the network and the shared blob cache, so every way they can fail
// — a name that does not parse, a registry that is down, a platform that is not
// published, a layer that does not verify — leaves the host bit-for-bit
// unchanged, with no directory to remove and no address to release. A typo in
// an image name is the commonest failure in the whole stage, and it costs
// nothing.
//
// The three sub-steps folded into step 9 all happen inside the window it opens:
// the child's first act is a blocking read (ADR-0008), so it cannot run a single
// instruction of the container's own binary until step 10. Everything a
// container must be born with — its limits, its interface — is therefore in
// place before it, rather than racing it.
//
// A container with no image skips steps 1 to 4 entirely and enters at step 5,
// which is why every Stage 1 to 4 container behaves exactly as it did.
//
// # How a container starts
//
// Namespaces are created by clone(2), but most of what makes a container a
// container can only be done by code running *inside* the new namespaces:
// setting the hostname (FR-1.2), detaching the mount tree from the host's
// (FR-1.3), and building the container's root filesystem (FR-2.1, FR-2.2).
// Forge therefore starts itself rather than the container's binary:
//
//	forge run          →  clone(CLONE_NEWPID|NEWUTS|NEWNS|NEWNET)
//	                        →  /proc/self/exe __init      (this package, Init)
//	                             →  namespace.Apply
//	                             →  network.Configure     (its own interface)
//	                             →  mount.Apply           (the container's mounts)
//	                             →  mount.PivotRoot       (its root filesystem)
//	                             →  execve(user binary)
//
// The configuration crosses that boundary as JSON on an inherited pipe. See
// ADR-0008. Stage 4 adds one more thing to that list — network.Configure,
// which runs between namespace.Apply and the mounts, because an interface is
// configured over netlink and needs no filesystem at all (ADR-0018).
//
// # Cleanup
//
// One rule, applied without exception: a cleanup is registered on the stack the
// moment the resource it releases exists, and never later. Registration order
// is therefore acquisition order, and the stack unwinds in reverse (SSOT §11.3):
//
//	release network   →   release cgroup   →   remove filesystem
//
// The reversal is load-bearing rather than tidy. The network is released first
// because the container may still be holding an interface plugged into the
// bridge; the cgroup next, because a cgroup cannot be removed while a process
// is in it; the filesystem last, because it is the thing a still-running
// container has open.
//
// Registering *before* the next thing can fail is what makes partial failure
// safe. Every intermediate state a run can die in — an address claimed but no
// veth made, a veth made but never moved, a container attached but never told
// what to do — unwinds through a Destroy that already knows about it, and every
// Destroy is idempotent (SSOT §13.3). Nothing releases what it did not create,
// and nothing is created that something is not already prepared to release.
//
// # What the parent does
//
// The parent creates only directories: <root>/<id>/rootfs, which FR-2.4
// requires and which the child mounts into. It makes no mounts of its own, so
// the mounts a container has are exactly the mounts its namespace holds, and
// the kernel releases all of them when the container exits (ADR-0012).
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stevenstank/forge/internal/cgroup"
	"github.com/stevenstank/forge/internal/image"
	"github.com/stevenstank/forge/internal/logs"
	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/namespace"
	"github.com/stevenstank/forge/internal/network"
	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/rootfs"
	"github.com/stevenstank/forge/internal/state"
)

// InitCommandName is the hidden subcommand Forge re-executes itself as, to run
// Init inside the container's new namespaces. The double underscore marks it as
// internal and keeps it clear of the user-facing verbs in SSOT §9.
const InitCommandName = "__init"

// initArgv0 is the argv[0] the init process runs under. It exists only to make
// the process legible in host tooling such as ps.
const initArgv0 = "forge-init"

// Config is the runtime's own configuration, as opposed to a container's.
type Config struct {
	// Root is the directory per-container root filesystems are stored under,
	// from the --root flag (SSOT §9).
	Root string

	// ImageRoot is the directory downloaded layers are cached in, from the
	// --image-root flag. Empty means DefaultImageRoot.
	//
	// It is independent of Root so the two can sit on different filesystems.
	// Nothing is created here unless a container is actually run from an image.
	ImageRoot string

	// Registry configures the OCI Distribution client: timeouts, retry policy,
	// the manifest size cap. The zero value takes internal/image's defaults,
	// which is the production configuration.
	//
	// It is a struct rather than flattened fields because it is passed through
	// verbatim: the runtime decides *which* image to pull, never how long a
	// registry may take to answer.
	Registry image.ClientConfig

	// StateDir is the directory container metadata is persisted under, from
	// the --state-dir flag (SSOT §9). Empty means DefaultStateDir.
	//
	// It is the parent of the state store's tree rather than the tree itself,
	// so the records of a Forge and the logs of the same Forge sit side by
	// side under one directory a user can point at.
	StateDir string

	// CgroupRoot is the mount point of the cgroup v2 unified hierarchy.
	// Empty means cgroup.DefaultRoot, which is where every distribution
	// mounts it; it exists so tests can point the runtime at a directory of
	// their own.
	CgroupRoot string

	// Network is the host side of container networking: the bridge every
	// container is plugged into, the subnet their addresses come from, and
	// where their IP leases are recorded. The zero value takes
	// internal/network's defaults, which is the production configuration.
	//
	// It is a struct rather than three flattened fields because it is passed
	// through verbatim: the runtime decides *whether* a container is
	// networked, never what the host's bridge is called.
	Network network.Config
}

// Spec describes a container to run. It is the caller's complete statement of
// intent; this package reads no flags, environment, or global state.
type Spec struct {
	// Command is the argument vector to execute inside the container.
	//
	// Command[0] is normally a path, resolved inside the container's root
	// filesystem. With an Image it may also be a bare name, which is resolved
	// against the container's own PATH by the container's init, after the pivot
	// — the only process that can see the filesystem being searched.
	//
	// Empty is valid only with an Image, which then supplies the command from
	// its Entrypoint and Cmd. Arguments given here replace the image's Cmd and
	// keep its Entrypoint, as Docker does.
	Command []string

	// Image is an OCI image reference such as "alpine:3.20" to run the
	// container from. Forge pulls it, verifies it, and unpacks its layers into
	// the container's own root filesystem directory.
	//
	// Mutually exclusive with Rootfs: they are two answers to the same
	// question, and a caller who gave both more likely made a mistake than a
	// choice.
	Image string

	// Env is the container's complete environment. Nil means an empty
	// environment: nothing is inherited from the host implicitly.
	Env []string

	// Hostname is the container's hostname. Empty means Forge uses the
	// container ID, which is what Docker does.
	Hostname string

	// Rootfs is a host directory to use as the container's root filesystem.
	// Empty means the container shares the host's filesystem, which is what
	// Stage 1 did and remains valid.
	Rootfs string

	// Mounts are bind mounts to make inside the container, in addition to the
	// default set every container gets. Requires Rootfs or Image.
	Mounts []mount.Mount

	// ReadonlyRoot mounts the container's root filesystem read-only. Requires
	// Rootfs or Image.
	ReadonlyRoot bool

	// WorkingDir is the directory the container's binary starts in, inside the
	// container. Empty means the image's, or "/" if it declares none. Requires
	// Rootfs or Image.
	WorkingDir string

	// Limits are the container's resource limits. The zero value asks for
	// none, which is what Stage 1 and Stage 2 containers get: the container
	// still runs in a cgroup of its own, but nothing is capped.
	Limits cgroup.Limits

	// Network is how the container is attached to the network. Empty means
	// network.ModeBridge, so a container that asks for nothing gets the
	// isolated, connected network FR-4.1 requires — what a container gets
	// when nobody asks is the runtime's decision, not the caller's (SSOT §2).
	//
	// network.ModeHost is the escape hatch back to Stages 1 to 3: the
	// container shares the host's network namespace and Forge creates
	// nothing.
	Network network.Mode

	// NetworkMTU is the MTU of the container's interface. Zero leaves the
	// kernel's default, which is what almost every container wants; it is
	// here for hosts whose uplink is itself tunnelled. Requires a mode with
	// an interface to apply it to.
	NetworkMTU int

	// Keep retains the container's record and root filesystem after it exits,
	// so that `forge ps -a` can list it and `forge logs` could read it, until
	// `forge rm` removes it.
	//
	// The default is not to: an attached run's output has already gone to the
	// user's terminal, and Stages 1 to 5 have always left nothing behind. What
	// changes that calculation is a container whose output exists only in a
	// file — which is what the detached mode this flag anticipates produces,
	// and why it will default the other way when it lands.
	Keep bool

	// Stdin, Stdout and Stderr are wired to the container process.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Validate reports whether the spec can be run. It is pure and performs no
// syscalls, so bad input is rejected before anything is forked.
func (s Spec) Validate() error {
	if err := s.validateImage(); err != nil {
		return err
	}

	if err := s.validateCommand(); err != nil {
		return err
	}

	if s.Hostname != "" {
		// Checked here rather than in Run so an invalid hostname is a usage
		// error from the CLI rather than a failure part-way through starting a
		// container.
		if err := (namespace.Config{UTS: true, Hostname: s.Hostname}).Validate(); err != nil {
			return err
		}
	}

	if err := s.validateFilesystem(); err != nil {
		return err
	}

	if err := s.validateNetwork(); err != nil {
		return err
	}

	// Checked here, in the parent, for the same reason as everything else in
	// Validate: a limit the kernel would refuse should be a usage error, not a
	// container that fails half-way through starting.
	return s.Limits.Validate()
}

// validateImage checks the Stage 5 half of the spec. It is pure: ParseReference
// performs no I/O, so a malformed reference is a usage error reported before
// anything is forked or any socket is opened.
func (s Spec) validateImage() error {
	if s.Image == "" {
		return nil
	}
	if s.Rootfs != "" {
		return fmt.Errorf("%w: %q and --rootfs %q are two answers to the same question",
			ErrImageAndRootfs, s.Image, s.Rootfs)
	}

	_, err := image.ParseReference(s.Image)
	return err
}

// validateCommand checks that there is something to run, and that Forge will be
// able to find it.
//
// The PATH rule narrowed in Stage 5 rather than disappearing, and the asymmetry
// is the honest one: searching a PATH is safe exactly when Forge knows which
// filesystem it is searching.
//
//   - With an image there is finally a PATH that means something inside the
//     container, so a bare name is accepted and resolved child-side, after the
//     pivot, by the only process that can see that filesystem.
//   - Without one, a bare name would be resolved against the *host's* PATH,
//     which is a surprise worth refusing outright. That is Stage 1's rule and it
//     is unchanged.
func (s Spec) validateCommand() error {
	if len(s.Command) == 0 || s.Command[0] == "" {
		if s.Image == "" {
			return ErrNoCommand
		}
		// An image may supply the command from its entrypoint and cmd. Whether
		// it actually does is only knowable after the pull, so it is checked in
		// containerImage.apply rather than guessed at here.
		return nil
	}

	if !strings.Contains(s.Command[0], "/") && s.Image == "" {
		return fmt.Errorf("%w: %q is not a path; without an image forge does not search PATH, give a path inside the container such as /bin/%s",
			ErrNotAPath, s.Command[0], s.Command[0])
	}

	return nil
}

// validateFilesystem checks the Stage 2 half of the spec.
func (s Spec) validateFilesystem() error {
	if s.Rootfs == "" && s.Image == "" {
		// Every filesystem option needs something to apply to. Accepting them
		// silently would produce a container that ignored them.
		switch {
		case len(s.Mounts) > 0:
			return fmt.Errorf("%w: --mount needs an image or a --rootfs to mount into", ErrMountWithoutRootfs)
		case s.ReadonlyRoot:
			return fmt.Errorf("%w: --read-only needs an image or a --rootfs to make read-only", ErrMountWithoutRootfs)
		case s.WorkingDir != "":
			return fmt.Errorf("%w: --workdir needs an image or a --rootfs to resolve it in", ErrMountWithoutRootfs)
		}
		return nil
	}

	if s.Rootfs != "" && !filepath.IsAbs(s.Rootfs) {
		return fmt.Errorf("%w: --rootfs %q must be an absolute path", ErrRootfsNotAbsolute, s.Rootfs)
	}
	if s.WorkingDir != "" && !filepath.IsAbs(s.WorkingDir) {
		return fmt.Errorf("%w: --workdir %q must be an absolute path inside the container",
			ErrWorkingDirNotAbsolute, s.WorkingDir)
	}

	// The plan the mounts end up in is validated as a whole once the container
	// directory is known; this catches what can be known now, in the parent,
	// before anything is created.
	//
	// With an image the source is the container's own root filesystem — the
	// self-bind ADR-0010 anticipated — and that directory does not exist yet,
	// so a placeholder stands in for it. Only the mounts are being judged here.
	source := s.Rootfs
	if source == "" {
		source = filepath.Join(string(filepath.Separator), "placeholder-source")
	}

	return mount.Plan{
		Source: source,
		Root:   filepath.Join(string(filepath.Separator), "placeholder"),
		Mounts: s.Mounts,
	}.Validate()
}

// NetworkMode returns the mode the spec asks for, with the empty value
// resolved to the default.
//
// It is exported because "empty means bridge" is a decision callers have to be
// able to see: a CLI that prints what it is about to do, or a test that asserts
// the default, would otherwise have to hard-code the same rule.
func (s Spec) NetworkMode() network.Mode {
	if s.Network == "" {
		return network.ModeBridge
	}
	return s.Network
}

// MTU bounds, from the kernel's veth driver (min_mtu and max_mtu in
// drivers/net/veth.c). They are checked here so a value the kernel would refuse
// is a usage error rather than a container that fails part-way through starting.
const (
	minMTU = 68
	maxMTU = 65535
)

// validateNetwork checks the Stage 4 half of the spec.
func (s Spec) validateNetwork() error {
	mode := s.NetworkMode()
	if err := mode.Validate(); err != nil {
		return err
	}

	if s.NetworkMTU == 0 {
		return nil
	}

	// An MTU with no interface to apply it to would be accepted and then
	// silently ignored, which is the failure mode validateFilesystem exists to
	// prevent for --mount and --workdir.
	if !mode.NeedsVeth() {
		return fmt.Errorf("%w: -mtu needs an interface to apply to, which %q networking does not create",
			ErrMTUWithoutInterface, string(mode))
	}
	if s.NetworkMTU < minMTU || s.NetworkMTU > maxMTU {
		return fmt.Errorf("%w: %d is outside the %d to %d a veth accepts",
			ErrInvalidMTU, s.NetworkMTU, minMTU, maxMTU)
	}

	return nil
}

// Sentinel errors callers may branch on.
var (
	// ErrNoCommand reports a Spec with nothing to execute.
	ErrNoCommand = errors.New("a command is required")

	// ErrNotAPath reports a command that is a bare name rather than a path.
	ErrNotAPath = errors.New("command must be a path")

	// ErrRootfsNotAbsolute reports a relative --rootfs, which would resolve
	// against whatever directory forge was started in.
	ErrRootfsNotAbsolute = errors.New("root filesystem path must be absolute")

	// ErrWorkingDirNotAbsolute reports a relative --workdir.
	ErrWorkingDirNotAbsolute = errors.New("working directory must be absolute")

	// ErrMountWithoutRootfs reports a filesystem option given to a container
	// that has no root filesystem of its own.
	ErrMountWithoutRootfs = errors.New("option requires a root filesystem")

	// ErrMTUWithoutInterface reports an MTU given to a container whose network
	// mode creates no interface to put it on.
	ErrMTUWithoutInterface = errors.New("option requires bridge networking")

	// ErrInvalidMTU reports an MTU the kernel would refuse.
	ErrInvalidMTU = errors.New("invalid MTU")

	// ErrImageAndRootfs reports a spec naming both an image and a host
	// directory to use as the container's root filesystem.
	ErrImageAndRootfs = errors.New("an image and a root filesystem cannot both be given")

	// ErrCommandNotFound reports a bare command name that no directory on the
	// container's PATH provides. It is returned by the container's init, after
	// the pivot, because that is the only process that can see the filesystem
	// being searched.
	ErrCommandNotFound = errors.New("command not found in the container")

	// ErrPathSearchWithoutRootfs reports an init payload asking for a PATH
	// search in a container that has no filesystem of its own. The search would
	// run against the host's directories, which is never what was meant.
	ErrPathSearchWithoutRootfs = errors.New("a bare command name needs a container filesystem to resolve against")

	// ErrNetworkWithoutNetns reports an init payload carrying an interface for
	// a container that has no network namespace to configure. Applying it
	// would reconfigure the host.
	ErrNetworkWithoutNetns = errors.New("an interface was given to a container with no network namespace")
)

// Runner runs containers. Construct it with NewRunner.
type Runner struct {
	logger   *slog.Logger
	store    *rootfs.Store
	cgroups  *cgroup.Manager
	networks *network.Manager
	images   *image.Cache
	registry *image.Client
	state    *state.Store
	logs     *logs.Store

	// openProcess opens a handle on a container's init process. It is a field
	// so that the whole of `forge stop` — signal, grace period, kill — is
	// testable without root, without a real container, and without waiting on
	// a real process to die. There is one implementation in production, so it
	// is a function rather than an interface (the same reasoning, and the same
	// shape, as internal/network's reclaimStale).
	openProcess func(pid int) (containerProcess, error)

	// pollInterval is how often Stop re-checks a container it is waiting on,
	// and killGrace is how long it waits after SIGKILL. Both are fields for
	// the same reason as openProcess: a test that had to wait out the real
	// values would be a test nobody runs (SSOT §7 forbids sleeps in tests, and
	// this is how that is kept possible).
	pollInterval time.Duration
	killGrace    time.Duration
}

// NewRunner returns a Runner that logs through logger and stores container root
// filesystems under cfg.Root, creating that directory if it does not exist.
//
// The logger is injected rather than global so every container operation can be
// correlated by the container_id attribute this package attaches (SSOT §6).
//
// Constructing the cgroup, network, image and registry components touches
// nothing: whether the host has a usable cgroup v2 hierarchy or a usable bridge
// depends on what a container asks for, so both are decided per container — in
// prepareCgroup and prepareNetwork — where they can be reported against that
// container. The image cache does not even create its directories, so a runner
// that is only ever used with --rootfs leaves no cache behind. The only things
// that can fail here are configurations that are wrong on their face, such as
// an unparseable subnet or a relative cache root.
func NewRunner(logger *slog.Logger, cfg Config) (*Runner, error) {
	store, err := rootfs.NewStore(cfg.Root, logger)
	if err != nil {
		return nil, err
	}

	networks, err := network.New(logger, cfg.Network)
	if err != nil {
		return nil, err
	}

	imageRoot := cfg.ImageRoot
	if imageRoot == "" {
		imageRoot = DefaultImageRoot
	}
	images, err := image.NewCache(imageRoot, logger)
	if err != nil {
		return nil, err
	}

	registry, err := image.New(logger, cfg.Registry)
	if err != nil {
		return nil, err
	}

	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	records, err := state.New(stateDir)
	if err != nil {
		return nil, err
	}

	captured, err := logs.New(filepath.Join(stateDir, logsDirName))
	if err != nil {
		return nil, err
	}

	return &Runner{
		logger:       logger,
		store:        store,
		cgroups:      cgroup.New(cfg.CgroupRoot),
		networks:     networks,
		images:       images,
		registry:     registry,
		state:        records,
		logs:         captured,
		openProcess:  openContainerProcess,
		pollInterval: defaultPollInterval,
		killGrace:    KillGrace,
	}, nil
}

// Run creates a container from spec, runs it to completion, and reports how it
// exited.
//
// A container that exits non-zero is not an error: its status is returned with
// a nil error, and only a failure of Forge itself produces an error. Run blocks
// until the container terminates. If ctx is cancelled the container is killed
// rather than abandoned.
func (r *Runner) Run(ctx context.Context, spec Spec) (process.Status, error) {
	if err := spec.Validate(); err != nil {
		return process.Status{}, err
	}

	// Fail fast rather than fork-bombing if the calling binary re-executed
	// itself without routing InitCommandName to Init (PRD NFR-8).
	if os.Getenv(envInitGuard) != "" {
		return process.Status{}, ErrNestedInit
	}

	id, err := NewID()
	if err != nil {
		return process.Status{}, err
	}
	log := r.logger.With("container_id", id)

	// Everything Forge creates on the host is registered here and released in
	// reverse order when Run returns, however it returns (SSOT §11.3).
	cleanup := newCleanupStack(log)
	defer cleanup.unwind()

	// retain is set once the container has actually run. Until then, every
	// path out of Run removes the record and the filesystem along with
	// everything else: a container that failed to start is not a container a
	// user asked to keep, and Stages 1 to 5 leave nothing behind when a run
	// fails.
	retain := false

	// Steps 1 to 4, and they happen before the stack has a single entry on it.
	// The image is resolved, downloaded and verified while the only things
	// Forge has touched are the network and the shared blob cache — so every
	// way this can fail leaves the host unchanged, with no directory to remove
	// and no address to release (see image.go).
	img, err := r.resolveImage(ctx, log, spec)
	if err != nil {
		return process.Status{}, fmt.Errorf("container %s: %w", id, err)
	}
	if img != nil {
		// The image's command, environment and working directory become the
		// spec's defaults. This is the last change to the spec, and it happens
		// before anything is created, so what is built below is what will run.
		if spec, err = img.apply(spec); err != nil {
			return process.Status{}, fmt.Errorf("container %s: %w", id, err)
		}
	}

	// The record, and it goes here for the same reason steps 1 to 4 went
	// first: it is the last moment before anything container-specific exists
	// on the host, and the first moment at which there is something to
	// attribute. Everything below creates a resource that a `forge rm` running
	// tomorrow will have to find, and the record is how it finds them
	// (FR-6.5).
	//
	// Not before the pull, deliberately. A mistyped image name is the
	// commonest failure in the whole runtime, and Stage 5's arrangement makes
	// it cost nothing — no directory created, no address claimed, nothing to
	// unwind. Creating a record above would have bought a `forge ps` that
	// shows a container while its image downloads, and paid for it by making
	// the cheapest failure in Forge write to disk twice.
	//
	// Registered first on the stack, so it unwinds last: the record is the
	// list of what to clean up, so it cannot be the first thing cleaned up.
	if err := r.createRecord(id, spec); err != nil {
		return process.Status{}, fmt.Errorf("container %s: %w", id, err)
	}
	cleanup.push("removing the container record", func() error {
		if retain {
			return nil
		}
		return r.state.Remove(id)
	})

	// The log, second on the stack and second to be acquired: everything
	// below this point can produce output, and output with nowhere to go is
	// output lost. Capturing it costs a file and changes nothing about how an
	// attached run behaves — the caller's terminal is still written to, and
	// the log is a second copy (FR-6.4).
	spec, captured, err := r.openLogs(spec, id)
	if err != nil {
		return process.Status{}, fmt.Errorf("container %s: %w", id, err)
	}
	cleanup.push("closing the container log", func() error {
		return r.closeLogs(captured, id, &retain)
	})

	// Steps 5 and 6: the container's directory, its contents unpacked from the
	// image, and the plan describing what its init will mount.
	plan, err := r.prepareFilesystem(ctx, log, id, spec, img, cleanup, &retain)
	if err != nil {
		return process.Status{}, fmt.Errorf("container %s: %w", id, err)
	}

	// The cgroup is created — and its limits written — before the container
	// exists, so the limits are in force from the moment it joins rather than
	// some time after it starts.
	cgroupID, err := r.prepareCgroup(log, id, spec, cleanup)
	if err != nil {
		return process.Status{}, fmt.Errorf("container %s: %w", id, err)
	}

	// Like the cgroup, the address is reserved before the container exists.
	// The half that needs a namespace to exist — the veth pair — happens in
	// start, once there is a PID to name it by.
	cnet, err := r.prepareNetwork(log, id, spec, cleanup)
	if err != nil {
		return process.Status{}, fmt.Errorf("container %s: %w", id, err)
	}

	// The namespaces are described last because one of them is the network's
	// to decide: CLONE_NEWNET is what host mode opts out of, and prepareNetwork
	// is where that is resolved. Nothing has been cloned yet — this is a
	// description, and it is not acted on until start.
	nsCfg := namespace.Config{
		PID:      true,
		UTS:      true,
		Mount:    true,
		Net:      cnet.mode.NeedsNetns(),
		Hostname: spec.Hostname,
	}
	if nsCfg.Hostname == "" {
		nsCfg.Hostname = id
	}

	self, err := os.Executable()
	if err != nil {
		return process.Status{}, fmt.Errorf("locating the forge binary to re-execute: %w", err)
	}

	payload, err := json.Marshal(initPayload{
		Namespace:  nsCfg,
		Command:    spec.Command,
		Env:        spec.Env,
		Mount:      plan,
		WorkingDir: spec.WorkingDir,
		Network:    cnet.iface(),
	})
	if err != nil {
		return process.Status{}, fmt.Errorf("encoding init payload: %w", err)
	}

	log.Debug("creating container",
		"command", spec.Command,
		"hostname", nsCfg.Hostname,
		"network", string(cnet.mode),
		"clone_flags", fmt.Sprintf("%#x", nsCfg.CloneFlags()),
	)

	status, err := r.start(ctx, log, id, self, payload, nsCfg, spec, cgroupID, cnet)
	if err != nil {
		return process.Status{}, fmt.Errorf("container %s: %w", id, err)
	}

	// The container ran, so it is now a container a user may have asked to
	// keep. This is also the last moment at which that is still a decision:
	// the deferred unwind below reads it.
	retain = spec.Keep

	// Recorded before the unwind, because the unwind may delete the record
	// this is written into — and because a `forge stop` waiting for this
	// container to finish is watching for exactly this write.
	r.recordExit(log, id, status)

	log.Info("container exited", "exit_code", status.Code, "status", status.String())

	return status, nil
}

// prepareFilesystem creates the container's root filesystem directory, fills it
// if the container came from an image, and builds the plan describing what the
// container's init will mount into it.
//
// A spec with neither Rootfs nor Image returns a nil plan, which is what keeps a
// Stage 1 container running against the host's filesystem: nothing is created,
// nothing is mounted, and no pivot happens.
//
// The two sources of content are mutually exclusive and converge here:
//
//	--rootfs <dir>   the directory is bind-mounted over the container's root
//	                 by its init, inside the mount namespace (Stage 2)
//	an image         the layers are unpacked into the container's root here,
//	                 in the parent, and the init self-binds it to satisfy
//	                 pivot_root's mount-point precondition (ADR-0010)
//
// Either way internal/rootfs owns the directory and internal/image owns the
// bytes in it. The sequencing between them is this function's, which is the
// whole of why ADR-0020 needs no edge between those two packages.
func (r *Runner) prepareFilesystem(
	ctx context.Context,
	log *slog.Logger,
	id string,
	spec Spec,
	img *containerImage,
	cleanup *cleanupStack,
	retain *bool,
) (*mount.Plan, error) {
	if spec.Rootfs == "" && img == nil {
		return nil, nil
	}

	if img == nil {
		source, err := rootfs.ValidateSource(spec.Rootfs)
		if err != nil {
			return nil, err
		}
		spec.Rootfs = source // a copy; the caller's spec is untouched
	}

	dir, err := r.store.Prepare(id)
	if err != nil {
		return nil, err
	}
	r.recordFilesystem(log, id, dir.Rootfs)
	cleanup.push("removing the container root filesystem", func() error {
		// A kept container keeps its filesystem: it is what `forge ps -a`
		// describes and what `forge rm` removes. retain is a pointer because
		// it is not decided until the container has run — a container that
		// failed to start leaves nothing behind, whatever was asked for.
		if *retain {
			return nil
		}

		// Cleanup runs after the container is gone, which includes the case
		// where ctx was cancelled to kill it. Inheriting that cancellation
		// would mean an interrupted run leaked exactly what it was cancelled
		// to release.
		ctx := context.WithoutCancel(ctx)

		// Nothing Forge does mounts on the host, so this normally finds
		// nothing. It is here because Store.Remove refuses to delete a tree
		// with mounts under it, and reconciling residue from a previous
		// crashed run is the only way that refusal can be hit.
		if err := mount.Cleanup(ctx, dir.Base); err != nil {
			return err
		}
		return r.store.Remove(ctx, id)
	})

	// Step 5, and it is deliberately the first thing after the cleanup that
	// removes what it writes. Extraction is the only part of a run that puts
	// files inside a container's directory, so there must be no window in which
	// those files exist and nothing is responsible for them: a layer that fails
	// half-way through, a full disk, a cancelled context — all of them unwind
	// through the push above (FR-5.3, PRD §10.4).
	if img != nil {
		stats, err := image.BuildRootfs(ctx, r.images, img.manifest.Layers, dir.Rootfs)
		if err != nil {
			return nil, err
		}

		// The source of the bind is the container's own root filesystem. That
		// self-bind does one job and one only: pivot_root(2) requires its new
		// root to be a mount point, and after unpacking this directory is an
		// ordinary directory (ADR-0001, ADR-0010).
		spec.Rootfs = dir.Rootfs

		log.Info("unpacked image layers",
			"reference", img.ref.String(), "layers", len(img.manifest.Layers),
			"files", stats.Files, "dirs", stats.Dirs, "bytes", stats.Bytes)

		if stats.UnownedEntries > 0 {
			log.Warn("some unpacked files could not be given their image ownership",
				"entries", stats.UnownedEntries)
		}
	}

	plan, err := mountPlan(spec, dir)
	if err != nil {
		return nil, err
	}

	log.Debug("prepared container filesystem",
		"source", plan.Source,
		"rootfs", plan.Root,
		"mounts", len(plan.Mounts),
		"read_only", plan.ReadonlyRoot,
	)

	return &plan, nil
}

// start performs the re-exec handshake and supervises the container. It is
// separated from Run so the resource-ordering — pipe, process, cgroup, network,
// payload, wait — reads top to bottom.
//
// Everything between clone(2) and the payload write happens while the child is
// blocked on its first read (ADR-0008). That window is the only place a
// container can be joined to a cgroup or handed an interface, because both need
// a PID that does not exist until clone returns and both must be in place
// before the container's own binary runs.
//
// cgroupID is the container's cgroup, or empty for a container that has none.
func (r *Runner) start(
	ctx context.Context,
	log *slog.Logger,
	id string,
	self string,
	payload []byte,
	nsCfg namespace.Config,
	spec Spec,
	cgroupID string,
	cnet containerNetwork,
) (process.Status, error) {
	payloadReader, payloadWriter, err := os.Pipe()
	if err != nil {
		return process.Status{}, fmt.Errorf("creating init payload pipe: %w", err)
	}

	p, err := process.New(process.Config{
		Path: self,
		Args: []string{initArgv0, InitCommandName},
		// The init process is Forge's own code and needs no environment beyond
		// the guard that makes a missing init dispatch fail loudly. The
		// container's own environment is applied by execve inside Init.
		Env:        []string{envInitGuard + "=1"},
		CloneFlags: nsCfg.CloneFlags(),
		Stdin:      spec.Stdin,
		Stdout:     spec.Stdout,
		Stderr:     spec.Stderr,
		ExtraFiles: []*os.File{payloadReader},
	})
	if err != nil {
		closeFile(log, payloadReader, "init payload reader")
		closeFile(log, payloadWriter, "init payload writer")
		return process.Status{}, err
	}

	if err := p.Start(ctx); err != nil {
		closeFile(log, payloadReader, "init payload reader")
		closeFile(log, payloadWriter, "init payload writer")
		return process.Status{}, translateCloneError(err)
	}

	// The child holds its own descriptor now. Closing ours is what lets the
	// child's read reach EOF.
	closeFile(log, payloadReader, "init payload reader")

	log.Info("container started", "pid", p.PID(), "state", p.State().String())

	// Recorded in the handshake window, alongside the cgroup attach and for
	// the same reason: this is the first moment the PID exists, and the
	// container has still not run an instruction of its own. A `forge stop`
	// arriving between here and the payload write finds a container it can
	// signal rather than one it can only watch.
	r.recordCreated(log, id, p.PID())

	// The container joins its cgroup here, in the window the handshake opens.
	//
	// A cgroup can only be joined by writing a PID to cgroup.procs, so there is
	// no way to be a member before clone(2) returns. What closes that window is
	// what the child is doing right now: forge-init's first act is a blocking
	// read on the payload pipe (ADR-0008), so it cannot mount, pivot, execve or
	// fork until the parent writes below. Attaching first therefore guarantees
	// every limit is in force before a single instruction of the container's
	// own binary runs.
	if cgroupID != "" {
		if err := r.cgroups.Add(cgroupID, p.PID()); err != nil {
			closeFile(log, payloadWriter, "init payload writer")
			r.abandon(ctx, log, p, "a failed cgroup attach")
			return process.Status{}, err
		}
		log.Debug("container joined its cgroup", "pid", p.PID(), "cgroup", cgroupID)
	}

	// The interface is pushed across the namespace boundary in the same
	// window, and for the same reason: a namespace can only be named by the
	// PID of a process already inside it. The container configures what it
	// finds there from the payload written below, so by the time its own
	// binary runs the interface is present, addressed and routed.
	//
	// Nothing is rolled back here. A partial attach is released by the Destroy
	// that prepareNetwork already registered, which is idempotent and covers
	// every intermediate state this can fail in (SSOT §11.3, §13.3).
	if err := r.attachNetwork(log, cnet, p.PID()); err != nil {
		closeFile(log, payloadWriter, "init payload writer")
		r.abandon(ctx, log, p, "a failed network attach")
		return process.Status{}, err
	}

	if err := writePayload(payloadWriter, payload); err != nil {
		r.abandon(ctx, log, p, "a failed handshake")
		return process.Status{}, err
	}

	// The payload released the child, so from here the container's own binary
	// is what is executing.
	r.recordRunning(log, id)

	status, err := p.Wait(ctx)
	if err != nil {
		return status, err
	}

	return status, nil
}

// abandon kills and reaps a container that was started but cannot be allowed to
// proceed, so a failure between clone(2) and the handshake leaves no orphan
// (PRD NFR-8).
//
// It returns nothing: it runs on a path that already has an error to report,
// and that error is the one the caller must see (SSOT §5). Failures here are
// logged rather than discarded (SSOT §13.7).
func (r *Runner) abandon(ctx context.Context, log *slog.Logger, p *process.Process, why string) {
	if err := p.Signal(syscall.SIGKILL); err != nil {
		log.Warn("killing container after "+why, "error", err)
	}
	if _, err := p.Wait(ctx); err != nil {
		log.Warn("reaping container after "+why, "error", err)
	}
}

// writePayload hands the init configuration to the child and closes the pipe,
// which is what tells the child it has the whole payload.
//
// The write happens after the child has been started, so the child is already
// draining the pipe; a payload larger than the pipe buffer cannot deadlock.
func writePayload(w *os.File, payload []byte) error {
	_, writeErr := w.Write(payload)
	closeErr := w.Close()

	if writeErr != nil {
		return fmt.Errorf("sending init payload: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing init payload pipe: %w", closeErr)
	}
	return nil
}

// closeFile closes f, logging rather than returning a failure: these closes
// happen on cleanup paths where the original error must not be masked
// (SSOT §5). Never silently discarded (SSOT §13.7).
func closeFile(log *slog.Logger, f *os.File, what string) {
	if err := f.Close(); err != nil {
		log.Warn("closing "+what, "error", err)
	}
}

// translateCloneError turns the kernel's EPERM into an actionable message.
// Creating namespaces is the first privileged thing Forge does, so this is
// where an unprivileged user finds out.
func translateCloneError(err error) error {
	if errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("%w: %w", namespace.ErrPermission, err)
	}
	return err
}
