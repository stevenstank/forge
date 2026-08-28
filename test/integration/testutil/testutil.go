//go:build integration

// Package testutil holds the helpers Forge's privileged integration tests share.
//
// It sits behind the "integration" build tag with the tests themselves, so
// nothing here is compiled into an ordinary `go test` run (SSOT §13.8).
//
// Two things live here. The first is a thin reader for the cgroup v2 interface
// files a test needs to assert against — the tests read the real hierarchy
// under /sys/fs/cgroup, never a fake, because the whole point of an integration
// test is to find out what the kernel actually did.
//
// The second is Live: a container that keeps running while the test inspects
// it. Every other Forge test asserts on what a finished container left behind,
// but a cgroup's counters are destroyed with the cgroup, so Stage 3 has to look
// while the container is still there.
package testutil

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/runtime"
)

// Where Forge's cgroups live, mirroring internal/cgroup's own constants. They
// are repeated rather than imported so that a test failure means "the kernel
// does not have what we expected" rather than "the package agrees with itself".
const (
	CgroupRoot  = "/sys/fs/cgroup"
	ForgeCgroup = CgroupRoot + "/forge"
)

// Polling parameters. Nothing in these tests sleeps for a fixed duration to
// wait for a state change (SSOT §7): every wait is a condition polled until it
// holds or a deadline expires, so a fast machine is fast and a slow one is
// still correct.
const (
	PollInterval   = 2 * time.Millisecond
	DefaultTimeout = 30 * time.Second
)

// RequireCgroupV2 skips a test when the host has no usable cgroup v2 unified
// hierarchy, naming what is missing.
//
// A cgroup v1 or hybrid host cannot run any of Stage 3, and skipping with a
// clear reason is more useful than failing a suite that is otherwise fine.
func RequireCgroupV2(t *testing.T) {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(CgroupRoot, "cgroup.controllers"))
	if err != nil {
		t.Skipf("cgroup v2 unified hierarchy not mounted at %s: %v", CgroupRoot, err)
	}

	available := make(map[string]bool)
	for _, name := range strings.Fields(string(content)) {
		available[name] = true
	}
	for _, want := range []string{"cpu", "memory", "pids"} {
		if !available[want] {
			t.Skipf("the %q controller is not available at %s (have: %s)",
				want, CgroupRoot, strings.TrimSpace(string(content)))
		}
	}
}

// Leaves returns the names of the container cgroups currently under Forge's own
// cgroup. A missing parent is an empty list, not an error: it has simply not
// been created yet.
func Leaves(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(ForgeCgroup)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("reading %s: %v", ForgeCgroup, err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}

	return names
}

// RemoveForgeCgroupIfEmpty tidies up Forge's own cgroup at the end of a test.
//
// Forge deliberately never removes it — concurrent runs would race to remove a
// directory the other is about to create in — but a test suite should leave the
// host's hierarchy exactly as it found it (PRD NFR-5), so the tests do what
// Forge will not. It is best effort: a parent that is not empty belongs to
// something else and is left alone.
func RemoveForgeCgroupIfEmpty(t *testing.T) {
	t.Helper()

	if len(Leaves(t)) == 0 {
		// A cgroup that another test's container has just entered, or that a
		// concurrent run recreated, is not this helper's to insist on.
		if err := os.Remove(ForgeCgroup); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Logf("leaving %s in place: %v", ForgeCgroup, err)
		}
	}
}

// PollUntil waits for cond to hold, failing the test if it has not by the
// deadline. what names the condition, so a timeout says what never happened.
//
// Nothing here waits for a fixed duration and then assumes: the condition is
// re-evaluated on a ticker until it holds, so a fast machine proceeds
// immediately and a slow one is given the whole timeout before it is called a
// failure (SSOT §7).
func PollUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()

	if cond() {
		return
	}

	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		select {
		case <-ticker.C:
			if cond() {
				return
			}
		case <-deadline.C:
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
	}
}

// ReadInt64Quiet is ReadInt64 for callers that cannot fail a test: a sampling
// goroutine, where t.Fatalf would be reported from the wrong goroutine, and
// where the file legitimately vanishes when the container exits.
func ReadInt64Quiet(path string) (int64, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	value, err := strconv.ParseInt(strings.TrimSpace(string(content)), 10, 64)
	if err != nil {
		return 0, false
	}

	return value, true
}

// SamplePeak watches a cgroup file holding a single number and reports the
// highest value it saw before stop was closed.
//
// Some of what Stage 3 asserts is a claim about every moment rather than about
// the end state — "pids.current never exceeds pids.max" is only interesting
// while the container is still forking — and a single reading afterwards would
// not support it.
func SamplePeak(path string, stop <-chan struct{}) <-chan int64 {
	peak := make(chan int64, 1)

	go func() {
		ticker := time.NewTicker(PollInterval)
		defer ticker.Stop()

		highest := int64(0)
		for {
			select {
			case <-ticker.C:
				if value, ok := ReadInt64Quiet(path); ok && value > highest {
					highest = value
				}
			case <-stop:
				peak <- highest
				return
			}
		}
	}()

	return peak
}

// WaitForNewLeaf waits for a container cgroup that was not in before to appear,
// and returns its full path.
//
// This is how a test learns the container's cgroup: Run does not report the
// container ID, and the leaf exists only while the container does.
func WaitForNewLeaf(t *testing.T, before []string, timeout time.Duration) string {
	t.Helper()

	existing := make(map[string]bool, len(before))
	for _, name := range before {
		existing[name] = true
	}

	var found string
	PollUntil(t, "the container's cgroup to appear under "+ForgeCgroup, timeout, func() bool {
		for _, name := range Leaves(t) {
			if !existing[name] {
				found = filepath.Join(ForgeCgroup, name)
				return true
			}
		}
		return false
	})

	return found
}

// ReadInt64 reads a cgroup interface file holding a single number, such as
// pids.current. "max" reads as -1, matching internal/cgroup's convention.
func ReadInt64(t *testing.T, path string) int64 {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	text := strings.TrimSpace(string(content))
	if text == "max" {
		return -1
	}

	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		t.Fatalf("%s contains %q, want a number: %v", path, text, err)
	}

	return value
}

// Procs returns the PIDs that are members of the cgroup at dir, from
// cgroup.procs.
//
// This is how a test asks "is anything in this cgroup?". It deliberately does
// not use pids.current, which looks like the obvious choice and is a trap: a
// controller's interface files exist in a cgroup only when that controller is
// enabled in its parent's cgroup.subtree_control, and Forge delegates only the
// controllers a container's limits actually require. A leaf created with no
// limits, or with only a memory limit, therefore has no pids.current at all,
// and reading it fails with ENOENT — which is easily misread as the cgroup
// having been destroyed. cgroup.procs is a core interface file and is present
// in every cgroup unconditionally.
func Procs(t *testing.T, dir string) []int {
	t.Helper()

	path := filepath.Join(dir, "cgroup.procs")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var pids []int
	for _, field := range strings.Fields(string(content)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("%s contains %q, want process IDs: %v", path, field, err)
		}
		pids = append(pids, pid)
	}

	return pids
}

// KeyedValue reads one field from a cgroup "flat keyed" file — memory.events,
// cpu.stat, pids.events — each line of which is "key value".
func KeyedValue(t *testing.T, path, key string) int64 {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	for line := range strings.SplitSeq(strings.TrimSpace(string(content)), "\n") {
		name, value, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found || name != key {
			continue
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			t.Fatalf("%s: %s = %q, want a number: %v", path, key, value, err)
		}
		return parsed
	}

	t.Fatalf("%s has no %q field:\n%s", path, key, content)
	return 0
}

// SyncBuffer is an io.Writer a test can read while a container is still writing
// to it. The container runs in its own goroutine, so an ordinary bytes.Buffer
// would be a data race.
type SyncBuffer struct {
	mu      sync.Mutex
	content strings.Builder
}

// Write implements io.Writer.
func (b *SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.content.Write(p)
}

// String returns everything written so far.
func (b *SyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.content.String()
}

// Live is a container that runs until the test tells it to stop.
//
// The container's binary and the test speak a two-line protocol over the
// container's stdin and stdout, which is what makes these tests deterministic
// without a single fixed-duration wait:
//
//	container  → "ready"        once it is up and idle
//	test       → "go"           when the test has finished preparing the cgroup
//	container  → whatever it observed
//	test       → closes stdin   which is the container's signal to exit
//
// The container's stdin is a real os.Pipe rather than an io.Pipe, and that is
// load-bearing rather than incidental. os/exec wires an *os.File straight
// through to the child as a file descriptor, but any other io.Reader gets a
// copying goroutine — and cmd.Wait does not return until that goroutine stops
// reading. A test that holds the container's stdin open while waiting for the
// container to die on its own would therefore wait forever: the container is
// gone, but the copier is still blocked on a Read that will never complete, so
// Run cannot return. With a real pipe there is no copier, and Run returns as
// soon as the container does, whoever is still holding the write end.
type Live struct {
	Stdout *SyncBuffer
	Stderr *SyncBuffer

	stdin  *os.File
	reader *os.File
	cancel context.CancelFunc
	done   chan struct{}

	status process.Status
	err    error
}

// StartLive starts a container and returns without waiting for it.
//
// Cleanup is registered before the container exists: whatever the test does or
// fails to do afterwards, stdin is closed, the container is given the chance to
// exit, and it is killed through the context if it does not take it. A test
// that fails half-way therefore still leaves no container and no cgroup behind
// (PRD NFR-5).
func StartLive(ctx context.Context, t *testing.T, spec runtime.Spec) *Live {
	t.Helper()

	return StartLiveWithRunner(ctx, t, NewRunner(t), spec)
}

// NewRunner returns a Runner whose state, containers and logs all live under
// the test's own temporary directory.
//
// Pointing the state directory at a temp dir is what keeps a privileged test
// run from writing container records into the host's real /var/lib/forge — and
// from being confused by any that a previous run left there (PRD §10.4).
func NewRunner(t *testing.T) *runtime.Runner {
	t.Helper()

	dir := t.TempDir()
	runner, err := runtime.NewRunner(
		logging.New(io.Discard, slog.LevelError),
		runtime.Config{
			Root:     filepath.Join(dir, "containers"),
			StateDir: dir,
		},
	)
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	return runner
}

// StartLiveWithRunner is StartLive against a Runner the test already holds.
//
// Stage 6's verbs act on a container through the Runner that started it — the
// record is how `ps`, `exec` and `stop` find a container at all — so a test
// that wants to exec into a live container has to share one.
func StartLiveWithRunner(ctx context.Context, t *testing.T, runner *runtime.Runner, spec runtime.Spec) *Live {
	t.Helper()

	ctx, cancel := context.WithCancel(ctx)

	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		cancel()
		t.Fatalf("creating the container's stdin pipe: %v", err)
	}

	live := &Live{
		Stdout: &SyncBuffer{},
		Stderr: &SyncBuffer{},
		stdin:  stdinWriter,
		reader: stdinReader,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	spec.Stdin = stdinReader
	spec.Stdout = live.Stdout
	spec.Stderr = live.Stderr

	go func() {
		defer close(live.done)
		live.status, live.err = runner.Run(ctx, spec)
	}()

	t.Cleanup(func() {
		live.stop(t)
		cancel()
		// The child holds its own descriptor for the read end; this is the
		// parent's copy, closed once the container is gone so a long suite does
		// not accumulate one open pipe per test.
		if err := live.reader.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("closing the parent's copy of the container's stdin pipe: %v", err)
		}
		if out := live.Stdout.String(); out != "" {
			t.Logf("container stdout:\n%s", out)
		}
		if errOut := live.Stderr.String(); errOut != "" {
			t.Logf("container stderr:\n%s", errOut)
		}
	})

	return live
}

// Done is closed when the container has exited. It lets a test assert that a
// container is *still running* without waiting for it, which is the shape of
// every "this did not disturb the container" check.
func (l *Live) Done() <-chan struct{} { return l.done }

// Send writes a line to the container's stdin.
func (l *Live) Send(t *testing.T, line string) {
	t.Helper()

	if _, err := io.WriteString(l.stdin, line+"\n"); err != nil {
		t.Fatalf("sending %q to the container: %v", line, err)
	}
}

// CloseStdin tells the container to exit, by giving it EOF.
func (l *Live) CloseStdin(t *testing.T) {
	t.Helper()

	// Closing twice is not an error: a test may close stdin explicitly and the
	// registered cleanup will then close it again.
	if err := l.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("closing the container's stdin: %v", err)
	}
}

// WaitForOutput waits for the container to write a line containing marker.
func (l *Live) WaitForOutput(t *testing.T, marker string, timeout time.Duration) {
	t.Helper()

	PollUntil(t, "the container to report "+strconv.Quote(marker), timeout, func() bool {
		if strings.Contains(l.Stdout.String(), marker) {
			return true
		}
		// A container that has already exited is never going to say it.
		select {
		case <-l.done:
			t.Fatalf("container exited before reporting %q (status %v, error %v)\nstdout: %s\nstderr: %s",
				marker, l.status, l.err, l.Stdout.String(), l.Stderr.String())
		default:
		}
		return false
	})
}

// Wait waits for the container to exit and returns how it did. A container that
// failed to start is a test failure; one that exited non-zero is not.
func (l *Live) Wait(t *testing.T, timeout time.Duration) process.Status {
	t.Helper()

	select {
	case <-l.done:
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for the container to exit\nstdout: %s\nstderr: %s",
			timeout, l.Stdout.String(), l.Stderr.String())
	}

	if l.err != nil {
		t.Fatalf("Run() = %v\ncontainer stderr: %s", l.err, l.Stderr.String())
	}

	return l.status
}

// stop closes stdin and waits for the container, escalating to the context if
// it does not exit. It is idempotent, so a test that already stopped the
// container cleanly costs nothing at cleanup time.
func (l *Live) stop(t *testing.T) {
	t.Helper()

	// stop is idempotent, so a test that already ended the container cleanly
	// finds the write end closed; anything else failed for a real reason.
	if err := l.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("closing the container's stdin: %v", err)
	}

	select {
	case <-l.done:
		return
	case <-time.After(DefaultTimeout):
	}

	// The container ignored EOF. Cancelling is what Run turns into a SIGKILL.
	l.cancel()
	select {
	case <-l.done:
	case <-time.After(DefaultTimeout):
		t.Error("the container survived both EOF and a cancelled context")
	}
}
