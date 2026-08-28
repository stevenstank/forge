package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/namespace"
	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/state"
)

// The plumbing between a `forge run` and the container's init: the payload
// pipe, the working directory, the descriptors that must not survive into the
// container, and the record writes that make a container findable by anything
// other than the process that started it.
//
// None of it needs a container. The pipe is a pipe, the record store is a
// directory, and the one thing that genuinely needs root — clone(2) with new
// namespaces — is represented here only by the translation of the error it
// returns when it is refused.

// TestWritePayloadSendsAndClosesThePipe covers the half of the handshake that
// runs in the parent. Closing is not a formality: the child reads to EOF, so a
// pipe left open would hang the container's init forever.
func TestWritePayloadSendsAndClosesThePipe(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	payload := []byte(`{"command":["/bin/echo"]}`)

	done := make(chan []byte, 1)
	go func() {
		got, _ := io.ReadAll(r)
		done <- got
	}()

	if err := writePayload(w, payload); err != nil {
		t.Fatalf("writePayload() = %v", err)
	}

	if got := <-done; !bytes.Equal(got, payload) {
		t.Errorf("child read %q, want %q", got, payload)
	}

	// The write end is closed, so a second write must fail rather than
	// succeeding against a descriptor the parent no longer owns.
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("the payload pipe was left open after writePayload")
	}
}

// TestWritePayloadReportsAClosedReader checks the error path: a child that died
// before the payload was sent must produce a diagnosable failure rather than a
// SIGPIPE-killed forge.
func TestWritePayloadReportsAClosedReader(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	err = writePayload(w, []byte(`{"command":["/bin/echo"]}`))
	if err == nil {
		t.Fatal("writePayload() = nil, want a failure when the reader has gone")
	}
	if !strings.Contains(err.Error(), "init payload") {
		t.Errorf("writePayload() = %q, want it to name the payload", err)
	}
}

// The init payload arrives on descriptor 3, which a test cannot simply install
// in its own process: the test binary already has one there. So the child's
// half of the handshake is driven the way the container's init is — as a
// separate process with the pipe wired to that exact descriptor.

const (
	helperEnv     = "FORGE_RUNTIME_TEST_HELPER"
	helperReadFD3 = "read-init-payload"
	helperSleep   = "sleep-forever"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case helperReadFD3:
	case helperSleep:
		// A container that will not exit on its own, for the paths that have
		// to kill one.
		select {}
	default:
		os.Exit(m.Run())
	}

	payload, err := readInitPayload()
	if err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(3)
	}

	// Descriptors survive execve, so the payload pipe must be closed before
	// the container's binary can see it. EBADF here is the promise being kept.
	var buf [1]byte
	_, readErr := syscall.Read(initPayloadFD, buf[:])

	fmt.Printf("command: %s\n", strings.Join(payload.Command, " "))
	fmt.Printf("fd%d: %v\n", initPayloadFD, readErr)
	os.Exit(0)
}

// readInitPayloadInAChild runs the helper above with content on descriptor 3
// and returns what it printed together with its exit code.
func readInitPayloadInAChild(t *testing.T, content []byte) (string, int) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		helperEnv+"="+helperReadFD3,
		// Silences the coverage runtime's warning on a binary built with
		// -cover, which would otherwise land in the output under test.
		"GOCOVERDIR="+t.TempDir(),
	)
	cmd.ExtraFiles = []*os.File{r}

	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := cmd.Output()

	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running the helper: %v", err)
	}

	return string(out), code
}

// TestReadInitPayloadReadsTheInheritedDescriptor covers the child's half of the
// handshake, including the close that keeps Forge's plumbing out of the
// container.
func TestReadInitPayloadReadsTheInheritedDescriptor(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(initPayload{Command: []string{"/bin/echo", "hello"}})
	if err != nil {
		t.Fatal(err)
	}

	out, code := readInitPayloadInAChild(t, encoded)
	if code != 0 {
		t.Fatalf("helper exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "command: /bin/echo hello") {
		t.Errorf("helper output = %q, want the decoded command", out)
	}
	if !strings.Contains(out, "bad file descriptor") {
		t.Errorf("helper output = %q, want fd %d closed after the read", out, initPayloadFD)
	}
}

// TestReadInitPayloadReportsAnEmptyPipe covers `forge __init` typed by hand,
// which is the reason ErrNoInitPayload exists.
func TestReadInitPayloadReportsAnEmptyPipe(t *testing.T) {
	t.Parallel()

	out, code := readInitPayloadInAChild(t, nil)
	if code == 0 {
		t.Fatalf("helper succeeded on an empty pipe:\n%s", out)
	}
	if !strings.Contains(out, ErrNoInitPayload.Error()) {
		t.Errorf("helper output = %q, want ErrNoInitPayload", out)
	}
}

// TestReadInitPayloadReportsAMalformedPayload covers a payload that arrived but
// is not one.
func TestReadInitPayloadReportsAMalformedPayload(t *testing.T) {
	t.Parallel()

	out, code := readInitPayloadInAChild(t, []byte("{not json"))
	if code == 0 {
		t.Fatalf("helper succeeded on a malformed payload:\n%s", out)
	}
	if !strings.Contains(out, "decoding init payload") {
		t.Errorf("helper output = %q, want a decode failure", out)
	}
}

// TestEnterWorkingDir covers the last thing the container's init does before
// execve.
func TestEnterWorkingDir(t *testing.T) {
	// Not parallel: it changes this process's working directory.
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("an empty directory leaves the process where it is", func(t *testing.T) {
		if err := enterWorkingDir(""); err != nil {
			t.Fatalf("enterWorkingDir(\"\") = %v", err)
		}
		got, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if got != original {
			t.Errorf("working directory = %q, want it unchanged at %q", got, original)
		}
	})

	t.Run("a directory that exists is entered", func(t *testing.T) {
		// Resolved because macOS-style symlinked temp roots would otherwise
		// make the comparison below fail for the wrong reason.
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}

		if err := enterWorkingDir(dir); err != nil {
			t.Fatalf("enterWorkingDir(%s) = %v", dir, err)
		}
		got, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if got != dir {
			t.Errorf("working directory = %q, want %q", got, dir)
		}
	})

	t.Run("a missing directory is reported by name", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "no-such-dir")

		err := enterWorkingDir(missing)
		if err == nil {
			t.Fatal("enterWorkingDir() = nil for a directory that does not exist")
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("enterWorkingDir() = %q, want it to name %q", err, missing)
		}
	})

	t.Run("a file is not a directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "regular")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := enterWorkingDir(file); err == nil {
			t.Error("enterWorkingDir() = nil for a regular file")
		}
	})
}

// TestTranslateCloneError covers the message an unprivileged user gets from the
// first privileged thing Forge does.
func TestTranslateCloneError(t *testing.T) {
	t.Parallel()

	if got := translateCloneError(syscall.EPERM); !errors.Is(got, namespace.ErrPermission) {
		t.Errorf("translateCloneError(EPERM) = %v, want the namespace permission sentinel", got)
	}
	if got := translateCloneError(syscall.EPERM); !errors.Is(got, syscall.EPERM) {
		t.Error("translateCloneError(EPERM) dropped its cause")
	}

	other := errors.New("clone: no memory")
	if got := translateCloneError(other); !errors.Is(got, other) {
		t.Errorf("translateCloneError(%v) = %v, want it passed through", other, got)
	}
	if got := translateCloneError(nil); got != nil {
		t.Errorf("translateCloneError(nil) = %v, want nil", got)
	}
}

// TestCloseFileLogsRatherThanFailing pins SSOT §13.7 for the cleanup closes: a
// failure is never discarded, and never masks the error being reported.
func TestCloseFileLogsRatherThanFailing(t *testing.T) {
	t.Parallel()

	var logged strings.Builder
	log := logging.New(&logged, slog.LevelDebug)

	f, err := os.CreateTemp(t.TempDir(), "close")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// The second close fails, and must be visible in the log.
	closeFile(log, f, "the test file")

	if !strings.Contains(logged.String(), "the test file") {
		t.Errorf("closeFile did not report the failure:\n%s", logged.String())
	}
}

// TestTranslateStateError maps storage failures onto the sentinels the CLI
// classifies exit codes from. An unknown container and an unusable id are the
// same thing to a user: no such container.
func TestTranslateStateError(t *testing.T) {
	t.Parallel()

	const id = "7f3c9a1b2d04"

	tests := []struct {
		name    string
		err     error
		want    error
		message string
	}{
		{name: "not found", err: state.ErrNotFound, want: ErrNotFound, message: id},
		{name: "invalid id", err: state.ErrInvalidID, want: ErrNotFound, message: id},
		{name: "anything else", err: errors.New("disk on fire"), want: nil, message: "disk on fire"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := translateStateError(id, tc.err)
			if tc.want != nil && !errors.Is(got, tc.want) {
				t.Errorf("translateStateError(%v) = %v, want %v", tc.err, got, tc.want)
			}
			if tc.want == nil && errors.Is(got, ErrNotFound) {
				t.Errorf("translateStateError(%v) = %v, want it not to claim the container is missing", tc.err, got)
			}
			if !strings.Contains(got.Error(), tc.message) {
				t.Errorf("translateStateError(%v) = %q, want it to mention %q", tc.err, got, tc.message)
			}
		})
	}
}

// TestRecordWritesAdvanceTheLifecycle covers the record writes made in the
// handshake window, which are what let a `forge stop` in another terminal find
// a container this process has only just created.
func TestRecordWritesAdvanceTheLifecycle(t *testing.T) {
	t.Parallel()

	r := testRunner(t, nil)
	log := logging.New(io.Discard, slog.LevelError)

	const id = "7f3c9a1b2d04"
	if err := r.createRecord(id, Spec{Image: "alpine:3.20", Command: []string{"/bin/sh"}}); err != nil {
		t.Fatalf("createRecord() = %v", err)
	}

	m := load(t, r, id)
	if m.Status != state.StatusCreating {
		t.Errorf("status after createRecord = %s, want %s", m.Status, state.StatusCreating)
	}
	if m.Image != "alpine:3.20" {
		t.Errorf("image = %q, want alpine:3.20", m.Image)
	}

	r.recordFilesystem(log, id, "/var/lib/forge/containers/"+id)
	r.recordCreated(log, id, 4242)

	m = load(t, r, id)
	if m.PID != 4242 {
		t.Errorf("pid = %d, want 4242", m.PID)
	}
	if m.Status != state.StatusCreated {
		t.Errorf("status after recordCreated = %s, want %s", m.Status, state.StatusCreated)
	}
	if m.RootfsPath == "" {
		t.Error("recordFilesystem left no rootfs path, so forge rm could not find the tree")
	}

	r.recordRunning(log, id)

	m = load(t, r, id)
	if m.Status != state.StatusRunning {
		t.Errorf("status after recordRunning = %s, want %s", m.Status, state.StatusRunning)
	}
	if m.StartedAt == nil {
		t.Error("recordRunning left no start time, so forge ps could not report an age")
	}
}

// TestNoteLogsRatherThanFailing is the reason record.go can be called from the
// middle of a start: the container matters more than the bookkeeping, so a
// record write that fails is reported and the run continues.
func TestNoteLogsRatherThanFailing(t *testing.T) {
	t.Parallel()

	r := testRunner(t, nil)

	var logged strings.Builder
	log := logging.New(&logged, slog.LevelDebug)

	// No record exists for this id, so every mutation below fails in the store.
	r.recordCreated(log, "7f3c9a1b2d04", 4242)
	r.recordRunning(log, "7f3c9a1b2d04")

	out := logged.String()
	if !strings.Contains(out, "the container pid") {
		t.Errorf("a failed pid write was not reported:\n%s", out)
	}
	if !strings.Contains(out, "the container start") {
		t.Errorf("a failed start write was not reported:\n%s", out)
	}
}

// TestOpenCgroupDegradesWithoutAHierarchy covers the exec path on a host with
// no cgroup v2: the command runs unaccounted rather than being refused, which
// is the same degradation Stage 3 makes for `forge run`.
func TestOpenCgroupDegradesWithoutAHierarchy(t *testing.T) {
	t.Parallel()

	r := testRunner(t, nil)
	log := logging.New(io.Discard, slog.LevelError)

	const id = "7f3c9a1b2d04"

	f, err := r.openCgroup(log, id)
	if err != nil {
		t.Fatalf("openCgroup() with no cgroup = %v, want nil", err)
	}
	if f != nil {
		t.Error("openCgroup() returned a descriptor for a cgroup that does not exist")
		_ = f.Close()
	}

	// Once the leaf exists it is opened, because that is what places the
	// exec'd process into the container's cgroup at birth.
	dir := prepareCgroupLeaf(t, r, id)

	f, err = r.openCgroup(log, id)
	if err != nil {
		t.Fatalf("openCgroup() = %v", err)
	}
	if f == nil {
		t.Fatalf("openCgroup() = nil for the cgroup at %s", dir)
	}
	t.Cleanup(func() { _ = f.Close() })

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Error("openCgroup did not open a directory")
	}
}

// TestOpenCgroupRejectsAnEscapingID checks that the id is validated before it
// is joined onto the cgroup root, so `forge exec ../../..` cannot open a
// directory outside Forge's hierarchy.
func TestOpenCgroupRejectsAnEscapingID(t *testing.T) {
	t.Parallel()

	r := testRunner(t, nil)

	if _, err := r.openCgroup(logging.New(io.Discard, slog.LevelError), "../../etc"); err == nil {
		t.Error("openCgroup() = nil for an id that escapes the hierarchy")
	}
}

// sleepingChild starts the helper above as an ordinary child process, which is
// what a container's init is once clone(2) has returned.
func sleepingChild(t *testing.T) *process.Process {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	p, err := process.New(process.Config{
		Path: exe,
		Args: []string{"forge-test-helper"},
		Env:  []string{helperEnv + "=" + helperSleep, "GOCOVERDIR=" + t.TempDir()},
	})
	if err != nil {
		t.Fatalf("process.New() = %v", err)
	}
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	return p
}

// TestAbandonKillsAndReapsTheContainer covers PRD NFR-8 at its sharpest point.
//
// abandon runs when a container has been started but cannot be allowed to
// proceed — a cgroup that could not be created, a payload that could not be
// sent. If it merely killed without reaping, forge would exit leaving a zombie;
// if it did not kill, the container would run on with nobody supervising it.
func TestAbandonKillsAndReapsTheContainer(t *testing.T) {
	t.Parallel()

	r := testRunner(t, nil)
	log := logging.New(io.Discard, slog.LevelError)

	p := sleepingChild(t)
	pid := p.PID()
	if pid <= 0 {
		t.Fatalf("PID() = %d, want a started process", pid)
	}

	r.abandon(t.Context(), log, p, "a test")

	// Reaped: the process is gone and its status has been collected, so a
	// second Wait answers from what abandon already learned rather than
	// blocking or failing.
	status, err := p.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait() after abandon = %v", err)
	}
	if status.Signal != syscall.SIGKILL {
		t.Errorf("status = %+v, want it killed by SIGKILL", status)
	}

	// Killed: nothing answers to the PID any more. A reaped child cannot be
	// signalled even as a zombie, so ESRCH here is the whole claim.
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("kill(%d, 0) = %v, want ESRCH", pid, err)
	}
}

// TestAbandonReportsFailuresRatherThanDiscardingThem covers SSOT §13.7 on the
// path that already has an error to report: abandon returns nothing, so a
// failure to kill or reap has to reach the log or it reaches nobody.
func TestAbandonReportsFailuresRatherThanDiscardingThem(t *testing.T) {
	t.Parallel()

	r := testRunner(t, nil)

	var logged strings.Builder
	log := logging.New(&logged, slog.LevelDebug)

	// A process that was never started: signalling and reaping it both fail,
	// which is the shape of a clone(2) that returned an error and left a
	// half-built Process behind.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := process.New(process.Config{Path: exe, Args: []string{"forge-test-helper"}})
	if err != nil {
		t.Fatal(err)
	}

	r.abandon(t.Context(), log, p, "a test")

	out := logged.String()
	if !strings.Contains(out, "killing container after a test") {
		t.Errorf("abandon did not report the failed kill:\n%s", out)
	}
	if !strings.Contains(out, "reaping container after a test") {
		t.Errorf("abandon did not report the failed reap:\n%s", out)
	}
}

// TestOpenContainerProcess covers the production opener, including the trap its
// comment names: a *process.Handle returned as a non-nil interface holding a
// nil pointer would make every "did this fail?" check in Exec and Stop wrong.
func TestOpenContainerProcess(t *testing.T) {
	t.Parallel()

	t.Run("a live process", func(t *testing.T) {
		t.Parallel()

		proc, err := openContainerProcess(os.Getpid())
		if err != nil {
			t.Fatalf("openContainerProcess() = %v", err)
		}
		if proc == nil {
			t.Fatal("openContainerProcess() = nil for a process that exists")
		}
		t.Cleanup(func() { _ = proc.Close() })

		if !proc.Alive() {
			t.Error("Alive() = false for this very process")
		}
	})

	t.Run("a process that does not exist", func(t *testing.T) {
		t.Parallel()

		// The kernel's pid_max is at most 2^22, so this PID cannot be in use.
		proc, err := openContainerProcess(1 << 30)
		if err == nil {
			t.Fatal("openContainerProcess() = nil error for a PID that cannot exist")
		}
		if proc != nil {
			t.Error("openContainerProcess() returned a non-nil interface alongside an error")
		}
	})
}

// TestConfigureNetworkForHostNetworking covers the container-side branch that
// must do nothing at all: a container sharing the host's network namespace has
// no interface of its own to bring up, and touching anything here would
// reconfigure the host.
func TestConfigureNetworkForHostNetworking(t *testing.T) {
	t.Parallel()

	payload := initPayload{Command: []string{"/bin/sh"}}
	payload.Namespace.Net = false

	if err := configureNetwork(payload); err != nil {
		t.Errorf("configureNetwork() for host networking = %v, want nil", err)
	}
}
