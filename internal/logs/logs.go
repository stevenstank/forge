// Package logs captures and replays a container's output.
//
// A container's stdout and stderr belong to whoever started it. For an
// attached `forge run` that is a terminal, and when the run ends the output is
// wherever the user's scrollback keeps it. `forge logs` exists because that is
// not good enough: a container that ran an hour ago, or one running under a
// `forge run` in a terminal somebody else has, still has output worth reading.
//
// # What it is not
//
// It knows nothing about containers beyond an ID-shaped name for a file. It
// does not read container state, which is why it cannot decide when following
// should stop — that answer lives in internal/state, and a package that
// reached for it would be the first primitive-to-primitive edge in the tree
// (SSOT §13.2). The caller closes a channel instead.
//
// # The on-disk shape
//
//	<root>/logs/<id>.log
//
// One file per container, appended to, holding both streams. One file rather
// than two is what makes ordering representable at all: stdout and stderr are
// interleaved by a program that expects them to arrive in the order it wrote
// them, and two files can only say what happened within each stream, never
// between them.
//
// # The format
//
// One JSON object per line, one line per write the container performed:
//
//	{"t":"2026-08-07T18:22:03.114233Z","s":"stdout","m":"hello\n"}
//	{"t":"2026-08-07T18:22:03.115901Z","s":"stderr","m":"sh: nope: not found\n"}
//
// The alternative — two raw files — is simpler to write and simpler to cat,
// and it cannot express the one thing a reader actually needs. JSON Lines
// costs one encoding call per write and answers all of it: which stream, in
// what order, and when.
//
// Framing is per write, not per line. What the writer receives is whatever the
// kernel handed it from the pipe, and a container that writes half a line and
// then thinks for a second should not have that half withheld. Readers
// concatenate the messages verbatim, so the bytes a reader sees are the bytes
// the container produced; the framing is only visible when timestamps are
// asked for.
//
// # Ordering
//
// Within a stream, the order is exactly the order the container wrote in.
// Between the two streams it is the order the writes arrived, which is the
// most any runtime can offer: stdout and stderr are separate pipes, and the
// kernel makes no promise about the relative order of two writes to two pipes.
// Docker's json-file driver behaves the same way for the same reason.
package logs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Stream is one of a container's two output streams.
type Stream string

// The streams a container writes to.
const (
	// Stdout is the container's standard output.
	Stdout Stream = "stdout"

	// Stderr is the container's standard error.
	Stderr Stream = "stderr"
)

// Valid reports whether s is a stream this package knows.
func (s Stream) Valid() bool { return s == Stdout || s == Stderr }

// Sentinel errors callers may branch on.
var (
	// ErrNotFound reports a container with no log file. It is what `forge
	// logs` reports for a container that never wrote anything, which is not
	// the same as a container that does not exist — that distinction belongs
	// to the caller, which has the record.
	ErrNotFound = errors.New("no logs for container")

	// ErrClosed reports use of a writer or reader after Close.
	ErrClosed = errors.New("log handle is closed")

	// ErrInvalidID reports an ID that cannot safely name a file.
	ErrInvalidID = errors.New("invalid container id")

	// ErrInvalidStream reports a write to something other than stdout or
	// stderr.
	ErrInvalidStream = errors.New("invalid stream")

	// ErrCorruptEntry reports a line that is not a log entry. A reader that
	// meets one returns it and carries on: a crash mid-write, or a file
	// somebody edited, must not cost the reader the rest of the log.
	ErrCorruptEntry = errors.New("corrupt log entry")
)

// Entry is one write a container performed.
type Entry struct {
	// Time is when the write reached Forge, not when the container made it.
	// The difference is a pipe, and it is microseconds.
	Time time.Time

	// Stream is which of the container's streams it came from.
	Stream Stream

	// Message is the bytes written, verbatim and unframed.
	//
	// It is a string rather than a []byte so the file stays readable — a
	// []byte marshals to base64, and a log nobody can cat gives up most of
	// what one-file-per-container was for. The cost is that output which is
	// not valid UTF-8 has its invalid bytes replaced when it is encoded, as
	// Docker's json-file driver also does. A container writing a binary
	// stream to stdout is not what this is for.
	Message string
}

// wireEntry is Entry as it appears on disk. The field names are short because
// they are repeated once per line, and the type is separate from Entry so the
// on-disk format is not the Go API — either can change without the other.
type wireEntry struct {
	Time    time.Time `json:"t"`
	Stream  Stream    `json:"s"`
	Message string    `json:"m"`
}

// Permissions for what this package creates. A container's output routinely
// contains everything its operator did not mean to print, so it is no more
// other users' to read than the record is.
const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// logSuffix is what a container's log file is called after its ID.
const logSuffix = ".log"

// Store owns the directory holding every container's log file.
//
// Construct it with New.
type Store struct {
	dir string
}

// New returns a Store keeping logs in dir.
//
// dir must be absolute, for the reason every other path in Forge must be: it
// is resolved independently by every command, and a relative one would mean a
// different directory depending on where forge was started.
//
// It performs no I/O. The directory is created by the first Open, which is
// also the first moment there is anything to put in it (SSOT §13, and the
// precedent image.NewCache set in Stage 5).
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("log directory is required")
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("log directory must be an absolute path, got %q", dir)
	}

	return &Store{dir: filepath.Clean(dir)}, nil
}

// Dir returns the directory the store manages.
func (s *Store) Dir() string { return s.dir }

// Path returns a container's log file. It touches nothing, and does not imply
// the file exists.
func (s *Store) Path(id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}

	return filepath.Join(s.dir, id+logSuffix), nil
}

// Remove deletes a container's log file.
//
// It is idempotent: removing the logs of a container that never wrote any, or
// whose logs are already gone, is not an error, so cleanup paths can call it
// unconditionally (SSOT §13.3).
func (s *Store) Remove(id string) error {
	path, err := s.Path(id)
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the container log %q: %w", path, err)
	}

	return nil
}

// ValidateID reports whether id can safely name a log file.
//
// The same containment check internal/state makes, for the same reason: an ID
// containing a separator or ".." would put a log outside the store, and Remove
// would then delete a file that is not Forge's. The duplication is deliberate
// — the alternative is an import between two primitives (SSOT §13.2).
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
		return fmt.Errorf("%w: %q begins with a dot", ErrInvalidID, id)
	}

	return nil
}
