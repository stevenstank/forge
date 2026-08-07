package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// DefaultPollInterval is how often a following reader looks for new output.
//
// Polling rather than inotify: golang.org/x/sys/unix offers InotifyInit1, so
// this is a choice rather than a limitation. One file descriptor checked every
// 100ms costs nothing measurable, and it avoids the queue-overflow handling
// that makes inotify code long — for output a human is reading, prompt is what
// matters and exact is not. If following ever needs to be exact, inotify is a
// change behind this same reader.
const DefaultPollInterval = 100 * time.Millisecond

// ReadOptions configures a read.
type ReadOptions struct {
	// Tail is how many entries at the end of the log to start from. Zero
	// means the whole log.
	Tail int

	// Follow keeps the reader open after the end of the log, waiting for the
	// container to write more.
	Follow bool

	// Done ends a follow. The caller closes it when the container has
	// finished, and the reader then drains what is left and reports io.EOF.
	//
	// It is a channel rather than something this package works out for itself
	// because working it out means reading container state, and a log package
	// that imported internal/state would be the first edge between two
	// primitives (SSOT §13.2). A nil Done follows until the context is
	// cancelled, which is what a user pressing Ctrl-C does.
	Done <-chan struct{}

	// PollInterval is how often a following reader checks for new output.
	// Zero means DefaultPollInterval.
	PollInterval time.Duration
}

// pollInterval resolves the configured interval.
func (o ReadOptions) pollInterval() time.Duration {
	if o.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return o.PollInterval
}

// Reader replays a container's log.
//
// Readers are independent of each other and of the writer: each holds its own
// descriptor and its own position, and appending to a file cannot disturb a
// read already in progress. Any number can run at once, including while the
// container is still writing.
type Reader struct {
	f      *os.File
	br     *bufio.Reader
	opts   ReadOptions
	closed bool

	// pending is a line the writer has not finished. A reader can catch the
	// file part-way through an append and see a line with no newline on the
	// end of it; that is not a corrupt entry, it is an entry that does not
	// exist yet, and it is held here until the rest of it arrives.
	pending []byte

	// queued holds the entries Tail selected, waiting to be returned.
	queued []Entry

	// drained records that Done was observed and the extra pass over the file
	// it buys has been taken. See waitForMore.
	drained bool
}

// Read opens a container's log for reading.
//
// It returns ErrNotFound if the container has no log file, which for a
// container that ran and printed nothing is the ordinary case rather than a
// failure.
func (s *Store) Read(id string, opts ReadOptions) (*Reader, error) {
	path, err := s.Path(id)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("opening the container log %q: %w", path, err)
	}

	r := &Reader{f: f, br: bufio.NewReader(f), opts: opts}

	if opts.Tail > 0 {
		if err := r.seekToTail(opts.Tail); err != nil {
			return nil, errors.Join(err, r.Close())
		}
	}

	return r, nil
}

// seekToTail reads forward to the end of the log, keeping only the last n
// entries.
//
// It scans rather than seeking backwards from the end, and the trade is
// deliberate: scanning costs one pass over the file and holds n entries in
// memory, where seeking backwards costs nothing to read but needs code that
// hunts for line boundaries in a buffer it slides backwards through — and gets
// that wrong on exactly the log that is too big to test with. The property
// that matters for a large log is that the whole file is never held in memory,
// and a ring of n entries has that whatever the file's size.
func (r *Reader) seekToTail(n int) error {
	ring := make([]Entry, 0, n)

	for {
		line, err := r.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}

		entry, parseErr := parseEntry(line)
		if parseErr != nil {
			// A corrupt line inside the window being counted is skipped
			// rather than returned: Tail promises the last n entries, and a
			// line that is not an entry is not one of them.
			continue
		}

		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, entry)
	}

	r.queued = ring

	return nil
}

// Next returns the next entry.
//
// It reports io.EOF at the end of the log — immediately for a reader that is
// not following, and once the container has finished for one that is.
//
// A line that cannot be parsed is reported as ErrCorruptEntry and the reader
// stays usable: the next call carries on with the line after it. A crash
// mid-write, or a file somebody has edited, costs the entry it damaged and
// nothing else.
func (r *Reader) Next(ctx context.Context) (Entry, error) {
	if r.closed {
		return Entry{}, ErrClosed
	}

	if len(r.queued) > 0 {
		entry := r.queued[0]
		r.queued = r.queued[1:]
		return entry, nil
	}

	for {
		line, err := r.readLine()
		switch {
		case err == nil:
			entry, parseErr := parseEntry(line)
			if parseErr != nil {
				return Entry{}, parseErr
			}
			return entry, nil

		case !errors.Is(err, io.EOF):
			return Entry{}, err
		}

		// End of the log as it stands. Whether that is the end of the log
		// depends on whether anything is still writing to it.
		if !r.opts.Follow {
			return Entry{}, io.EOF
		}

		done, err := r.waitForMore(ctx)
		if err != nil {
			return Entry{}, err
		}
		if done {
			return Entry{}, io.EOF
		}
	}
}

// waitForMore blocks until there may be more output, and reports whether the
// log is finished.
//
// The handling of Done is what closes the last race in following, and it is
// why seeing Done does not end the follow on the spot. The order of events at
// the end of a container's life is:
//
//	the writer's last write   happens-before
//	the record going terminal happens-before
//	the caller closing Done   happens-before
//	this reader observing it
//
// So a reader that has seen Done has certainly been *given* every byte — but
// it arrived here by reading to EOF, and that read may have happened a moment
// before the last write landed. Observing Done therefore buys exactly one more
// pass over the file, and that pass is guaranteed to find whatever was
// outstanding. Only when it too comes up empty is the log finished.
func (r *Reader) waitForMore(ctx context.Context) (done bool, err error) {
	if r.drained {
		// The extra pass has already happened and found nothing.
		return true, nil
	}

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-r.opts.Done:
		r.drained = true
		return false, nil
	default:
	}

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-r.opts.Done:
		r.drained = true
		return false, nil
	case <-time.After(r.opts.pollInterval()):
		return false, nil
	}
}

// readLine returns the next complete line, without its newline.
//
// A line the writer has not finished is not a line. It is held in pending and
// io.EOF is reported, so the caller either stops — in which case a partial
// line left by a crashed writer is correctly ignored — or waits and asks
// again, by which time the rest of it has usually arrived.
func (r *Reader) readLine() ([]byte, error) {
	chunk, err := r.br.ReadBytes('\n')

	if err != nil {
		// Everything read so far is the start of a line that is still being
		// written. bufio.Reader clears its stored error after returning it, so
		// the next call reads from the file again and picks up the rest.
		r.pending = append(r.pending, chunk...)
		return nil, err
	}

	line := chunk[:len(chunk)-1]
	if len(r.pending) > 0 {
		line = append(r.pending, line...)
		r.pending = nil
	}

	return line, nil
}

// parseEntry decodes one line.
func parseEntry(line []byte) (Entry, error) {
	var wire wireEntry
	if err := json.Unmarshal(line, &wire); err != nil {
		return Entry{}, fmt.Errorf("%w: %w", ErrCorruptEntry, err)
	}
	if !wire.Stream.Valid() {
		return Entry{}, fmt.Errorf("%w: unknown stream %q", ErrCorruptEntry, string(wire.Stream))
	}

	return Entry{Time: wire.Time, Stream: wire.Stream, Message: wire.Message}, nil
}

// Close releases the reader's descriptor.
//
// It is idempotent. Every path that opens a reader defers this, which is what
// keeps a `forge logs -f` that a user interrupts, or a follow that ends on its
// own, from leaving a descriptor behind — and what lets a long-lived process
// read a thousand logs without running out of them.
func (r *Reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true

	if err := r.f.Close(); err != nil {
		return fmt.Errorf("closing the container log: %w", err)
	}

	return nil
}
