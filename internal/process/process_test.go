package process_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/process"
)

// These tests start real processes but request no namespaces, so they run
// unprivileged and satisfy SSOT §7. The subject is always this test binary
// re-executed in a helper mode, which keeps the tests independent of whatever
// binaries happen to exist on the host.

const (
	helperEnv      = "FORGE_PROCESS_TEST_HELPER"
	helperExitCode = "FORGE_PROCESS_TEST_EXIT_CODE"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "exit":
		code, err := strconv.Atoi(os.Getenv(helperExitCode))
		if err != nil {
			os.Exit(255)
		}
		os.Exit(code)
	case "echo-args":
		os.Stdout.WriteString(strings.Join(os.Args[1:], "|"))
		os.Exit(0)
	case "echo-env":
		os.Stdout.WriteString(strings.Join(os.Environ(), "\n"))
		os.Exit(0)
	case "copy-stdin":
		if _, err := os.Stdout.ReadFrom(os.Stdin); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "write-stderr":
		os.Stderr.WriteString("to stderr")
		os.Exit(0)
	case "sleep-forever":
		select {}
	default:
		os.Exit(254)
	}
}

// helperConfig returns a Config that re-runs this test binary in the given
// helper mode.
func helperConfig(t *testing.T, mode string, env ...string) process.Config {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	return process.Config{
		Path: exe,
		Args: []string{"forge-test-helper"},
		Env:  append([]string{helperEnv + "=" + mode}, env...),
	}
}

// startHelper starts a helper process and fails the test if it cannot start.
func startHelper(t *testing.T, cfg process.Config) *process.Process {
	t.Helper()

	p, err := process.New(cfg)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	t.Cleanup(func() {
		_ = p.Signal(syscall.SIGKILL)
	})

	return p
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     process.Config
		wantErr error
	}{
		{
			name: "path and args are sufficient",
			cfg:  process.Config{Path: "/bin/true", Args: []string{"true"}},
		},
		{
			name:    "missing path is rejected",
			cfg:     process.Config{Args: []string{"true"}},
			wantErr: process.ErrNoPath,
		},
		{
			name:    "missing args is rejected",
			cfg:     process.Config{Path: "/bin/true"},
			wantErr: process.ErrNoArgs,
		},
		{
			name:    "empty args is rejected",
			cfg:     process.Config{Path: "/bin/true", Args: []string{}},
			wantErr: process.ErrNoArgs,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.cfg.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	p, err := process.New(process.Config{})
	if !errors.Is(err, process.ErrNoPath) {
		t.Errorf("New() error = %v, want %v", err, process.ErrNoPath)
	}
	if p != nil {
		t.Errorf("New() = %v, want nil process on error", p)
	}
}

// TestLifecycleStates covers FR-1.4: Forge tracks start, running and exited.
func TestLifecycleStates(t *testing.T) {
	t.Parallel()

	cfg := helperConfig(t, "exit", helperExitCode+"=0")

	p, err := process.New(cfg)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if got := p.State(); got != process.StateCreated {
		t.Errorf("State() before Start = %v, want %v", got, process.StateCreated)
	}
	if got := p.PID(); got != 0 {
		t.Errorf("PID() before Start = %d, want 0", got)
	}

	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if got := p.State(); got != process.StateRunning {
		t.Errorf("State() after Start = %v, want %v", got, process.StateRunning)
	}
	if got := p.PID(); got <= 0 {
		t.Errorf("PID() after Start = %d, want a positive pid", got)
	}

	if _, err := p.Wait(t.Context()); err != nil {
		t.Fatalf("Wait() = %v", err)
	}
	if got := p.State(); got != process.StateExited {
		t.Errorf("State() after Wait = %v, want %v", got, process.StateExited)
	}
}

// TestExitCodeIsReported covers the FR-1.4 requirement to report the exit code.
func TestExitCodeIsReported(t *testing.T) {
	t.Parallel()

	for _, want := range []int{0, 1, 42, 255} {
		t.Run(strconv.Itoa(want), func(t *testing.T) {
			t.Parallel()

			cfg := helperConfig(t, "exit", helperExitCode+"="+strconv.Itoa(want))
			p := startHelper(t, cfg)

			status, err := p.Wait(t.Context())
			if err != nil {
				t.Fatalf("Wait() = %v", err)
			}
			if status.Code != want {
				t.Errorf("Status.Code = %d, want %d", status.Code, want)
			}
			if status.Signaled() {
				t.Errorf("Status.Signaled() = true, want false for a normal exit")
			}
		})
	}
}

func TestSignalledProcessReportsSignal(t *testing.T) {
	t.Parallel()

	p := startHelper(t, helperConfig(t, "sleep-forever"))

	if err := p.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("Signal() = %v", err)
	}

	status, err := p.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait() = %v", err)
	}
	if !status.Signaled() {
		t.Errorf("Status.Signaled() = false, want true")
	}
	if status.Signal != syscall.SIGKILL {
		t.Errorf("Status.Signal = %v, want %v", status.Signal, syscall.SIGKILL)
	}
	// Shell convention: 128 + signal number.
	if want := 128 + int(syscall.SIGKILL); status.Code != want {
		t.Errorf("Status.Code = %d, want %d", status.Code, want)
	}
}

// TestWaitKillsOnContextCancellation covers PRD NFR-5: a cancelled run must not
// leave the process behind.
func TestWaitKillsOnContextCancellation(t *testing.T) {
	t.Parallel()

	p := startHelper(t, helperConfig(t, "sleep-forever"))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan process.Status, 1)
	go func() {
		status, err := p.Wait(ctx)
		if err != nil {
			t.Errorf("Wait() = %v", err)
		}
		done <- status
	}()

	cancel()

	select {
	case status := <-done:
		if status.Signal != syscall.SIGKILL {
			t.Errorf("Status.Signal = %v, want %v", status.Signal, syscall.SIGKILL)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Wait did not return after its context was cancelled")
	}

	if got := p.State(); got != process.StateExited {
		t.Errorf("State() = %v, want %v", got, process.StateExited)
	}
}

func TestWaitIsRepeatable(t *testing.T) {
	t.Parallel()

	p := startHelper(t, helperConfig(t, "exit", helperExitCode+"=7"))

	first, err := p.Wait(t.Context())
	if err != nil {
		t.Fatalf("first Wait() = %v", err)
	}
	second, err := p.Wait(t.Context())
	if err != nil {
		t.Fatalf("second Wait() = %v", err)
	}
	if first != second {
		t.Errorf("second Wait() = %+v, want the recorded %+v", second, first)
	}
}

func TestOperationsBeforeStart(t *testing.T) {
	t.Parallel()

	p, err := process.New(helperConfig(t, "exit", helperExitCode+"=0"))
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	if _, err := p.Wait(t.Context()); !errors.Is(err, process.ErrNotStarted) {
		t.Errorf("Wait() before Start = %v, want %v", err, process.ErrNotStarted)
	}
	if err := p.Signal(syscall.SIGTERM); !errors.Is(err, process.ErrNotStarted) {
		t.Errorf("Signal() before Start = %v, want %v", err, process.ErrNotStarted)
	}
}

func TestStartTwiceIsRejected(t *testing.T) {
	t.Parallel()

	p := startHelper(t, helperConfig(t, "sleep-forever"))

	if err := p.Start(t.Context()); !errors.Is(err, process.ErrAlreadyStarted) {
		t.Errorf("second Start() = %v, want %v", err, process.ErrAlreadyStarted)
	}
}

// TestSignalAfterExitIsNoOp lets callers signal unconditionally during cleanup.
func TestSignalAfterExitIsNoOp(t *testing.T) {
	t.Parallel()

	p := startHelper(t, helperConfig(t, "exit", helperExitCode+"=0"))
	if _, err := p.Wait(t.Context()); err != nil {
		t.Fatalf("Wait() = %v", err)
	}

	if err := p.Signal(syscall.SIGKILL); err != nil {
		t.Errorf("Signal() after exit = %v, want nil", err)
	}
}

func TestStartRejectsCancelledContext(t *testing.T) {
	t.Parallel()

	p, err := process.New(helperConfig(t, "sleep-forever"))
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := p.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Start() = %v, want %v", err, context.Canceled)
	}
	if got := p.State(); got != process.StateCreated {
		t.Errorf("State() = %v, want %v after a refused Start", got, process.StateCreated)
	}
}

func TestStartReportsMissingBinary(t *testing.T) {
	t.Parallel()

	p, err := process.New(process.Config{
		Path: "/nonexistent/forge-test-binary",
		Args: []string{"forge-test-binary"},
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	err = p.Start(t.Context())
	if err == nil {
		t.Fatal("Start() = nil, want an error for a missing binary")
	}
	if !strings.Contains(err.Error(), "/nonexistent/forge-test-binary") {
		t.Errorf("error %q does not name the binary", err)
	}
}

func TestStdioIsWired(t *testing.T) {
	t.Parallel()

	t.Run("stdout", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		cfg := helperConfig(t, "echo-args")
		cfg.Args = []string{"forge-test-helper", "alpha", "beta"}
		cfg.Stdout = &stdout

		p := startHelper(t, cfg)
		if _, err := p.Wait(t.Context()); err != nil {
			t.Fatalf("Wait() = %v", err)
		}
		if got, want := stdout.String(), "alpha|beta"; got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("stderr", func(t *testing.T) {
		t.Parallel()

		var stderr bytes.Buffer
		cfg := helperConfig(t, "write-stderr")
		cfg.Stderr = &stderr

		p := startHelper(t, cfg)
		if _, err := p.Wait(t.Context()); err != nil {
			t.Fatalf("Wait() = %v", err)
		}
		if got, want := stderr.String(), "to stderr"; got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("stdin", func(t *testing.T) {
		t.Parallel()

		var stdout bytes.Buffer
		cfg := helperConfig(t, "copy-stdin")
		cfg.Stdin = strings.NewReader("piped input")
		cfg.Stdout = &stdout

		p := startHelper(t, cfg)
		if _, err := p.Wait(t.Context()); err != nil {
			t.Fatalf("Wait() = %v", err)
		}
		if got, want := stdout.String(), "piped input"; got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
	})
}

// TestEnvIsExplicit guards the documented promise that a container's
// environment is exactly what the caller supplied, never implicitly inherited
// from the host. It cannot run in parallel because it uses t.Setenv.
func TestEnvIsExplicit(t *testing.T) {
	const sentinel = "FORGE_TEST_SENTINEL"
	t.Setenv(sentinel, "leaked")

	var stdout bytes.Buffer
	cfg := helperConfig(t, "echo-env")
	cfg.Stdout = &stdout

	p := startHelper(t, cfg)
	if _, err := p.Wait(t.Context()); err != nil {
		t.Fatalf("Wait() = %v", err)
	}

	if strings.Contains(stdout.String(), sentinel) {
		t.Errorf("child environment leaked %s:\n%s", sentinel, stdout.String())
	}
}

func TestStateString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state process.State
		want  string
	}{
		{process.StateCreated, "created"},
		{process.StateRunning, "running"},
		{process.StateExited, "exited"},
		{process.State(99), "unknown(99)"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", int(tt.state), got, tt.want)
		}
	}
}

func TestStatusString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status process.Status
		want   string
	}{
		{name: "normal exit", status: process.Status{Code: 0}, want: "exit 0"},
		{name: "failure exit", status: process.Status{Code: 42}, want: "exit 42"},
		{
			name:   "signalled",
			status: process.Status{Code: 137, Signal: syscall.SIGKILL},
			want:   "killed (exit 137)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.String(); got != tt.want {
				t.Errorf("Status.String() = %q, want %q", got, tt.want)
			}
		})
	}
}
