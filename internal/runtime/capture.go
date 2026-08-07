package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/stevenstank/forge/internal/logs"
)

// The Stage 6 half of the orchestration that deals with a container's output
// (FR-6.4).
//
// internal/logs owns the file and its format and decides nothing; the policy
// below — that output goes to the log *and* the terminal, that the log is
// acquired after the record and before the filesystem, and when a follow is
// finished — is cross-package sequencing and so lives here (SSOT §2, §13.2).
//
// # Where the output is intercepted
//
// Nowhere. The container's stdout and stderr are already pipes that the parent
// copies from, because that is what os/exec does with an io.Writer, and Stage
// 1 has been handing it the caller's terminal since the beginning. Capturing
// output is therefore not a new mechanism but a second destination:
//
//	spec.Stdout ─┬─▶ the user's terminal   (attached runs, as before)
//	             └─▶ the container's log   (always)
//
// The copying goroutines os/exec starts are joined by Wait before it returns,
// so by the time a run reaches its cleanup there are no writes still in
// flight, and the log can be closed without racing them.

// LogOptions configures a read of a container's logs.
type LogOptions struct {
	// Follow keeps printing until the container finishes.
	Follow bool

	// Tail is how many entries at the end to start from. Zero means all.
	Tail int

	// Timestamps prefixes each entry with when Forge received it.
	Timestamps bool
}

// timestampLayout is how `forge logs -t` renders a time: RFC 3339 to the
// microsecond. Nanoseconds are noise from a pipe read, and seconds are too
// coarse to order the output of a program that is doing anything.
const timestampLayout = "2006-01-02T15:04:05.000000Z07:00"

// Logs writes a container's captured output to stdout and stderr (FR-6.4).
//
// The two streams are kept apart, so a caller piping `forge logs` gets the
// container's stdout and only its stdout — the same thing they would have got
// from the run itself. Their relative order is preserved because both come
// from one file.
//
// With Follow it does not return until the container finishes or ctx is
// cancelled. Whether the container has finished is a question about its
// record, which internal/logs cannot answer and this package can, so the
// watch below is what ends the follow.
func (r *Runner) Logs(ctx context.Context, id string, opts LogOptions, stdout, stderr io.Writer) error {
	m, err := r.state.Load(id)
	if err != nil {
		return translateStateError(id, err)
	}

	readOpts := logs.ReadOptions{
		Tail:         opts.Tail,
		Follow:       opts.Follow,
		PollInterval: r.pollInterval,
	}

	if opts.Follow {
		// A container that has already finished is not followed: there is
		// nothing left to wait for, and a `forge logs -f` on a stopped
		// container that hung would be a bug users would meet immediately.
		if m.Status.Terminal() {
			readOpts.Follow = false
		} else {
			done, stop := r.watchForExit(ctx, id)
			defer stop()
			readOpts.Done = done
		}
	}

	reader, err := r.logs.Read(id, readOpts)
	if err != nil {
		if errors.Is(err, logs.ErrNotFound) {
			// A container that printed nothing has no file. That is not a
			// failure, and reporting one would have every script that reads
			// logs special-case silence.
			return nil
		}
		return fmt.Errorf("container %s: %w", id, err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			r.logger.With("container_id", id).Warn("closing the container log", "error", err)
		}
	}()

	return r.copyLog(ctx, reader, opts, stdout, stderr)
}

// copyLog writes every entry the reader produces.
func (r *Runner) copyLog(ctx context.Context, reader *logs.Reader, opts LogOptions, stdout, stderr io.Writer) error {
	log := r.logger

	for {
		entry, err := reader.Next(ctx)
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case errors.Is(err, logs.ErrCorruptEntry):
			// One damaged line — a crash caught mid-write — must not cost the
			// user the rest of the log. Reported rather than swallowed
			// (SSOT §13.7), and then skipped.
			log.Warn("skipping a corrupt log entry", "error", err)
			continue
		case err != nil:
			return err
		}

		w := stdout
		if entry.Stream == logs.Stderr {
			w = stderr
		}
		if w == nil {
			continue
		}

		if opts.Timestamps {
			if _, err := io.WriteString(w, entry.Time.Format(timestampLayout)+" "); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, entry.Message); err != nil {
			return err
		}
	}
}

// watchForExit returns a channel that is closed once the container's record
// reaches a terminal status, and a function to stop watching.
//
// This is the answer internal/logs cannot work out for itself. It polls the
// record rather than watching the process, because the record is what carries
// the container's death across process boundaries — the `forge run` that
// supervises the container writes it, and this may be a `forge logs` in an
// entirely different terminal.
func (r *Runner) watchForExit(ctx context.Context, id string) (<-chan struct{}, func()) {
	done := make(chan struct{})
	watchCtx, cancel := context.WithCancel(ctx)

	go func() {
		defer close(done)

		ticker := time.NewTicker(r.pollInterval)
		defer ticker.Stop()

		for {
			if m, err := r.state.Load(id); err != nil || m.Status.Terminal() {
				// A record that has gone — removed underneath us — ends the
				// follow too. There is nothing left to wait for.
				return
			}

			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	return done, func() {
		cancel()
		<-done
	}
}

// openLogs starts capturing a container's output, and returns the spec with
// its streams teed into the log.
//
// The caller's own writers are preserved rather than replaced: an attached run
// still puts the container's output on the user's terminal, exactly as every
// stage before this one did, and the log is a second copy. A detached run will
// pass nil writers and get only the log, with no branch needed here.
func (r *Runner) openLogs(spec Spec, id string) (Spec, *logs.Writer, error) {
	w, err := r.logs.Open(id)
	if err != nil {
		return spec, nil, err
	}

	spec.Stdout = tee(spec.Stdout, w.Stream(logs.Stdout))
	spec.Stderr = tee(spec.Stderr, w.Stream(logs.Stderr))

	return spec, w, nil
}

// tee returns a writer that writes to both, skipping a nil destination.
//
// io.MultiWriter cannot be handed a nil io.Writer — it would panic on the
// first write — and a nil Stdout is how a caller says "discard", which Stage 1
// has always allowed.
func tee(caller, log io.Writer) io.Writer {
	if caller == nil {
		return log
	}
	return io.MultiWriter(caller, log)
}

// closeLogs closes the log writer and, for a container that is not being kept,
// removes the file.
//
// Both failures are returned joined rather than one masking the other: a log
// that could not be flushed and a log that could not be removed are different
// problems, and the cleanup stack logs whatever it is given (SSOT §13.7).
func (r *Runner) closeLogs(w *logs.Writer, id string, retain *bool) error {
	err := w.Close()

	if !*retain {
		err = errors.Join(err, r.logs.Remove(id))
	}

	return err
}
