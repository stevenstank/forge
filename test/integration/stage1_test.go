//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/runtime"
)

// These tests exercise Stage 1 against the real kernel. The container's binary
// is always this test binary re-executed in a helper mode, which keeps the
// tests independent of whatever binaries the host happens to have.

const (
	helperEnv     = "FORGE_INTEGRATION_HELPER"
	helperDirEnv  = "FORGE_INTEGRATION_DIR"
	helperCodeEnv = "FORGE_INTEGRATION_EXIT_CODE"
)

// readyMarker is what a long-running helper prints once it is up, so tests can
// synchronise on it instead of sleeping (SSOT §7).
const readyMarker = "ready"

func TestMain(m *testing.M) {
	// This binary plays the role cmd/forge plays in production: Forge starts a
	// container by re-executing the *current* executable as its container init,
	// so the test binary must route that command to runtime.Init exactly as the
	// real CLI does. Without this it would re-enter the test suite inside every
	// container it starts. See ADR-0008.
	if runtime.IsInitCommand(os.Args) {
		if err := runtime.Init(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(runtime.InitExitCode)
		}
		os.Exit(runtime.InitExitCode)
	}

	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())

	case "print-pid":
		// getpid(2), not /proc: inside a PID namespace this is the container's
		// own view of itself.
		fmt.Println(os.Getpid())
		os.Exit(0)

	case "print-pid-ns":
		os.Exit(printNamespace("pid"))

	case "print-mount-ns":
		os.Exit(printNamespace("mnt"))

	case "print-uts-ns":
		os.Exit(printNamespace("uts"))

	case "print-hostname":
		name, err := os.Hostname()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(name)
		os.Exit(0)

	case "mount-tmpfs":
		os.Exit(mountTmpfsHelper())

	case "exit":
		code, err := strconv.Atoi(os.Getenv(helperCodeEnv))
		if err != nil {
			os.Exit(254)
		}
		os.Exit(code)

	case "sleep-forever":
		fmt.Println(readyMarker)
		select {}

	default:
		os.Exit(252)
	}
}

// printNamespace writes the inode identity of one of this process's namespaces,
// in the "pid:[4026531836]" form the kernel uses.
func printNamespace(kind string) int {
	link, err := os.Readlink("/proc/self/ns/" + kind)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(link)
	return 0
}

// mountTmpfsHelper mounts a tmpfs inside the container and drops a marker file
// into it. If the container's mount namespace is properly isolated, neither the
// mount nor the marker is ever visible to the host.
func mountTmpfsHelper() int {
	target := os.Getenv(helperDirEnv)
	if target == "" {
		return 251
	}
	if err := syscall.Mount("tmpfs", target, "tmpfs", 0, ""); err != nil {
		fmt.Fprintln(os.Stderr, "mount:", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(target, "marker"), []byte("container"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		return 1
	}
	return 0
}

// requireRoot skips a test that cannot run without CAP_SYS_ADMIN.
func requireRoot(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("stage 1 integration tests need root: run `sudo -E make test-integration`")
	}
}

// result is everything a test wants to know about a finished container.
type result struct {
	status process.Status
	stdout string
	stderr string
}

// runContainer runs spec to completion and returns its output. A container that
// fails to start is a test failure; a container that exits non-zero is not.
func runContainer(ctx context.Context, t *testing.T, spec runtime.Spec) result {
	t.Helper()

	var stdout, stderr, logs bytes.Buffer
	spec.Stdout = &stdout
	spec.Stderr = &stderr

	status, err := runtime.NewRunner(logging.New(&logs, slog.LevelDebug)).Run(ctx, spec)
	t.Logf("forge log:\n%s", logs.String())
	if err != nil {
		t.Fatalf("Run() = %v\ncontainer stderr: %s", err, stderr.String())
	}

	return result{
		status: status,
		stdout: strings.TrimSpace(stdout.String()),
		stderr: strings.TrimSpace(stderr.String()),
	}
}

// helperSpec returns a Spec that runs this test binary in the given mode.
func helperSpec(t *testing.T, mode string, env ...string) runtime.Spec {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	return runtime.Spec{
		Command: []string{exe},
		Env:     append([]string{helperEnv + "=" + mode}, env...),
	}
}

// hostNamespace returns the inode identity of one of the test process's
// namespaces, for comparison against a container's.
func hostNamespace(t *testing.T, kind string) string {
	t.Helper()

	link, err := os.Readlink("/proc/self/ns/" + kind)
	if err != nil {
		t.Fatalf("reading host %s namespace: %v", kind, err)
	}
	return link
}

// --- FR-1.1: process isolation via CLONE_NEWPID ---------------------------

// TestContainerInitIsPIDOne is the headline Stage 1 behaviour: the container's
// process believes it is PID 1.
func TestContainerInitIsPIDOne(t *testing.T) {
	requireRoot(t)

	got := runContainer(t.Context(), t, helperSpec(t, "print-pid"))

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if got.stdout != "1" {
		t.Errorf("container reported pid %q, want \"1\"", got.stdout)
	}
}

// TestContainerHasItsOwnPIDNamespace confirms the process really was placed in
// a fresh namespace rather than merely reporting a low PID.
func TestContainerHasItsOwnPIDNamespace(t *testing.T) {
	requireRoot(t)

	host := hostNamespace(t, "pid")
	got := runContainer(t.Context(), t, helperSpec(t, "print-pid-ns"))

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if got.stdout == host {
		t.Errorf("container pid namespace %q is the host's; want a distinct namespace", got.stdout)
	}
	if !strings.HasPrefix(got.stdout, "pid:[") {
		t.Errorf("container pid namespace = %q, want a pid:[inode] identity", got.stdout)
	}
}

// --- FR-1.2: hostname isolation via CLONE_NEWUTS --------------------------

// TestHostnameIsIsolated covers FR-1.2 in both directions: the container sees
// the hostname Forge set, and the host's own hostname is untouched.
func TestHostnameIsIsolated(t *testing.T) {
	requireRoot(t)

	hostBefore, err := os.Hostname()
	if err != nil {
		t.Fatalf("reading host hostname: %v", err)
	}

	const containerHostname = "forge-stage1-uts"
	spec := helperSpec(t, "print-hostname")
	spec.Hostname = containerHostname

	got := runContainer(t.Context(), t, spec)

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if got.stdout != containerHostname {
		t.Errorf("container hostname = %q, want %q", got.stdout, containerHostname)
	}

	hostAfter, err := os.Hostname()
	if err != nil {
		t.Fatalf("re-reading host hostname: %v", err)
	}
	if hostAfter != hostBefore {
		t.Errorf("host hostname changed from %q to %q; the UTS namespace leaked", hostBefore, hostAfter)
	}
}

// TestContainerHasItsOwnUTSNamespace confirms a fresh UTS namespace was created.
func TestContainerHasItsOwnUTSNamespace(t *testing.T) {
	requireRoot(t)

	host := hostNamespace(t, "uts")
	got := runContainer(t.Context(), t, helperSpec(t, "print-uts-ns"))

	if got.stdout == host {
		t.Errorf("container uts namespace %q is the host's; want a distinct namespace", got.stdout)
	}
}

// TestDefaultHostnameIsTheContainerID documents the Docker-like default Forge
// applies when the caller does not choose a hostname.
func TestDefaultHostnameIsTheContainerID(t *testing.T) {
	requireRoot(t)

	got := runContainer(t.Context(), t, helperSpec(t, "print-hostname"))

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}

	idPattern := regexp.MustCompile(`^[0-9a-f]{12}$`)
	if !idPattern.MatchString(got.stdout) {
		t.Errorf("default hostname = %q, want a 12-character hex container ID", got.stdout)
	}
}

// --- FR-1.3: mount isolation via CLONE_NEWNS ------------------------------

// TestMountsDoNotPropagateToHost is the direct test of FR-1.3. It fails if the
// container's mount namespace inherits shared propagation from the host, which
// is the default on any systemd system.
func TestMountsDoNotPropagateToHost(t *testing.T) {
	requireRoot(t)

	target := t.TempDir()

	spec := helperSpec(t, "mount-tmpfs", helperDirEnv+"="+target)
	got := runContainer(t.Context(), t, spec)

	if got.status.Code != 0 {
		t.Fatalf("container failed to mount tmpfs: exit %d (stderr: %s)", got.status.Code, got.stderr)
	}

	// The marker was written into a tmpfs that only ever existed inside the
	// container's mount namespace, so the host directory must still be empty.
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("reading %s on the host: %v", target, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("host sees %v in %s; the container's mount propagated", names, target)
	}

	// And the host's mount table must never have learned about it.
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("reading host mountinfo: %v", err)
	}
	if strings.Contains(string(mounts), target) {
		t.Errorf("host mount table contains %s; the container's mount propagated", target)
	}
}

// TestContainerHasItsOwnMountNamespace confirms a fresh mount namespace exists.
func TestContainerHasItsOwnMountNamespace(t *testing.T) {
	requireRoot(t)

	host := hostNamespace(t, "mnt")
	got := runContainer(t.Context(), t, helperSpec(t, "print-mount-ns"))

	if got.stdout == host {
		t.Errorf("container mount namespace %q is the host's; want a distinct namespace", got.stdout)
	}
}

// --- FR-1.4: lifecycle tracking and exit codes ----------------------------

// TestExitCodeIsReported covers FR-1.4 across success and failure statuses.
func TestExitCodeIsReported(t *testing.T) {
	requireRoot(t)

	for _, want := range []int{0, 1, 7, 42, 255} {
		t.Run(strconv.Itoa(want), func(t *testing.T) {
			spec := helperSpec(t, "exit", helperCodeEnv+"="+strconv.Itoa(want))
			got := runContainer(t.Context(), t, spec)

			if got.status.Code != want {
				t.Errorf("exit code = %d, want %d (stderr: %s)", got.status.Code, want, got.stderr)
			}
			if got.status.Signaled() {
				t.Errorf("Signaled() = true, want false for a normal exit")
			}
		})
	}
}

// TestMissingBinaryReportsInitFailure distinguishes "the container could not
// start" from "the container ran and exited", per ADR-0008.
func TestMissingBinaryReportsInitFailure(t *testing.T) {
	requireRoot(t)

	spec := runtime.Spec{Command: []string{"/nonexistent/forge-stage1-binary"}}
	got := runContainer(t.Context(), t, spec)

	if got.status.Code != runtime.InitExitCode {
		t.Errorf("exit code = %d, want %d for a container that could not start", got.status.Code, runtime.InitExitCode)
	}
	if !strings.Contains(got.stderr, "/nonexistent/forge-stage1-binary") {
		t.Errorf("container stderr %q does not name the missing binary", got.stderr)
	}
}

// --- FR-1.5: cleanup ------------------------------------------------------

// TestCancellingTheContextKillsTheContainer covers FR-1.5 and PRD NFR-5: an
// interrupted run must not leave a container process behind.
func TestCancellingTheContextKillsTheContainer(t *testing.T) {
	requireRoot(t)

	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close()

	spec := helperSpec(t, "sleep-forever")
	spec.Stdout = stdoutWriter

	var logs bytes.Buffer

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type outcome struct {
		status process.Status
		err    error
	}
	done := make(chan outcome, 1)

	go func() {
		status, err := runtime.NewRunner(logging.New(&logs, slog.LevelDebug)).Run(ctx, spec)
		// Unblock any reader still waiting on the container's stdout.
		_ = stdoutWriter.Close()
		done <- outcome{status, err}
	}()

	ready := make(chan struct{})
	go func() {
		line, err := bufio.NewReader(stdoutReader).ReadString('\n')
		if err == nil && strings.TrimSpace(line) == readyMarker {
			close(ready)
		}
	}()

	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("container never signalled readiness")
	}

	cancel()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Run() = %v (log: %s)", got.err, logs.String())
		}
		if got.status.Signal != syscall.SIGKILL {
			t.Errorf("Signal = %v, want SIGKILL after cancellation", got.status.Signal)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after its context was cancelled; the container was abandoned")
	}
}

// TestNoHostResidueAfterRun checks the FR-1.5 promise that a completed
// container leaves the host as it found it.
func TestNoHostResidueAfterRun(t *testing.T) {
	requireRoot(t)

	hostnameBefore, err := os.Hostname()
	if err != nil {
		t.Fatalf("reading host hostname: %v", err)
	}
	mountsBefore := hostMountCount(t)

	spec := helperSpec(t, "mount-tmpfs", helperDirEnv+"="+t.TempDir())
	spec.Hostname = "forge-residue-check"

	if got := runContainer(t.Context(), t, spec); got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}

	hostnameAfter, err := os.Hostname()
	if err != nil {
		t.Fatalf("re-reading host hostname: %v", err)
	}
	if hostnameAfter != hostnameBefore {
		t.Errorf("host hostname changed from %q to %q", hostnameBefore, hostnameAfter)
	}
	if after := hostMountCount(t); after != mountsBefore {
		t.Errorf("host mount count changed from %d to %d; a mount leaked", mountsBefore, after)
	}
}

// hostMountCount returns the number of entries in the host's mount table.
func hostMountCount(t *testing.T) int {
	t.Helper()

	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("reading host mountinfo: %v", err)
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}
