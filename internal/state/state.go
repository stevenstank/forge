// Package state persists what Forge knows about a container, so that a
// container outlives the process that created it.
//
// Every stage before this one holds a container entirely in the memory of the
// `forge run` that started it: the ID, the PID, the resources to release. That
// is enough while `run` is the only verb, and it is enough for nothing else.
// `forge ps` has to describe a container it did not create, `forge stop` has to
// find one, and both have to work after the process that started the container
// is gone. This package is the record that makes those questions answerable.
//
// # What it is not
//
// Per SSOT §2 it performs no kernel resource management. It does not create
// containers, does not signal processes, and — deliberately — does not ask
// whether a PID is still alive. A record says "this container's init was PID
// 41120"; deciding what that means today needs /proc, and belongs to the
// caller. Keeping that judgement out of this package is also what makes the
// whole of it testable with a temporary directory and no processes at all.
//
// It also stores no other package's types. A Metadata carrying a
// cgroup.Limits or a network.Mode would create exactly the
// primitive-to-primitive edge SSOT §13.2 forbids, and it would tie a format
// that has to stay readable across versions to structs that are free to
// change. Every field here is a string, an int, or a time.
//
// # The on-disk shape
//
//	<root>/state/containers/<id>/metadata.json   the record
//	<root>/state/containers/<id>/.lock           flock target, never read
//
// One file per container rather than one database for all of them (ADR-0006).
// The failure mode of a database is a corrupt database; the failure mode of a
// directory of small files is one unreadable file, which LoadAll reports and
// steps over. There is no query here more complex than "list them all", and a
// state store that needs a tool to inspect would sit badly in a project whose
// point is that you can read it.
//
// # The three properties that matter
//
// Writes are atomic. A record is written to a temporary file in its own
// directory, fsynced, and renamed over the live one; rename(2) within a
// directory either happens or does not, so a reader sees the whole old record
// or the whole new one and never a torn one. The directory is fsynced after
// the rename, which is what makes the new name survive power loss rather than
// merely a crash.
//
// Reads are lock-free. Atomicity is what buys that: Load opens whatever the
// name pointed at when it opened it, and a writer replacing the name
// underneath cannot affect a read already in flight.
//
// Read-modify-write is serialised. Update takes an exclusive flock on the
// container's own lock file for the whole of its read, mutate and write, so
// two callers changing a record at once take turns instead of interleaving.
// flock is released by the kernel when the holder exits — including a holder
// that was killed — so a crash cannot leave a container permanently locked.
package state

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Schema is the version of the on-disk format this build writes.
//
// It is written into every record and checked on every read. A record from a
// newer Forge is refused rather than guessed at: silently reinterpreting a
// format you do not know is how a state store corrupts itself.
const Schema = 1

// Sentinel errors callers may branch on.
var (
	// ErrNotFound reports a container with no record.
	ErrNotFound = errors.New("no such container")

	// ErrExists reports a Save for a container that already has a record.
	// Two containers sharing an ID would share a rootfs, a cgroup and an
	// interface name, so the collision is refused rather than resolved.
	ErrExists = errors.New("container already has a record")

	// ErrInvalidID reports an ID that cannot name a directory safely.
	ErrInvalidID = errors.New("invalid container id")

	// ErrInvalid reports metadata that fails Validate. It is returned before
	// anything is written: a record that could not be read back is worse than
	// no record.
	ErrInvalid = errors.New("invalid container metadata")

	// ErrCorrupt reports a record that is not readable as JSON, or whose
	// contents contradict themselves. The file is left exactly as it is —
	// this package never repairs, because a repair is a guess.
	ErrCorrupt = errors.New("corrupt container metadata")

	// ErrSchema reports a record written by a Forge that is newer than this
	// one.
	ErrSchema = errors.New("unsupported metadata schema")
)

// Status is where a container is in its lifecycle (SSOT §12).
type Status string

// The statuses a container moves through.
//
// Three of these describe transitions rather than resting places, and they
// exist because a crash is likeliest during a transition: StatusCreating and
// StatusRemoving mark the windows in which resources are being acquired and
// released, and a recovering caller needs to tell "half-built" from "built"
// and "half-removed" from "intact" by looking rather than by guessing.
const (
	// StatusCreating means the record exists and resources are being
	// acquired. Nothing of the user's has run.
	StatusCreating Status = "creating"

	// StatusCreated means every resource is prepared and the init process is
	// cloned but still blocked on its payload pipe (ADR-0008).
	StatusCreated Status = "created"

	// StatusRunning means the payload was written and the container's own
	// binary is executing.
	StatusRunning Status = "running"

	// StatusStopping means a stop was requested and the grace period is
	// running.
	StatusStopping Status = "stopping"

	// StatusExited means the init process terminated on its own.
	StatusExited Status = "exited"

	// StatusStopped means the init process was terminated by forge stop.
	StatusStopped Status = "stopped"

	// StatusRemoving means the retained resources are being released. A
	// record in this state is finished by whoever finds it.
	StatusRemoving Status = "removing"
)

// Valid reports whether s is a status this build knows.
func (s Status) Valid() bool {
	switch s {
	case StatusCreating, StatusCreated, StatusRunning, StatusStopping,
		StatusExited, StatusStopped, StatusRemoving:
		return true
	default:
		return false
	}
}

// Terminal reports whether the container has finished: its process is gone and
// its exit status, if it was ever observed, will not change again.
//
// StatusRemoving is not terminal in this sense. The container is finished, but
// the record is mid-flight and a caller that treats it as settled would list a
// container that is being deleted.
func (s Status) Terminal() bool {
	return s == StatusExited || s == StatusStopped
}

// String implements fmt.Stringer.
func (s Status) String() string { return string(s) }

// CanTransitionTo reports whether a container may move from s to next.
//
// The transition table is here, rather than in the caller, because it is pure:
// keeping it next to the statuses it constrains is what lets the whole
// lifecycle be tested without a filesystem, a process, or root. This package
// does not enforce it on Update — sequencing is internal/runtime's job
// (SSOT §13.2) — it only answers the question.
//
// The shape of the table is a one-way street with two exits. Nothing leaves a
// terminal state except into removal, so a late write from a supervisor that
// was presumed dead cannot resurrect a container someone has already
// reconciled.
func (s Status) CanTransitionTo(next Status) error {
	if !s.Valid() {
		return fmt.Errorf("%w: %q is not a status", ErrInvalid, string(s))
	}
	if !next.Valid() {
		return fmt.Errorf("%w: %q is not a status", ErrInvalid, string(next))
	}
	if s == next {
		// Re-asserting the current state is what an idempotent retry does.
		return nil
	}

	var allowed []Status
	switch s {
	case StatusCreating:
		// Creation can fail at any point, and a failure that never started
		// the container ends in exited with no exit code.
		allowed = []Status{StatusCreated, StatusExited}
	case StatusCreated:
		allowed = []Status{StatusRunning, StatusStopping, StatusExited, StatusStopped}
	case StatusRunning:
		allowed = []Status{StatusStopping, StatusExited, StatusStopped}
	case StatusStopping:
		allowed = []Status{StatusStopped, StatusExited}
	case StatusExited, StatusStopped:
		allowed = []Status{StatusRemoving}
	case StatusRemoving:
		allowed = nil
	}

	for _, a := range allowed {
		if next == a {
			return nil
		}
	}

	return fmt.Errorf("%w: %s to %s", ErrInvalid, s, next)
}

// Metadata is everything Forge persists about one container.
//
// Pointer fields are the ones that are genuinely unknown rather than zero.
// A container that is still running has no finish time, and a container whose
// supervisor was killed alongside it has no exit code that anyone observed —
// null says so, where 0 would claim it exited cleanly and -1 would invent a
// value the kernel never produced.
type Metadata struct {
	// Schema is the format version. Save fills it in; callers do not set it.
	Schema int `json:"schema"`

	// ID is the container ID (SSOT §8): 12 lowercase hex characters. It is
	// also the directory name this record lives in, which is why it is
	// validated as a path element rather than as a style.
	ID string `json:"id"`

	// Image is the reference the container was created from, as the user
	// wrote it. Empty for a container run from a -rootfs directory, which is
	// still a valid container (Stages 2 to 4).
	Image string `json:"image,omitempty"`

	// Command is the argument vector the container runs, after the image's
	// entrypoint and cmd have been resolved into it. It is what ps prints.
	Command []string `json:"command,omitempty"`

	// PID is the host's PID of the container's init process, or 0 before it
	// has been cloned.
	//
	// A PID is a hint, not an identity: PIDs are reused, and a caller that
	// signals this number without first checking that it is still the same
	// process will one day signal something else. Judging that needs /proc,
	// which is the caller's to read.
	PID int `json:"pid,omitempty"`

	// Status is where the container is in its lifecycle.
	Status Status `json:"status"`

	// ExitCode is how the container terminated: the exit status, or
	// 128+signal for a container killed by one, matching the convention
	// process.Status already uses. Nil means no exit was observed.
	ExitCode *int `json:"exit_code,omitempty"`

	// CreatedAt is when the record was created, which is the first moment
	// Forge touched the host on this container's behalf.
	CreatedAt time.Time `json:"created_at"`

	// StartedAt is when the container's own binary began executing. Nil
	// until it does, and nil forever for a container that failed to start.
	StartedAt *time.Time `json:"started_at,omitempty"`

	// FinishedAt is when the container terminated. Nil while it runs.
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// RootfsPath is the host directory that is the container's root
	// filesystem, <root>/<id>/rootfs. Empty for a container sharing the
	// host's filesystem, which is what Stage 1 did and remains valid.
	RootfsPath string `json:"rootfs_path,omitempty"`

	// NetworkMode is how the container is attached to the network — "bridge"
	// or "host". It is a string rather than a network.Mode because this
	// package imports no other primitive (SSOT §13.2).
	NetworkMode string `json:"network_mode,omitempty"`
}

// Validate reports whether the metadata can be persisted and read back
// meaningfully. It is pure and touches nothing.
//
// It runs before every write. A record that cannot be read back is worse than
// no record at all: the absence of a record is a state recovery knows how to
// handle, while a record that contradicts itself is one it has to guess at.
func (m Metadata) Validate() error {
	if err := ValidateID(m.ID); err != nil {
		return err
	}

	if m.Schema != 0 && m.Schema != Schema {
		return fmt.Errorf("%w: %d, this build writes %d", ErrInvalid, m.Schema, Schema)
	}

	if !m.Status.Valid() {
		return fmt.Errorf("%w: status %q", ErrInvalid, string(m.Status))
	}

	if m.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created time is required", ErrInvalid)
	}

	if m.PID < 0 {
		return fmt.Errorf("%w: pid %d is negative", ErrInvalid, m.PID)
	}

	if m.ExitCode != nil && (*m.ExitCode < 0 || *m.ExitCode > 255) {
		return fmt.Errorf("%w: exit code %d is outside 0 to 255", ErrInvalid, *m.ExitCode)
	}

	// The clock is not the point of these checks — a record whose times run
	// backwards is a record something built wrongly, and it is cheaper to
	// refuse it here than to explain a negative uptime in ps later.
	if m.StartedAt != nil && m.StartedAt.Before(m.CreatedAt) {
		return fmt.Errorf("%w: started before created", ErrInvalid)
	}
	if m.FinishedAt != nil {
		if m.FinishedAt.Before(m.CreatedAt) {
			return fmt.Errorf("%w: finished before created", ErrInvalid)
		}
		if m.StartedAt != nil && m.FinishedAt.Before(*m.StartedAt) {
			return fmt.Errorf("%w: finished before started", ErrInvalid)
		}
	}

	return nil
}

// Running reports whether the container is one a caller could still act on.
func (m Metadata) Running() bool {
	return m.Status == StatusCreated || m.Status == StatusRunning || m.Status == StatusStopping
}

// ValidateID reports whether id can safely name a directory inside the store.
//
// This is a containment check, not a style check, and it is the reason this
// package will not accept an arbitrary string: an ID containing a separator or
// ".." would place a record outside the store, and Remove would then delete
// something that is not Forge's. internal/network makes the same check for the
// same reason; the duplication is deliberate, since the alternative is an
// import between two primitives (SSOT §13.2).
func ValidateID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: empty", ErrInvalidID)
	case id == "." || id == "..":
		return fmt.Errorf("%w: %q is a path element with special meaning", ErrInvalidID, id)
	case strings.ContainsAny(id, `/\`):
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidID, id)
	case strings.ContainsRune(id, 0):
		return fmt.Errorf("%w: contains a NUL byte", ErrInvalidID)
	case strings.HasPrefix(id, "."):
		// The store's own bookkeeping is dot-prefixed, so this keeps a
		// container from ever colliding with it.
		return fmt.Errorf("%w: %q begins with a dot", ErrInvalidID, id)
	}

	return nil
}
