package cgroup_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/cgroup"
)

// These tests exercise the half of internal/cgroup that touches the
// filesystem — Create, Add and Destroy — without root and without a cgroup
// filesystem.
//
// The seam is Manager's root. cgroup v2 is a filesystem interface and this
// package uses nothing but os and path/filepath to drive it, so pointing the
// root at a t.TempDir() holding a synthetic cgroup.controllers exercises the
// real code against real files.
//
// The fake models kernfs faithfully, which means it models one thing the tests
// have to work around: interface files exist only where something created
// them. The kernel creates a leaf's files when the leaf is made; a temp
// directory does not. So the tests here seed the files they need and cover the
// exported surface, the writes themselves are covered in apply_internal_test.go
// against a seeded leaf, and what Create sends to a real kernel is covered by
// the Stage 3 integration tests (SSOT §7).

// fakeHierarchy builds a directory that looks enough like the root of a cgroup
// v2 unified hierarchy for Manager to work in. controllers is the content of
// cgroup.controllers: the set the kernel is offering.
func fakeHierarchy(t *testing.T, controllers string) string {
	t.Helper()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "cgroup.controllers"), controllers)
	writeFile(t, filepath.Join(root, "cgroup.subtree_control"), "")

	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// readFile returns the content of a controller file, failing the test if it is
// absent.
func readFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return string(content)
}

func assertExists(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s not to exist, stat returned %v", path, err)
	}
}

func TestNewDefaultsToTheConventionalRoot(t *testing.T) {
	t.Parallel()

	if got := cgroup.New("").Root(); got != cgroup.DefaultRoot {
		t.Errorf("New(\"\").Root() = %q, want %q", got, cgroup.DefaultRoot)
	}
	if got := cgroup.New("/somewhere/else/").Root(); got != "/somewhere/else" {
		t.Errorf("Root() = %q, want %q", got, "/somewhere/else")
	}
}

func TestPath(t *testing.T) {
	t.Parallel()

	m := cgroup.New("/sys/fs/cgroup")

	got, err := m.Path("abc123")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := "/sys/fs/cgroup/forge/abc123"; got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
	if want := "/sys/fs/cgroup/forge"; m.Parent() != want {
		t.Errorf("Parent() = %q, want %q", m.Parent(), want)
	}
}

// Create makes the leaf; what it writes into it is asserted in
// apply_internal_test.go and, against a real kernel, in the Stage 3
// integration tests. A leaf's interface files are created by the kernel at
// mkdir time, so a temp-directory hierarchy has none and Create's limit writes
// cannot run here — which is exactly the property that keeps Forge from
// creating those files itself.
func TestCreateMakesTheLeaf(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu io memory pids")
	m := cgroup.New(root)

	if err := m.Create("abc123", cgroup.Limits{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	leaf := filepath.Join(root, "forge", "abc123")
	assertExists(t, leaf)

	path, err := m.Path("abc123")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if path != leaf {
		t.Errorf("Path() = %q, want %q", path, leaf)
	}
}

// FR-3.1 is unconditional: a container with no limits still gets a leaf, and an
// unlimited cgroup still accounts for what it uses.
func TestCreateWithoutLimitsStillCreatesTheLeaf(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu io memory pids")
	m := cgroup.New(root)

	if err := m.Create("abc123", cgroup.Limits{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	leaf := filepath.Join(root, "forge", "abc123")
	assertExists(t, leaf)

	entries, err := os.ReadDir(leaf)
	if err != nil {
		t.Fatalf("reading the leaf: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no controller files to be written, found %d", len(entries))
	}
	if got := readFile(t, filepath.Join(root, "cgroup.subtree_control")); got != "" {
		t.Errorf("cgroup.subtree_control = %q, want it untouched", got)
	}
}

func TestCreateRejectsInvalidLimitsBeforeCreatingAnything(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu io memory pids")
	m := cgroup.New(root)

	memory := cgroup.Bytes(0)
	err := m.Create("abc123", cgroup.Limits{MemoryMax: &memory})
	if !errors.Is(err, cgroup.ErrInvalidLimit) {
		t.Fatalf("Create = %v, want ErrInvalidLimit", err)
	}

	assertMissing(t, filepath.Join(root, "forge", "abc123"))
}

// A container ID becomes a path element under <root>/forge. An ID carrying a
// separator would put the cgroup somewhere else entirely, and Destroy would
// then remove a directory that is not Forge's.
func TestCreateRejectsContainerIDsThatEscape(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu io memory pids")
	m := cgroup.New(root)

	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "a\x00b"} {
		t.Run("id="+strings.ReplaceAll(id, "\x00", `\0`), func(t *testing.T) {
			if err := m.Create(id, cgroup.Limits{}); !errors.Is(err, cgroup.ErrInvalidContainerID) {
				t.Fatalf("Create(%q) = %v, want ErrInvalidContainerID", id, err)
			}
			if _, err := m.Path(id); !errors.Is(err, cgroup.ErrInvalidContainerID) {
				t.Errorf("Path(%q) = %v, want ErrInvalidContainerID", id, err)
			}
			if err := m.Add(id, 1234); !errors.Is(err, cgroup.ErrInvalidContainerID) {
				t.Errorf("Add(%q) = %v, want ErrInvalidContainerID", id, err)
			}
			if err := m.Destroy(id); !errors.Is(err, cgroup.ErrInvalidContainerID) {
				t.Errorf("Destroy(%q) = %v, want ErrInvalidContainerID", id, err)
			}
		})
	}

	// Nothing under forge, and nothing beside it either.
	assertMissing(t, filepath.Join(root, "escape"))
}

// The absence of cgroup.controllers is how a cgroup v1 or hybrid host is
// detected: v1 has no such interface file.
func TestCreateOnANonUnifiedHierarchy(t *testing.T) {
	t.Parallel()

	m := cgroup.New(t.TempDir())

	if err := m.Create("abc123", cgroup.Limits{}); !errors.Is(err, cgroup.ErrUnifiedHierarchyNotMounted) {
		t.Fatalf("Create = %v, want ErrUnifiedHierarchyNotMounted", err)
	}
}

func TestCreateWithAnUnavailableController(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu pids") // no memory controller
	m := cgroup.New(root)

	memory := cgroup.Bytes(64 << 20)
	err := m.Create("abc123", cgroup.Limits{MemoryMax: &memory})
	if !errors.Is(err, cgroup.ErrControllerUnavailable) {
		t.Fatalf("Create = %v, want ErrControllerUnavailable", err)
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Errorf("Create error %q should name the missing controller", err)
	}

	// A limit the caller asked for is never silently dropped: nothing is
	// created and the container does not start unconstrained.
	assertMissing(t, filepath.Join(root, "forge", "abc123"))
}

// Two containers with the same ID is a bug in the caller, not something to
// paper over by reusing the cgroup of a container that may still be running.
func TestCreateTwiceIsAnError(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu io memory pids")
	m := cgroup.New(root)

	if err := m.Create("abc123", cgroup.Limits{}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := m.Create("abc123", cgroup.Limits{}); !errors.Is(err, cgroup.ErrExists) {
		t.Fatalf("second Create = %v, want ErrExists", err)
	}
}

func TestAddWritesThePIDToCgroupProcs(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu io memory pids")
	m := cgroup.New(root)

	if err := m.Create("abc123", cgroup.Limits{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The kernel creates cgroup.procs with the cgroup; Forge opens it and never
	// creates it, so the fake hierarchy has to supply it.
	writeFile(t, filepath.Join(root, "forge", "abc123", "cgroup.procs"), "")

	if err := m.Add("abc123", 4242); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got := readFile(t, filepath.Join(root, "forge", "abc123", "cgroup.procs"))
	if got != "4242" {
		t.Errorf("cgroup.procs = %q, want %q", got, "4242")
	}
}

func TestAddFailures(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu io memory pids")
	m := cgroup.New(root)

	if err := m.Add("never-created", 4242); !errors.Is(err, cgroup.ErrNotFound) {
		t.Errorf("Add to a missing cgroup = %v, want ErrNotFound", err)
	}

	if err := m.Create("abc123", cgroup.Limits{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Zero is not a PID: written to cgroup.procs it would mean "the writing
	// process", which is forge itself.
	if err := m.Add("abc123", 0); err == nil {
		t.Error("Add with pid 0 should be refused")
	}
	if err := m.Add("abc123", -1); err == nil {
		t.Error("Add with a negative pid should be refused")
	}
}

// FR-3.5, and the idempotence SSOT §13.3 requires of every teardown path.
func TestDestroy(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu io memory pids")
	m := cgroup.New(root)

	if err := m.Create("abc123", cgroup.Limits{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	leaf := filepath.Join(root, "forge", "abc123")
	assertExists(t, leaf)

	if err := m.Destroy("abc123"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	assertMissing(t, leaf)

	// Twice is not an error, so a cleanup stack can run it after a failed
	// Create without knowing how far that Create got.
	if err := m.Destroy("abc123"); err != nil {
		t.Errorf("second Destroy: %v", err)
	}
	if err := m.Destroy("never-created"); err != nil {
		t.Errorf("Destroy of a cgroup that never existed: %v", err)
	}
}

// Forge's own cgroup is shared by every container and is never removed: two
// concurrent runs would race to remove a directory the other is about to
// create in.
func TestDestroyLeavesForgesOwnCgroup(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu io memory pids")
	m := cgroup.New(root)

	if err := m.Create("abc123", cgroup.Limits{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Destroy("abc123"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	assertExists(t, filepath.Join(root, "forge"))
}

// A cgroup cannot be removed while it has child cgroups. A container cannot
// create them itself, but a leaf reconciled after a crash might have some.
func TestDestroyRemovesChildCgroups(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu io memory pids")
	m := cgroup.New(root)

	if err := m.Create("abc123", cgroup.Limits{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	leaf := filepath.Join(root, "forge", "abc123")
	nested := filepath.Join(leaf, "child", "grandchild")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("creating a child cgroup: %v", err)
	}

	if err := m.Destroy("abc123"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	assertMissing(t, leaf)
}

// Destroy after a Create that failed part-way is the case the cleanup stack in
// internal/runtime relies on: this package rolls nothing back itself, so
// whatever a failed Create left behind has to be removable by ID alone, with no
// handle and no knowledge of how far it got.
func TestDestroyAfterAFailedCreate(t *testing.T) {
	t.Parallel()

	root := fakeHierarchy(t, "cpu pids") // no memory controller
	m := cgroup.New(root)

	memory := cgroup.Bytes(64 << 20)
	if err := m.Create("abc123", cgroup.Limits{MemoryMax: &memory}); err == nil {
		t.Fatal("Create = nil, want a failure")
	}

	if err := m.Destroy("abc123"); err != nil {
		t.Fatalf("Destroy after a failed Create: %v", err)
	}
	assertMissing(t, filepath.Join(root, "forge", "abc123"))
}

// On a real cgroup filesystem the kernel ignores a leaf's interface files when
// deciding whether it is empty, so rmdir removes a fully-populated cgroup in
// one call. Nothing in Forge unlinks those files — production code must not
// carry a general-purpose file remover pointed at a caller-supplied path — and
// that behaviour is asserted against the kernel in the Stage 3 integration
// tests, which destroy leaves holding every limit Forge writes.
