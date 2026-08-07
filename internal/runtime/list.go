package runtime

import "errors"

// List returns the containers Forge knows about (FR-6.1).
//
// With all false it reports only the containers that are still live — created,
// running, or stopping — which is what `forge ps` shows and what Docker shows.
// With all true it reports every record, including the containers that have
// finished and are waiting for `forge rm`.
//
// # Why it returns errors alongside the containers
//
// A state directory can hold a record Forge cannot read: one truncated by a
// crash mid-write, or written by a newer version. Exactly one of those must
// not happen: a single bad file hiding the other nine containers from the
// command a user runs to find out what is going on. So the readable records
// and the unreadable ones are both returned, and `forge ps` prints the first
// and warns about the second.
//
// # What List does not do
//
// It reports what the records say, and does not go looking to see whether they
// are still true. A container whose supervising `forge run` was killed leaves a
// record that still says `running`, and List will say `running` too.
//
// Reconciling that — checking each recorded PID and rewriting the records of
// containers that have died unobserved — is real work with a real failure mode
// of its own, and it belongs to the crash-recovery half of Stage 6 rather than
// being smuggled into a listing. What exists today is that `forge stop` handles
// the case correctly when it meets it: a container whose process is gone is
// finalised rather than signalled.
func (r *Runner) List(all bool) ([]Container, []error) {
	records, errs := r.state.LoadAll()

	containers := make([]Container, 0, len(records))
	for _, m := range records {
		c := containerFrom(m)
		if !all && !c.Running() {
			continue
		}
		containers = append(containers, c)
	}

	return containers, errs
}

// Inspect returns one container's full record.
//
// It is what `forge stop` and `forge rm` use to find out what they are acting
// on, and it is exported because a caller that has an ID and wants to know
// whether it is worth stopping should not have to list every container to find
// out.
func (r *Runner) Inspect(id string) (Container, error) {
	m, err := r.state.Load(id)
	if err != nil {
		return Container{}, translateStateError(id, err)
	}

	return containerFrom(m), nil
}

// Sentinel errors the Stage 6 verbs return, which internal/cli maps onto exit
// codes (SSOT §9).
var (
	// ErrNotFound reports an ID with no container behind it.
	ErrNotFound = errors.New("no such container")

	// ErrRunning reports an operation that requires a stopped container,
	// attempted on a running one.
	ErrRunning = errors.New("container is running")

	// ErrNotRunning reports an operation that requires a running container.
	ErrNotRunning = errors.New("container is not running")

	// ErrStopFailed reports a container that was still running after SIGKILL
	// and the grace period that follows it. It means something is wrong at the
	// kernel level — an unkillable process stuck in uninterruptible sleep —
	// rather than that the container is being stubborn.
	ErrStopFailed = errors.New("container did not stop")
)
