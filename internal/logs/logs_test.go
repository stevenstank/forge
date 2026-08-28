package logs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/logs"
)

const testID = "7f3c9a1b2d04"

// newStore returns a store over a temporary directory.
func newStore(t *testing.T) (*logs.Store, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "logs")
	store, err := logs.New(dir)
	if err != nil {
		t.Fatalf("New(%q) = %v", dir, err)
	}

	return store, dir
}

// openWriter opens a writer and closes it when the test ends.
func openWriter(t *testing.T, s *logs.Store, id string) *logs.Writer {
	t.Helper()

	w, err := s.Open(id)
	if err != nil {
		t.Fatalf("Open(%s) = %v", id, err)
	}
	t.Cleanup(func() { _ = w.Close() })

	return w
}

// drain reads every entry a reader will produce.
func drain(t *testing.T, r *logs.Reader) []logs.Entry {
	t.Helper()

	var entries []logs.Entry
	for {
		entry, err := r.Next(t.Context())
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatalf("Next() = %v", err)
		}
		entries = append(entries, entry)
	}
}

// readAll opens a reader, drains it, and closes it.
func readAll(t *testing.T, s *logs.Store, id string) []logs.Entry {
	t.Helper()

	r, err := s.Read(id, logs.ReadOptions{})
	if err != nil {
		t.Fatalf("Read(%s) = %v", id, err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	}()

	return drain(t, r)
}

// messages joins the entries of one stream, which is what a reader printing
// them produces.
func messages(entries []logs.Entry, stream logs.Stream) string {
	var b strings.Builder
	for _, e := range entries {
		if e.Stream == stream {
			b.WriteString(e.Message)
		}
	}

	return b.String()
}

func TestNewRejectsUnusableDirectories(t *testing.T) {
	for _, dir := range []string{"", "logs", "./logs"} {
		if _, err := logs.New(dir); err == nil {
			t.Errorf("New(%q) = nil, want an error", dir)
		}
	}
}

// TestNewPerformsNoIO pins the invariant: constructing a store touches
// nothing, so a forge that never runs a container leaves no directories.
func TestNewPerformsNoIO(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")

	if _, err := logs.New(dir); err != nil {
		t.Fatalf("New() = %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("New created %q; it must perform no I/O", dir)
	}
}

// TestCaptureStdout is FR-6.4's first half: what a container writes to stdout
// comes back out, byte for byte.
func TestCaptureStdout(t *testing.T) {
	store, dir := newStore(t)
	w := openWriter(t, store, testID)

	out := w.Stream(logs.Stdout)
	for _, line := range []string{"hello\n", "from ", "forge\n"} {
		if n, err := io.WriteString(out, line); err != nil || n != len(line) {
			t.Fatalf("Write(%q) = %d, %v, want %d, nil", line, n, err, len(line))
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	entries := readAll(t, store, testID)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: framing is per write", len(entries))
	}
	if got := messages(entries, logs.Stdout); got != "hello\nfrom forge\n" {
		t.Errorf("stdout = %q, want the bytes the container wrote", got)
	}
	for _, e := range entries {
		if e.Stream != logs.Stdout {
			t.Errorf("entry stream = %q, want stdout", e.Stream)
		}
		if e.Time.IsZero() {
			t.Error("entry has no timestamp")
		}
	}

	// The documented layout, checked rather than assumed.
	if _, err := os.Stat(filepath.Join(dir, testID+".log")); err != nil {
		t.Errorf("log is not at <dir>/<id>.log: %v", err)
	}
}

// TestCaptureStderr is the other half, and the reason the two are tagged
// rather than merged: a caller has to be able to get one without the other.
func TestCaptureStderr(t *testing.T) {
	store, _ := newStore(t)
	w := openWriter(t, store, testID)

	if _, err := io.WriteString(w.Stream(logs.Stderr), "sh: nope: not found\n"); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if _, err := io.WriteString(w.Stream(logs.Stdout), "fine\n"); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	entries := readAll(t, store, testID)
	if got := messages(entries, logs.Stderr); got != "sh: nope: not found\n" {
		t.Errorf("stderr = %q", got)
	}
	if got := messages(entries, logs.Stdout); got != "fine\n" {
		t.Errorf("stdout = %q", got)
	}
}

// TestPreservesOrderingAcrossStreams is why both streams share one file: a
// program that writes a prompt to stdout and a complaint to stderr expects
// them to come back in that order, and two files could never say so.
func TestPreservesOrderingAcrossStreams(t *testing.T) {
	store, _ := newStore(t)
	w := openWriter(t, store, testID)

	want := []struct {
		stream logs.Stream
		msg    string
	}{
		{logs.Stdout, "one\n"},
		{logs.Stderr, "two\n"},
		{logs.Stderr, "three\n"},
		{logs.Stdout, "four\n"},
	}
	for _, x := range want {
		if _, err := w.Write(x.stream, []byte(x.msg)); err != nil {
			t.Fatalf("Write() = %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	entries := readAll(t, store, testID)
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d", len(entries), len(want))
	}
	for i, x := range want {
		if entries[i].Stream != x.stream || entries[i].Message != x.msg {
			t.Errorf("entry %d = %s %q, want %s %q", i,
				entries[i].Stream, entries[i].Message, x.stream, x.msg)
		}
	}

	// Timestamps are non-decreasing, so a reader sorting by them would not
	// disturb the order the container wrote in.
	for i := 1; i < len(entries); i++ {
		if entries[i].Time.Before(entries[i-1].Time) {
			t.Errorf("entry %d is stamped before entry %d", i, i-1)
		}
	}
}

// TestRestartRecovery is the point of persisting at all: a log written by one
// process is read whole by another, and appended to rather than truncated.
func TestRestartRecovery(t *testing.T) {
	store, dir := newStore(t)

	first := openWriter(t, store, testID)
	if _, err := first.Write(logs.Stdout, []byte("before the restart\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	// A different process, knowing only the directory.
	restarted, err := logs.New(dir)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	second, err := restarted.Open(testID)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if _, err := second.Write(logs.Stderr, []byte("after the restart\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	entries := readAll(t, restarted, testID)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want both writes: the second Open truncated the log", len(entries))
	}
	if entries[0].Message != "before the restart\n" || entries[1].Message != "after the restart\n" {
		t.Errorf("entries = %q, %q, want them in the order they were written",
			entries[0].Message, entries[1].Message)
	}
	if entries[0].Stream != logs.Stdout || entries[1].Stream != logs.Stderr {
		t.Error("the streams did not survive the restart")
	}
}

// TestFollowMode covers -f: a reader that reaches the end of the log waits
// there, and picks up what the container writes next.
func TestFollowMode(t *testing.T) {
	store, _ := newStore(t)
	w := openWriter(t, store, testID)

	if _, err := w.Write(logs.Stdout, []byte("first\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	done := make(chan struct{})
	r, err := store.Read(testID, logs.ReadOptions{
		Follow:       true,
		Done:         done,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	defer func() { _ = r.Close() }()

	// What is already there arrives without waiting for anything.
	entry, err := r.Next(t.Context())
	if err != nil {
		t.Fatalf("Next() = %v", err)
	}
	if entry.Message != "first\n" {
		t.Fatalf("first entry = %q", entry.Message)
	}

	// The reader is now at the end of the log. It must block there rather
	// than reporting EOF, and see what is written next.
	type result struct {
		entries []logs.Entry
		err     error
	}
	got := make(chan result, 1)
	go func() {
		var entries []logs.Entry
		for {
			e, err := r.Next(t.Context())
			if err != nil {
				got <- result{entries, err}
				return
			}
			entries = append(entries, e)
		}
	}()

	for _, msg := range []string{"second\n", "third\n"} {
		if _, err := w.Write(logs.Stdout, []byte(msg)); err != nil {
			t.Fatalf("Write() = %v", err)
		}
	}

	// The container finishes. The follow ends on its own — after draining
	// what is left, which is what the second read past Done is for.
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	close(done)

	select {
	case res := <-got:
		if !errors.Is(res.err, io.EOF) {
			t.Fatalf("the follow ended with %v, want io.EOF", res.err)
		}
		if len(res.entries) != 2 {
			t.Fatalf("the follow saw %d entries, want 2: %v", len(res.entries), res.entries)
		}
		if res.entries[0].Message != "second\n" || res.entries[1].Message != "third\n" {
			t.Errorf("the follow saw %q and %q", res.entries[0].Message, res.entries[1].Message)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follow never ended after Done was closed")
	}
}

// TestFollowSeesWritesThatLandAsTheContainerFinishes is the race the second
// read past Done exists to close: the last write and the close happen
// together, and a reader that stopped the moment it saw Done would lose the
// container's final words.
func TestFollowSeesWritesThatLandAsTheContainerFinishes(t *testing.T) {
	for range 50 {
		store, _ := newStore(t)
		w := openWriter(t, store, testID)

		done := make(chan struct{})
		r, err := store.Read(testID, logs.ReadOptions{
			Follow:       true,
			Done:         done,
			PollInterval: time.Millisecond,
		})
		if err != nil {
			t.Fatalf("Read() = %v", err)
		}

		// Write and signal completion as close together as possible, which is
		// what a container exiting looks like.
		go func() {
			_, _ = w.Write(logs.Stdout, []byte("last words\n"))
			close(done)
		}()

		entries := drain(t, r)
		if err := r.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}

		if len(entries) != 1 || entries[0].Message != "last words\n" {
			t.Fatalf("the follow lost the final write: %v", entries)
		}
	}
}

// TestFollowStopsOnContextCancellation covers the user pressing Ctrl-C.
func TestFollowStopsOnContextCancellation(t *testing.T) {
	store, _ := newStore(t)
	w := openWriter(t, store, testID)
	if _, err := w.Write(logs.Stdout, []byte("hello\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	r, err := store.Read(testID, logs.ReadOptions{Follow: true, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	defer func() { _ = r.Close() }()

	if _, err := r.Next(t.Context()); err != nil {
		t.Fatalf("Next() = %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := r.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next() = %v, want context.Canceled", err)
	}
}

// TestConcurrentReaders runs readers against a log that is being written,
// which is what `forge logs` in three terminals during a run looks like. Each
// must see whole entries, never a line the writer had not finished.
func TestConcurrentReaders(t *testing.T) {
	store, _ := newStore(t)
	w := openWriter(t, store, testID)

	const (
		readers = 6
		writes  = 200
	)

	var (
		start   sync.WaitGroup
		writing sync.WaitGroup
		reading sync.WaitGroup
	)
	start.Add(1)
	writing.Add(1)
	reading.Add(readers)

	writeErrs := make(chan error, 1)
	go func() {
		defer writing.Done()

		start.Wait()
		for i := range writes {
			// Long messages, so a reader has a real chance of catching the
			// file part-way through an append.
			msg := fmt.Sprintf("%04d %s\n", i, strings.Repeat("x", 512))
			stream := logs.Stdout
			if i%3 == 0 {
				stream = logs.Stderr
			}
			if _, err := w.Write(stream, []byte(msg)); err != nil {
				writeErrs <- err
				return
			}
		}
	}()

	readErrs := make(chan error, readers*4)
	counts := make([]int, readers)
	done := make(chan struct{})

	for i := range readers {
		go func() {
			defer reading.Done()

			start.Wait()
			for {
				select {
				case <-done:
					return
				default:
				}

				r, err := store.Read(testID, logs.ReadOptions{})
				if err != nil {
					readErrs <- err
					return
				}

				for {
					entry, err := r.Next(t.Context())
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						readErrs <- err
						_ = r.Close()
						return
					}
					// A torn read shows up here long before it fails to
					// parse: every entry is one whole write or it is nothing.
					if !strings.HasSuffix(entry.Message, "\n") || len(entry.Message) != 4+1+512+1 {
						readErrs <- fmt.Errorf("partial entry: %q", entry.Message)
						_ = r.Close()
						return
					}
					counts[i]++
				}

				if err := r.Close(); err != nil {
					readErrs <- err
					return
				}
			}
		}()
	}

	start.Done()
	writing.Wait()
	close(done)
	reading.Wait()
	close(writeErrs)
	close(readErrs)

	for err := range writeErrs {
		t.Errorf("Write during concurrent reads = %v", err)
	}
	for err := range readErrs {
		t.Errorf("Read during concurrent writes = %v", err)
	}

	var total int
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		t.Fatal("no entries were read; the test proved nothing")
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if got := len(readAll(t, store, testID)); got != writes {
		t.Errorf("the finished log has %d entries, want %d", got, writes)
	}
}

// TestConcurrentWritersAreSerialised covers the two goroutines os/exec uses to
// copy a container's two pipes: they write at the same time, and neither may
// land inside the other's line.
func TestConcurrentWritersAreSerialised(t *testing.T) {
	store, _ := newStore(t)
	w := openWriter(t, store, testID)

	const perStream = 300

	var wg sync.WaitGroup
	wg.Add(2)
	for _, stream := range []logs.Stream{logs.Stdout, logs.Stderr} {
		go func() {
			defer wg.Done()

			for i := range perStream {
				msg := fmt.Sprintf("%s %03d %s\n", stream, i, strings.Repeat("y", 300))
				if _, err := w.Write(stream, []byte(msg)); err != nil {
					t.Errorf("Write() = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	entries := readAll(t, store, testID)
	if len(entries) != 2*perStream {
		t.Fatalf("got %d entries, want %d", len(entries), 2*perStream)
	}

	// Each stream's own writes are still in order, whatever the interleaving
	// between them.
	next := map[logs.Stream]int{logs.Stdout: 0, logs.Stderr: 0}
	for _, e := range entries {
		want := fmt.Sprintf("%s %03d %s\n", e.Stream, next[e.Stream], strings.Repeat("y", 300))
		if e.Message != want {
			t.Fatalf("out of order or torn entry for %s: got %.20q, want %.20q", e.Stream, e.Message, want)
		}
		next[e.Stream]++
	}
}

// TestLargeLog covers output no reader should ever hold in memory at once, and
// a single entry larger than any buffer the reader uses.
func TestLargeLog(t *testing.T) {
	store, dir := newStore(t)
	w := openWriter(t, store, testID)

	// One entry far larger than bufio's default 4KB buffer, which is what a
	// line-scanning reader would break on.
	huge := strings.Repeat("z", 512*1024) + "\n"
	if _, err := w.Write(logs.Stdout, []byte(huge)); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	const lines = 4000
	for i := range lines {
		msg := fmt.Sprintf("%05d %s\n", i, strings.Repeat("w", 1024))
		if _, err := w.Write(logs.Stdout, []byte(msg)); err != nil {
			t.Fatalf("Write() = %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, testID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 4<<20 {
		t.Fatalf("the log is only %d bytes; the test is not exercising a large log", info.Size())
	}

	entries := readAll(t, store, testID)
	if len(entries) != lines+1 {
		t.Fatalf("got %d entries, want %d", len(entries), lines+1)
	}
	if entries[0].Message != huge {
		t.Errorf("the oversized entry came back as %d bytes, want %d", len(entries[0].Message), len(huge))
	}

	// Tail reads the same file and returns only what was asked for.
	r, err := store.Read(testID, logs.ReadOptions{Tail: 3})
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	defer func() { _ = r.Close() }()

	tail := drain(t, r)
	if len(tail) != 3 {
		t.Fatalf("Tail(3) returned %d entries", len(tail))
	}
	for i, e := range tail {
		want := fmt.Sprintf("%05d ", lines-3+i)
		if !strings.HasPrefix(e.Message, want) {
			t.Errorf("tail entry %d = %.10q, want it to start %q", i, e.Message, want)
		}
	}
}

func TestTail(t *testing.T) {
	store, _ := newStore(t)
	w := openWriter(t, store, testID)

	for i := range 10 {
		if _, err := w.Write(logs.Stdout, []byte(fmt.Sprintf("%d\n", i))); err != nil {
			t.Fatalf("Write() = %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	tests := []struct {
		tail int
		want []string
	}{
		{tail: 0, want: []string{"0\n", "1\n", "2\n", "3\n", "4\n", "5\n", "6\n", "7\n", "8\n", "9\n"}},
		{tail: 1, want: []string{"9\n"}},
		{tail: 3, want: []string{"7\n", "8\n", "9\n"}},
		{tail: 100, want: []string{"0\n", "1\n", "2\n", "3\n", "4\n", "5\n", "6\n", "7\n", "8\n", "9\n"}},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("tail %d", tc.tail), func(t *testing.T) {
			r, err := store.Read(testID, logs.ReadOptions{Tail: tc.tail})
			if err != nil {
				t.Fatalf("Read() = %v", err)
			}
			defer func() { _ = r.Close() }()

			got := drain(t, r)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d entries, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i].Message != tc.want[i] {
					t.Errorf("entry %d = %q, want %q", i, got[i].Message, tc.want[i])
				}
			}
		})
	}
}

// TestNoDescriptorLeaks is the property a long-lived process depends on: a
// forge that reads a thousand logs must not run out of file descriptors.
func TestNoDescriptorLeaks(t *testing.T) {
	store, _ := newStore(t)

	w := openWriter(t, store, testID)
	if _, err := w.Write(logs.Stdout, []byte("hello\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	before := openDescriptors(t)

	for range 200 {
		writer, err := store.Open(testID)
		if err != nil {
			t.Fatalf("Open() = %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}

		reader, err := store.Read(testID, logs.ReadOptions{})
		if err != nil {
			t.Fatalf("Read() = %v", err)
		}
		if _, err := reader.Next(t.Context()); err != nil {
			t.Fatalf("Next() = %v", err)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}
	}

	// A reader abandoned mid-read still releases its descriptor on Close,
	// which is the case a cancelled `forge logs -f` produces.
	for range 200 {
		reader, err := store.Read(testID, logs.ReadOptions{Follow: true, PollInterval: time.Hour})
		if err != nil {
			t.Fatalf("Read() = %v", err)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}
	}

	if after := openDescriptors(t); after > before {
		t.Errorf("open descriptors went from %d to %d; something is not being closed", before, after)
	}
}

// openDescriptors counts this process's open file descriptors.
func openDescriptors(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd to count descriptors with: %v", err)
	}

	return len(entries)
}

// TestCloseIsIdempotent lets callers defer Close and still close explicitly on
// an error path (SSOT §13.3).
func TestCloseIsIdempotent(t *testing.T) {
	store, _ := newStore(t)

	w, err := store.Open(testID)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if _, err := w.Write(logs.Stdout, []byte("hello\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	for i := range 3 {
		if err := w.Close(); err != nil {
			t.Fatalf("writer Close() call %d = %v", i+1, err)
		}
	}
	if _, err := w.Write(logs.Stdout, []byte("more\n")); !errors.Is(err, logs.ErrClosed) {
		t.Errorf("Write() after Close = %v, want ErrClosed", err)
	}

	r, err := store.Read(testID, logs.ReadOptions{})
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	for i := range 3 {
		if err := r.Close(); err != nil {
			t.Fatalf("reader Close() call %d = %v", i+1, err)
		}
	}
	if _, err := r.Next(t.Context()); !errors.Is(err, logs.ErrClosed) {
		t.Errorf("Next() after Close = %v, want ErrClosed", err)
	}
}

// TestReadReportsAContainerWithNoLog covers a container that printed nothing.
func TestReadReportsAContainerWithNoLog(t *testing.T) {
	store, _ := newStore(t)

	if _, err := store.Read(testID, logs.ReadOptions{}); !errors.Is(err, logs.ErrNotFound) {
		t.Fatalf("Read() = %v, want ErrNotFound", err)
	}
}

// TestPartialLineIsNotAnEntry covers a writer killed mid-write: the trailing
// fragment it left is not a log entry and must not be reported as one, nor as
// corruption.
func TestPartialLineIsNotAnEntry(t *testing.T) {
	store, dir := newStore(t)
	w := openWriter(t, store, testID)

	if _, err := w.Write(logs.Stdout, []byte("complete\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	// The wreckage of a write that never finished.
	f, err := os.OpenFile(filepath.Join(dir, testID+".log"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"t":"2026-08-07T18:22:03Z","s":"stdo`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	entries := readAll(t, store, testID)
	if len(entries) != 1 || entries[0].Message != "complete\n" {
		t.Fatalf("got %v, want just the complete entry", entries)
	}
}

// TestCorruptEntryIsReportedAndSkipped covers a damaged line in the middle of
// a log: it costs the entry it damaged and nothing else.
func TestCorruptEntryIsReportedAndSkipped(t *testing.T) {
	store, dir := newStore(t)
	path := filepath.Join(dir, testID+".log")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"t":"2026-08-07T18:22:03Z","s":"stdout","m":"first\n"}` + "\n" +
		"not json at all\n" +
		`{"t":"2026-08-07T18:22:04Z","s":"stdout","m":"third\n"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := store.Read(testID, logs.ReadOptions{})
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	defer func() { _ = r.Close() }()

	first, err := r.Next(t.Context())
	if err != nil || first.Message != "first\n" {
		t.Fatalf("first entry = %q, %v", first.Message, err)
	}

	if _, err := r.Next(t.Context()); !errors.Is(err, logs.ErrCorruptEntry) {
		t.Fatalf("Next() on a damaged line = %v, want ErrCorruptEntry", err)
	}

	// And the reader carries on.
	third, err := r.Next(t.Context())
	if err != nil || third.Message != "third\n" {
		t.Fatalf("third entry = %q, %v", third.Message, err)
	}
}

func TestWriteRejectsAnUnknownStream(t *testing.T) {
	store, _ := newStore(t)
	w := openWriter(t, store, testID)

	if _, err := w.Write("sideways", []byte("x")); !errors.Is(err, logs.ErrInvalidStream) {
		t.Errorf("Write() = %v, want ErrInvalidStream", err)
	}
	if n, err := w.Write(logs.Stdout, nil); n != 0 || err != nil {
		t.Errorf("Write(nil) = %d, %v, want 0, nil", n, err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if got := readAll(t, store, testID); len(got) != 0 {
		t.Errorf("got %d entries, want none written", len(got))
	}
}

// TestRemoveIsIdempotent covers the cleanup path `forge rm` uses.
func TestRemoveIsIdempotent(t *testing.T) {
	store, dir := newStore(t)
	w := openWriter(t, store, testID)

	if _, err := w.Write(logs.Stdout, []byte("hello\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	for i := range 3 {
		if err := store.Remove(testID); err != nil {
			t.Fatalf("Remove() call %d = %v", i+1, err)
		}
	}
	if err := store.Remove("0000deadbeef"); err != nil {
		t.Fatalf("Remove() of a container with no log = %v, want nil", err)
	}

	if _, err := os.Stat(filepath.Join(dir, testID+".log")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Remove left the log behind: %v", err)
	}
	if _, err := store.Read(testID, logs.ReadOptions{}); !errors.Is(err, logs.ErrNotFound) {
		t.Errorf("Read after Remove = %v, want ErrNotFound", err)
	}
}

// TestRejectsEscapingIDs is the containment check: an ID is a file name, and
// one with a separator in it would put a log outside the store — and have
// Remove delete a file that is not Forge's.
func TestRejectsEscapingIDs(t *testing.T) {
	store, _ := newStore(t)

	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\b`, ".hidden", "nul\x00byte"} {
		if _, err := store.Path(id); !errors.Is(err, logs.ErrInvalidID) {
			t.Errorf("Path(%q) = %v, want ErrInvalidID", id, err)
		}
		if _, err := store.Open(id); !errors.Is(err, logs.ErrInvalidID) {
			t.Errorf("Open(%q) = %v, want ErrInvalidID", id, err)
		}
		if _, err := store.Read(id, logs.ReadOptions{}); !errors.Is(err, logs.ErrInvalidID) {
			t.Errorf("Read(%q) = %v, want ErrInvalidID", id, err)
		}
		if err := store.Remove(id); !errors.Is(err, logs.ErrInvalidID) {
			t.Errorf("Remove(%q) = %v, want ErrInvalidID", id, err)
		}
	}
}

// TestLogFileIsReadableJSON holds the format to the argument for choosing it:
// a log has to be legible to somebody with a terminal and no forge.
func TestLogFileIsReadableJSON(t *testing.T) {
	store, dir := newStore(t)
	w := openWriter(t, store, testID)

	if _, err := w.Write(logs.Stdout, []byte("hello\n")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, testID+".log"))
	if err != nil {
		t.Fatal(err)
	}

	line := string(data)
	for _, want := range []string{`"s":"stdout"`, `"m":"hello\n"`, `"t":"`} {
		if !strings.Contains(line, want) {
			t.Errorf("the log line is missing %s:\n%s", want, line)
		}
	}
	if !strings.HasSuffix(line, "\n") {
		t.Error("the log does not end in a newline")
	}
}

func TestFilePermissions(t *testing.T) {
	store, dir := newStore(t)
	w := openWriter(t, store, testID)
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, testID+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log mode = %#o, want %#o", perm, 0o600)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("log directory mode = %#o, want %#o", perm, 0o700)
	}
}

// TestStoreDirReportsWhereItWrites pins the accessor `forge rm` and the runtime
// use to name a container's log in an error, and the cleaning New applies to it.
func TestStoreDirReportsWhereItWrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	store, err := logs.New(dir)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if got := store.Dir(); got != dir {
		t.Errorf("Dir() = %q, want %q", got, dir)
	}

	// A path spelled with a trailing separator, or with a redundant "." in it,
	// names the same directory and must be reported the one way.
	store, err = logs.New(dir + "/./")
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if got := store.Dir(); got != dir {
		t.Errorf("Dir() = %q, want it cleaned to %q", got, dir)
	}
}
