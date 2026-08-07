package runtime

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/logs"
	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/state"
)

// Log capture at the orchestration level: that a container's output reaches
// both the terminal and the file, and that `forge logs` gets it back out.
//
// Starting a real container needs root, so what is exercised here is the
// wiring either side of it — the tee that openLogs builds, and the reader
// Logs drives. The container itself is a writer, which is all it is to this
// code: os/exec hands the parent an io.Writer's worth of bytes and nothing
// about the mechanism cares where they came from.

// writeAs writes to a container's log the way a running container would, then
// closes the writer.
func writeAs(t *testing.T, r *Runner, id string, entries ...[2]string) {
	t.Helper()

	w, err := r.logs.Open(id)
	if err != nil {
		t.Fatalf("Open(%s) = %v", id, err)
	}
	for _, e := range entries {
		if _, err := w.Write(logs.Stream(e[0]), []byte(e[1])); err != nil {
			t.Fatalf("Write() = %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

// TestOpenLogsTeesToBothDestinations covers the wiring an attached run gets:
// the user's terminal keeps working exactly as it did before Stage 6, and the
// log is a second copy rather than a replacement.
func TestOpenLogsTeesToBothDestinations(t *testing.T) {
	r := testRunner(t, nil)
	seed(t, r, runningRecord("7f3c9a1b2d04"))

	var terminalOut, terminalErr bytes.Buffer
	spec := Spec{Stdout: &terminalOut, Stderr: &terminalErr}

	spec, w, err := r.openLogs(spec, "7f3c9a1b2d04")
	if err != nil {
		t.Fatalf("openLogs() = %v", err)
	}

	if _, err := io.WriteString(spec.Stdout, "to stdout\n"); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if _, err := io.WriteString(spec.Stderr, "to stderr\n"); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	if terminalOut.String() != "to stdout\n" {
		t.Errorf("the terminal's stdout = %q, want the container's output", terminalOut.String())
	}
	if terminalErr.String() != "to stderr\n" {
		t.Errorf("the terminal's stderr = %q", terminalErr.String())
	}

	var logOut, logErr bytes.Buffer
	if err := r.Logs(t.Context(), "7f3c9a1b2d04", LogOptions{}, &logOut, &logErr); err != nil {
		t.Fatalf("Logs() = %v", err)
	}
	if logOut.String() != "to stdout\n" {
		t.Errorf("the log's stdout = %q", logOut.String())
	}
	if logErr.String() != "to stderr\n" {
		t.Errorf("the log's stderr = %q", logErr.String())
	}
}

// TestOpenLogsWithNoTerminal covers a caller that supplies no writers, which
// is what a detached run will do. io.MultiWriter would panic on a nil writer,
// so the tee has to handle it rather than assume.
func TestOpenLogsWithNoTerminal(t *testing.T) {
	r := testRunner(t, nil)
	seed(t, r, runningRecord("7f3c9a1b2d04"))

	spec, w, err := r.openLogs(Spec{}, "7f3c9a1b2d04")
	if err != nil {
		t.Fatalf("openLogs() = %v", err)
	}
	if _, err := io.WriteString(spec.Stdout, "still captured\n"); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	var out bytes.Buffer
	if err := r.Logs(t.Context(), "7f3c9a1b2d04", LogOptions{}, &out, &out); err != nil {
		t.Fatalf("Logs() = %v", err)
	}
	if out.String() != "still captured\n" {
		t.Errorf("logs = %q, want the output of a container with no terminal", out.String())
	}
}

// TestCloseLogsHonoursRetention pins which containers keep their output: the
// log follows the record and the filesystem, so a run that leaves nothing
// behind leaves no log either, and one given -keep keeps all three.
func TestCloseLogsHonoursRetention(t *testing.T) {
	tests := []struct {
		name   string
		retain bool
		want   bool
	}{
		{name: "an ordinary run leaves nothing", retain: false},
		{name: "a kept run keeps its log", retain: true, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := testRunner(t, nil)
			const id = "7f3c9a1b2d04"

			_, w, err := r.openLogs(Spec{}, id)
			if err != nil {
				t.Fatalf("openLogs() = %v", err)
			}
			if _, err := w.Write(logs.Stdout, []byte("hello\n")); err != nil {
				t.Fatalf("Write() = %v", err)
			}

			retain := tc.retain
			if err := r.closeLogs(w, id, &retain); err != nil {
				t.Fatalf("closeLogs() = %v", err)
			}

			path, err := r.logs.Path(id)
			if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(path)
			if tc.want && statErr != nil {
				t.Errorf("the log was removed from a kept container: %v", statErr)
			}
			if !tc.want && !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("the log outlived a container that was not kept: %v", statErr)
			}
		})
	}
}

// TestLogsSeparatesTheStreams covers the reason Logs takes two writers: a
// caller redirecting one of them must get that stream and only that stream.
func TestLogsSeparatesTheStreams(t *testing.T) {
	r := testRunner(t, nil)
	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusExited
	seed(t, r, m)

	writeAs(t, r, m.ID,
		[2]string{"stdout", "one\n"},
		[2]string{"stderr", "two\n"},
		[2]string{"stdout", "three\n"},
	)

	var out, errOut bytes.Buffer
	if err := r.Logs(t.Context(), m.ID, LogOptions{}, &out, &errOut); err != nil {
		t.Fatalf("Logs() = %v", err)
	}

	if out.String() != "one\nthree\n" {
		t.Errorf("stdout = %q, want the stdout entries in order", out.String())
	}
	if errOut.String() != "two\n" {
		t.Errorf("stderr = %q", errOut.String())
	}

	// And with one writer they interleave in the order the container wrote,
	// which is what a terminal shows.
	var both bytes.Buffer
	if err := r.Logs(t.Context(), m.ID, LogOptions{}, &both, &both); err != nil {
		t.Fatalf("Logs() = %v", err)
	}
	if both.String() != "one\ntwo\nthree\n" {
		t.Errorf("interleaved = %q, want the original order", both.String())
	}
}

func TestLogsOptions(t *testing.T) {
	r := testRunner(t, nil)
	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusExited
	seed(t, r, m)

	writeAs(t, r, m.ID,
		[2]string{"stdout", "one\n"},
		[2]string{"stdout", "two\n"},
		[2]string{"stdout", "three\n"},
	)

	t.Run("tail", func(t *testing.T) {
		var out bytes.Buffer
		if err := r.Logs(t.Context(), m.ID, LogOptions{Tail: 2}, &out, &out); err != nil {
			t.Fatalf("Logs() = %v", err)
		}
		if out.String() != "two\nthree\n" {
			t.Errorf("tail 2 = %q", out.String())
		}
	})

	t.Run("timestamps", func(t *testing.T) {
		var out bytes.Buffer
		if err := r.Logs(t.Context(), m.ID, LogOptions{Timestamps: true, Tail: 1}, &out, &out); err != nil {
			t.Fatalf("Logs() = %v", err)
		}
		got := out.String()
		if !strings.HasSuffix(got, " three\n") {
			t.Errorf("timestamped entry = %q, want the message after a time", got)
		}
		if _, err := time.Parse(timestampLayout, strings.Fields(got)[0]); err != nil {
			t.Errorf("the prefix %q is not a timestamp: %v", strings.Fields(got)[0], err)
		}
	})
}

// TestLogsOfAContainerThatPrintedNothing covers a container with no log file,
// which is an ordinary outcome rather than a failure.
func TestLogsOfAContainerThatPrintedNothing(t *testing.T) {
	r := testRunner(t, nil)
	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusExited
	seed(t, r, m)

	var out bytes.Buffer
	if err := r.Logs(t.Context(), m.ID, LogOptions{}, &out, &out); err != nil {
		t.Fatalf("Logs() = %v, want nil for a silent container", err)
	}
	if out.Len() != 0 {
		t.Errorf("logs = %q, want nothing", out.String())
	}
}

func TestLogsOfAnUnknownContainer(t *testing.T) {
	r := testRunner(t, nil)

	var out bytes.Buffer
	if err := r.Logs(t.Context(), "0000deadbeef", LogOptions{}, &out, &out); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Logs() = %v, want ErrNotFound", err)
	}
}

// TestLogsFollowEndsWhenTheContainerStops is the half of following that
// internal/logs cannot do for itself: knowing that the container has finished
// is a question about its record, and this is where that question is answered.
func TestLogsFollowEndsWhenTheContainerStops(t *testing.T) {
	r := testRunner(t, nil)
	m := seed(t, r, runningRecord("7f3c9a1b2d04"))

	w, err := r.logs.Open(m.ID)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if _, err := w.Write(logs.Stdout, []byte("first\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	var (
		out  lockedBuffer
		done = make(chan error, 1)
	)
	go func() {
		done <- r.Logs(t.Context(), m.ID, LogOptions{Follow: true}, &out, &out)
	}()

	// The follow must still be running: the container has not finished.
	select {
	case err := <-done:
		t.Fatalf("the follow returned %v while the container was still running", err)
	case <-time.After(20 * time.Millisecond):
	}

	if _, err := w.Write(logs.Stdout, []byte("second\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	// The container exits, as its supervisor would record it.
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	r.recordExit(r.logger, m.ID, process.Status{Code: 0})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the follow returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follow did not end when the container stopped")
	}

	if got := out.String(); got != "first\nsecond\n" {
		t.Errorf("the follow produced %q, want everything the container wrote", got)
	}
}

// TestLogsFollowOnAFinishedContainerReturns covers `forge logs -f` on a
// container that has already stopped: there is nothing to wait for, and
// hanging would be the first thing a user hit.
func TestLogsFollowOnAFinishedContainerReturns(t *testing.T) {
	r := testRunner(t, nil)
	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusStopped
	seed(t, r, m)

	writeAs(t, r, m.ID, [2]string{"stdout", "all done\n"})

	returned := make(chan error, 1)
	var out bytes.Buffer
	go func() {
		returned <- r.Logs(t.Context(), m.ID, LogOptions{Follow: true}, &out, &out)
	}()

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Logs() = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("logs -f hung on a container that had already stopped")
	}

	if out.String() != "all done\n" {
		t.Errorf("logs = %q", out.String())
	}
}

// TestLogsSkipsACorruptEntry keeps one damaged line — a crash caught
// mid-write — from costing the user the rest of the log.
func TestLogsSkipsACorruptEntry(t *testing.T) {
	r := testRunner(t, nil)
	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusExited
	seed(t, r, m)

	path, err := r.logs.Path(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.logs.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"t":"2026-08-07T18:22:03Z","s":"stdout","m":"before\n"}` + "\n" +
		"}}} not an entry\n" +
		`{"t":"2026-08-07T18:22:04Z","s":"stdout","m":"after\n"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := r.Logs(t.Context(), m.ID, LogOptions{}, &out, &out); err != nil {
		t.Fatalf("Logs() = %v", err)
	}
	if out.String() != "before\nafter\n" {
		t.Errorf("logs = %q, want the entries either side of the damaged line", out.String())
	}
}

// TestLogsConcurrentReaders covers several `forge logs` running at once
// against a container that is still writing.
func TestLogsConcurrentReaders(t *testing.T) {
	r := testRunner(t, nil)
	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusExited
	seed(t, r, m)

	const lines = 100
	w, err := r.logs.Open(m.ID)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	for range lines {
		if _, err := w.Write(logs.Stdout, []byte(strings.Repeat("x", 100)+"\n")); err != nil {
			t.Fatalf("Write() = %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			var out bytes.Buffer
			if err := r.Logs(t.Context(), m.ID, LogOptions{}, &out, &out); err != nil {
				errs <- err
				return
			}
			if got := strings.Count(out.String(), "\n"); got != lines {
				errs <- errors.New("a reader saw an incomplete log")
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Logs = %v", err)
	}
}

// lockedBuffer is a bytes.Buffer that can be written from one goroutine while
// being read from another, which is what a follow under test needs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
