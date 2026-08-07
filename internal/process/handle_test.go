package process_test

import (
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/process"
)

// Handles on processes this test binary started, but which — as far as the
// handle is concerned — it did not. That is the whole point of the type: what
// `forge stop` has is a PID out of a file, and no relationship to the process
// behind it.

// TestHandleSignalsARunningProcess is the ordinary case: a handle opened on a
// live process delivers a signal to it.
func TestHandleSignalsARunningProcess(t *testing.T) {
	p := startHelper(t, helperConfig(t, "sleep-forever"))

	h, err := process.Open(p.PID())
	if err != nil {
		t.Fatalf("Open(%d) = %v", p.PID(), err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	}()

	if h.PID() != p.PID() {
		t.Errorf("PID() = %d, want %d", h.PID(), p.PID())
	}
	if !h.Alive() {
		t.Error("Alive() = false, want true for a running process")
	}

	if err := h.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("Signal() = %v", err)
	}

	// Reaping is the parent's job, and this test happens to be the parent.
	// Doing it here is also what makes the assertion below deterministic:
	// until a process is reaped it lingers as a zombie.
	status, err := p.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait() = %v", err)
	}
	if status.Signal != syscall.SIGKILL {
		t.Errorf("status = %v, want killed by SIGKILL: the signal did not land", status)
	}

	if h.Alive() {
		t.Error("Alive() = true after the process was killed and reaped")
	}
}

// TestHandleSignalAfterExitIsNotAnError covers the race every cleanup path
// runs into: the process went away between deciding to signal it and doing so.
func TestHandleSignalAfterExitIsNotAnError(t *testing.T) {
	p := startHelper(t, helperConfig(t, "sleep-forever"))

	h, err := process.Open(p.PID())
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	if err := h.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("Signal() = %v", err)
	}
	if _, err := p.Wait(t.Context()); err != nil {
		t.Fatalf("Wait() = %v", err)
	}

	if err := h.Signal(syscall.SIGTERM); err != nil {
		t.Errorf("Signal() on an exited process = %v, want nil", err)
	}
}

// TestHandleRefusesAZombie pins the distinction that matters to `forge stop`:
// a process that has terminated but has not been reaped is not something to
// wait for. Its entry lingers for as long as its parent takes to collect it,
// which for a container whose supervisor died is until host init gets to it.
func TestHandleRefusesAZombie(t *testing.T) {
	p := startHelper(t, helperConfig(t, "exit", "FORGE_PROCESS_TEST_EXIT_CODE=0"))

	// Deliberately not reaped: that is what makes it a zombie, and the only
	// way Open can fail here.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := process.Open(p.PID())
		if errors.Is(err, process.ErrNoProcess) {
			break
		}
		if err != nil {
			t.Fatalf("Open() = %v, want nil or ErrNoProcess", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper never became a zombie; Open kept reporting it as live")
		}
	}

	if _, err := p.Wait(t.Context()); err != nil {
		t.Fatalf("Wait() = %v", err)
	}
}

// TestHandleOpenRejectsAnImpossiblePID covers the arguments that cannot name a
// process at all.
func TestHandleOpenRejectsAnImpossiblePID(t *testing.T) {
	for _, pid := range []int{0, -1, -4242} {
		if _, err := process.Open(pid); !errors.Is(err, process.ErrInvalidPID) {
			t.Errorf("Open(%d) = %v, want ErrInvalidPID", pid, err)
		}
	}
}

// TestHandleOpenReportsAMissingProcess covers a PID that named a process once
// and does not any more — which is what a container's record holds after the
// container has gone.
func TestHandleOpenReportsAMissingProcess(t *testing.T) {
	p := startHelper(t, helperConfig(t, "exit", "FORGE_PROCESS_TEST_EXIT_CODE=0"))

	pid := p.PID()
	if _, err := p.Wait(t.Context()); err != nil {
		t.Fatalf("Wait() = %v", err)
	}

	if _, err := process.Open(pid); !errors.Is(err, process.ErrNoProcess) {
		t.Errorf("Open(%d) after it exited = %v, want ErrNoProcess", pid, err)
	}
}

// TestHandleCloseIsIdempotent lets a caller defer Close and still close on an
// error path, which is how every cleanup in Forge is written.
func TestHandleCloseIsIdempotent(t *testing.T) {
	p := startHelper(t, helperConfig(t, "sleep-forever"))

	h, err := process.Open(p.PID())
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}

	for i := range 3 {
		if err := h.Close(); err != nil {
			t.Fatalf("Close() call %d = %v, want nil", i+1, err)
		}
	}

	if h.Alive() {
		t.Error("Alive() = true on a closed handle")
	}
	if err := h.Signal(syscall.SIGTERM); !errors.Is(err, process.ErrHandleClosed) {
		t.Errorf("Signal() on a closed handle = %v, want ErrHandleClosed", err)
	}
}
