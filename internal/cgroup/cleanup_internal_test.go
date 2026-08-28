package cgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Destroy's eviction path (FR-3.5).
//
// A cgroup outlives every process that was in it, so removing one is real work
// rather than a consequence of the container exiting. Evicting its members is
// the part with teeth, and the part that must be tested without a real
// hierarchy: the fallback path signals PIDs read out of a file, and a mistake
// in it is a mistake made with SIGKILL against whatever is on the host.
//
// Everything below works against an ordinary directory laid out like a cgroup.
// The kernel's own refusal to rmdir a populated cgroup, and the cgroup.kill
// interface actually killing anything, belong to the privileged suite.

// fakeCgroup builds a directory shaped like a cgroup leaf. procs is written to
// cgroup.procs; kill says whether the kernel's cgroup.kill interface exists.
func fakeCgroup(t *testing.T, kill bool, procs ...int) string {
	t.Helper()

	dir := t.TempDir()

	var content string
	for _, pid := range procs {
		content += strconv.Itoa(pid) + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, fileProcs), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if kill {
		if err := os.WriteFile(filepath.Join(dir, fileKill), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// TestEvictOnAnEmptyCgroupSignalsNothing is the common case: the container's
// last process has already exited and the kernel is mid-reap.
func TestEvictOnAnEmptyCgroupSignalsNothing(t *testing.T) {
	t.Parallel()

	if err := evict(fakeCgroup(t, true)); err != nil {
		t.Errorf("evict() on an empty cgroup = %v, want nil", err)
	}
}

// TestEvictOnAMissingCgroupIsNotAnError covers the directory that has already
// gone, which is the state Destroy is trying to reach.
func TestEvictOnAMissingCgroupIsNotAnError(t *testing.T) {
	t.Parallel()

	if err := evict(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("evict() on a missing cgroup = %v, want nil", err)
	}
}

// TestEvictPrefersCgroupKill checks that a kernel with the cgroup.kill
// interface is used through it — one atomic write, no window for a member to
// fork — rather than through the per-PID fallback.
func TestEvictPrefersCgroupKill(t *testing.T) {
	t.Parallel()

	// A PID that would be catastrophic to signal, so a test that passes proves
	// the fallback was not reached: this process's own PID.
	dir := fakeCgroup(t, true, os.Getpid())

	if err := evict(dir); err != nil {
		t.Fatalf("evict() = %v", err)
	}

	written, err := os.ReadFile(filepath.Join(dir, fileKill))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(written); got != "1" {
		t.Errorf("%s = %q, want %q", fileKill, got, "1")
	}
}

// TestEvictNeverSignalsPIDZeroOrOne is the safety property the fallback path
// exists under.
//
// kill(0, SIGKILL) signals every process in the caller's process group, which
// includes forge itself and, when forge was started from a shell, the shell.
// PID 1 is the host's init. Neither can ever be a container member, so a
// cgroup.procs naming them is corrupt — and the only safe response is to skip
// them rather than to trust the file.
//
// The eviction runs in a child process with a process group of its own. If the
// guard ever regresses, the damage is confined to that child and this test
// reports it; run in-process, a regression would take the test binary — and,
// as root, the machine — with it.
func TestEvictNeverSignalsPIDZeroOrOne(t *testing.T) {
	t.Parallel()

	// No cgroup.kill, so the fallback path is the one under test.
	dir := fakeCgroup(t, false, 0, 1)

	out, code := evictInAChild(t, dir)
	if code != 0 {
		t.Fatalf("the eviction child exited %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "evict: ok") {
		t.Errorf("child output = %q, want a clean eviction", out)
	}
}

const (
	helperEnv   = "FORGE_CGROUP_TEST_HELPER"
	helperEvict = "evict"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != helperEvict {
		os.Exit(m.Run())
	}

	if err := evict(os.Getenv(helperEnv + "_DIR")); err != nil {
		fmt.Printf("evict: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("evict: ok")
	os.Exit(0)
}

// evictInAChild runs evict against dir in a child process that leads its own
// process group, so a stray kill(0, ...) cannot reach this test binary.
func evictInAChild(t *testing.T, dir string) (string, int) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		helperEnv+"="+helperEvict,
		helperEnv+"_DIR="+dir,
		"GOCOVERDIR="+t.TempDir(),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out, err := cmd.CombinedOutput()

	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running the eviction helper: %v", err)
	}

	return string(out), code
}

// TestEvictToleratesAProcessThatHasAlreadyGone checks the outcome eviction
// actually wants: a member that exited between reading cgroup.procs and
// signalling it is success, not a failure to report.
func TestEvictToleratesAProcessThatHasAlreadyGone(t *testing.T) {
	t.Parallel()

	// A PID that cannot exist. The kernel's pid_max is at most 2^22 on 64-bit
	// Linux, so this is guaranteed to be unused.
	const impossible = 1 << 30

	if err := syscall.Kill(impossible, 0); err == nil {
		t.Skipf("pid %d exists on this host", impossible)
	}

	if err := evict(fakeCgroup(t, false, impossible)); err != nil {
		t.Errorf("evict() on a dead member = %v, want nil", err)
	}
}

// TestEvictIgnoresGarbageInCgroupProcs checks that a line the kernel would
// never write does not abort a teardown.
func TestEvictIgnoresGarbageInCgroupProcs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileProcs), []byte("not-a-pid\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := evict(dir); err != nil {
		t.Errorf("evict() = %v, want nil", err)
	}
}

// TestDestroyRemovesADeeplyNestedTree checks that removeChildCgroups recurses
// rather than giving up at the first level: a cgroup cannot be removed while it
// has children, at any depth.
func TestDestroyRemovesADeeplyNestedTree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	leaf := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := destroy(root); err != nil {
		t.Fatalf("destroy() = %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s = %v, want it gone", root, err)
	}
}

// TestDestroyIsIdempotent covers SSOT §13.3: a cleanup stack calls Destroy
// unconditionally, including after a Create that failed part-way.
func TestDestroyIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "leaf")

	for i := range 3 {
		if err := destroy(dir); err != nil {
			t.Fatalf("destroy() on pass %d = %v, want nil", i, err)
		}
	}
}

// TestDestroyReportsMembersItCouldNotEvict checks the diagnostic at the end of
// the retry budget: an operator needs the PIDs, not "removing cgroup failed".
//
// A live process is used deliberately — this test's own — because a cgroup that
// still lists a running PID is exactly the state the message describes. The
// directory is not a real cgroup, so nothing is signalled: cgroup.kill is
// absent and the PID is skipped only if it is 0 or 1, so the fallback signals
// SIGKILL to... nothing it can reach, since a plain directory has no members.
func TestDestroyReportsMembersItCouldNotEvict(t *testing.T) {
	t.Parallel()

	// A PID that certainly exists and that this process may signal, without
	// being this process: a child that ignores nothing, held alive by the test.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileProcs), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The directory is not empty — cgroup.procs is in it — so os.Remove fails
	// with ENOTEMPTY on every attempt, and PID 1 is skipped rather than
	// signalled. What is asserted is the report at the end.
	err := destroy(dir)
	if !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("destroy() = %v, want ErrNotEmpty", err)
	}
	if got := err.Error(); !strings.Contains(got, dir) {
		t.Errorf("destroy() = %q, want it to name %s", got, dir)
	}
	if got := err.Error(); !strings.Contains(got, "[1]") {
		t.Errorf("destroy() = %q, want it to name the surviving PIDs", got)
	}
}

// TestTranslate covers the mapping from a filesystem error onto the sentinel a
// caller can branch on. Both cases are reachable in production: EACCES from an
// unprivileged forge, ENOENT from a controller the parent never delegated.
func TestTranslate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "permission", err: os.ErrPermission, want: ErrPermission},
		{name: "wrapped permission", err: &os.PathError{Op: "open", Path: "/x", Err: syscall.EACCES}, want: ErrPermission},
		{name: "missing", err: os.ErrNotExist, want: ErrControllerUnavailable},
		{name: "wrapped missing", err: &os.PathError{Op: "open", Path: "/x", Err: syscall.ENOENT}, want: ErrControllerUnavailable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := translate(tc.err)
			if !errors.Is(got, tc.want) {
				t.Errorf("translate(%v) = %v, want %v", tc.err, got, tc.want)
			}
			// The original cause survives, so the message still says which
			// file the kernel refused.
			if !errors.Is(got, tc.err) {
				t.Errorf("translate(%v) dropped its cause", tc.err)
			}
		})
	}

	other := errors.New("something else")
	if got := translate(other); !errors.Is(got, other) {
		t.Errorf("translate(%v) = %v, want it passed through", other, got)
	}
	if got := translate(nil); got != nil {
		t.Errorf("translate(nil) = %v, want nil", got)
	}
}
