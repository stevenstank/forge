package runtime_test

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/runtime"
)

func TestSpecValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    runtime.Spec
		wantErr error
	}{
		{
			name: "absolute path is valid",
			spec: runtime.Spec{Command: []string{"/bin/echo", "hello"}},
		},
		{
			name: "relative path is valid",
			spec: runtime.Spec{Command: []string{"./forge-test"}},
		},
		{
			name:    "no command is rejected",
			spec:    runtime.Spec{},
			wantErr: runtime.ErrNoCommand,
		},
		{
			name:    "empty command is rejected",
			spec:    runtime.Spec{Command: []string{}},
			wantErr: runtime.ErrNoCommand,
		},
		{
			name:    "empty binary is rejected",
			spec:    runtime.Spec{Command: []string{""}},
			wantErr: runtime.ErrNoCommand,
		},
		{
			name:    "bare name is rejected because forge does not search PATH",
			spec:    runtime.Spec{Command: []string{"echo"}},
			wantErr: runtime.ErrNotAPath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.spec.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateSuggestsAPath keeps the PATH refusal actionable.
func TestValidateSuggestsAPath(t *testing.T) {
	t.Parallel()

	err := runtime.Spec{Command: []string{"echo"}}.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error")
	}
	if got := err.Error(); !strings.Contains(got, "/bin/echo") {
		t.Errorf("error %q does not suggest a concrete path", got)
	}
}

func TestIsInitCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "init command", args: []string{"forge-init", runtime.InitCommandName}, want: true},
		{name: "init command with trailing args", args: []string{"forge", runtime.InitCommandName, "x"}, want: true},
		{name: "run command", args: []string{"forge", "run", "/bin/echo"}, want: false},
		{name: "no arguments", args: []string{"forge"}, want: false},
		{name: "empty vector", args: nil, want: false},
		{name: "init name in the wrong position", args: []string{runtime.InitCommandName}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := runtime.IsInitCommand(tt.args); got != tt.want {
				t.Errorf("IsInitCommand(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestRunRefusesNestedInit covers the fork-bomb guard: a binary that
// re-executes itself without dispatching the init command must fail
// immediately rather than starting containers forever (PRD NFR-8).
//
// It cannot run in parallel because it manipulates the process environment.
func TestRunRefusesNestedInit(t *testing.T) {
	t.Setenv("FORGE_CONTAINER_INIT", "1")

	runner := runtime.NewRunner(logging.New(io.Discard, slog.LevelError))

	_, err := runner.Run(t.Context(), runtime.Spec{Command: []string{"/bin/echo", "hi"}})
	if !errors.Is(err, runtime.ErrNestedInit) {
		t.Fatalf("Run() = %v, want %v", err, runtime.ErrNestedInit)
	}
}

// TestRunRejectsInvalidSpecBeforeForking confirms validation happens in the
// parent, so bad input never reaches the kernel. This is why it needs no root.
func TestRunRejectsInvalidSpecBeforeForking(t *testing.T) {
	t.Parallel()

	runner := runtime.NewRunner(logging.New(io.Discard, slog.LevelError))

	if _, err := runner.Run(t.Context(), runtime.Spec{}); !errors.Is(err, runtime.ErrNoCommand) {
		t.Errorf("Run() = %v, want %v", err, runtime.ErrNoCommand)
	}
	if _, err := runner.Run(t.Context(), runtime.Spec{Command: []string{"echo"}}); !errors.Is(err, runtime.ErrNotAPath) {
		t.Errorf("Run() = %v, want %v", err, runtime.ErrNotAPath)
	}
}

// Init itself is exercised end to end by the Stage 1 integration tests; its
// wire format is covered by the decodeInitPayload tests in init_internal_test.go.
// Calling Init here would read whatever the test harness left on descriptor 3.
