package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// newTestApp returns an app with the given commands registered, plus the
// buffers its output was written to.
func newTestApp(cmds ...Command) (*app, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	a := &app{
		commands: cmds,
		stdin:    strings.NewReader(""),
		stdout:   &stdout,
		stderr:   &stderr,
	}
	return a, &stdout, &stderr
}

func TestRunExitCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		want       int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "no arguments is a usage error",
			args:       nil,
			want:       ExitUsage,
			wantStderr: "no command given",
		},
		{
			name:       "help is written to stdout and succeeds",
			args:       []string{"-h"},
			want:       ExitOK,
			wantStdout: "Usage:",
		},
		{
			name:       "unknown command names the command",
			args:       []string{"sprint"},
			want:       ExitUsage,
			wantStderr: `unknown command "sprint"`,
		},
		{
			name:       "unknown flag is a usage error",
			args:       []string{"-nonesuch"},
			want:       ExitUsage,
			wantStderr: "nonesuch",
		},
		{
			name:       "invalid log level is a usage error",
			args:       []string{"-log-level", "verbose", "noop"},
			want:       ExitUsage,
			wantStderr: "verbose",
		},
		{
			name:       "relative state dir is a usage error",
			args:       []string{"-state-dir", "relative/path", "noop"},
			want:       ExitUsage,
			wantStderr: "-state-dir must be an absolute path",
		},
		{
			name:       "relative root is a usage error",
			args:       []string{"-root", "relative/path", "noop"},
			want:       ExitUsage,
			wantStderr: "-root must be an absolute path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			noop := Command{Name: "noop", Exec: func(context.Context, *Env, []string) error { return nil }}
			a, stdout, stderr := newTestApp(noop)

			got := a.run(t.Context(), tt.args)
			if got != tt.want {
				t.Errorf("exit code = %d, want %d (stderr: %q)", got, tt.want, stderr)
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

func TestRunDispatchesToCommand(t *testing.T) {
	t.Parallel()

	var gotArgs []string
	cmd := Command{
		Name: "noop",
		Exec: func(_ context.Context, _ *Env, args []string) error {
			gotArgs = args
			return nil
		},
	}

	a, _, stderr := newTestApp(cmd)

	if got := a.run(t.Context(), []string{"noop", "alpine", "--", "-x"}); got != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %q)", got, ExitOK, stderr)
	}
	if want := []string{"alpine", "--", "-x"}; !slices.Equal(gotArgs, want) {
		t.Errorf("command received args %q, want %q", gotArgs, want)
	}
}

func TestRunPassesGlobalOptionsToCommand(t *testing.T) {
	t.Parallel()

	var got Options
	cmd := Command{
		Name: "noop",
		Exec: func(_ context.Context, env *Env, _ []string) error {
			got = env.Opts
			return nil
		},
	}

	a, _, stderr := newTestApp(cmd)
	args := []string{"-log-level", "debug", "-state-dir", "/srv/forge/", "-root", "/srv/forge/rootfs", "noop"}

	if code := a.run(t.Context(), args); code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %q)", code, ExitOK, stderr)
	}

	want := Options{LogLevel: slog.LevelDebug, StateDir: "/srv/forge", Root: "/srv/forge/rootfs"}
	if got != want {
		t.Errorf("options = %+v, want %+v", got, want)
	}
}

func TestRunDefaultOptions(t *testing.T) {
	t.Parallel()

	var got Options
	cmd := Command{
		Name: "noop",
		Exec: func(_ context.Context, env *Env, _ []string) error {
			got = env.Opts
			return nil
		},
	}

	a, _, stderr := newTestApp(cmd)
	if code := a.run(t.Context(), []string{"noop"}); code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %q)", code, ExitOK, stderr)
	}

	want := Options{LogLevel: slog.LevelInfo, StateDir: DefaultStateDir, Root: DefaultRoot}
	if got != want {
		t.Errorf("options = %+v, want %+v", got, want)
	}
}

func TestRunCommandErrorsMapToExitCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		// Only a wrapped sentinel counts; matching on message text would make
		// exit codes depend on error prose (SSOT §5).
		{name: "message merely containing the sentinel text", err: errors.New("bad container id: " + ErrUsage.Error()), want: ExitInternal},
		{name: "wrapped ErrUsage", err: fmtErrUsage(), want: ExitUsage},
		{name: "internal error", err: errors.New("mount failed"), want: ExitInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := Command{Name: "noop", Exec: func(context.Context, *Env, []string) error { return tt.err }}
			a, _, stderr := newTestApp(cmd)

			if got := a.run(t.Context(), []string{"noop"}); got != tt.want {
				t.Errorf("exit code = %d, want %d", got, tt.want)
			}
			if !strings.Contains(stderr.String(), "forge:") {
				t.Errorf("stderr = %q, want it to carry the forge prefix", stderr)
			}
		})
	}
}

// fmtErrUsage builds the error a command reports for bad user input.
func fmtErrUsage() error {
	return errors.Join(ErrUsage, errors.New("container id is required"))
}

// TestEnvLoggerWritesToStderr guards SSOT §6: diagnostics never contaminate the
// machine-readable stdout stream.
func TestEnvLoggerWritesToStderr(t *testing.T) {
	t.Parallel()

	cmd := Command{
		Name: "noop",
		Exec: func(_ context.Context, env *Env, _ []string) error {
			env.Logger.Warn("something to report")
			return nil
		},
	}

	a, stdout, stderr := newTestApp(cmd)
	if code := a.run(t.Context(), []string{"noop"}); code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want it to be empty", stdout)
	}
	if !strings.Contains(stderr.String(), "something to report") {
		t.Errorf("stderr = %q, want the log record", stderr)
	}
}

// TestEnvLoggerHonoursLogLevel confirms -log-level reaches the injected logger.
func TestEnvLoggerHonoursLogLevel(t *testing.T) {
	t.Parallel()

	cmd := Command{
		Name: "noop",
		Exec: func(_ context.Context, env *Env, _ []string) error {
			env.Logger.Debug("syscall detail")
			return nil
		},
	}

	a, _, stderr := newTestApp(cmd)
	if code := a.run(t.Context(), []string{"noop"}); code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want debug records suppressed at the default level", stderr)
	}
}

// TestUsageListsRegisteredCommands ensures help text is derived from the
// registry rather than hard-coded, so later stages get it for free.
func TestUsageListsRegisteredCommands(t *testing.T) {
	t.Parallel()

	cmd := Command{Name: "noop", Summary: "do nothing at all"}
	a, stdout, _ := newTestApp(cmd)

	if code := a.run(t.Context(), []string{"-h"}); code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}

	got := stdout.String()
	for _, want := range []string{"noop", "do nothing at all", "-log-level", "-state-dir", "-root"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage output does not mention %q:\n%s", want, got)
		}
	}
}

// TestUsageOmitsEmptyCommandSection covers Forge's current state: with no
// commands registered, help must not print a dangling "Commands:" heading.
func TestUsageOmitsEmptyCommandSection(t *testing.T) {
	t.Parallel()

	a, stdout, _ := newTestApp()
	if code := a.run(t.Context(), []string{"-h"}); code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if strings.Contains(stdout.String(), "Commands:") {
		t.Errorf("usage output has an empty command section:\n%s", stdout)
	}
}

// TestRegisteredCommandsMatchTheCurrentStage pins the stage boundary: no verb
// from SSOT §9 may be registered until its own stage implements it. Stage 1
// delivers "run" and the internal init entry point, and nothing else.
func TestRegisteredCommandsMatchTheCurrentStage(t *testing.T) {
	t.Parallel()

	var got []string
	for _, c := range commands() {
		got = append(got, c.Name)
	}

	want := []string{"run", "__init"}
	if !slices.Equal(got, want) {
		t.Errorf("commands() = %v, want %v", got, want)
	}

	// Verbs belonging to Stages 2-6 must not exist yet.
	for _, later := range []string{"ps", "exec", "stop", "logs", "rm"} {
		if slices.Contains(got, later) {
			t.Errorf("%q is registered but belongs to a later stage", later)
		}
	}
}

// TestInitCommandIsHidden keeps Forge's internal re-exec entry point out of the
// user-facing surface described by SSOT §9.
func TestInitCommandIsHidden(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if got := Run(t.Context(), []string{"-h"}, strings.NewReader(""), &stdout, &stderr); got != ExitOK {
		t.Fatalf("exit code = %d, want %d", got, ExitOK)
	}
	if strings.Contains(stdout.String(), "__init") {
		t.Errorf("help output exposes the internal init command:\n%s", &stdout)
	}
	if !strings.Contains(stdout.String(), "run") {
		t.Errorf("help output omits the run command:\n%s", &stdout)
	}
}

// TestRunReportsUnknownCommand is the behaviour a user sees for a typo.
func TestRunReportsUnknownCommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if got := Run(t.Context(), []string{"sprint"}, strings.NewReader(""), &stdout, &stderr); got != ExitUsage {
		t.Errorf("exit code = %d, want %d", got, ExitUsage)
	}
	if !strings.Contains(stderr.String(), `unknown command "sprint"`) {
		t.Errorf("stderr = %q, want an unknown-command message", &stderr)
	}
}

func TestExitErrorOverridesClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		want       int
		wantStderr string
	}{
		{
			name: "bare container status prints nothing",
			err:  &ExitError{Code: 42},
			want: 42,
		},
		{
			name:       "wrapped cause is reported",
			err:        &ExitError{Code: 127, Err: errors.New("no such file")},
			want:       127,
			wantStderr: "no such file",
		},
		{
			name:       "exit error code beats a wrapped usage sentinel",
			err:        &ExitError{Code: 3, Err: ErrUsage},
			want:       3,
			wantStderr: "invalid usage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := Command{Name: "noop", Exec: func(context.Context, *Env, []string) error { return tt.err }}
			a, _, stderr := newTestApp(cmd)

			if got := a.run(t.Context(), []string{"noop"}); got != tt.want {
				t.Errorf("exit code = %d, want %d", got, tt.want)
			}
			if tt.wantStderr == "" {
				if stderr.Len() != 0 {
					t.Errorf("stderr = %q, want it empty for a bare container status", stderr)
				}
				return
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

func TestExitErrorUnwraps(t *testing.T) {
	t.Parallel()

	cause := errors.New("underlying")
	err := error(&ExitError{Code: 9, Err: cause})

	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(%v, cause) = false, want true", err)
	}
	if got, want := err.Error(), "underlying"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got, want := (&ExitError{Code: 5}).Error(), "exit status 5"; got != want {
		t.Errorf("Error() with no cause = %q, want %q", got, want)
	}
}
