package runtime

import (
	"log/slog"
	"syscall"
	"time"

	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/state"
)

// The Stage 6 half of the orchestration: keeping the on-disk record in step
// with what is actually happening to a container.
//
// internal/state stores records and decides nothing; every decision about
// *when* a container's status changes, what a status change implies, and which
// resources a status makes it safe to release is cross-package sequencing and
// so lives here (SSOT §2, §13.2). This mirrors limits.go and network.go, which
// do the same job for cgroups and networking.
//
// # Why a failed write is not a failed container
//
// The record is created before anything else exists, and that write is fatal:
// a container Forge cannot name is a container it cannot clean up, so there is
// no point starting one. Every write *after* that is best-effort and logged at
// WARN rather than returned.
//
// The asymmetry is deliberate. Once a container is running, the alternative to
// a stale record is killing a working container because a JSON file could not
// be replaced, and that trade is not worth making: a user whose disk filled up
// would rather have their container and a wrong `forge ps` than neither. What
// it costs is honesty about the failure, which is what the WARN is for
// (SSOT §13.7).

// DefaultStateDir is where Forge persists container metadata when the caller
// names no other directory, per SSOT §9. It is the parent of the state store's
// own tree, not the tree itself.
//
// For a Forge run without root — which cannot start containers, but can still
// list and remove the records of containers that a privileged run left behind —
// state.DefaultRoot resolves the XDG per-user location instead.
const DefaultStateDir = "/var/lib/forge"

// Container is one container as reported to a caller: the row `forge ps`
// prints.
//
// It is the runtime's own view, translated from state.Metadata, so that
// internal/cli never sees the on-disk schema. A CLI formatting a
// state.Metadata directly would make the file format part of the user
// interface, and every schema change a CLI change (SSOT §13.6).
type Container struct {
	// ID is the container ID.
	ID string

	// Image is the reference the container was created from, empty for a
	// container run from a -rootfs directory.
	Image string

	// Command is the argument vector the container runs.
	Command []string

	// Status is where the container is in its lifecycle.
	Status string

	// PID is the host PID of the container's init process, or 0 if it never
	// got that far.
	PID int

	// Created is when Forge first touched the host for this container.
	Created time.Time

	// Started is when the container's own binary began executing, or nil if
	// it never did.
	Started *time.Time

	// Finished is when the container terminated, or nil while it runs.
	Finished *time.Time

	// ExitCode is how it terminated, or nil if no exit was ever observed —
	// which is different from exiting 0 and is reported differently.
	ExitCode *int

	// Rootfs is the container's root filesystem directory on the host.
	Rootfs string

	// Network is how the container is attached to the network.
	Network string
}

// Running reports whether the container is one a caller could still stop.
func (c Container) Running() bool {
	switch state.Status(c.Status) {
	case state.StatusCreated, state.StatusRunning, state.StatusStopping:
		return true
	default:
		return false
	}
}

// containerFrom translates a record into the reported view.
func containerFrom(m state.Metadata) Container {
	return Container{
		ID:       m.ID,
		Image:    m.Image,
		Command:  m.Command,
		Status:   string(m.Status),
		PID:      m.PID,
		Created:  m.CreatedAt,
		Started:  m.StartedAt,
		Finished: m.FinishedAt,
		ExitCode: m.ExitCode,
		Rootfs:   m.RootfsPath,
		Network:  m.NetworkMode,
	}
}

// containerProcess is a container's init process as seen from a Forge that did
// not start it: something to signal and to ask about, never to wait on.
//
// It is an interface defined at the point of use (SSOT §4) with one production
// implementation, process.Handle. The second implementation is the fake in
// this package's tests, and it is the reason the interface exists: `forge
// stop`'s whole behaviour is a conversation with a process — signal, wait, kill
// — and none of it should need root, a real container, or a timing-dependent
// test to exercise.
type containerProcess interface {
	// Signal sends a signal to the process.
	Signal(sig syscall.Signal) error

	// Alive reports whether the process is still running.
	Alive() bool

	// Close releases the handle.
	Close() error
}

// openContainerProcess is the production opener: a real pidfd on a real PID.
func openContainerProcess(pid int) (containerProcess, error) {
	h, err := process.Open(pid)
	if err != nil {
		// Returned as a nil interface rather than a typed nil, which would be
		// non-nil when compared against nil by the caller.
		return nil, err
	}
	return h, nil
}

// createRecord writes the container's first record, before any resource
// exists.
//
// This is the first thing Forge does to the host on a container's behalf, and
// deliberately so: from here on, everything created is attributable to an ID
// that something on disk knows about. It is also the only record write whose
// failure stops the run.
func (r *Runner) createRecord(id string, spec Spec) error {
	return r.state.Save(state.Metadata{
		ID:          id,
		Image:       spec.Image,
		Command:     spec.Command,
		Status:      state.StatusCreating,
		CreatedAt:   time.Now().UTC(),
		NetworkMode: string(spec.NetworkMode()),
	})
}

// note applies a mutation to a container's record, logging rather than
// returning a failure. See the header comment for why.
func (r *Runner) note(log *slog.Logger, id, what string, mutate func(*state.Metadata) error) {
	if err := r.state.Update(id, mutate); err != nil {
		log.Warn("recording "+what, "error", err)
	}
}

// recordFilesystem notes where the container's root filesystem is, so that
// `forge rm` can remove a directory whose location it was not told at the time.
func (r *Runner) recordFilesystem(log *slog.Logger, id, dir string) {
	r.note(log, id, "the container root filesystem", func(m *state.Metadata) error {
		m.RootfsPath = dir
		return nil
	})
}

// recordCreated notes the container's PID, as soon as there is one.
//
// It happens in the handshake window, while the container's init is still
// blocked on its payload pipe, for the same reason the cgroup attach does: the
// PID is the only way anything outside this process can find the container
// again, and recording it after the container has begun running would leave a
// window in which a `forge stop` could find a container it could not signal.
func (r *Runner) recordCreated(log *slog.Logger, id string, pid int) {
	r.note(log, id, "the container pid", func(m *state.Metadata) error {
		m.PID = pid
		m.Status = state.StatusCreated
		return nil
	})
}

// recordRunning notes that the container's own binary is now executing.
func (r *Runner) recordRunning(log *slog.Logger, id string) {
	now := time.Now().UTC()
	r.note(log, id, "the container start", func(m *state.Metadata) error {
		m.Status = state.StatusRunning
		m.StartedAt = &now
		return nil
	})
}

// recordExit notes how the container terminated.
//
// Whether that is `exited` or `stopped` is not this process's to decide from
// what it saw: a `forge stop` in another terminal is what makes the difference,
// and the only trace of it is the `stopping` status that stop wrote. So the
// record is read for the answer rather than assumed — the container's own
// supervisor learns from the record that its container was asked to stop
// (SSOT §12).
func (r *Runner) recordExit(log *slog.Logger, id string, status process.Status) {
	now := time.Now().UTC()
	code := status.Code

	r.note(log, id, "the container exit", func(m *state.Metadata) error {
		m.Status = terminalFor(m.Status)
		m.ExitCode = &code
		m.FinishedAt = &now
		return nil
	})
}

// terminalFor returns the terminal status a container in the given status ends
// in: stopped if somebody asked it to stop, exited if it went on its own.
func terminalFor(current state.Status) state.Status {
	if current == state.StatusStopping || current == state.StatusStopped {
		return state.StatusStopped
	}
	return state.StatusExited
}
