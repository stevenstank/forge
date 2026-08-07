package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	goruntime "runtime"

	"github.com/stevenstank/forge/internal/namespace"
	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/state"
)

// The Stage 6 half of the orchestration that runs a second process inside a
// container that already exists (FR-6.2).
//
// internal/namespace performs the setns calls and decides nothing; internal/
// process forks and waits and knows nothing about containers. The policy below
// — which namespaces, in which order, which process is allowed to be joined at
// all, and what happens on the one thread that has been given the container's
// view of the world — is cross-package sequencing and so lives here
// (SSOT §2, §13.2).
//
// # The shape of the operation
//
//	1  load the container's record          it holds the PID, and the PID is
//	                                        the only handle on a container
//	                                        another process has
//	2  refuse anything but a running one    FR-6.2, and §3 below
//	3  verify the process is really there   a record can outlive its container
//	4  open the namespace descriptors       all of them, before joining any
//	5  lock a thread and join               the thread is spent afterwards
//	6  fork from that same thread           the child inherits its namespaces
//	7  wait, and report how it exited       the caller's exit code
//
// Steps 1 to 4 create nothing. Everything they can fail on — an unknown
// container, one that has stopped, one whose process is gone — leaves the host
// untouched and the container undisturbed, which is the same arrangement Stage
// 5 uses for image pulls and for the same reason.
//
// # Why exec cannot leak into the rest of Forge
//
// Joining namespaces is a per-thread operation, and the thread it happens on
// is deliberately sacrificed: it is locked to one goroutine, never unlocked,
// and terminated by the Go runtime when that goroutine returns. No other
// goroutine is scheduled onto it while it lives, and it does not outlive the
// fork it exists to perform. A `forge exec` that fails half-way therefore
// leaves a process whose other threads have never left the host.

// ExecSpec describes a process to run inside an existing container.
type ExecSpec struct {
	// ID is the container to run it in.
	ID string

	// Command is the argument vector to execute. Command[0] is resolved
	// inside the container: a path is used as given, and a bare name is
	// searched for on PATH — which is safe here in a way it is not for `forge
	// run`, because the search happens after the mount namespace has been
	// joined and so reads the container's directories rather than the host's.
	Command []string

	// Env is the environment for the process. Nil means DefaultEnv, which is
	// what a container gets when nobody says otherwise.
	Env []string

	// WorkingDir is the directory inside the container to start in. Empty
	// means the container's root.
	WorkingDir string

	// Stdin, Stdout and Stderr are wired to the process.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Validate reports whether the spec can be run. It is pure and performs no
// syscalls, so bad input is rejected before a container is touched.
func (s ExecSpec) Validate() error {
	if err := state.ValidateID(s.ID); err != nil {
		return fmt.Errorf("%w: %s", ErrNotFound, s.ID)
	}
	if len(s.Command) == 0 || s.Command[0] == "" {
		return ErrNoCommand
	}

	return nil
}

// Exec runs a command inside a running container and returns how it exited
// (FR-6.2).
//
// A command that exits non-zero is not an error: its status is returned with a
// nil error, exactly as Run does, and only a failure of Forge itself produces
// an error. If ctx is cancelled the command is killed rather than abandoned.
//
// The container is not disturbed. Nothing here signals it, writes to its
// record, or touches the resources it holds; the command joins its namespaces
// and its cgroup and is otherwise an ordinary child of this process. When it
// exits, the container carries on.
func (r *Runner) Exec(ctx context.Context, spec ExecSpec) (process.Status, error) {
	if err := spec.Validate(); err != nil {
		return process.Status{}, err
	}

	log := r.logger.With("container_id", spec.ID)

	m, err := r.state.Load(spec.ID)
	if err != nil {
		return process.Status{}, translateStateError(spec.ID, err)
	}

	proc, err := r.verifyRunning(m)
	if err != nil {
		return process.Status{}, err
	}
	// The handle is held for the whole of the setup below. It is a pidfd, so
	// while it is open the container's PID cannot be recycled onto another
	// process — which is what makes "the namespaces of PID n" mean the same
	// thing at step 4 as it did at step 3.
	defer func() {
		if err := proc.Close(); err != nil {
			log.Warn("closing the container process handle", "error", err)
		}
	}()

	handles, err := namespace.Open(m.PID, namespace.EntryOrder...)
	if err != nil {
		if errors.Is(err, namespace.ErrNoSuchNamespace) {
			return process.Status{}, fmt.Errorf("%w: %s exited while starting the command", ErrNotRunning, spec.ID)
		}
		return process.Status{}, fmt.Errorf("container %s: %w", spec.ID, err)
	}
	defer func() {
		if err := namespace.CloseAll(handles); err != nil {
			log.Warn("closing the container's namespace handles", "error", err)
		}
	}()

	cgroupFD, err := r.openCgroup(log, spec.ID)
	if err != nil {
		return process.Status{}, err
	}
	if cgroupFD != nil {
		defer closeFile(log, cgroupFD, "the container's cgroup directory")
	}

	cfg := process.Config{
		Args:     spec.Command,
		Env:      spec.Env,
		Dir:      spec.WorkingDir,
		Stdin:    spec.Stdin,
		Stdout:   spec.Stdout,
		Stderr:   spec.Stderr,
		CgroupFD: cgroupFD,
	}
	if cfg.Env == nil {
		cfg.Env = DefaultEnv()
	}

	p, err := r.startInNamespaces(ctx, handles, cfg)
	if err != nil {
		return process.Status{}, fmt.Errorf("container %s: %w", spec.ID, err)
	}

	log.Info("exec started", "pid", p.PID(), "command", spec.Command)

	status, err := p.Wait(ctx)
	if err != nil {
		return status, fmt.Errorf("container %s: %w", spec.ID, err)
	}

	log.Info("exec exited", "exit_code", status.Code, "status", status.String())

	return status, nil
}

// verifyRunning refuses to exec into anything but a live container, and
// returns a handle proving it is one.
//
// The two checks are different questions and both have to be asked. The record
// says what Forge believes; the process says what is true. A container whose
// supervising `forge run` was killed leaves a record that still reads
// "running", and joining the namespaces of a PID that has since been recycled
// would run the user's command in some unrelated process's namespaces — as
// root. That is the failure this function exists to prevent.
func (r *Runner) verifyRunning(m state.Metadata) (*process.Handle, error) {
	switch {
	case m.Status.Terminal():
		return nil, fmt.Errorf("%w: %s is %s", ErrNotRunning, m.ID, m.Status)
	case m.Status == state.StatusRemoving:
		return nil, fmt.Errorf("%w: %s is being removed", ErrNotRunning, m.ID)
	case m.Status == state.StatusCreating:
		// The container has no process yet. This is a moment rather than a
		// state, so the message says to try again rather than implying that
		// the container is broken.
		return nil, fmt.Errorf("%w: %s is still starting", ErrNotRunning, m.ID)
	case m.PID <= 0:
		return nil, fmt.Errorf("%w: %s has no process", ErrNotRunning, m.ID)
	}

	proc, err := process.Open(m.PID)
	if err != nil {
		if errors.Is(err, process.ErrNoProcess) {
			return nil, fmt.Errorf("%w: %s is recorded as %s but its process is gone",
				ErrNotRunning, m.ID, m.Status)
		}
		return nil, fmt.Errorf("container %s: %w", m.ID, err)
	}

	return proc, nil
}

// openCgroup opens the container's cgroup directory, for the kernel to place
// the exec'd process into at birth.
//
// A container with no cgroup — a host without a cgroup v2 hierarchy, which
// Stage 3 tolerates — gets a nil descriptor and an exec that simply is not
// accounted for. That is a degradation of the same kind and for the same
// reason: refusing to exec would regress a host that can still run containers.
func (r *Runner) openCgroup(log *slog.Logger, id string) (*os.File, error) {
	path, err := r.cgroups.Path(id)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Debug("container has no cgroup; the exec'd process is unaccounted", "path", path)
			return nil, nil
		}
		return nil, fmt.Errorf("opening the container cgroup %q: %w", path, err)
	}

	return f, nil
}

// startInNamespaces joins the container's namespaces on a dedicated thread and
// forks the command from it.
//
// Everything about this function is arranged around one requirement: the fork
// must happen on the same OS thread that did the setns calls, because a child
// inherits the namespaces of the thread that created it. Go gives no way to
// say "run these two operations on one thread" other than locking a goroutine
// to it, so that is what the goroutine below is for. It is not concurrency —
// the caller blocks on it immediately — it is a thread with a particular
// property, and a goroutine is the only way to ask for one.
//
// The thread is never unlocked. It has the container's mount namespace, root
// and working directory, and handing it back to the runtime would eventually
// schedule an unrelated goroutine onto it — one that would then resolve host
// paths inside a container. Letting the locked goroutine return instead makes
// the runtime terminate the thread, which is exactly the disposal wanted.
//
// Waiting is deliberately left to the caller, on an ordinary thread. Once the
// child exists it is a child of the process rather than of the thread, so any
// thread may wait for it — and the alternative would keep the poisoned thread
// alive for the whole life of the command.
func (r *Runner) startInNamespaces(
	ctx context.Context,
	handles []namespace.Handle,
	cfg process.Config,
) (*process.Process, error) {
	type outcome struct {
		p   *process.Process
		err error
	}
	done := make(chan outcome, 1)

	go func() {
		goruntime.LockOSThread()
		// No UnlockOSThread, and no defer: see above. The thread dies with
		// this goroutine.

		if err := namespace.Enter(handles); err != nil {
			done <- outcome{err: err}
			return
		}

		// Resolved after the join, so PATH is searched in the container's
		// filesystem — the only place the answer means anything — and by a
		// thread that can actually see it.
		path, err := resolveCommand(cfg.Args[0], cfg.Env)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		cfg.Path = path

		p, err := process.New(cfg)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		if err := p.Start(ctx); err != nil {
			done <- outcome{err: err}
			return
		}

		done <- outcome{p: p}
	}()

	res := <-done

	return res.p, res.err
}
