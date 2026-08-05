// Package runtime orchestrates Forge's primitive packages into a container
// lifecycle.
//
// Per SSOT §13.2 it is the only orchestrator: the primitive packages never call
// one another, and every cross-package sequencing decision lives here. As of
// Stage 4 it composes six — internal/namespace, internal/process,
// internal/rootfs, internal/mount, internal/cgroup and internal/network.
//
// # The lifecycle
//
// Every container follows the same ten steps, and the order of the first eight
// is forced rather than chosen. Each column says what makes the step impossible
// any earlier.
//
//	 1  prepare filesystem   parent    the container directory and mount plan
//	 2  prepare cgroup       parent    limits written before anything can join
//	 3  prepare network      parent    bridge, NAT and an address; no container
//	                                   needed, so it is claimed before there is
//	                                   one to leak it
//	 -- clone(2) ------------------------------------------------------------
//	 4  start process        parent    the namespaces exist; the child blocks
//	                                   on the payload pipe and does nothing
//	 5  attach pid           parent    cgroup.procs needs a pid, and a pid needs
//	                                   clone(2) to have returned
//	 6  move interface       parent    a netns can only be named by the pid of
//	                                   a process already inside it
//	 7  configure host side  parent    the host veth is enslaved to the bridge
//	                                   and brought up
//	 8  send payload         parent    releases the child, which configures its
//	                                   own interface, mounts, pivots and execs
//	 9  wait                 parent    supervise until the container exits
//	10  cleanup              parent    unwind the stack, in reverse
//
// Steps 5 to 7 all happen inside the window step 4 opens: the child's first act
// is a blocking read (ADR-0008), so it cannot run a single instruction of the
// container's own binary until step 8. Everything a container must be born with
// — its limits, its interface — is therefore in place before it, rather than
// racing it.
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

	"github.com/stevenstank/forge/internal/cgroup"
	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/namespace"
	"github.com/stevenstank/forge/internal/network"
	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/rootfs"
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
	// Command[0] is the path to the binary, resolved inside the container's
	// root filesystem.
	Command []string

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
	// default set every container gets. Requires Rootfs.
	Mounts []mount.Mount

	// ReadonlyRoot mounts the container's root filesystem read-only. Requires
	// Rootfs.
	ReadonlyRoot bool

	// WorkingDir is the directory the container's binary starts in, inside the
	// container. Empty means "/". Requires Rootfs.
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

	// Stdin, Stdout and Stderr are wired to the container process.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Validate reports whether the spec can be run. It is pure and performs no
// syscalls, so bad input is rejected before anything is forked.
func (s Spec) Validate() error {
	if len(s.Command) == 0 || s.Command[0] == "" {
		return ErrNoCommand
	}
	// Forge resolves the command inside the container, and a container has no
	// PATH of its own until images arrive in Stage 5. A bare name would be
	// resolved against the *host's* PATH — a surprise worth refusing outright.
	if !strings.Contains(s.Command[0], "/") {
		return fmt.Errorf("%w: %q is not a path; forge does not search PATH, give a path inside the container such as /bin/%s",
			ErrNotAPath, s.Command[0], s.Command[0])
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

// validateFilesystem checks the Stage 2 half of the spec.
func (s Spec) validateFilesystem() error {
	if s.Rootfs == "" {
		// Every filesystem option needs something to apply to. Accepting them
		// silently would produce a container that ignored them.
		switch {
		case len(s.Mounts) > 0:
			return fmt.Errorf("%w: --mount needs a --rootfs to mount into", ErrMountWithoutRootfs)
		case s.ReadonlyRoot:
			return fmt.Errorf("%w: --read-only needs a --rootfs to make read-only", ErrMountWithoutRootfs)
		case s.WorkingDir != "":
			return fmt.Errorf("%w: --workdir needs a --rootfs to resolve it in", ErrMountWithoutRootfs)
		}
		return nil
	}

	if !filepath.IsAbs(s.Rootfs) {
		return fmt.Errorf("%w: --rootfs %q must be an absolute path", ErrRootfsNotAbsolute, s.Rootfs)
	}
	if s.WorkingDir != "" && !filepath.IsAbs(s.WorkingDir) {
		return fmt.Errorf("%w: --workdir %q must be an absolute path inside the container",
			ErrWorkingDirNotAbsolute, s.WorkingDir)
	}

	// The plan the mounts end up in is validated as a whole once the container
	// directory is known; this catches what can be known now, in the parent,
	// before anything is created.
	return mount.Plan{
		Source: s.Rootfs,
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
}

// NewRunner returns a Runner that logs through logger and stores container root
// filesystems under cfg.Root, creating that directory if it does not exist.
//
// The logger is injected rather than global so every container operation can be
// correlated by the container_id attribute this package attaches (SSOT §6).
//
// Constructing the cgroup and network managers touches nothing: whether the
// host has a usable cgroup v2 hierarchy or a usable bridge depends on what a
// container asks for, so both are decided per container — in prepareCgroup and
// prepareNetwork — where they can be reported against that container. The only
// thing that can fail here is a network configuration that is wrong on its
// face, such as an unparseable subnet.
func NewRunner(logger *slog.Logger, cfg Config) (*Runner, error) {
	store, err := rootfs.NewStore(cfg.Root, logger)
	if err != nil {
		return nil, err
	}

	networks, err := network.New(logger, cfg.Network)
	if err != nil {
		return nil, err
	}

	return &Runner{
		logger:   logger,
		store:    store,
		cgroups:  cgroup.New(cfg.CgroupRoot),
		networks: networks,
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

	plan, err := r.prepareFilesystem(ctx, log, id, spec, cleanup)
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

	status, err := r.start(ctx, log, self, payload, nsCfg, spec, cgroupID, cnet)
	if err != nil {
		return process.Status{}, fmt.Errorf("container %s: %w", id, err)
	}

	log.Info("container exited", "exit_code", status.Code, "status", status.String())

	return status, nil
}

// prepareFilesystem creates the container's root filesystem directory and the
// plan describing what the container's init will mount into it.
//
// A spec with no Rootfs returns a nil plan, which is what keeps a Stage 1
// container running against the host's filesystem: nothing is created, nothing
// is mounted, and no pivot happens.
func (r *Runner) prepareFilesystem(
	ctx context.Context,
	log *slog.Logger,
	id string,
	spec Spec,
	cleanup *cleanupStack,
) (*mount.Plan, error) {
	if spec.Rootfs == "" {
		return nil, nil
	}

	source, err := rootfs.ValidateSource(spec.Rootfs)
	if err != nil {
		return nil, err
	}
	spec.Rootfs = source // a copy; the caller's spec is untouched

	dir, err := r.store.Prepare(id)
	if err != nil {
		return nil, err
	}
	cleanup.push("removing the container root filesystem", func() error {
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
		r.abandon(ctx, log, p, "a failed network attach")
		return process.Status{}, err
	}

	if err := writePayload(payloadWriter, payload); err != nil {
		r.abandon(ctx, log, p, "a failed handshake")
		return process.Status{}, err
	}

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
