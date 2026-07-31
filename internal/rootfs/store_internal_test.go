package rootfs

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
)

// Remove ends in os.RemoveAll. If a bind mount into the host filesystem is
// still live underneath the tree, that call walks through it and deletes the
// host's files. There is no recovering from that, so Remove consults the mount
// table first and refuses to proceed if anything is mounted under the tree.
//
// These tests replace that lookup with a stub, because the real one needs root
// and a real mount; the end-to-end version lives in test/integration.

func newTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := NewStore(filepath.Join(t.TempDir(), "containers"), logging.New(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatalf("NewStore() = %v", err)
	}
	return store
}

func TestRemoveRefusesWhileMounted(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	const id = "a1b2c3d4e5f6"

	dir, err := store.Prepare(id)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}

	mounted := filepath.Join(dir.Rootfs, "data")
	store.mountsUnder = func(string) ([]string, error) {
		return []string{mounted}, nil
	}

	err = store.Remove(t.Context(), id)
	if !errors.Is(err, ErrStillMounted) {
		t.Fatalf("Remove() = %v, want %v", err, ErrStillMounted)
	}
	if _, statErr := os.Stat(dir.Base); statErr != nil {
		t.Errorf("stat %s = %v, want the tree left intact", dir.Base, statErr)
	}
}

// TestRemoveErrorNamesTheMount keeps the refusal debuggable: the operator needs
// to know what is still mounted, not just that something is.
func TestRemoveErrorNamesTheMount(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	const id = "a1b2c3d4e5f6"

	dir, err := store.Prepare(id)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}

	mounted := filepath.Join(dir.Rootfs, "data")
	store.mountsUnder = func(string) ([]string, error) {
		return []string{mounted}, nil
	}

	err = store.Remove(t.Context(), id)
	if err == nil {
		t.Fatal("Remove() = nil, want an error")
	}
	if got := err.Error(); !strings.Contains(got, mounted) {
		t.Errorf("error %q does not name the mount %q", got, mounted)
	}
}

// TestRemoveFailsClosedWhenTheMountTableCannotBeRead is the important half of
// the guard. If Forge cannot tell whether something is mounted, it must assume
// something is. A Remove that proceeded on an unreadable mount table would
// delete host files exactly when the system is least healthy.
func TestRemoveFailsClosedWhenTheMountTableCannotBeRead(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	const id = "a1b2c3d4e5f6"

	dir, err := store.Prepare(id)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}

	store.mountsUnder = func(string) ([]string, error) {
		return nil, errors.New("cannot read /proc/self/mountinfo")
	}

	if err := store.Remove(t.Context(), id); err == nil {
		t.Fatal("Remove() = nil, want a failure when the mount table is unreadable")
	}
	if _, statErr := os.Stat(dir.Base); statErr != nil {
		t.Errorf("stat %s = %v, want the tree left intact", dir.Base, statErr)
	}
}

// TestRemoveProceedsWhenNothingIsMounted confirms the guard is not simply
// refusing everything: the ordinary path still deletes.
func TestRemoveProceedsWhenNothingIsMounted(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	const id = "a1b2c3d4e5f6"

	dir, err := store.Prepare(id)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}

	var asked string
	store.mountsUnder = func(path string) ([]string, error) {
		asked = path
		return nil, nil
	}

	if err := store.Remove(t.Context(), id); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	if asked != dir.Base {
		t.Errorf("checked %q for mounts, want the whole container tree %q", asked, dir.Base)
	}
	if _, err := os.Stat(dir.Base); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s = %v, want the tree to be gone", dir.Base, err)
	}
}
