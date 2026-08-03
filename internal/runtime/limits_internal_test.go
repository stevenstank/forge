package runtime

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/cgroup"
	"github.com/stevenstank/forge/internal/logging"
)

// prepareCgroup is where Stage 3's orchestration decisions live, and each of
// them is invisible from outside the package: whether the teardown was
// registered, when it was registered relative to everything else, and whether a
// failed creation left anything behind. These tests reach in and check.

// newTestRunner builds a Runner whose cgroup hierarchy is a temp directory.
// controllers is the content of the synthetic cgroup.controllers; an empty
// string means the file is not created at all, which is what a cgroup v1 host
// looks like.
func newTestRunner(t *testing.T, logger *slog.Logger, controllers string) (*Runner, string) {
	t.Helper()

	cgroupRoot := t.TempDir()
	if controllers != "" {
		if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.controllers"), []byte(controllers), 0o644); err != nil {
			t.Fatalf("writing cgroup.controllers: %v", err)
		}
		if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"), nil, 0o644); err != nil {
			t.Fatalf("writing cgroup.subtree_control: %v", err)
		}
	}

	runner, err := NewRunner(logger, Config{
		Root:       filepath.Join(t.TempDir(), "containers"),
		CgroupRoot: cgroupRoot,
	})
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	return runner, cgroupRoot
}

func discardLogger() *slog.Logger { return logging.New(io.Discard, slog.LevelError) }

func megabytes(mb cgroup.Bytes) *cgroup.Bytes { return &mb }

// TestPrepareCgroupRegistersTeardownOnSuccess is the stage's central cleanup
// guarantee: the moment a leaf exists, the thing that removes it is on the
// stack, so every later failure — and the ordinary exit path too — unwinds
// through it (SSOT §11.3).
func TestPrepareCgroupRegistersTeardownOnSuccess(t *testing.T) {
	t.Parallel()

	runner, cgroupRoot := newTestRunner(t, discardLogger(), "cpu io memory pids")
	cleanup := newCleanupStack(discardLogger())

	// A limit-free container. The leaf's interface files are created by the
	// kernel at mkdir time, which a temp directory does not do, so what Create
	// writes into them is asserted in internal/cgroup and against a real kernel
	// in the Stage 3 integration tests. What matters here is the orchestration:
	// the leaf exists, and its teardown is registered.
	spec := Spec{Command: []string{"/bin/true"}}

	cgroupID, err := runner.prepareCgroup(discardLogger(), "abc123", spec, cleanup)
	if err != nil {
		t.Fatalf("prepareCgroup() = %v", err)
	}
	if cgroupID != "abc123" {
		t.Errorf("prepareCgroup() = %q, want the container id so the child can be attached", cgroupID)
	}

	leaf := filepath.Join(cgroupRoot, "forge", "abc123")
	if _, err := os.Stat(leaf); err != nil {
		t.Fatalf("expected the leaf to exist: %v", err)
	}

	if len(cleanup.fns) != 1 {
		t.Fatalf("cleanup stack has %d steps, want the cgroup teardown registered", len(cleanup.fns))
	}

	cleanup.unwind()

	if _, err := os.Stat(leaf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the leaf survived the unwind: %v", err)
	}
}

// TestPrepareCgroupTeardownUnwindsBeforeTheRootfs pins the ordering the stage
// depends on. The stack unwinds in reverse, so the cgroup — registered last —
// is released first, and nothing the container left running is still holding
// its directory open when the rootfs is removed.
func TestPrepareCgroupTeardownUnwindsBeforeTheRootfs(t *testing.T) {
	t.Parallel()

	runner, _ := newTestRunner(t, discardLogger(), "cpu io memory pids")
	cleanup := newCleanupStack(discardLogger())

	// Stand in for prepareFilesystem, which registers its own teardown first.
	cleanup.push("removing the container root filesystem", func() error { return nil })

	if _, err := runner.prepareCgroup(discardLogger(), "abc123", Spec{Command: []string{"/bin/true"}}, cleanup); err != nil {
		t.Fatalf("prepareCgroup() = %v", err)
	}

	if len(cleanup.fns) != 2 {
		t.Fatalf("cleanup stack has %d steps, want 2", len(cleanup.fns))
	}
	if last := cleanup.fns[len(cleanup.fns)-1].name; !strings.Contains(last, "cgroup") {
		t.Errorf("last registered step is %q, want the cgroup so it unwinds first", last)
	}
}

// FR-3.1 is unconditional: a container that asked for nothing still gets a
// cgroup, and still gets it cleaned up.
func TestPrepareCgroupCreatesALeafEvenWithoutLimits(t *testing.T) {
	t.Parallel()

	runner, cgroupRoot := newTestRunner(t, discardLogger(), "cpu io memory pids")
	cleanup := newCleanupStack(discardLogger())

	cgroupID, err := runner.prepareCgroup(discardLogger(), "abc123", Spec{Command: []string{"/bin/true"}}, cleanup)
	if err != nil {
		t.Fatalf("prepareCgroup() = %v", err)
	}
	if cgroupID == "" {
		t.Error("prepareCgroup() returned no cgroup for a container that should have one")
	}
	if len(cleanup.fns) != 1 {
		t.Errorf("cleanup stack has %d steps, want the cgroup teardown registered", len(cleanup.fns))
	}

	entries, err := os.ReadDir(filepath.Join(cgroupRoot, "forge", "abc123"))
	if err != nil {
		t.Fatalf("reading the leaf: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("leaf contains %d files, want no limits written", len(entries))
	}
}

// A failed creation must leave neither a cgroup nor a registered teardown for a
// cgroup that does not exist.
func TestPrepareCgroupLeavesNothingBehindWhenCreationFails(t *testing.T) {
	t.Parallel()

	// The host is not offering the memory controller.
	runner, cgroupRoot := newTestRunner(t, discardLogger(), "cpu pids")
	cleanup := newCleanupStack(discardLogger())

	spec := Spec{Command: []string{"/bin/true"}, Limits: cgroup.Limits{MemoryMax: megabytes(64 << 20)}}

	cgroupID, err := runner.prepareCgroup(discardLogger(), "abc123", spec, cleanup)
	if !errors.Is(err, cgroup.ErrControllerUnavailable) {
		t.Fatalf("prepareCgroup() = %v, want %v", err, cgroup.ErrControllerUnavailable)
	}
	if cgroupID != "" {
		t.Errorf("prepareCgroup() = %q, want no cgroup to attach to", cgroupID)
	}
	if len(cleanup.fns) != 0 {
		t.Errorf("cleanup stack has %d steps after a failed creation, want none", len(cleanup.fns))
	}

	if _, err := os.Stat(filepath.Join(cgroupRoot, "forge", "abc123")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed creation left a leaf behind: %v", err)
	}
}

// A container that asked for limits on a host that cannot apply them does not
// start. Silently running it unconstrained would be the worst outcome
// available: the caller believes it is capped and it is not.
func TestPrepareCgroupRefusesLimitsWithoutAHierarchy(t *testing.T) {
	t.Parallel()

	runner, _ := newTestRunner(t, discardLogger(), "") // no cgroup.controllers: a v1 host
	cleanup := newCleanupStack(discardLogger())

	spec := Spec{Command: []string{"/bin/true"}, Limits: cgroup.Limits{MemoryMax: megabytes(64 << 20)}}

	_, err := runner.prepareCgroup(discardLogger(), "abc123", spec, cleanup)
	if !errors.Is(err, cgroup.ErrUnifiedHierarchyNotMounted) {
		t.Fatalf("prepareCgroup() = %v, want %v", err, cgroup.ErrUnifiedHierarchyNotMounted)
	}
	if len(cleanup.fns) != 0 {
		t.Errorf("cleanup stack has %d steps, want none", len(cleanup.fns))
	}
}

// The other side of that rule: an unlimited container on a v1 host still runs.
// Stage 3 must not break Stages 1 and 2 on the hosts where they work today.
func TestPrepareCgroupDegradesWhenNothingWasAskedFor(t *testing.T) {
	t.Parallel()

	var logs strings.Builder
	runner, _ := newTestRunner(t, logging.New(&logs, slog.LevelWarn), "")
	cleanup := newCleanupStack(discardLogger())

	cgroupID, err := runner.prepareCgroup(
		logging.New(&logs, slog.LevelWarn), "abc123", Spec{Command: []string{"/bin/true"}}, cleanup)
	if err != nil {
		t.Fatalf("prepareCgroup() = %v, want the missing hierarchy to be tolerated", err)
	}
	if cgroupID != "" {
		t.Errorf("prepareCgroup() = %q, want no cgroup to attach to", cgroupID)
	}
	if len(cleanup.fns) != 0 {
		t.Errorf("cleanup stack has %d steps, want nothing to release", len(cleanup.fns))
	}

	// Degraded, but never silently (SSOT §13.7).
	if got := logs.String(); !strings.Contains(got, "cgroup") {
		t.Errorf("log %q does not warn that the container runs without a cgroup", got)
	}
}

// The cgroup teardown must survive being run twice, because a run that fails
// after Create can unwind while the container's own exit path is also tidying
// up. Idempotence is what makes that safe (SSOT §13.3).
func TestCgroupTeardownIsIdempotent(t *testing.T) {
	t.Parallel()

	runner, cgroupRoot := newTestRunner(t, discardLogger(), "cpu io memory pids")
	cleanup := newCleanupStack(discardLogger())

	if _, err := runner.prepareCgroup(discardLogger(), "abc123", Spec{Command: []string{"/bin/true"}}, cleanup); err != nil {
		t.Fatalf("prepareCgroup() = %v", err)
	}

	teardown := cleanup.fns[0].fn
	if err := teardown(); err != nil {
		t.Fatalf("first teardown = %v", err)
	}
	// The second call finds the leaf already gone. ENOENT is success.
	if err := teardown(); err != nil {
		t.Fatalf("second teardown = %v, want nil", err)
	}

	if _, err := os.Stat(filepath.Join(cgroupRoot, "forge", "abc123")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the leaf survived: %v", err)
	}
}

func TestLimitSummary(t *testing.T) {
	t.Parallel()

	if got := limitSummary(cgroup.Limits{}); got != "none" {
		t.Errorf("limitSummary(zero) = %q, want %q", got, "none")
	}

	procs := int64(64)
	got := limitSummary(cgroup.Limits{MemoryMax: megabytes(64 << 20), PIDsMax: &procs})
	// The swap limit appears because it is part of what a memory limit means.
	if want := "memory.max=67108864 memory.swap.max=0 pids.max=64"; got != want {
		t.Errorf("limitSummary() = %q, want %q", got, want)
	}
}
