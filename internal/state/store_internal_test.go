package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// tempDirWithRecord returns a container directory holding a known record.
func tempDirWithRecord(t *testing.T, content string) (dir, path string) {
	t.Helper()

	dir = t.TempDir()
	path = filepath.Join(dir, metadataFileName)
	if err := os.WriteFile(path, []byte(content), filePerm); err != nil {
		t.Fatal(err)
	}

	return dir, path
}

// tempFiles returns the names of the in-flight write files in dir. A write
// that finished leaves none.
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var found []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			found = append(found, e.Name())
		}
	}

	return found
}

func TestWriteAtomicReplacesInPlace(t *testing.T) {
	dir, path := tempDirWithRecord(t, "old contents\n")

	if err := writeAtomic(path, []byte("new contents\n")); err != nil {
		t.Fatalf("writeAtomic = %v, want nil", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new contents\n" {
		t.Errorf("contents = %q, want %q", got, "new contents\n")
	}

	// A finished write leaves nothing but the record. Anything else is
	// residue that LoadAll would have to explain away forever.
	if left := tempFiles(t, dir); len(left) != 0 {
		t.Errorf("writeAtomic left %v behind", left)
	}
}

func TestWriteAtomicCreatesANewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, metadataFileName)

	if err := writeAtomic(path, []byte("first\n")); err != nil {
		t.Fatalf("writeAtomic = %v, want nil", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\n" {
		t.Errorf("contents = %q, want %q", got, "first\n")
	}
}

// TestWriteAtomicFailureLeavesTheOriginal is the interrupted-write case with
// the interruption where it hurts most: after the temporary file is written,
// at the moment of the rename. The record must be the old one, whole, and no
// residue may be left.
func TestWriteAtomicFailureLeavesTheOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, metadataFileName)

	// A directory where the record belongs: rename(2) refuses to replace it,
	// which is a real failure at exactly the step being tested.
	if err := os.Mkdir(path, dirPerm); err != nil {
		t.Fatal(err)
	}

	err := writeAtomic(path, []byte("new contents\n"))
	if err == nil {
		t.Fatal("writeAtomic = nil, want an error")
	}
	if !strings.Contains(err.Error(), "replacing") {
		t.Errorf("error = %v, want it to name the failed step", err)
	}

	if left := tempFiles(t, dir); len(left) != 0 {
		t.Errorf("a failed writeAtomic left %v behind", left)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || !info.IsDir() {
		t.Errorf("the failed write disturbed the target: %v", statErr)
	}
}

func TestWriteAtomicFailsWhenTheDirectoryIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", metadataFileName)

	if err := writeAtomic(path, []byte("x")); err == nil {
		t.Fatal("writeAtomic = nil, want an error")
	}
}

// TestWriteAtomicSetsPermissionsRegardlessOfUmask covers the chmod that looks
// redundant next to os.CreateTemp's 0600: CreateTemp is subject to the process
// umask, and a record's mode is a decision rather than whatever the user's
// shell was configured with.
func TestWriteAtomicSetsPermissionsRegardlessOfUmask(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, metadataFileName)

	// Set after the temporary directory exists: a umask this hostile stops
	// the test framework from creating it.
	old := unix.Umask(0o377)
	t.Cleanup(func() { unix.Umask(old) })

	if err := writeAtomic(path, []byte("contents\n")); err != nil {
		t.Fatalf("writeAtomic = %v, want nil", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("mode = %#o, want %#o", perm, filePerm)
	}
}

// TestInterruptedWriteLeavesNoPartialRecord simulates the crash writeAtomic is
// built around: the process died after creating its temporary file and before
// renaming it. The temporary file is still on disk, holding half a record.
//
// Nothing may see it. The record is whatever the name still points at, and the
// residue is invisible to every read and cleaned up by removal.
func TestInterruptedWriteLeavesNoPartialRecord(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	const id = "7f3c9a1b2d04"
	good := Metadata{
		ID:        id,
		Image:     "alpine:3.20",
		Command:   []string{"/bin/sh"},
		Status:    StatusRunning,
		CreatedAt: time.Date(2026, time.August, 7, 18, 22, 1, 0, time.UTC),
	}
	if err := store.Save(good); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}

	dir, err := store.Dir(id)
	if err != nil {
		t.Fatal(err)
	}

	// The wreckage of a write that never landed: half a JSON object, under
	// the name writeAtomic would have used.
	partial := filepath.Join(dir, tempPrefix+"crashed")
	if err := os.WriteFile(partial, []byte(`{"schema":1,"id":"7f3c9a1b2d0`), filePerm); err != nil {
		t.Fatal(err)
	}

	got, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if got.Status != StatusRunning || got.Image != good.Image {
		t.Fatalf("Load returned %+v, want the record from before the crash", got)
	}

	records, errs := store.LoadAll()
	if len(errs) != 0 {
		t.Fatalf("LoadAll errors = %v, want none", errs)
	}
	if len(records) != 1 {
		t.Fatalf("LoadAll returned %d records, want 1", len(records))
	}

	// A later write succeeds over the residue rather than tripping on it...
	if err := store.Update(id, func(rec *Metadata) error {
		rec.Status = StatusExited
		return nil
	}); err != nil {
		t.Fatalf("Update after an interrupted write = %v, want nil", err)
	}
	if got, err := store.Load(id); err != nil || got.Status != StatusExited {
		t.Fatalf("Load = %+v, %v, want the updated record", got, err)
	}

	// ...and removal takes the residue with it, because the whole directory
	// goes.
	if err := store.Remove(id); err != nil {
		t.Fatalf("Remove = %v, want nil", err)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Remove left the interrupted write behind: %v", err)
	}
}

// TestSaveClaimsAnIDLeftByACrash covers the other half of the same crash: the
// directory was created but the record never landed, so the ID was never
// really claimed and Save must be able to take it.
func TestSaveClaimsAnIDLeftByACrash(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	const id = "7f3c9a1b2d04"
	dir := filepath.Join(root, stateDirName, containersDirName, id)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatal(err)
	}
	// A lock file from the process that died, too.
	if err := os.WriteFile(filepath.Join(dir, lockFileName), nil, filePerm); err != nil {
		t.Fatal(err)
	}

	m := Metadata{
		ID:        id,
		Status:    StatusCreating,
		CreatedAt: time.Date(2026, time.August, 7, 18, 22, 1, 0, time.UTC),
	}
	if err := store.Save(m); err != nil {
		t.Fatalf("Save over a crashed container's directory = %v, want nil", err)
	}
	if _, err := store.Load(id); err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
}

// TestLockDirExcludes proves the lock actually excludes, and does so between
// two holders in one process — which is the case flock is chosen for and the
// case a mutex would have handled while leaving two processes unprotected.
func TestLockDirExcludes(t *testing.T) {
	dir := t.TempDir()

	first, err := lockDir(dir)
	if err != nil {
		t.Fatalf("lockDir = %v, want nil", err)
	}

	acquired := make(chan struct{})
	failed := make(chan error, 1)
	go func() {
		second, err := lockDir(dir)
		if err != nil {
			failed <- err
			return
		}
		close(acquired)
		if err := second.unlock(); err != nil {
			failed <- err
		}
	}()

	// A best-effort check: the goroutine may not have reached the flock yet,
	// so this can pass a broken implementation, but it can never fail a
	// correct one. The real assertion is the receive below, which would hang
	// if the lock were never released.
	select {
	case <-acquired:
		t.Fatal("a second lock was taken while the first was held")
	case err := <-failed:
		t.Fatalf("second lockDir = %v, want it to block", err)
	default:
	}

	if err := first.unlock(); err != nil {
		t.Fatalf("unlock = %v, want nil", err)
	}

	select {
	case <-acquired:
	case err := <-failed:
		t.Fatalf("second lockDir = %v, want nil", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the lock was never released to the waiting holder")
	}

	select {
	case err := <-failed:
		t.Fatalf("second unlock = %v, want nil", err)
	default:
	}
}

func TestLockDirFailsWhenTheDirectoryIsMissing(t *testing.T) {
	if _, err := lockDir(filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
		t.Fatal("lockDir = nil, want an error")
	}
}

func TestStoreDirRejectsEscapingIDs(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\b`, ".hidden", "nul\x00byte"} {
		if _, err := store.Dir(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Dir(%q) = %v, want ErrInvalidID", id, err)
		}
	}
}

func TestStoreDirLayout(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Dir("7f3c9a1b2d04")
	if err != nil {
		t.Fatalf("Dir = %v, want nil", err)
	}
	if want := filepath.Join(root, "state", "containers", "7f3c9a1b2d04"); got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
	if store.Root() != root {
		t.Errorf("Root() = %q, want %q", store.Root(), root)
	}
}

func TestSyncDirRejectsAMissingDirectory(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
		t.Fatal("syncDir = nil, want an error")
	}
}
