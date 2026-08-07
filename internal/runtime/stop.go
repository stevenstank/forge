package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"syscall"
	"time"

	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/state"
)

// Stop timings (FR-6.3).
const (
	// DefaultStopTimeout is how long a container is given to exit after
	// SIGTERM before SIGKILL, matching Docker's default. Ten seconds is long
	// enough for a process that handles the signal to finish what it is doing
	// and short enough that a user who typed `forge stop` does not think the
	// command has hung.
	DefaultStopTimeout = 10 * time.Second

	// KillGrace is how long SIGKILL is given afterwards. It is not a courtesy
	// to the container — SIGKILL is not refusable — but time for the kernel to
	// tear down a process that may have a lot of memory to unmap, and for its
	// parent to reap it.
	KillGrace = 5 * time.Second

	// defaultPollInterval is how often Stop re-checks whether the container
	// has gone. It is a field on Runner so tests can shorten it.
	defaultPollInterval = 25 * time.Millisecond
)

// StopOptions configures a stop.
type StopOptions struct {
	// Timeout is how long the container is given after SIGTERM. Zero means
	// DefaultStopTimeout.
	Timeout time.Duration

	// Remove removes the container once it has stopped, as `forge stop --rm`
	// does.
	Remove bool
}

// timeout resolves the configured grace period.
func (o StopOptions) timeout() time.Duration {
	if o.Timeout <= 0 {
		return DefaultStopTimeout
	}
	return o.Timeout
}

// Stop terminates a running container (FR-6.3).
//
// It sends SIGTERM, waits up to the timeout, and sends SIGKILL if the container
// is still there. Once the container is gone its runtime resources are
// released and its record is moved to a terminal status.
//
// It is idempotent. Stopping a container that has already stopped is success,
// not an error: a caller scripting a shutdown should not have to race the
// container to find out whether its own command was necessary.
//
// # Who declares the container dead
//
// The interesting case is that Forge has no daemon, so the process running
// Stop is almost never the container's parent — and only a parent can reap a
// child or read its exit status. The `forge run` supervising the container is
// the one that will collect it and write the exit code.
//
// So Stop signals, and then watches for either of two things: the process
// disappearing, or the record going terminal. Whichever happens first, it does
// not invent an exit code it cannot know. If the supervisor is doing its job
// the record gains the real exit status and Stop simply observes it; if the
// supervisor is gone — killed, crashed — Stop finalises the record itself, with
// no exit code, because there is genuinely nobody left who saw one. A null exit
// code is honest where a fabricated 137 is not.
func (r *Runner) Stop(ctx context.Context, id string, opts StopOptions) error {
	log := r.logger.With("container_id", id)

	m, err := r.state.Load(id)
	if err != nil {
		return translateStateError(id, err)
	}

	// Already finished. Releasing its resources again is free and covers the
	// container whose supervisor died between exiting and cleaning up, so it
	// happens before the early return rather than instead of it.
	if m.Status.Terminal() {
		if err := r.releaseRuntimeResources(log, id); err != nil {
			return err
		}
		if opts.Remove {
			return r.Remove(ctx, id, RemoveOptions{})
		}
		return nil
	}

	if m.Status == state.StatusRemoving {
		return fmt.Errorf("%w: %s is being removed", ErrNotRunning, id)
	}

	if err := r.signalAndWait(ctx, log, m, opts); err != nil {
		return err
	}

	// The container is gone. Its network and cgroup go with it — the retained
	// resources (its filesystem, its record) survive until `forge rm`, which is
	// what makes `forge ps -a` able to describe a container that has stopped
	// (§10 of the Stage 6 design).
	if err := r.releaseRuntimeResources(log, id); err != nil {
		return err
	}

	if err := r.finaliseRecord(log, id); err != nil {
		return err
	}

	log.Info("container stopped")

	if opts.Remove {
		return r.Remove(ctx, id, RemoveOptions{})
	}

	return nil
}

// signalAndWait performs the SIGTERM, grace period, SIGKILL sequence and
// returns once the container is gone.
func (r *Runner) signalAndWait(ctx context.Context, log *slog.Logger, m state.Metadata, opts StopOptions) error {
	if m.PID <= 0 {
		// A container that never reached clone(2). There is nothing to signal,
		// and the resources it did acquire are released by the caller.
		log.Debug("container has no process to signal", "status", string(m.Status))
		return nil
	}

	proc, err := r.openProcess(m.PID)
	if err != nil {
		if errors.Is(err, process.ErrNoProcess) {
			// The container is already gone and nobody recorded it — its
			// supervisor died with it, or died before it. Finalising is the
			// caller's next step either way.
			log.Debug("container process is already gone", "pid", m.PID)
			return nil
		}
		return fmt.Errorf("container %s: %w", m.ID, err)
	}
	defer func() {
		if err := proc.Close(); err != nil {
			log.Warn("closing the container process handle", "error", err)
		}
	}()

	// Marked before the signal, so that a `forge ps` during the grace period
	// says "stopping" rather than "running", and so that the supervisor knows
	// to record this as a stop rather than an exit of the container's own
	// accord (SSOT §12).
	r.markStopping(log, m.ID)

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("container %s: %w", m.ID, err)
	}
	log.Debug("sent SIGTERM", "pid", m.PID, "timeout", opts.timeout().String())

	gone, err := r.awaitExit(ctx, m.ID, proc, opts.timeout())
	if err != nil {
		return err
	}
	if gone {
		return nil
	}

	// The container did not go. This is the ordinary outcome rather than an
	// exceptional one: a container's init is PID 1 of its PID namespace, and
	// the kernel discards a signal from an ancestor namespace unless the
	// process installed a handler for it. A shell, a `sleep`, and most
	// single-binary images install none, so their grace period always expires.
	// SIGKILL is special-cased by the kernel and always lands.
	log.Info("container did not exit after SIGTERM; sending SIGKILL",
		"pid", m.PID, "timeout", opts.timeout().String())

	if err := proc.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("container %s: %w", m.ID, err)
	}

	gone, err = r.awaitExit(ctx, m.ID, proc, r.killGrace)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("%w: container %s (pid %d) is still running %s after SIGKILL",
			ErrStopFailed, m.ID, m.PID, r.killGrace)
	}

	return nil
}

// awaitExit waits for the container to be gone, and reports whether it is.
//
// Either signal ends the wait. The process disappearing is the direct
// observation; the record going terminal is the container's own supervisor
// reporting that it has reaped it, which can happen first and carries the exit
// status this process could never obtain.
func (r *Runner) awaitExit(ctx context.Context, id string, proc containerProcess, within time.Duration) (bool, error) {
	deadline := time.NewTimer(within)
	defer deadline.Stop()

	poll := time.NewTicker(r.pollInterval)
	defer poll.Stop()

	for {
		if !proc.Alive() {
			return true, nil
		}
		if m, err := r.state.Load(id); err == nil && m.Status.Terminal() {
			return true, nil
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-poll.C:
		}
	}
}

// markStopping records that a stop is in flight.
//
// A failure here is logged rather than returned, for the reason record.go
// gives: the container is more important than the bookkeeping, and a stop that
// refused to proceed because it could not write a status would leave a
// container running that the user asked to have stopped.
func (r *Runner) markStopping(log *slog.Logger, id string) {
	r.note(log, id, "the stop request", func(m *state.Metadata) error {
		if err := m.Status.CanTransitionTo(state.StatusStopping); err != nil {
			// Not an error the caller needs to see: a container that reached a
			// terminal status while we were looking at it has done what was
			// asked of it.
			return nil
		}
		m.Status = state.StatusStopping
		return nil
	})
}

// finaliseRecord moves a stopped container's record to its terminal status.
//
// It leaves a record the supervisor already finished alone, exit code and all.
// Only when nobody else has done it — the supervisor died with its container —
// does this write the terminal status, and then without an exit code, because
// the process that could have observed one no longer exists.
func (r *Runner) finaliseRecord(log *slog.Logger, id string) error {
	err := r.state.Update(id, func(m *state.Metadata) error {
		if m.Status.Terminal() {
			return nil
		}

		m.Status = state.StatusStopped
		if m.FinishedAt == nil {
			now := time.Now().UTC()
			m.FinishedAt = &now
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			// Removed underneath us, which is what `forge rm -f` racing a
			// `forge stop` looks like. The container is stopped either way.
			return nil
		}
		return translateStateError(id, err)
	}

	return nil
}

// releaseRuntimeResources releases what a container may not hold once it is
// dead: its network and its cgroup.
//
// The order is the reverse of acquisition, as everywhere else in Forge
// (SSOT §11.3): the network first, because the container may still have an
// interface plugged into the bridge; the cgroup second, because a cgroup
// cannot be removed while a process is in it.
//
// Both Destroys are idempotent, so this is safe to run against a container
// that never had either, and safe to run twice — which it routinely is, since
// the supervising `forge run` unwinds the same two resources as it exits. Two
// processes releasing the same thing at the same time is the ordinary case
// here, not a race to be prevented.
//
// The container's filesystem, its logs and its record are deliberately not
// touched. They are what `forge ps -a` and `forge rm` are for.
func (r *Runner) releaseRuntimeResources(log *slog.Logger, id string) error {
	var errs []error

	if err := r.networks.Destroy(id); err != nil {
		errs = append(errs, err)
	}
	if err := r.cgroups.Destroy(id); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("container %s: releasing resources: %w", id, errors.Join(errs...))
	}

	log.Debug("released the container's network and cgroup")

	return nil
}

// translateStateError maps the state store's sentinels onto this package's, so
// callers branch on runtime errors rather than on storage errors.
func translateStateError(id string, err error) error {
	switch {
	case errors.Is(err, state.ErrNotFound):
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	case errors.Is(err, state.ErrInvalidID):
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	default:
		return fmt.Errorf("container %s: %w", id, err)
	}
}
