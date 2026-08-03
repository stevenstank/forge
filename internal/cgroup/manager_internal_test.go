package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Controller delegation is tested here rather than through Create for the same
// reason as the writes in apply_internal_test.go: cgroup.subtree_control is an
// interface file the kernel creates with the cgroup, and Forge never creates
// one. A test therefore has to seed the cgroups it wants to delegate between,
// which means seeding Forge's own cgroup before Create would make it.

// newHierarchy returns a root that looks like a cgroup v2 unified hierarchy
// offering controllers, with Forge's own cgroup already present under it.
func newHierarchy(t *testing.T, controllers, enabled string) string {
	t.Helper()

	root := t.TempDir()
	seed(t, filepath.Join(root, fileControllers), controllers)
	seed(t, filepath.Join(root, fileSubtreeControl), enabled)

	parent := filepath.Join(root, ParentName)
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("creating %s: %v", parent, err)
	}
	seed(t, filepath.Join(parent, fileControllers), controllers)
	seed(t, filepath.Join(parent, fileSubtreeControl), "")

	return root
}

func seed(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seeding %s: %v", path, err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return string(content)
}

// Controllers are delegated at both levels, because cgroup v2 delegation is one
// level deep: the root must delegate to <root>/forge, and <root>/forge must
// delegate to the leaves, or a leaf has no memory.max to write to.
func TestPrepareParentDelegatesDownTheChain(t *testing.T) {
	t.Parallel()

	root := newHierarchy(t, "cpu io memory pids", "")
	m := New(root)

	if err := m.prepareParent([]Controller{ControllerMemory}); err != nil {
		t.Fatalf("prepareParent() = %v", err)
	}

	for _, dir := range []string{root, m.Parent()} {
		if got := read(t, filepath.Join(dir, fileSubtreeControl)); got != "+memory" {
			t.Errorf("%s/%s = %q, want %q", dir, fileSubtreeControl, got, "+memory")
		}
	}
}

// Only what the container asked for is delegated, so a host that is not
// offering a controller Forge is not using stays usable.
func TestPrepareParentDelegatesOnlyWhatIsNeeded(t *testing.T) {
	t.Parallel()

	root := newHierarchy(t, "cpu io memory pids", "")
	m := New(root)

	if err := m.prepareParent([]Controller{ControllerPIDs}); err != nil {
		t.Fatalf("prepareParent() = %v", err)
	}

	if got := read(t, filepath.Join(root, fileSubtreeControl)); got != "+pids" {
		t.Errorf("%s = %q, want %q", fileSubtreeControl, got, "+pids")
	}
}

// A controller that is already delegated must not be rewritten. On a delegated
// hierarchy — Forge running inside a container — that write can fail even when
// the controllers the caller needs are already on.
func TestPrepareParentSkipsWhatIsAlreadyEnabled(t *testing.T) {
	t.Parallel()

	root := newHierarchy(t, "cpu io memory pids", "cpu memory pids")
	seed(t, filepath.Join(root, ParentName, fileSubtreeControl), "cpu memory pids")
	m := New(root)

	if err := m.prepareParent([]Controller{ControllerMemory, ControllerCPU}); err != nil {
		t.Fatalf("prepareParent() = %v", err)
	}

	if got := read(t, filepath.Join(root, fileSubtreeControl)); got != "cpu memory pids" {
		t.Errorf("%s = %q, want it untouched", fileSubtreeControl, got)
	}
}

// Asking for nothing writes nothing: a container with no limits needs no
// controller, and must not fail on a host that cannot delegate one.
func TestPrepareParentWithNoControllers(t *testing.T) {
	t.Parallel()

	root := newHierarchy(t, "cpu io memory pids", "")
	m := New(root)

	if err := m.prepareParent(nil); err != nil {
		t.Fatalf("prepareParent() = %v", err)
	}
	if got := read(t, filepath.Join(root, fileSubtreeControl)); got != "" {
		t.Errorf("%s = %q, want it untouched", fileSubtreeControl, got)
	}
}

func TestPrepareParentRejectsAnUnavailableController(t *testing.T) {
	t.Parallel()

	root := newHierarchy(t, "cpu pids", "") // no memory controller
	m := New(root)

	err := m.prepareParent([]Controller{ControllerMemory})
	if !errors.Is(err, ErrControllerUnavailable) {
		t.Fatalf("prepareParent() = %v, want ErrControllerUnavailable", err)
	}
}

// Forge's own cgroup is created on demand, and an existing one is reused rather
// than fought over: concurrent runs race to create it and both must win.
func TestPrepareParentCreatesForgesCgroupOnce(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	seed(t, filepath.Join(root, fileControllers), "cpu io memory pids")
	seed(t, filepath.Join(root, fileSubtreeControl), "")
	m := New(root)

	for range 2 {
		if err := m.prepareParent(nil); err != nil {
			t.Fatalf("prepareParent() = %v", err)
		}
	}

	if _, err := os.Stat(m.Parent()); err != nil {
		t.Errorf("expected %s to exist: %v", m.Parent(), err)
	}
}
