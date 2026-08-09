package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/state"
)

// The Stage 6 verbs, tested without root, without a container, and without
// waiting on anything real.
//
// `forge stop` is a conversation with a process — signal it, watch it, kill it
// if it will not go — and every interesting case in that conversation is about
// timing: a container that exits promptly, one that ignores SIGTERM, one that
// died before anyone looked. Driving those with real processes would mean a
// suite that is slow, that needs privileges to create anything worth stopping,
// and that fails intermittently on a loaded machine. So the process is the one
// thing faked here, through the seam Runner.openProcess exists to provide.
// Everything else — the records, the directories, the cgroup tree — is real.

// fakeProcess is a container's init process that does what the test tells it.
type fakeProcess struct {
	mu sync.Mutex

	// alive is whether the process is still running.
	alive bool

	// dieOn is the signal that ends it. The zero value means no signal does,
	// which is the container that has to be reported as unkillable.
	dieOn syscall.Signal

	// signals is every signal received, in order.
	signals []syscall.Signal

	// closed records that the handle was released, which matters because the
	// production handle holds a file descriptor.
	closed bool

	// signalErr, when set, is returned by Signal instead of delivering it.
	signalErr error
}

func (f *fakeProcess) Signal(sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.signals = append(f.signals, sig)
	if f.signalErr != nil {
		return f.signalErr
	}
	if f.dieOn != 0 && sig == f.dieOn {
		f.alive = false
	}

	return nil
}

func (f *fakeProcess) Alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.alive
}

func (f *fakeProcess) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed = true
	return nil
}

// sent returns the signals the process received.
func (f *fakeProcess) sent() []syscall.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]syscall.Signal(nil), f.signals...)
}

func (f *fakeProcess) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closed
}

// testRunner returns a Runner over temporary directories, with the process
// seam pointed at proc.
//
// A nil proc means no process can be opened, which is how a container whose
// init has already gone is expressed.
func testRunner(t *testing.T, proc *fakeProcess) *Runner {
	t.Helper()

	r, err := NewRunner(
		logging.New(io.Discard, slog.LevelError),
		Config{
			Root:       filepath.Join(t.TempDir(), "containers"),
			StateDir:   t.TempDir(),
			CgroupRoot: t.TempDir(),
		},
	)
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	var opens int
	r.openProcess = func(int) (containerProcess, error) {
		opens++
		if proc == nil {
			return nil, process.ErrNoProcess
		}
		return proc, nil
	}

	// Short enough that the timeout paths resolve in milliseconds, which is
	// what keeps this suite free of sleeps and of arbitrary deadlines.
	r.pollInterval = time.Millisecond
	r.killGrace = 5 * time.Millisecond

	return r
}

// seed writes a record directly, standing in for the `forge run` that would
// have written it.
func seed(t *testing.T, r *Runner, m state.Metadata) state.Metadata {
	t.Helper()

	if m.ID == "" {
		m.ID = "7f3c9a1b2d04"
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC().Add(-time.Minute)
	}
	if err := r.state.Save(m); err != nil {
		t.Fatalf("seeding the record: %v", err)
	}

	return m
}

// runningRecord is a container that is up: an image, a command, a PID.
func runningRecord(id string) state.Metadata {
	started := time.Now().UTC().Add(-30 * time.Second)

	return state.Metadata{
		ID:          id,
		Image:       "alpine:3.20",
		Command:     []string{"/bin/sh"},
		PID:         4242,
		Status:      state.StatusRunning,
		CreatedAt:   time.Now().UTC().Add(-time.Minute),
		StartedAt:   &started,
		NetworkMode: "bridge",
	}
}

// load reads a record back, failing the test if it cannot.
func load(t *testing.T, r *Runner, id string) state.Metadata {
	t.Helper()

	m, err := r.state.Load(id)
	if err != nil {
		t.Fatalf("Load(%s) = %v", id, err)
	}

	return m
}

// prepareRootfs creates the container's directory tree, as a run would.
func prepareRootfs(t *testing.T, r *Runner, id string) string {
	t.Helper()

	dir, err := r.store.Prepare(id)
	if err != nil {
		t.Fatalf("Prepare(%s) = %v", id, err)
	}

	return dir.Base
}

// prepareLogs creates the container's log file, as a run would.
func prepareLogs(t *testing.T, r *Runner, id string) string {
	t.Helper()

	path, err := r.logs.Path(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	entry := `{"t":"2026-08-07T18:22:03.114233Z","s":"stdout","m":"hello\n"}` + "\n"
	if err := os.WriteFile(path, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// prepareCgroupLeaf creates the container's cgroup directory, as a run would.
// Destroying it is an rmdir, which needs no privileges against a temp tree.
func prepareCgroupLeaf(t *testing.T, r *Runner, id string) string {
	t.Helper()

	dir, err := r.cgroups.Path(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	return dir
}

func assertGone(t *testing.T, path, what string) {
	t.Helper()

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists at %q: %v", what, path, err)
	}
}

func assertPresent(t *testing.T, path, what string) {
	t.Helper()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s should still exist at %q: %v", what, path, err)
	}
}

func assertSignals(t *testing.T, got []syscall.Signal, want ...syscall.Signal) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("signals = %v, want %v", got, want)
		}
	}
}

// TestStopRunningContainer is the ordinary case: a container that handles
// SIGTERM and goes.
func TestStopRunningContainer(t *testing.T) {
	proc := &fakeProcess{alive: true, dieOn: syscall.SIGTERM}
	r := testRunner(t, proc)
	m := seed(t, r, runningRecord("7f3c9a1b2d04"))

	cgroupDir := prepareCgroupLeaf(t, r, m.ID)
	rootfsDir := prepareRootfs(t, r, m.ID)

	if err := r.Stop(t.Context(), m.ID, StopOptions{}); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}

	// SIGTERM was enough; the container was never killed.
	assertSignals(t, proc.sent(), syscall.SIGTERM)
	if !proc.wasClosed() {
		t.Error("the process handle was not released")
	}

	got := load(t, r, m.ID)
	if got.Status != state.StatusStopped {
		t.Errorf("status = %q, want %q", got.Status, state.StatusStopped)
	}
	if got.FinishedAt == nil {
		t.Error("FinishedAt was not recorded")
	}

	// The runtime resources go with the container...
	assertGone(t, cgroupDir, "the container's cgroup")

	// ...and the retained ones stay, for ps -a and rm.
	assertPresent(t, rootfsDir, "the container's filesystem")
}

// TestStopAlreadyStoppedContainer covers idempotence: stopping a container
// that has finished is success, and must not disturb what the run recorded
// about how it finished.
func TestStopAlreadyStoppedContainer(t *testing.T) {
	proc := &fakeProcess{alive: true, dieOn: syscall.SIGTERM}
	r := testRunner(t, proc)

	code := 0
	finished := time.Now().UTC().Add(-10 * time.Second)
	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusExited
	m.ExitCode = &code
	m.FinishedAt = &finished
	seed(t, r, m)

	for i := range 3 {
		if err := r.Stop(t.Context(), m.ID, StopOptions{}); err != nil {
			t.Fatalf("Stop() call %d = %v, want nil", i+1, err)
		}
	}

	// Nothing was signalled: there is nothing there to signal, and a PID from
	// a finished container may belong to something else entirely by now.
	if sent := proc.sent(); len(sent) != 0 {
		t.Errorf("signals = %v, want none", sent)
	}

	got := load(t, r, m.ID)
	if got.Status != state.StatusExited {
		t.Errorf("status = %q, want it left at %q", got.Status, state.StatusExited)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want the recorded 0", got.ExitCode)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Errorf("FinishedAt = %v, want the recorded %v", got.FinishedAt, finished)
	}
}

// TestStopTimeoutPath is the case that is the rule rather than the exception:
// a container whose init installed no SIGTERM handler. The kernel discards the
// signal, the grace period expires, and SIGKILL ends it.
func TestStopTimeoutPath(t *testing.T) {
	proc := &fakeProcess{alive: true, dieOn: syscall.SIGKILL}
	r := testRunner(t, proc)
	m := seed(t, r, runningRecord("7f3c9a1b2d04"))

	start := time.Now()
	err := r.Stop(t.Context(), m.ID, StopOptions{Timeout: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}

	// The grace period was actually waited out, in the order that matters.
	assertSignals(t, proc.sent(), syscall.SIGTERM, syscall.SIGKILL)
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Errorf("stop took %s, want at least the 5ms timeout", elapsed)
	}

	if got := load(t, r, m.ID); got.Status != state.StatusStopped {
		t.Errorf("status = %q, want %q", got.Status, state.StatusStopped)
	}
}

// TestStopReportsAContainerThatWillNotDie covers the outcome that is not the
// container being stubborn but the kernel being stuck: a process that survives
// SIGKILL is in uninterruptible sleep, and there is nothing Forge can do but
// say so.
func TestStopReportsAContainerThatWillNotDie(t *testing.T) {
	proc := &fakeProcess{alive: true}
	r := testRunner(t, proc)
	m := seed(t, r, runningRecord("7f3c9a1b2d04"))

	err := r.Stop(t.Context(), m.ID, StopOptions{Timeout: time.Millisecond})
	if !errors.Is(err, ErrStopFailed) {
		t.Fatalf("Stop() = %v, want ErrStopFailed", err)
	}

	assertSignals(t, proc.sent(), syscall.SIGTERM, syscall.SIGKILL)

	// The record says stopping, which is the truth: a stop was asked for and
	// has not completed. Claiming it stopped would be a lie a later rm would
	// act on.
	if got := load(t, r, m.ID); got.Status != state.StatusStopping {
		t.Errorf("status = %q, want %q", got.Status, state.StatusStopping)
	}
}

// TestStopContainerWhoseProcessIsGone is the crashed-supervisor case: the
// container is already dead and nobody recorded it, because the process that
// would have was killed too.
func TestStopContainerWhoseProcessIsGone(t *testing.T) {
	r := testRunner(t, nil) // no process to open
	m := seed(t, r, runningRecord("7f3c9a1b2d04"))
	cgroupDir := prepareCgroupLeaf(t, r, m.ID)

	if err := r.Stop(t.Context(), m.ID, StopOptions{}); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}

	got := load(t, r, m.ID)
	if got.Status != state.StatusStopped {
		t.Errorf("status = %q, want %q", got.Status, state.StatusStopped)
	}
	// No exit code is invented. Nobody saw one, and 0 or 137 would both be
	// fabrications a user could not distinguish from an observation.
	if got.ExitCode != nil {
		t.Errorf("ExitCode = %d, want none: no process observed this container exit", *got.ExitCode)
	}
	if got.FinishedAt == nil {
		t.Error("FinishedAt was not recorded")
	}

	// The resources it was still holding are released, which is the whole
	// reason stop does not simply fail here.
	assertGone(t, cgroupDir, "the container's cgroup")
}

// TestStopContainerThatNeverStarted covers a record with no PID: a run that
// died between creating the record and cloning.
func TestStopContainerThatNeverStarted(t *testing.T) {
	proc := &fakeProcess{alive: true, dieOn: syscall.SIGTERM}
	r := testRunner(t, proc)

	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusCreating
	m.PID = 0
	m.StartedAt = nil
	seed(t, r, m)

	if err := r.Stop(t.Context(), m.ID, StopOptions{}); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	if sent := proc.sent(); len(sent) != 0 {
		t.Errorf("signals = %v, want none: there was no process", sent)
	}
	if got := load(t, r, m.ID); got.Status != state.StatusStopped {
		t.Errorf("status = %q, want %q", got.Status, state.StatusStopped)
	}
}

func TestStopUnknownContainer(t *testing.T) {
	r := testRunner(t, nil)

	err := r.Stop(t.Context(), "0000deadbeef", StopOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stop() = %v, want ErrNotFound", err)
	}
}

// TestStopMarksStoppingBeforeSignalling pins the transition a concurrent
// `forge ps` depends on, and that the supervising run reads to decide whether
// its container exited or was stopped.
func TestStopMarksStoppingBeforeSignalling(t *testing.T) {
	var (
		r      *Runner
		id     = "7f3c9a1b2d04"
		atTerm state.Status
	)

	proc := &fakeProcess{alive: true, dieOn: syscall.SIGTERM}
	r = testRunner(t, proc)
	seed(t, r, runningRecord(id))

	// Observe the record from inside the signal, which is the only moment the
	// intermediate state exists.
	r.openProcess = func(int) (containerProcess, error) {
		return &observingProcess{fakeProcess: proc, onSignal: func() {
			atTerm = load(t, r, id).Status
		}}, nil
	}

	if err := r.Stop(t.Context(), id, StopOptions{}); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}

	if atTerm != state.StatusStopping {
		t.Errorf("status at the moment of SIGTERM = %q, want %q", atTerm, state.StatusStopping)
	}
	if got := load(t, r, id); got.Status != state.StatusStopped {
		t.Errorf("final status = %q, want %q", got.Status, state.StatusStopped)
	}
}

// observingProcess runs a callback when it is signalled, so a test can look at
// the world at that instant.
type observingProcess struct {
	*fakeProcess
	onSignal func()
}

func (o *observingProcess) Signal(sig syscall.Signal) error {
	o.onSignal()
	return o.fakeProcess.Signal(sig)
}

// TestStopWithRemove covers `forge stop --rm`.
func TestStopWithRemove(t *testing.T) {
	proc := &fakeProcess{alive: true, dieOn: syscall.SIGTERM}
	r := testRunner(t, proc)
	m := seed(t, r, runningRecord("7f3c9a1b2d04"))

	rootfsDir := prepareRootfs(t, r, m.ID)
	logsDir := prepareLogs(t, r, m.ID)
	cgroupDir := prepareCgroupLeaf(t, r, m.ID)

	if err := r.Stop(t.Context(), m.ID, StopOptions{Remove: true}); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}

	assertGone(t, rootfsDir, "the container's filesystem")
	assertGone(t, logsDir, "the container's logs")
	assertGone(t, cgroupDir, "the container's cgroup")
	if _, err := r.state.Load(m.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Load after stop --rm = %v, want ErrNotFound", err)
	}
}

// TestStopWithRemoveToleratesAnAlreadyRemovedRecord is the race `forge stop
// --rm` runs against every attached container that was not started with
// --keep: the supervising run deletes the record as it unwinds, and the exit
// that triggers that unwind is the same exit this stop is waiting for.
//
// The removal is staged from inside the signal, which is the deterministic
// stand-in for the supervisor winning by a few microseconds. The stop asked
// for a container to be gone and the container is gone, so it succeeds.
func TestStopWithRemoveToleratesAnAlreadyRemovedRecord(t *testing.T) {
	var (
		r  *Runner
		id = "7f3c9a1b2d04"
	)

	proc := &fakeProcess{alive: true, dieOn: syscall.SIGTERM}
	r = testRunner(t, proc)
	seed(t, r, runningRecord(id))

	r.openProcess = func(int) (containerProcess, error) {
		return &observingProcess{fakeProcess: proc, onSignal: func() {
			// The supervisor reaping its container and taking the record with
			// it, at the one moment that makes the stop observe it.
			if err := r.state.Remove(id); err != nil {
				t.Errorf("removing the record underneath the stop: %v", err)
			}
		}}, nil
	}

	if err := r.Stop(t.Context(), id, StopOptions{Remove: true}); err != nil {
		t.Fatalf("Stop(--rm) = %v, want nil: the container is gone, which is what was asked", err)
	}

	if _, err := r.state.Load(id); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Load after stop --rm = %v, want ErrNotFound", err)
	}
}

// TestRemoveUnknownContainerIsStillAnError guards the other half of the rule
// above: only the --rm path forgives a missing record.
func TestRemoveUnknownContainerIsStillAnError(t *testing.T) {
	r := testRunner(t, nil)

	if err := r.Remove(t.Context(), "7f3c9a1b2d04", RemoveOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Remove() of an unknown container = %v, want ErrNotFound", err)
	}
}

// TestRemoveRunningContainer is the refusal that gives `rm` its meaning:
// deleting the filesystem of a container that is executing inside it does not
// stop it, it corrupts it.
func TestRemoveRunningContainer(t *testing.T) {
	tests := []struct {
		name   string
		status state.Status
	}{
		{name: "running", status: state.StatusRunning},
		{name: "created", status: state.StatusCreated},
		{name: "stopping", status: state.StatusStopping},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proc := &fakeProcess{alive: true, dieOn: syscall.SIGTERM}
			r := testRunner(t, proc)

			m := runningRecord("7f3c9a1b2d04")
			m.Status = tc.status
			seed(t, r, m)

			rootfsDir := prepareRootfs(t, r, m.ID)
			logsDir := prepareLogs(t, r, m.ID)

			err := r.Remove(t.Context(), m.ID, RemoveOptions{})
			if !errors.Is(err, ErrRunning) {
				t.Fatalf("Remove() = %v, want ErrRunning", err)
			}

			// Nothing was touched, including the record: a refused removal
			// must not leave the container half-described.
			assertPresent(t, rootfsDir, "the container's filesystem")
			assertPresent(t, logsDir, "the container's logs")
			if got := load(t, r, m.ID); got.Status != tc.status {
				t.Errorf("status = %q, want it left at %q", got.Status, tc.status)
			}
			if sent := proc.sent(); len(sent) != 0 {
				t.Errorf("signals = %v, want none: rm without -f stops nothing", sent)
			}
		})
	}
}

// TestRemoveStoppedContainer is the whole of FR-6.6: state, logs, filesystem
// and metadata all go.
func TestRemoveStoppedContainer(t *testing.T) {
	r := testRunner(t, nil)

	code := 137
	finished := time.Now().UTC()
	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusStopped
	m.ExitCode = &code
	m.FinishedAt = &finished
	seed(t, r, m)

	rootfsDir := prepareRootfs(t, r, m.ID)
	logsDir := prepareLogs(t, r, m.ID)
	cgroupDir := prepareCgroupLeaf(t, r, m.ID)
	stateDir, err := r.state.Dir(m.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Remove(t.Context(), m.ID, RemoveOptions{}); err != nil {
		t.Fatalf("Remove() = %v, want nil", err)
	}

	assertGone(t, rootfsDir, "the container's filesystem")
	assertGone(t, logsDir, "the container's logs")
	assertGone(t, cgroupDir, "the container's cgroup")
	assertGone(t, stateDir, "the container's metadata")

	if _, err := r.state.Load(m.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Load after Remove = %v, want ErrNotFound", err)
	}

	// And it is gone from ps, including ps -a.
	if containers, _ := r.List(true); len(containers) != 0 {
		t.Errorf("List(all) = %v, want empty", containers)
	}
}

// TestRemoveForceStopsFirst covers `forge rm -f`.
func TestRemoveForceStopsFirst(t *testing.T) {
	proc := &fakeProcess{alive: true, dieOn: syscall.SIGTERM}
	r := testRunner(t, proc)
	m := seed(t, r, runningRecord("7f3c9a1b2d04"))

	rootfsDir := prepareRootfs(t, r, m.ID)

	if err := r.Remove(t.Context(), m.ID, RemoveOptions{Force: true}); err != nil {
		t.Fatalf("Remove() = %v, want nil", err)
	}

	assertSignals(t, proc.sent(), syscall.SIGTERM)
	assertGone(t, rootfsDir, "the container's filesystem")
	if _, err := r.state.Load(m.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Load after Remove = %v, want ErrNotFound", err)
	}
}

// TestRemoveFinishesAnInterruptedRemoval is the crash-safety case the
// `removing` status exists for: a removal that died half-way leaves a record
// saying so, and the next rm completes it rather than refusing.
func TestRemoveFinishesAnInterruptedRemoval(t *testing.T) {
	r := testRunner(t, nil)

	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusRemoving
	seed(t, r, m)

	rootfsDir := prepareRootfs(t, r, m.ID)
	logsDir := prepareLogs(t, r, m.ID)

	if err := r.Remove(t.Context(), m.ID, RemoveOptions{}); err != nil {
		t.Fatalf("Remove() = %v, want nil", err)
	}

	assertGone(t, rootfsDir, "the container's filesystem")
	assertGone(t, logsDir, "the container's logs")
	if _, err := r.state.Load(m.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Load after Remove = %v, want ErrNotFound", err)
	}
}

// TestRemoveIsIdempotentForItsResources covers a container whose resources are
// already gone — removed by a previous run's cleanup, or by the kernel. The
// removal must still complete rather than tripping on the absence.
func TestRemoveIsIdempotentForItsResources(t *testing.T) {
	r := testRunner(t, nil)

	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusExited
	seed(t, r, m)

	// No rootfs, no logs, no cgroup: only the record.
	if err := r.Remove(t.Context(), m.ID, RemoveOptions{}); err != nil {
		t.Fatalf("Remove() = %v, want nil", err)
	}
	if _, err := r.state.Load(m.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Load after Remove = %v, want ErrNotFound", err)
	}

	// A second removal has nothing to work with and says so, rather than
	// pretending it removed something.
	if err := r.Remove(t.Context(), m.ID, RemoveOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Remove() = %v, want ErrNotFound", err)
	}
}

func TestRemoveUnknownContainer(t *testing.T) {
	r := testRunner(t, nil)

	if err := r.Remove(t.Context(), "0000deadbeef", RemoveOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove() = %v, want ErrNotFound", err)
	}
}

// TestList covers what `forge ps` shows and what it hides.
func TestList(t *testing.T) {
	r := testRunner(t, nil)

	live := runningRecord("aaaaaaaaaaaa")
	live.CreatedAt = time.Now().UTC().Add(-2 * time.Minute)
	seed(t, r, live)

	code := 0
	finished := time.Now().UTC()
	dead := runningRecord("bbbbbbbbbbbb")
	dead.Status = state.StatusExited
	dead.ExitCode = &code
	dead.FinishedAt = &finished
	dead.CreatedAt = time.Now().UTC().Add(-time.Minute)
	seed(t, r, dead)

	t.Run("default hides finished containers", func(t *testing.T) {
		got, errs := r.List(false)
		if len(errs) != 0 {
			t.Fatalf("List errors = %v, want none", errs)
		}
		if len(got) != 1 || got[0].ID != live.ID {
			t.Fatalf("List(false) = %v, want just the running container", got)
		}
		if !got[0].Running() {
			t.Error("the running container does not report as running")
		}
	})

	t.Run("all includes them", func(t *testing.T) {
		got, errs := r.List(true)
		if len(errs) != 0 {
			t.Fatalf("List errors = %v, want none", errs)
		}
		if len(got) != 2 {
			t.Fatalf("List(true) = %v, want both containers", got)
		}
		// Oldest first, which is the order the store returns and the order ps
		// relies on.
		if got[0].ID != live.ID || got[1].ID != dead.ID {
			t.Errorf("List(true) order = %s, %s, want %s, %s", got[0].ID, got[1].ID, live.ID, dead.ID)
		}
	})

	t.Run("reports every field ps prints", func(t *testing.T) {
		got, _ := r.List(true)
		c := got[0]

		if c.Image != "alpine:3.20" {
			t.Errorf("Image = %q, want alpine:3.20", c.Image)
		}
		if len(c.Command) != 1 || c.Command[0] != "/bin/sh" {
			t.Errorf("Command = %v, want [/bin/sh]", c.Command)
		}
		if c.PID != 4242 {
			t.Errorf("PID = %d, want 4242", c.PID)
		}
		if c.Status != string(state.StatusRunning) {
			t.Errorf("Status = %q, want running", c.Status)
		}
		if c.Created.IsZero() {
			t.Error("Created is zero")
		}
		if c.Network != "bridge" {
			t.Errorf("Network = %q, want bridge", c.Network)
		}
	})
}

// TestListSurvivesAnUnreadableRecord is the reason List returns errors
// alongside containers: the command a user runs to find out what is wrong must
// not be the command that a corrupt file breaks.
func TestListSurvivesAnUnreadableRecord(t *testing.T) {
	r := testRunner(t, nil)
	seed(t, r, runningRecord("aaaaaaaaaaaa"))

	dir := filepath.Join(r.state.Root(), "state", "containers", "bbbbbbbbbbbb")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"schema":1,`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, errs := r.List(true)
	if len(got) != 1 || got[0].ID != "aaaaaaaaaaaa" {
		t.Errorf("List = %v, want the readable container", got)
	}
	if len(errs) != 1 {
		t.Fatalf("List errors = %v, want exactly one", errs)
	}
}

func TestInspect(t *testing.T) {
	r := testRunner(t, nil)
	m := seed(t, r, runningRecord("7f3c9a1b2d04"))

	got, err := r.Inspect(m.ID)
	if err != nil {
		t.Fatalf("Inspect() = %v, want nil", err)
	}
	if got.ID != m.ID || got.PID != m.PID {
		t.Errorf("Inspect() = %+v, want the seeded record", got)
	}

	if _, err := r.Inspect("0000deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Inspect(unknown) = %v, want ErrNotFound", err)
	}
}

// TestTerminalFor pins the rule that decides whether a container that has died
// is reported as having exited or as having been stopped. The difference is
// invisible to the process watching it and is carried entirely by the record.
func TestTerminalFor(t *testing.T) {
	tests := []struct {
		from state.Status
		want state.Status
	}{
		{from: state.StatusRunning, want: state.StatusExited},
		{from: state.StatusCreated, want: state.StatusExited},
		{from: state.StatusCreating, want: state.StatusExited},
		{from: state.StatusStopping, want: state.StatusStopped},
		{from: state.StatusStopped, want: state.StatusStopped},
	}

	for _, tc := range tests {
		t.Run(string(tc.from), func(t *testing.T) {
			if got := terminalFor(tc.from); got != tc.want {
				t.Errorf("terminalFor(%q) = %q, want %q", tc.from, got, tc.want)
			}
		})
	}
}

// TestRecordExitReportsAStopAsAStop is the same rule through the function that
// applies it: a run whose container was killed by `forge stop` records
// "stopped", and one that exited on its own records "exited".
func TestRecordExitReportsAStopAsAStop(t *testing.T) {
	tests := []struct {
		name   string
		from   state.Status
		want   state.Status
		status process.Status
	}{
		{
			name:   "exited on its own",
			from:   state.StatusRunning,
			want:   state.StatusExited,
			status: process.Status{Code: 0},
		},
		{
			name:   "killed by forge stop",
			from:   state.StatusStopping,
			want:   state.StatusStopped,
			status: process.Status{Code: 137, Signal: syscall.SIGKILL},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := testRunner(t, nil)
			m := runningRecord("7f3c9a1b2d04")
			m.Status = tc.from
			seed(t, r, m)

			r.recordExit(logging.New(io.Discard, slog.LevelError), m.ID, tc.status)

			got := load(t, r, m.ID)
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q", got.Status, tc.want)
			}
			if got.ExitCode == nil || *got.ExitCode != tc.status.Code {
				t.Errorf("ExitCode = %v, want %d", got.ExitCode, tc.status.Code)
			}
			if got.FinishedAt == nil {
				t.Error("FinishedAt was not recorded")
			}
		})
	}
}

// TestStopHonoursContextCancellation keeps a stop from ignoring the Ctrl-C of
// the user who asked for it.
func TestStopHonoursContextCancellation(t *testing.T) {
	proc := &fakeProcess{alive: true} // never dies
	r := testRunner(t, proc)
	m := seed(t, r, runningRecord("7f3c9a1b2d04"))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := r.Stop(ctx, m.ID, StopOptions{Timeout: time.Hour})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop() = %v, want context.Canceled", err)
	}
}
