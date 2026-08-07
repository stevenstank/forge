package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/runtime"
)

// The Stage 6 verbs at the CLI layer.
//
// Per SSOT §13.6 there is no behaviour here to test beyond argument handling
// and formatting — stopping a container is internal/runtime's, and is tested
// there. What these cover is the part a user actually types at, and the part
// they read: the arguments a command refuses before it touches anything, and
// the table it prints when it does not.

// testEnv returns an Env writing into buffers the caller can inspect.
func testEnv() (*Env, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer

	env := &Env{
		Opts:   Options{StateDir: DefaultStateDir, Root: DefaultRoot, ImageRoot: DefaultImageRoot},
		Logger: logging.New(&stderr, slog.LevelError),
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: &stderr,
	}

	return env, &stdout, &stderr
}

// TestStage6CommandsRejectBadArguments covers every refusal that happens
// before a Runner is built — which is every refusal these tests can make
// without root, and deliberately all of the argument handling.
func TestStage6CommandsRejectBadArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		exec func(context.Context, *Env, []string) error
		args []string
	}{
		{name: "ps with an argument", exec: execPs, args: []string{"7f3c9a1b2d04"}},
		{name: "ps with an unknown flag", exec: execPs, args: []string{"-nope"}},
		{name: "stop with no container", exec: execStop, args: nil},
		{name: "stop with a negative timeout", exec: execStop, args: []string{"-t", "-1", "7f3c9a1b2d04"}},
		{name: "stop with an unknown flag", exec: execStop, args: []string{"-nope", "7f3c9a1b2d04"}},
		{name: "rm with no container", exec: execRemove, args: nil},
		{name: "rm with an unknown flag", exec: execRemove, args: []string{"-nope", "7f3c9a1b2d04"}},
		{name: "logs with no container", exec: execLogs, args: nil},
		{name: "logs with two containers", exec: execLogs, args: []string{"7f3c9a1b2d04", "0000deadbeef"}},
		{name: "logs with a negative tail", exec: execLogs, args: []string{"-n", "-1", "7f3c9a1b2d04"}},
		{name: "logs with an unknown flag", exec: execLogs, args: []string{"-nope", "7f3c9a1b2d04"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env, stdout, _ := testEnv()

			err := tc.exec(t.Context(), env, tc.args)
			if !errors.Is(err, ErrUsage) {
				t.Fatalf("%s = %v, want ErrUsage", tc.name, err)
			}
			if stdout.Len() != 0 {
				t.Errorf("a refused command wrote to stdout: %q", stdout)
			}
		})
	}
}

// TestStage6CommandsPrintHelp covers -h for each verb, which must not be
// treated as a usage error.
func TestStage6CommandsPrintHelp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		exec func(context.Context, *Env, []string) error
		want string
	}{
		{name: "ps", exec: execPs, want: "forge ps"},
		{name: "stop", exec: execStop, want: "forge stop"},
		{name: "rm", exec: execRemove, want: "forge rm"},
		{name: "logs", exec: execLogs, want: "forge logs"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env, _, stderr := testEnv()

			if err := tc.exec(t.Context(), env, []string{"-h"}); err != nil {
				t.Fatalf("%s -h = %v, want nil", tc.name, err)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%s -h did not print its usage:\n%s", tc.name, stderr)
			}
		})
	}
}

// TestStage6CommandsEndToEnd drives the real binary path — global flags,
// dispatch, a real state store, real formatting — against a temporary state
// directory.
//
// None of it needs root, because none of these three verbs starts a container:
// they read and remove what a `forge run` left behind, and a record written by
// hand is indistinguishable from one it wrote.
func TestStage6CommandsEndToEnd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	globals := []string{"-state-dir", dir, "-root", filepath.Join(dir, "containers")}

	forge := func(args ...string) (int, string, string) {
		t.Helper()

		var stdout, stderr bytes.Buffer
		code := Run(t.Context(), append(globals, args...), strings.NewReader(""), &stdout, &stderr)

		return code, stdout.String(), stderr.String()
	}

	// An empty store is a host with no containers, not an error.
	code, stdout, _ := forge("ps")
	if code != ExitOK {
		t.Fatalf("ps on an empty store = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(stdout, "CONTAINER ID") || strings.Count(stdout, "\n") != 1 {
		t.Errorf("ps printed more than a header:\n%s", stdout)
	}

	// An unknown container exits 1 with one line, and does not print the whole
	// of forge's usage: nothing about the command line was wrong.
	for _, verb := range []string{"stop", "rm"} {
		code, stdout, stderr := forge(verb, "0000deadbeef")
		if code != ExitUsage {
			t.Errorf("%s of an unknown container = %d, want %d", verb, code, ExitUsage)
		}
		if !strings.Contains(stderr, "no such container") {
			t.Errorf("%s did not report the missing container:\n%s", verb, stderr)
		}
		if strings.Contains(stderr, "Global flags:") {
			t.Errorf("%s of an unknown container printed forge's usage:\n%s", verb, stderr)
		}
		if stdout != "" {
			t.Errorf("%s wrote %q to stdout on failure", verb, stdout)
		}
	}

	// A container that has finished, as a `forge run -keep` would have left it.
	const id = "7f3c9a1b2d04"
	writeRecord(t, dir, id)
	rootfs := filepath.Join(dir, "containers", id)
	logs := filepath.Join(dir, "logs", id+".log")
	mustMkdir(t, filepath.Join(rootfs, "rootfs"))
	mustMkdir(t, filepath.Dir(logs))
	captured := `{"t":"2026-08-07T18:22:03Z","s":"stdout","m":"hello\n"}` + "\n" +
		`{"t":"2026-08-07T18:22:04Z","s":"stderr","m":"oh no\n"}` + "\n"
	if err := os.WriteFile(logs, []byte(captured), 0o600); err != nil {
		t.Fatal(err)
	}

	// It is finished, so ps hides it and ps -a shows it.
	if _, stdout, _ := forge("ps"); strings.Contains(stdout, id) {
		t.Errorf("ps listed a finished container:\n%s", stdout)
	}

	code, stdout, _ = forge("ps", "-a")
	if code != ExitOK {
		t.Fatalf("ps -a = %d, want %d", code, ExitOK)
	}
	for _, want := range []string{id, "alpine:3.20", "/bin/sh", "exited (137)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("ps -a is missing %q:\n%s", want, stdout)
		}
	}

	// -q is what makes `forge rm $(forge ps -a -q)` work, so it is exactly the
	// ID and nothing else.
	if _, stdout, _ := forge("ps", "-a", "-q"); stdout != id+"\n" {
		t.Errorf("ps -a -q = %q, want just the id", stdout)
	}

	// Its output comes back out, with the two streams kept apart so that
	// redirecting one gives you that stream alone.
	code, stdout, stderr := forge("logs", id)
	if code != ExitOK {
		t.Fatalf("logs = %d, want %d", code, ExitOK)
	}
	if stdout != "hello\n" {
		t.Errorf("logs stdout = %q, want the container's stdout", stdout)
	}
	if !strings.Contains(stderr, "oh no\n") {
		t.Errorf("logs stderr = %q, want the container's stderr", stderr)
	}

	// Removing it takes the filesystem, the logs and the record, and prints
	// the ID for the next command in the pipe (SSOT §9).
	code, stdout, _ = forge("rm", id)
	if code != ExitOK {
		t.Fatalf("rm = %d, want %d", code, ExitOK)
	}
	if stdout != id+"\n" {
		t.Errorf("rm printed %q, want the container id", stdout)
	}

	for _, path := range []string{rootfs, logs, filepath.Join(dir, "state", "containers", id)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("rm left %q behind: %v", path, err)
		}
	}
	if _, stdout, _ := forge("ps", "-a"); strings.Contains(stdout, id) {
		t.Errorf("ps -a still lists the removed container:\n%s", stdout)
	}
}

// TestPsRefusesToHideContainersBehindABadRecord covers the case ps exists for:
// something is wrong on the host. One unreadable record must be reported
// without costing the user sight of the containers that are fine.
func TestPsRefusesToHideContainersBehindABadRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeRecord(t, dir, "7f3c9a1b2d04")

	bad := filepath.Join(dir, "state", "containers", "0000deadbeef")
	mustMkdir(t, bad)
	if err := os.WriteFile(filepath.Join(bad, "metadata.json"), []byte(`{"schema":1,`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run(t.Context(),
		[]string{"-state-dir", dir, "-root", filepath.Join(dir, "containers"), "ps", "-a"},
		strings.NewReader(""), &stdout, &stderr)

	if code != ExitOK {
		t.Fatalf("ps -a = %d, want %d: a corrupt record must not fail the listing", code, ExitOK)
	}
	if !strings.Contains(stdout.String(), "7f3c9a1b2d04") {
		t.Errorf("the readable container was not listed:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "0000deadbeef") {
		t.Errorf("the unreadable record was not reported:\n%s", stderr.String())
	}
}

// writeRecord writes the metadata of a container that ran and exited, standing
// in for the `forge run -keep` that would have written it.
func writeRecord(t *testing.T, stateDir, id string) {
	t.Helper()

	dir := filepath.Join(stateDir, "state", "containers", id)
	mustMkdir(t, dir)

	created := time.Now().UTC().Add(-8 * time.Minute).Format(time.RFC3339Nano)
	finished := time.Now().UTC().Format(time.RFC3339Nano)
	record := fmt.Sprintf(`{
  "schema": 1,
  "id": %q,
  "image": "alpine:3.20",
  "command": ["/bin/sh"],
  "pid": 4242,
  "status": "exited",
  "exit_code": 137,
  "created_at": %q,
  "finished_at": %q,
  "rootfs_path": %q,
  "network_mode": "bridge"
}
`, id, created, finished, filepath.Join(stateDir, "containers", id, "rootfs"))

	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestWriteContainerTable pins the columns FR-6.1 asks for, and the shape a
// user's eye depends on.
func TestWriteContainerTable(t *testing.T) {
	t.Parallel()

	code := 137
	containers := []runtime.Container{
		{
			ID:      "7f3c9a1b2d04",
			Image:   "alpine:3.20",
			Command: []string{"/bin/sh", "-c", "while true; do date; sleep 1; done"},
			Status:  "running",
			PID:     41120,
			Created: time.Now().Add(-90 * time.Second),
		},
		{
			ID:       "0000deadbeef",
			Command:  []string{"/bin/true"},
			Status:   "stopped",
			PID:      41200,
			Created:  time.Now().Add(-3 * time.Hour),
			ExitCode: &code,
		},
	}

	var out bytes.Buffer
	writeContainerTable(&out, containers)
	got := out.String()

	for _, want := range []string{
		"CONTAINER ID", "IMAGE", "COMMAND", "STATUS", "CREATED", "PID",
		"7f3c9a1b2d04", "alpine:3.20", "running", "41120", "1 minute ago",
		"0000deadbeef", "stopped (137)", "3 hours ago",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("table is missing %q:\n%s", want, got)
		}
	}

	// A stopped container has no PID worth printing: the number in the record
	// belongs to a process that no longer exists, and printing it invites
	// somebody to use it.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 {
		t.Fatalf("table has %d lines, want a header and two containers:\n%s", len(lines), got)
	}
	if strings.Contains(lines[2], "41200") {
		t.Errorf("a stopped container printed its stale PID:\n%s", lines[2])
	}

	// The long command is cut rather than pushing the columns after it off
	// the screen.
	if strings.Contains(got, "sleep 1; done") {
		t.Errorf("a long command was not truncated:\n%s", got)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in    string
		limit int
		want  string
	}{
		{in: "", limit: 10, want: "-"},
		{in: "/bin/sh", limit: 10, want: "/bin/sh"},
		{in: "0123456789", limit: 10, want: "0123456789"},
		{in: "01234567890", limit: 10, want: "012345678…"},
		// Multi-byte input is cut by character, not by byte, so the result is
		// never invalid UTF-8 in a terminal.
		{in: "ααααααααααα", limit: 10, want: "ααααααααα…"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()

			if got := truncate(tc.in, tc.limit); got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
			}
		})
	}
}

func TestHumaniseAge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		d    time.Duration
		want string
	}{
		{d: -time.Second, want: "just now"},
		{d: 5 * time.Second, want: "5 seconds ago"},
		{d: 90 * time.Second, want: "1 minute ago"},
		{d: 5 * time.Minute, want: "5 minutes ago"},
		{d: 90 * time.Minute, want: "1 hour ago"},
		{d: 5 * time.Hour, want: "5 hours ago"},
		{d: 30 * time.Hour, want: "1 day ago"},
		{d: 72 * time.Hour, want: "3 days ago"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()

			if got := humaniseAge(tc.d); got != tc.want {
				t.Errorf("humaniseAge(%s) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

// TestIsContainerUserError pins which Stage 6 failures exit 1 rather than 2
// (SSOT §9): the ones the user can fix from what they typed.
func TestIsContainerUserError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "unknown container", err: runtime.ErrNotFound, want: true},
		{name: "still running", err: runtime.ErrRunning, want: true},
		{name: "not running", err: runtime.ErrNotRunning, want: true},
		{name: "wrapped", err: errors.Join(errors.New("x"), runtime.ErrNotFound), want: true},
		// A container that survived SIGKILL is a kernel problem, and telling
		// the user to check their command line would send them looking in
		// entirely the wrong place.
		{name: "unkillable container", err: runtime.ErrStopFailed, want: false},
		{name: "something else", err: errors.New("disk on fire"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isContainerUserError(tc.err); got != tc.want {
				t.Errorf("isContainerUserError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestRunKeepFlag covers the one Spec field Stage 6 adds to `forge run`.
func TestRunKeepFlag(t *testing.T) {
	t.Parallel()

	spec, err := parseRunSpec([]string{"/bin/true"})
	if err != nil {
		t.Fatalf("parseRunSpec() = %v", err)
	}
	if spec.Keep {
		t.Error("Keep is set without -keep; an attached run must leave nothing behind by default")
	}

	spec, err = parseRunSpec([]string{"-keep", "/bin/true"})
	if err != nil {
		t.Fatalf("parseRunSpec(-keep) = %v", err)
	}
	if !spec.Keep {
		t.Error("-keep did not set Keep")
	}
}
