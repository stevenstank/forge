package cli

import (
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/runtime"
)

// `forge run` needs root to actually start a container, so these tests cover
// the layer this package is responsible for: turning arguments into a Spec and
// a Status into an exit code (SSOT §13.6). The container behaviour itself is
// covered by test/integration.

func TestRunCommandArgumentErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "no command to run",
			args:       []string{"run"},
			wantStderr: "run requires a command to execute",
		},
		{
			name:       "unknown flag",
			args:       []string{"run", "-nonesuch", "/bin/echo"},
			wantStderr: "nonesuch",
		},
		{
			name:       "bare name is not a path",
			args:       []string{"run", "echo"},
			wantStderr: "does not search PATH",
		},
		{
			name:       "hostname flag requires a value",
			args:       []string{"run", "-hostname"},
			wantStderr: "hostname",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, stdout, stderr := newTestApp(commands()...)

			if got := a.run(t.Context(), tt.args); got != ExitUsage {
				t.Errorf("exit code = %d, want %d (stderr: %q)", got, ExitUsage, stderr)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
			// Diagnostics belong on stderr; stdout is the container's.
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
		})
	}
}

// TestRunCommandRejectsInvalidHostnameBeforeForking confirms a bad hostname is
// caught in the parent, which is why this test needs no root.
func TestRunCommandRejectsInvalidHostnameBeforeForking(t *testing.T) {
	t.Parallel()

	a, _, stderr := newTestApp(commands()...)

	code := a.run(t.Context(), []string{"run", "-hostname", "bad hostname", "/bin/echo", "hi"})
	if code == ExitOK {
		t.Fatalf("exit code = %d, want a failure", code)
	}
	if !strings.Contains(stderr.String(), "hostname") {
		t.Errorf("stderr = %q, want it to mention the hostname", stderr)
	}
}

func TestRunCommandHelp(t *testing.T) {
	t.Parallel()

	a, _, stderr := newTestApp(commands()...)

	if got := a.run(t.Context(), []string{"run", "-h"}); got != ExitOK {
		t.Errorf("exit code = %d, want %d", got, ExitOK)
	}

	got := stderr.String()
	for _, want := range []string{"forge run", "<path>", "-hostname", "does not search PATH"} {
		if !strings.Contains(got, want) {
			t.Errorf("run help does not mention %q:\n%s", want, got)
		}
	}
}

// TestDefaultContainerEnvIsMinimal pins the Stage 1 promise that a container's
// environment is explicit rather than inherited from the host.
func TestDefaultContainerEnvIsMinimal(t *testing.T) {
	t.Parallel()

	got := defaultContainerEnv()

	if len(got) != 1 {
		t.Fatalf("defaultContainerEnv() = %q, want exactly one entry", got)
	}
	if !strings.HasPrefix(got[0], "PATH=") {
		t.Errorf("defaultContainerEnv() = %q, want a PATH entry", got)
	}
}

// TestInitCommandIsWiredToTheRuntime pins the contract ADR-0008 depends on: the
// name Forge re-executes itself with must be the one the runtime looks for, and
// it must stay hidden from users.
//
// The command is not executed here — doing so would read descriptor 3, whose
// contents in a test binary are undefined. Its behaviour is covered end to end
// by test/integration.
func TestInitCommandIsWiredToTheRuntime(t *testing.T) {
	t.Parallel()

	cmd := newInitCommand()

	if cmd.Name != runtime.InitCommandName {
		t.Errorf("init command name = %q, want %q", cmd.Name, runtime.InitCommandName)
	}
	if !cmd.Hidden {
		t.Error("init command is visible; it must not appear in help output")
	}
	if cmd.Exec == nil {
		t.Error("init command has no implementation")
	}
	if !runtime.IsInitCommand([]string{"forge", cmd.Name}) {
		t.Errorf("runtime does not recognise %q as its init command", cmd.Name)
	}
}
