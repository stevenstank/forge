package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The process entry point.
//
// main is four lines of wiring — a cancellable context, a call into
// internal/cli, an exit code — and none of it is reachable in-process, because
// it ends in os.Exit. So it is driven the only way it can be: this test binary
// re-executes itself with the arguments under test and inspects what came out.
//
// Nothing here needs root. Every invocation below is refused before Forge
// touches a namespace, which is exactly the set of paths a user meets when they
// have made a mistake — and the set whose exit codes SSOT §9 pins.

const helperEnv = "FORGE_MAIN_TEST_ARGS"

func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv(helperEnv); ok {
		os.Args = append([]string{"forge"}, strings.Fields(args)...)
		main()
		return
	}

	os.Exit(m.Run())
}

// runForge re-executes this test binary as forge, with args.
func runForge(t *testing.T, args string) (stdout, stderr string, code int) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), helperEnv+"="+args, "GOCOVERDIR="+t.TempDir())

	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	err = cmd.Run()

	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("running forge %q: %v", args, err)
	}

	return out.String(), errOut.String(), code
}

// TestMainExitCodes pins the mapping SSOT §9 defines, through the real entry
// point rather than through internal/cli's dispatcher.
func TestMainExitCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args string
		want int
	}{
		{name: "help", args: "-h", want: 0},
		{name: "no command", args: "", want: 1},
		{name: "an unknown command", args: "nosuchverb", want: 1},
		{name: "a bad global flag", args: "-log-level nope ps", want: 1},
		{name: "a relative state directory", args: "-state-dir relative ps", want: 1},
		{name: "a relative root", args: "-root relative ps", want: 1},
		{name: "a relative image root", args: "-image-root relative ps", want: 1},
		{name: "run with no command", args: "run", want: 1},
		{name: "exec with no command", args: "exec 7f3c9a1b2d04", want: 1},
		{name: "stop with no container", args: "stop", want: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, stderr, code := runForge(t, tc.args)
			if code != tc.want {
				t.Errorf("forge %q exited %d, want %d\nstderr: %s", tc.args, code, tc.want, stderr)
			}
		})
	}
}

// TestMainHelpGoesToStdout checks the one case where output is the answer
// rather than a diagnostic: `forge -h` is a successful command, so its help
// belongs on stdout where it can be piped.
func TestMainHelpGoesToStdout(t *testing.T) {
	t.Parallel()

	stdout, _, code := runForge(t, "-h")
	if code != 0 {
		t.Fatalf("forge -h exited %d, want 0", code)
	}
	for _, want := range []string{"Usage:", "Commands:", "run", "ps", "stop", "exec", "logs", "rm"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("forge -h did not mention %q:\n%s", want, stdout)
		}
	}
	// The internal re-exec entry point is not a verb users type.
	if strings.Contains(stdout, "__init") {
		t.Errorf("forge -h advertises the internal init command:\n%s", stdout)
	}
}

// TestMainErrorsGoToStderr checks that a failure is reported where a script
// redirecting stdout will still see it, and that stdout stays clean.
func TestMainErrorsGoToStderr(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := runForge(t, "nosuchverb")
	if code == 0 {
		t.Fatal("an unknown command exited 0")
	}
	if !strings.Contains(stderr, "nosuchverb") {
		t.Errorf("stderr does not name the unknown command:\n%s", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("a failed command wrote to stdout: %q", stdout)
	}
}
