package logs

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Writer appends a container's output to its log file.
//
// One writer exists per container, held by the process supervising it for the
// container's whole life. That is what makes the file's contents the order the
// writes happened in, and it is why there is no locking between processes
// here: the only concurrency is the two goroutines copying the container's two
// pipes, and they are in the same process.
type Writer struct {
	mu     sync.Mutex
	f      *os.File
	closed bool

	// now is the clock. It is a field so tests can pin timestamps instead of
	// asserting around whatever the wall clock said.
	now func() time.Time
}

// Open returns a writer appending to a container's log.
//
// The file is opened O_APPEND, which is what makes a second run of the same
// container add to its log rather than truncate it, and what keeps two writers
// — which should not exist, but might, if a supervisor was presumed dead — from
// overwriting each other's bytes. Every write lands at the end of the file as
// it is at that moment, decided by the kernel rather than by a cached offset.
//
// This is the first call that touches the disk; the directory is created here.
func (s *Store) Open(id string) (*Writer, error) {
	path, err := s.Path(id)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(s.dir, dirPerm); err != nil {
		return nil, fmt.Errorf("creating the log directory %q: %w", s.dir, err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("opening the container log %q: %w", path, err)
	}

	return &Writer{f: f, now: time.Now}, nil
}

// Write appends one entry to the log.
//
// It returns len(p) on success rather than the number of bytes it actually
// wrote, which is larger: the framing is this package's business, and a caller
// wiring this into io.Copy would see a short write and fail if it were told
// the truth about the file. The io.Writer contract is about the caller's
// bytes.
//
// An empty write is dropped rather than recorded. A zero-length entry says
// nothing and would still cost a line.
func (w *Writer) Write(stream Stream, p []byte) (int, error) {
	if !stream.Valid() {
		return 0, fmt.Errorf("%w: %q", ErrInvalidStream, string(stream))
	}
	if len(p) == 0 {
		return 0, nil
	}

	line, err := json.Marshal(wireEntry{
		Time:    w.now().UTC(),
		Stream:  stream,
		Message: string(p),
	})
	if err != nil {
		return 0, fmt.Errorf("encoding a log entry: %w", err)
	}
	line = append(line, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, ErrClosed
	}

	// One write syscall per entry, holding the lock across it. Both matter:
	// the lock is what keeps the two streams' goroutines from interleaving
	// halves of two lines, and the single write is what gives a concurrent
	// reader a whole line to find rather than a line under construction.
	if _, err := w.f.Write(line); err != nil {
		return 0, fmt.Errorf("writing to the container log: %w", err)
	}

	return len(p), nil
}

// Stream returns an io.Writer that records everything written to it as coming
// from that stream.
//
// This is what gets handed to process.Config as the container's Stdout or
// Stderr — or to an io.MultiWriter alongside the caller's terminal, for an
// attached run, which is the whole reason it is an io.Writer rather than a
// method the caller has to drive.
func (w *Writer) Stream(stream Stream) io.Writer {
	return streamWriter{w: w, stream: stream}
}

// Close flushes the log to disk and releases the file.
//
// It is idempotent, so a caller can defer it and still close explicitly on an
// error path, which is how every cleanup in Forge is written (SSOT §13.3).
//
// The Sync is the difference between a container's last words surviving a lost
// machine and not. Writes are not synced individually — a container that logs
// a line per millisecond would spend its life waiting on the disk — so this is
// the one point at which the log is made durable rather than merely written.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true

	syncErr := w.f.Sync()
	closeErr := w.f.Close()

	if syncErr != nil {
		return fmt.Errorf("flushing the container log: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing the container log: %w", closeErr)
	}

	return nil
}

// streamWriter adapts one stream of a Writer to io.Writer.
type streamWriter struct {
	w      *Writer
	stream Stream
}

// Write implements io.Writer.
func (s streamWriter) Write(p []byte) (int, error) { return s.w.Write(s.stream, p) }
