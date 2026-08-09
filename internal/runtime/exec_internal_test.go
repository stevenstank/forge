package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stevenstank/forge/internal/state"
)

// The parts of `forge exec` that can be judged without a container.
//
// Joining namespaces needs root and a live container, so the mechanism itself
// is exercised by test/integration/stage6_test.go. What is here is everything
// that happens before any of that: the refusals. They matter on their own —
// "exec must fail if the container is stopped" is a requirement, and the check
// that the recorded process is still the container's is the one standing
// between a mistyped ID and running a user's command in an unrelated process's
// namespaces, as root.

func TestExecSpecValidate(t *testing.T) {
	tests := []struct {
		name string
		spec ExecSpec
		want error
	}{
		{
			name: "valid",
			spec: ExecSpec{ID: "7f3c9a1b2d04", Command: []string{"/bin/ls"}},
		},
		{
			name: "bare name is allowed",
			spec: ExecSpec{ID: "7f3c9a1b2d04", Command: []string{"ls"}},
		},
		{
			name: "no command",
			spec: ExecSpec{ID: "7f3c9a1b2d04"},
			want: ErrNoCommand,
		},
		{
			name: "empty command",
			spec: ExecSpec{ID: "7f3c9a1b2d04", Command: []string{""}},
			want: ErrNoCommand,
		},
		{
			name: "no container",
			spec: ExecSpec{Command: []string{"/bin/ls"}},
			want: ErrNotFound,
		},
		{
			name: "container id that escapes the state directory",
			spec: ExecSpec{ID: "../escape", Command: []string{"/bin/ls"}},
			want: ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestExecRefusesAContainerThatIsNotRunning is the requirement stated directly:
// exec must fail if the container has stopped. It covers every status a
// container can be in when the answer is no.
func TestExecRefusesAContainerThatIsNotRunning(t *testing.T) {
	tests := []struct {
		name   string
		status state.Status
		pid    int
	}{
		{name: "exited", status: state.StatusExited, pid: 4242},
		{name: "stopped", status: state.StatusStopped, pid: 4242},
		{name: "removing", status: state.StatusRemoving, pid: 4242},
		{name: "still starting", status: state.StatusCreating, pid: 0},
		{name: "running but with no process recorded", status: state.StatusRunning, pid: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := testRunner(t, nil)

			m := runningRecord("7f3c9a1b2d04")
			m.Status = tc.status
			m.PID = tc.pid
			if tc.status.Terminal() {
				finished := time.Now().UTC()
				m.FinishedAt = &finished
			}
			seed(t, r, m)

			_, err := r.Exec(t.Context(), ExecSpec{ID: m.ID, Command: []string{"/bin/true"}})
			if !errors.Is(err, ErrNotRunning) {
				t.Fatalf("Exec() = %v, want ErrNotRunning", err)
			}
		})
	}
}

// TestExecRefusesAContainerWhoseProcessIsGone covers the record that outlived
// its container: a supervising `forge run` that was killed leaves one reading
// "running" forever.
//
// Trusting it would mean joining the namespaces of whatever process now holds
// that PID. The recorded PID here is one nothing can be using — PID 0 is the
// kernel's own placeholder — so the check has something unambiguous to fail on.
func TestExecRefusesAContainerWhoseProcessIsGone(t *testing.T) {
	r := testRunner(t, nil)

	// A PID that cannot name a live process: above the maximum any host
	// allows, so this is a stale record rather than a race with a real one.
	m := runningRecord("7f3c9a1b2d04")
	m.PID = 1 << 30
	seed(t, r, m)

	_, err := r.Exec(t.Context(), ExecSpec{ID: m.ID, Command: []string{"/bin/true"}})
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Exec() = %v, want ErrNotRunning", err)
	}
}

func TestExecRefusesAnUnknownContainer(t *testing.T) {
	r := testRunner(t, nil)

	_, err := r.Exec(t.Context(), ExecSpec{ID: "0000deadbeef", Command: []string{"/bin/true"}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Exec() = %v, want ErrNotFound", err)
	}
}

// TestExecDoesNotDisturbTheContainer pins the other half of the requirement:
// a refused exec must leave the container exactly as it was. A `forge exec`
// against a stopped container is a common typo, and it must not be the thing
// that changes the container's record.
func TestExecDoesNotDisturbTheContainer(t *testing.T) {
	r := testRunner(t, nil)

	code := 0
	finished := time.Now().UTC()
	m := runningRecord("7f3c9a1b2d04")
	m.Status = state.StatusExited
	m.ExitCode = &code
	m.FinishedAt = &finished
	seed(t, r, m)

	if _, err := r.Exec(t.Context(), ExecSpec{ID: m.ID, Command: []string{"/bin/true"}}); err == nil {
		t.Fatal("Exec() = nil, want an error")
	}

	got := load(t, r, m.ID)
	if got.Status != state.StatusExited {
		t.Errorf("status = %q, want it left at %q", got.Status, state.StatusExited)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want the recorded 0", got.ExitCode)
	}
	if got.PID != m.PID {
		t.Errorf("PID = %d, want %d", got.PID, m.PID)
	}
}

// TestResolveCommand covers the resolution `forge exec` performs after joining
// the container's mount namespace, where the directories being searched are
// the container's.
//
// It runs here against the host's filesystem, which is the same code path: the
// function has no idea which mount namespace it is in, and that is exactly why
// it is safe to call from inside one.
func TestResolveCommand(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "runnable")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		command string
		env     []string
		want    string
		wantErr error
	}{
		{name: "a path is used as given", command: "/bin/ls", want: "/bin/ls"},
		{name: "a relative path is used as given", command: "./x", want: "./x"},
		{
			name:    "a bare name is searched for",
			command: "runnable",
			env:     []string{"PATH=" + dir},
			want:    binary,
		},
		{
			name:    "a bare name with no PATH",
			command: "runnable",
			wantErr: ErrCommandNotFound,
		},
		{
			name:    "a bare name that is nowhere on PATH",
			command: "not-there",
			env:     []string{"PATH=" + dir},
			wantErr: ErrCommandNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveCommand(tc.command, tc.env)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("resolveCommand() = %q, %v, want %v", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveCommand() = %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveCommand() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The thread the namespaces are joined on, which is the one part of exec that
// can damage the process running it.
//
// setns(2) moves a thread and nothing else, so `forge exec` is built around a
// thread it is willing to lose: locked to one goroutine, never unlocked,
// destroyed by the Go runtime when that goroutine returns. The initial thread
// is the exception — the runtime parks m0 rather than destroying it, and
// /proc/self reports the thread group leader's namespaces — so joining on it
// moves the whole process into the container, permanently and visibly.
//
// Neither test below performs a setns: what is being checked is which thread
// the work is handed to, and that is decided before any namespace is touched.

// TestOnDisposableThreadNeverUsesTheMainThread is the invariant, checked
// against the real scheduler.
func TestOnDisposableThreadNeverUsesTheMainThread(t *testing.T) {
	main := os.Getpid() // the initial thread's TID, by definition on Linux

	// Repeated, because the scheduler's choice of thread is what is being
	// guarded against and a single run proves very little about it.
	for i := range 200 {
		var tid int
		onDisposableThread(func() execOutcome {
			tid = unix.Gettid()
			return execOutcome{}
		})

		if tid == main {
			t.Fatalf("run %d joined namespaces on the main thread (tid %d): "+
				"the runtime cannot destroy it, so the process would be left in the container", i, tid)
		}
	}
}

// TestOnDisposableThreadStepsOffTheMainThread forces the case the test above
// can only wait for.
//
// The first thread to ask is told it is the main one, which is the scheduler
// making the unlucky choice. The work must land somewhere else, and the
// somewhere else must be a thread of its own rather than the same one asked
// twice.
func TestOnDisposableThreadStepsOffTheMainThread(t *testing.T) {
	var (
		mu       sync.Mutex
		pretend  int
		asked    []int
		observed int
	)

	original := isMainThread
	t.Cleanup(func() { isMainThread = original })

	isMainThread = func() bool {
		mu.Lock()
		defer mu.Unlock()

		tid := unix.Gettid()
		asked = append(asked, tid)
		if pretend == 0 {
			pretend = tid
		}
		return tid == pretend
	}

	res := onDisposableThread(func() execOutcome {
		observed = unix.Gettid()
		return execOutcome{}
	})
	if res.err != nil {
		t.Fatalf("onDisposableThread() = %v, want the work to have run", res.err)
	}

	mu.Lock()
	defer mu.Unlock()

	if observed == 0 {
		t.Fatal("the work never ran")
	}
	if observed == pretend {
		t.Errorf("the work ran on the main thread (tid %d)", observed)
	}
	if len(asked) != 2 {
		t.Errorf("the main-thread check ran %d times, want 2: once on the thread that steps "+
			"aside and once on the one that does the work", len(asked))
	}
}

// TestOnDisposableThreadReturnsTheOutcome checks the plumbing the two tests
// above depend on but do not assert: whatever the work produced comes back to
// the caller, from either thread.
func TestOnDisposableThreadReturnsTheOutcome(t *testing.T) {
	want := errors.New("the command could not be started")

	if got := onDisposableThread(func() execOutcome { return execOutcome{err: want} }); got.err != want {
		t.Errorf("err = %v, want %v", got.err, want)
	}

	original := isMainThread
	t.Cleanup(func() { isMainThread = original })

	first := true
	isMainThread = func() bool {
		if first {
			first = false
			return true
		}
		return false
	}

	if got := onDisposableThread(func() execOutcome { return execOutcome{err: want} }); got.err != want {
		t.Errorf("err through the main-thread path = %v, want %v", got.err, want)
	}
}
