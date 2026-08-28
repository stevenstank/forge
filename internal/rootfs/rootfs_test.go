package rootfs_test

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/rootfs"
)

// internal/rootfs owns the on-disk layout FR-2.4 promises, and nothing else: it
// creates directories, validates a source tree, and removes what it created. It
// mounts nothing, so all of it is testable without root.

func newStore(t *testing.T) (*rootfs.Store, string) {
	t.Helper()

	root := filepath.Join(t.TempDir(), "containers")

	store, err := rootfs.NewStore(root, logging.New(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatalf("NewStore(%q) = %v", root, err)
	}
	return store, root
}

// TestNewStoreCreatesItsRoot covers the first-run case: /var/lib/forge/containers
// does not exist on a fresh host.
func TestNewStoreCreatesItsRoot(t *testing.T) {
	t.Parallel()

	_, root := newStore(t)

	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat %s: %v", root, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", root)
	}
	// The store holds other containers' filesystems; it is not world-readable.
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("store root permissions = %#o, want %#o", perm, 0o700)
	}
}

func TestNewStoreRejectsARelativeRoot(t *testing.T) {
	t.Parallel()

	if _, err := rootfs.NewStore("var/lib/forge", logging.New(io.Discard, slog.LevelError)); err == nil {
		t.Fatal("NewStore(relative) = nil error, want a failure")
	}
}

// TestPrepareLayout pins the layout in the Stage 2 design: <root>/<id>/rootfs.
// The intermediate directory is where Stage 3 puts a cgroup path and Stage 6
// puts state, so it is part of the contract now rather than a migration later.
func TestPrepareLayout(t *testing.T) {
	t.Parallel()

	store, root := newStore(t)
	const id = "a1b2c3d4e5f6"

	dir, err := store.Prepare(id)
	if err != nil {
		t.Fatalf("Prepare(%q) = %v", id, err)
	}

	if dir.ID != id {
		t.Errorf("Dir.ID = %q, want %q", dir.ID, id)
	}
	if want := filepath.Join(root, id); dir.Base != want {
		t.Errorf("Dir.Base = %q, want %q", dir.Base, want)
	}
	if want := filepath.Join(root, id, "rootfs"); dir.Rootfs != want {
		t.Errorf("Dir.Rootfs = %q, want %q", dir.Rootfs, want)
	}

	for path, wantPerm := range map[string]fs.FileMode{
		dir.Base:   0o700,
		dir.Rootfs: 0o755,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", path)
		}
		if perm := info.Mode().Perm(); perm != wantPerm {
			t.Errorf("%s permissions = %#o, want %#o", path, perm, wantPerm)
		}
	}
}

// TestPrepareRefusesAnExistingContainer keeps Forge from adopting a directory
// tree whose contents it does not know. An ID collision is vanishingly unlikely
// (ADR-0005) but "vanishingly unlikely" is not "impossible".
func TestPrepareRefusesAnExistingContainer(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	const id = "a1b2c3d4e5f6"

	if _, err := store.Prepare(id); err != nil {
		t.Fatalf("first Prepare() = %v", err)
	}
	if _, err := store.Prepare(id); err == nil {
		t.Fatal("second Prepare() = nil, want a failure")
	}
}

// TestPrepareRejectsIDsThatEscapeTheStore is defence in depth: IDs come from
// NewID today, but an ID that reached the store from anywhere else must never
// be able to name a directory outside it.
func TestPrepareRejectsIDsThatEscapeTheStore(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)

	for _, id := range []string{"", ".", "..", "../escape", "a/b", "/absolute", "a\x00b"} {
		t.Run(strings.ReplaceAll(id, "/", "_"), func(t *testing.T) {
			t.Parallel()

			if _, err := store.Prepare(id); !errors.Is(err, rootfs.ErrInvalidID) {
				t.Errorf("Prepare(%q) = %v, want %v", id, err, rootfs.ErrInvalidID)
			}
		})
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	const id = "a1b2c3d4e5f6"

	prepared, err := store.Prepare(id)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}

	got, err := store.Lookup(id)
	if err != nil {
		t.Fatalf("Lookup(%q) = %v", id, err)
	}
	if got != prepared {
		t.Errorf("Lookup() = %+v, want %+v", got, prepared)
	}

	if _, err := store.Lookup("ffffffffffff"); !errors.Is(err, rootfs.ErrNotPrepared) {
		t.Errorf("Lookup(unknown) = %v, want %v", err, rootfs.ErrNotPrepared)
	}
}

// TestRemoveDeletesTheTree covers the normal teardown path FR-2.3 requires.
func TestRemoveDeletesTheTree(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	const id = "a1b2c3d4e5f6"

	dir, err := store.Prepare(id)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	if dir.Base == "" || dir.Rootfs == "" {
		t.Fatalf("Prepare() = %+v, want populated paths", dir)
	}
	// A container leaves files behind; removal must not care.
	if err := os.WriteFile(filepath.Join(dir.Rootfs, "written-by-the-container"), []byte("x"), 0o600); err != nil {
		t.Fatalf("writing into the rootfs: %v", err)
	}

	if err := store.Remove(t.Context(), id); err != nil {
		t.Fatalf("Remove(%q) = %v", id, err)
	}
	if _, err := os.Stat(dir.Base); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s = %v, want the tree to be gone", dir.Base, err)
	}
}

// TestRemoveIsIdempotent covers SSOT §13.3. Cleanup runs on paths where an
// earlier step may already have removed the tree, and on paths where it was
// never created at all.
func TestRemoveIsIdempotent(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	const id = "a1b2c3d4e5f6"

	if _, err := store.Prepare(id); err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	for i := range 3 {
		if err := store.Remove(t.Context(), id); err != nil {
			t.Fatalf("Remove() call %d = %v, want nil", i+1, err)
		}
	}
	if err := store.Remove(t.Context(), "ffffffffffff"); err != nil {
		t.Errorf("Remove(never prepared) = %v, want nil", err)
	}
}

func TestValidateSource(t *testing.T) {
	t.Parallel()

	base := t.TempDir()

	sourceDir := filepath.Join(base, "alpine")
	if err := os.MkdirAll(filepath.Join(sourceDir, "bin"), 0o755); err != nil {
		t.Fatalf("building a source tree: %v", err)
	}

	sourceFile := filepath.Join(base, "alpine.tar")
	if err := os.WriteFile(sourceFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", sourceFile, err)
	}

	linkToDir := filepath.Join(base, "link-to-dir")
	if err := os.Symlink(sourceDir, linkToDir); err != nil {
		t.Fatalf("linking: %v", err)
	}
	linkToFile := filepath.Join(base, "link-to-file")
	if err := os.Symlink(sourceFile, linkToFile); err != nil {
		t.Fatalf("linking: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr error
	}{
		{
			name: "a directory is valid",
			path: sourceDir,
			want: sourceDir,
		},
		{
			name: "a symlink to a directory resolves",
			path: linkToDir,
			want: sourceDir,
		},
		{
			name: "a trailing slash is cleaned",
			path: sourceDir + "/",
			want: sourceDir,
		},
		{
			name:    "a missing path is rejected",
			path:    filepath.Join(base, "nonexistent"),
			wantErr: rootfs.ErrSourceNotFound,
		},
		{
			name:    "a file is rejected",
			path:    sourceFile,
			wantErr: rootfs.ErrSourceNotADirectory,
		},
		{
			name:    "a symlink to a file is rejected",
			path:    linkToFile,
			wantErr: rootfs.ErrSourceNotADirectory,
		},
		{
			name:    "the host root is rejected",
			path:    "/",
			wantErr: rootfs.ErrSourceIsHostRoot,
		},
		{
			name:    "a relative path is rejected",
			path:    "alpine",
			wantErr: rootfs.ErrSourceNotFound,
		},
		{
			name:    "an empty path is rejected",
			path:    "",
			wantErr: rootfs.ErrSourceNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := rootfs.ValidateSource(tt.path)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ValidateSource(%q) = %v, want %v", tt.path, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateSource(%q) = %v", tt.path, err)
			}
			if got != tt.want {
				t.Errorf("ValidateSource(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// TestValidateSourceNamesThePath keeps the most common Stage 2 mistake — a
// typo'd --rootfs — diagnosable from the message alone.
func TestValidateSourceNamesThePath(t *testing.T) {
	t.Parallel()

	const path = "/nonexistent/forge-rootfs"

	_, err := rootfs.ValidateSource(path)
	if err == nil {
		t.Fatal("ValidateSource() = nil, want an error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path", err)
	}
}

// TestLookupRefusesSomethingThatIsNotADirectory covers the case a stray file in
// the store produces: `forge rm` must say the container was never prepared
// rather than trying to treat a regular file as a container tree.
func TestLookupRefusesSomethingThatIsNotADirectory(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "containers")
	store, err := rootfs.NewStore(root, logging.New(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatalf("NewStore() = %v", err)
	}

	const id = "a1b2c3d4e5f6"
	if err := os.WriteFile(filepath.Join(root, id), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = store.Lookup(id)
	if !errors.Is(err, rootfs.ErrNotPrepared) {
		t.Fatalf("Lookup() = %v, want ErrNotPrepared", err)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("Lookup() = %q, want it to say what is wrong", err)
	}
}

// TestPrepareReportsAnUnwritableStore checks the error a run gets when Forge's
// storage root cannot be written to, which is what an operator meets on a full
// or read-only filesystem.
func TestPrepareReportsAnUnwritableStore(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so this asserts nothing as root")
	}

	root := filepath.Join(t.TempDir(), "containers")
	store, err := rootfs.NewStore(root, logging.New(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatalf("NewStore() = %v", err)
	}

	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	const id = "a1b2c3d4e5f6"

	_, err = store.Prepare(id)
	if err == nil {
		t.Fatal("Prepare() = nil into an unwritable store")
	}
	if !strings.Contains(err.Error(), id) {
		t.Errorf("Prepare() = %q, want it to name the container directory", err)
	}
	// Nothing half-built is left behind (PRD NFR-8).
	if _, statErr := os.Stat(filepath.Join(root, id)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a failed Prepare left %s behind", filepath.Join(root, id))
	}
}
