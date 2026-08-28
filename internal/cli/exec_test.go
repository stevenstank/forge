package cli

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/runtime"
)

// `forge exec` at the CLI layer (FR-6.2).
//
// Per SSOT §13.6 the behaviour of exec — joining namespaces, resolving the
// command inside the container, propagating its status — is internal/runtime's
// and is tested there, and the parts of it that need a live container are the
// privileged suite's. What is testable here is everything the command decides
// before a container is touched: which argument shapes it refuses, where flag
// parsing stops, how repeated -env accumulates, and which failures are the
// user's fault and so must exit 1 rather than 2.

// tempEnv returns an Env whose Runner-backed directories are temporary, so a
// command that gets as far as building a Runner touches nothing real.
func tempEnv(t *testing.T) (*Env, *strings.Builder, *strings.Builder) {
	t.Helper()

	dir := t.TempDir()
	var stdout, stderr strings.Builder

	return &Env{
		Opts: Options{
			StateDir:  filepath.Join(dir, "state"),
			Root:      filepath.Join(dir, "containers"),
			ImageRoot: filepath.Join(dir, "images"),
		},
		Logger: logging.New(&stderr, slog.LevelError),
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: &stderr,
	}, &stdout, &stderr
}

// TestExecRejectsBadArguments covers every refusal exec makes before it builds
// a Runner, which is all of its argument handling.
func TestExecRejectsBadArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "nothing at all", args: nil},
		{name: "a container but no command", args: []string{"7f3c9a1b2d04"}},
		{name: "an unknown flag", args: []string{"-nope", "7f3c9a1b2d04", "/bin/ls"}},
		{name: "a flag with no value", args: []string{"-workdir"}},
		{name: "an empty -env entry", args: []string{"-env", "", "7f3c9a1b2d04", "/bin/ls"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env, stdout, _ := tempEnv(t)

			err := execExec(t.Context(), env, tc.args)
			if !errors.Is(err, ErrUsage) {
				t.Fatalf("exec %v = %v, want ErrUsage", tc.args, err)
			}
			if stdout.Len() != 0 {
				t.Errorf("a refused command wrote to stdout: %q", stdout)
			}
		})
	}
}

// TestExecPrintsHelp checks that -h is not treated as a usage error and that
// the help names the flags a user needs.
func TestExecPrintsHelp(t *testing.T) {
	t.Parallel()

	env, stdout, stderr := tempEnv(t)

	if err := execExec(t.Context(), env, []string{"-h"}); err != nil {
		t.Fatalf("exec -h = %v, want nil", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("help went to stdout: %q", stdout)
	}

	help := stderr.String()
	for _, want := range []string{"forge exec", "-workdir", "-env", "container-id"} {
		if !strings.Contains(help, want) {
			t.Errorf("exec -h does not mention %q:\n%s", want, help)
		}
	}
}

// TestExecUsageIsPrintedWithTheRefusal makes the failure legible: a user who
// typed too few arguments gets the usage as well as the error.
func TestExecUsageIsPrintedWithTheRefusal(t *testing.T) {
	t.Parallel()

	env, _, stderr := tempEnv(t)

	if err := execExec(t.Context(), env, []string{"7f3c9a1b2d04"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("exec with no command = %v, want ErrUsage", err)
	}
	if !strings.Contains(stderr.String(), "forge exec") {
		t.Errorf("the refusal printed no usage:\n%s", stderr)
	}
}

// TestExecFlagsStopAtTheContainerID pins the behaviour the help promises: a
// flag after the container id belongs to the command, not to forge.
func TestExecFlagsStopAtTheContainerID(t *testing.T) {
	t.Parallel()

	fs, local := newExecFlagSet()
	args := []string{"-workdir", "/tmp", "-env", "A=1", "-env", "B=2", "7f3c9a1b2d04", "ls", "-l", "-workdir"}

	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse(%v) = %v", args, err)
	}

	if local.workdir != "/tmp" {
		t.Errorf("workdir = %q, want /tmp", local.workdir)
	}
	if got, want := strings.Join(local.env, ","), "A=1,B=2"; got != want {
		t.Errorf("env = %q, want %q", got, want)
	}

	want := []string{"7f3c9a1b2d04", "ls", "-l", "-workdir"}
	got := fs.Args()
	if len(got) != len(want) {
		t.Fatalf("positional = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("positional = %v, want %v", got, want)
		}
	}
}

// TestEnvListAccumulates covers the flag.Value the -env flag is built on.
func TestEnvListAccumulates(t *testing.T) {
	t.Parallel()

	var list envList

	if got := list.String(); got != "0 variables" {
		t.Errorf("empty String() = %q, want %q", got, "0 variables")
	}
	if err := list.Set(""); err == nil {
		t.Error("Set(\"\") = nil, want an error")
	}
	if err := list.Set("DEBUG=1"); err != nil {
		t.Fatalf("Set(DEBUG=1) = %v", err)
	}
	// A value with no "=" is not rejected here on purpose: the environment is
	// the container's to interpret, and only an empty entry is meaningless.
	if err := list.Set("BARE"); err != nil {
		t.Fatalf("Set(BARE) = %v", err)
	}
	if len(list) != 2 || list[0] != "DEBUG=1" || list[1] != "BARE" {
		t.Errorf("list = %v, want [DEBUG=1 BARE]", list)
	}
	if got := list.String(); got != "2 variables" {
		t.Errorf("String() = %q, want %q", got, "2 variables")
	}

	// The flag package calls String on the zero value while printing defaults,
	// which for a *envList is a nil pointer.
	var nilList *envList
	if got := nilList.String(); got != "" {
		t.Errorf("nil String() = %q, want empty", got)
	}
}

// TestExecOnAnUnknownContainerExitsOne drives the whole command — flag
// parsing, Runner construction against a temporary state directory, the call
// into the runtime, and the classification of what came back.
//
// It needs no root because it never reaches a container: an id with no record
// is refused by the state store.
func TestExecOnAnUnknownContainerExitsOne(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "a well-formed id with no record", args: []string{"7f3c9a1b2d04", "/bin/echo", "hello"}},
		{name: "an id that is not one", args: []string{"../etc", "/bin/echo"}},
		{name: "with flags of its own", args: []string{"-workdir", "/tmp", "-env", "A=1", "7f3c9a1b2d04", "/bin/echo"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env, stdout, _ := tempEnv(t)

			err := execExec(t.Context(), env, tc.args)

			var exitErr *ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("exec %v = %v, want an *ExitError", tc.args, err)
			}
			if exitErr.Code != ExitUsage {
				t.Errorf("exit code = %d, want %d", exitErr.Code, ExitUsage)
			}
			if !errors.Is(err, runtime.ErrNotFound) {
				t.Errorf("exec %v = %v, want it to wrap ErrNotFound", tc.args, err)
			}
			if stdout.Len() != 0 {
				t.Errorf("a failed exec wrote to stdout: %q", stdout)
			}
		})
	}
}

// TestExecExitCodeReachesTheProcessStatus checks the wiring from the command
// through the dispatcher: a user error from exec is exit 1, not exit 2, and
// forge's usage is not dumped over the one line that explains it.
func TestExecExitCodeReachesTheProcessStatus(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var stdout, stderr strings.Builder

	code := Run(t.Context(), []string{
		"-state-dir", filepath.Join(dir, "state"),
		"-root", filepath.Join(dir, "containers"),
		"-image-root", filepath.Join(dir, "images"),
		"exec", "7f3c9a1b2d04", "/bin/echo",
	}, strings.NewReader(""), &stdout, &stderr)

	if code != ExitUsage {
		t.Errorf("exit code = %d, want %d\nstderr: %s", code, ExitUsage, stderr.String())
	}
	if strings.Contains(stderr.String(), "Global flags:") {
		t.Errorf("an unknown container printed forge's whole usage:\n%s", stderr.String())
	}
}

// TestIsExecUserError pins the classification exec adds on top of the shared
// one: a command the container does not have is the user's mistake, a failure
// to join a namespace is Forge's.
func TestIsExecUserError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "unknown container", err: runtime.ErrNotFound, want: true},
		{name: "container not running", err: runtime.ErrNotRunning, want: true},
		{name: "container still running", err: runtime.ErrRunning, want: true},
		{name: "no command given", err: runtime.ErrNoCommand, want: true},
		{name: "command not in the container", err: runtime.ErrCommandNotFound, want: true},
		{name: "wrapped", err: errors.Join(errors.New("context"), runtime.ErrCommandNotFound), want: true},
		{name: "anything else", err: errors.New("setns: operation not permitted"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isExecUserError(tc.err); got != tc.want {
				t.Errorf("isExecUserError(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// TestExecIsRegistered checks the verb is reachable from the dispatcher under
// the name the documentation uses.
func TestExecIsRegistered(t *testing.T) {
	t.Parallel()

	a := &app{commands: commands()}

	cmd := a.lookup("exec")
	if cmd == nil {
		t.Fatal("no exec command is registered")
	}
	if cmd.Hidden {
		t.Error("exec is hidden; users are told to type it")
	}
	if cmd.Summary == "" {
		t.Error("exec has no summary, so forge -h cannot describe it")
	}
}
