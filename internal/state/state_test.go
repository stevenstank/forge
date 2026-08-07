package state_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/state"
)

// created is a fixed point in time, so a record's timestamps are values a test
// can assert on rather than whatever the clock said.
var created = time.Date(2026, time.August, 7, 18, 22, 1, 114233000, time.UTC)

// newStore returns a store over a temporary directory.
func newStore(t *testing.T) (*state.Store, string) {
	t.Helper()

	root := t.TempDir()
	store, err := state.New(root)
	if err != nil {
		t.Fatalf("New(%q) = %v, want nil", root, err)
	}

	return store, root
}

// sample returns a fully populated record: every field this package persists,
// so a round-trip test has something to lose.
func sample(id string) state.Metadata {
	started := created.Add(600 * time.Millisecond)
	finished := created.Add(9 * time.Second)
	code := 137

	return state.Metadata{
		ID:          id,
		Image:       "alpine:3.20",
		Command:     []string{"/bin/sh", "-c", "while true; do date; sleep 1; done"},
		PID:         41120,
		Status:      state.StatusStopped,
		ExitCode:    &code,
		CreatedAt:   created,
		StartedAt:   &started,
		FinishedAt:  &finished,
		RootfsPath:  "/var/lib/forge/containers/" + id + "/rootfs",
		NetworkMode: "bridge",
	}
}

// running returns the minimal record of a container that is still alive: no
// exit code, no finish time, which is the case the pointer fields exist for.
func running(id string) state.Metadata {
	started := created.Add(600 * time.Millisecond)

	return state.Metadata{
		ID:        id,
		Image:     "alpine:3.20",
		Command:   []string{"/bin/sh"},
		PID:       41120,
		Status:    state.StatusRunning,
		CreatedAt: created,
		StartedAt: &started,
	}
}

// recordPath is where a container's record lives, spelled out rather than
// asked of the store: the layout is part of what these tests assert.
func recordPath(root, id string) string {
	return filepath.Join(root, "state", "containers", id, "metadata.json")
}

// assertEqual compares two records field by field. reflect.DeepEqual is wrong
// for time.Time, which carries a monotonic reading and a location pointer that
// a JSON round-trip does not preserve even when the instant is identical.
func assertEqual(t *testing.T, got, want state.Metadata) {
	t.Helper()

	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
	if got.Image != want.Image {
		t.Errorf("Image = %q, want %q", got.Image, want.Image)
	}
	if len(got.Command) != len(want.Command) {
		t.Errorf("Command = %v, want %v", got.Command, want.Command)
	} else {
		for i := range want.Command {
			if got.Command[i] != want.Command[i] {
				t.Errorf("Command[%d] = %q, want %q", i, got.Command[i], want.Command[i])
			}
		}
	}
	if got.PID != want.PID {
		t.Errorf("PID = %d, want %d", got.PID, want.PID)
	}
	if got.Status != want.Status {
		t.Errorf("Status = %q, want %q", got.Status, want.Status)
	}
	assertIntPtr(t, "ExitCode", got.ExitCode, want.ExitCode)
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	assertTimePtr(t, "StartedAt", got.StartedAt, want.StartedAt)
	assertTimePtr(t, "FinishedAt", got.FinishedAt, want.FinishedAt)
	if got.RootfsPath != want.RootfsPath {
		t.Errorf("RootfsPath = %q, want %q", got.RootfsPath, want.RootfsPath)
	}
	if got.NetworkMode != want.NetworkMode {
		t.Errorf("NetworkMode = %q, want %q", got.NetworkMode, want.NetworkMode)
	}
	if got.Schema != state.Schema {
		t.Errorf("Schema = %d, want %d", got.Schema, state.Schema)
	}
}

func assertIntPtr(t *testing.T, name string, got, want *int) {
	t.Helper()

	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s = %v, want %v", name, fmtIntPtr(got), fmtIntPtr(want))
	case *got != *want:
		t.Errorf("%s = %d, want %d", name, *got, *want)
	}
}

func fmtIntPtr(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func assertTimePtr(t *testing.T, name string, got, want *time.Time) {
	t.Helper()

	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s = %v, want %v", name, got, want)
	case !got.Equal(*want):
		t.Errorf("%s = %v, want %v", name, *got, *want)
	}
}

func TestNewRejectsUnusableRoots(t *testing.T) {
	tests := []struct {
		name string
		root string
	}{
		{name: "empty", root: ""},
		{name: "relative", root: "forge/state"},
		{name: "bare relative", root: "."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := state.New(tc.root); err == nil {
				t.Fatalf("New(%q) = nil, want an error", tc.root)
			}
		})
	}
}

// TestNewPerformsNoIO pins the invariant that constructing a store touches
// nothing. A forge that only prints its usage must leave no directories
// behind.
func TestNewPerformsNoIO(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")

	store, err := state.New(root)
	if err != nil {
		t.Fatalf("New(%q) = %v, want nil", root, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("New created %q; it must perform no I/O", root)
	}

	// Reading is also free: a store with no directory has no containers,
	// which is not an error condition.
	records, errs := store.LoadAll()
	if len(records) != 0 || len(errs) != 0 {
		t.Fatalf("LoadAll on an empty root = %v, %v, want none of either", records, errs)
	}
	if _, err := store.Load("7f3c9a1b2d04"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Load on an empty root = %v, want ErrNotFound", err)
	}
}

func TestDefaultRoot(t *testing.T) {
	t.Run("honours XDG_DATA_HOME", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/xdg/data")

		got, err := state.DefaultRoot()
		if err != nil {
			t.Fatalf("DefaultRoot() = %v, want nil", err)
		}
		if want := "/xdg/data/forge"; got != want {
			t.Fatalf("DefaultRoot() = %q, want %q", got, want)
		}
	})

	t.Run("falls back to the XDG default", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "/home/tester")

		got, err := state.DefaultRoot()
		if err != nil {
			t.Fatalf("DefaultRoot() = %v, want nil", err)
		}
		if want := "/home/tester/.local/share/forge"; got != want {
			t.Fatalf("DefaultRoot() = %q, want %q", got, want)
		}
	})

	t.Run("ignores a relative XDG_DATA_HOME", func(t *testing.T) {
		// The specification says a relative XDG_DATA_HOME is invalid and must
		// be ignored, and a relative state root is one New would refuse
		// anyway.
		t.Setenv("XDG_DATA_HOME", "relative/data")
		t.Setenv("HOME", "/home/tester")

		got, err := state.DefaultRoot()
		if err != nil {
			t.Fatalf("DefaultRoot() = %v, want nil", err)
		}
		if want := "/home/tester/.local/share/forge"; got != want {
			t.Fatalf("DefaultRoot() = %q, want %q", got, want)
		}
	})
}

func TestSaveAndLoad(t *testing.T) {
	store, root := newStore(t)
	want := sample("7f3c9a1b2d04")

	if err := store.Save(want); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}

	got, err := store.Load(want.ID)
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	assertEqual(t, got, want)

	// The layout is documented, so it is asserted.
	if _, err := os.Stat(recordPath(root, want.ID)); err != nil {
		t.Fatalf("record not at the documented path: %v", err)
	}
}

// TestSaveWritesRestrictivePermissions guards the one thing a record leaks if
// the mode is wrong: the container's command line, which routinely carries
// arguments the user would not paste into a shared terminal.
func TestSaveWritesRestrictivePermissions(t *testing.T) {
	store, root := newStore(t)
	m := sample("7f3c9a1b2d04")

	if err := store.Save(m); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}

	info, err := os.Stat(recordPath(root, m.ID))
	if err != nil {
		t.Fatalf("Stat = %v, want nil", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("record mode = %#o, want %#o", perm, 0o600)
	}

	dir, err := store.Dir(m.ID)
	if err != nil {
		t.Fatalf("Dir = %v, want nil", err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat = %v, want nil", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %#o, want %#o", perm, 0o700)
	}
}

// startedEarly, finishedEarly and wrongSchema are the coherence failures: a
// record whose own fields disagree. They are worth refusing at the write
// because the alternative is explaining a negative uptime in ps later.
func startedEarly() state.Metadata {
	m := running("7f3c9a1b2d04")
	early := created.Add(-time.Second)
	m.StartedAt = &early
	return m
}

func finishedEarly() state.Metadata {
	m := sample("7f3c9a1b2d04")
	between := created.Add(time.Millisecond)
	m.FinishedAt = &between
	return m
}

func wrongSchema() state.Metadata {
	m := running("7f3c9a1b2d04")
	m.Schema = state.Schema + 1
	return m
}

func TestSaveRejectsInvalidMetadata(t *testing.T) {
	noStart := sample("7f3c9a1b2d04")
	noStart.StartedAt = nil
	early := created.Add(-time.Second)
	noStart.FinishedAt = &early

	badCode := sample("7f3c9a1b2d04")
	code := 900
	badCode.ExitCode = &code

	tests := []struct {
		name string
		m    state.Metadata
		want error
	}{
		{
			name: "no id",
			m:    state.Metadata{Status: state.StatusRunning, CreatedAt: created},
			want: state.ErrInvalidID,
		},
		{
			name: "id with a separator",
			m:    state.Metadata{ID: "../escape", Status: state.StatusRunning, CreatedAt: created},
			want: state.ErrInvalidID,
		},
		{
			name: "dot-prefixed id",
			m:    state.Metadata{ID: ".lock", Status: state.StatusRunning, CreatedAt: created},
			want: state.ErrInvalidID,
		},
		{
			name: "unknown status",
			m:    state.Metadata{ID: "7f3c9a1b2d04", Status: "wedged", CreatedAt: created},
			want: state.ErrInvalid,
		},
		{
			name: "no created time",
			m:    state.Metadata{ID: "7f3c9a1b2d04", Status: state.StatusRunning},
			want: state.ErrInvalid,
		},
		{
			name: "negative pid",
			m:    state.Metadata{ID: "7f3c9a1b2d04", Status: state.StatusRunning, CreatedAt: created, PID: -1},
			want: state.ErrInvalid,
		},
		{name: "finished before created", m: noStart, want: state.ErrInvalid},
		{name: "exit code out of range", m: badCode, want: state.ErrInvalid},
		{name: "started before created", m: startedEarly(), want: state.ErrInvalid},
		{name: "finished before started", m: finishedEarly(), want: state.ErrInvalid},
		{name: "schema this build does not write", m: wrongSchema(), want: state.ErrInvalid},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, root := newStore(t)

			err := store.Save(tc.m)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Save = %v, want %v", err, tc.want)
			}

			// Rejection happens before anything is written, so an invalid
			// record leaves no trace to clean up.
			if entries, err := os.ReadDir(filepath.Join(root, "state", "containers")); err == nil && len(entries) > 0 {
				t.Errorf("a rejected Save left %d directories behind", len(entries))
			}
		})
	}
}

// TestDuplicateIDs is the collision case. Two containers sharing an ID would
// go on to share a rootfs, a cgroup and an interface name, so the second Save
// must fail and — just as importantly — must not overwrite the first.
func TestDuplicateIDs(t *testing.T) {
	store, _ := newStore(t)
	first := sample("7f3c9a1b2d04")

	if err := store.Save(first); err != nil {
		t.Fatalf("first Save = %v, want nil", err)
	}

	second := running("7f3c9a1b2d04")
	second.Image = "busybox:latest"

	if err := store.Save(second); !errors.Is(err, state.ErrExists) {
		t.Fatalf("second Save = %v, want ErrExists", err)
	}

	got, err := store.Load(first.ID)
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	assertEqual(t, got, first)
}

// TestConcurrentDuplicateSaves runs the same collision through the lock. With
// the check and the write serialised, exactly one caller may win; without the
// lock, two callers would both see no record and both write one.
func TestConcurrentDuplicateSaves(t *testing.T) {
	store, _ := newStore(t)

	const callers = 8
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	done.Add(callers)

	results := make([]error, callers)
	for i := range callers {
		go func() {
			defer done.Done()

			m := running("7f3c9a1b2d04")
			m.PID = 1000 + i

			start.Wait()
			results[i] = store.Save(m)
		}()
	}

	start.Done()
	done.Wait()

	var winners, exists int
	for i, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, state.ErrExists):
			exists++
		default:
			t.Errorf("caller %d: Save = %v, want nil or ErrExists", i, err)
		}
	}

	if winners != 1 {
		t.Errorf("%d callers claimed the id, want exactly 1", winners)
	}
	if exists != callers-1 {
		t.Errorf("%d callers saw ErrExists, want %d", exists, callers-1)
	}

	// Whoever won, the record on disk is one of theirs and is readable.
	got, err := store.Load("7f3c9a1b2d04")
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if got.PID < 1000 || got.PID >= 1000+callers {
		t.Errorf("PID = %d, want one of the callers' values", got.PID)
	}
}

// TestSaveReportsAnUnusableStore covers the failure a caller can actually hit
// on a misconfigured host: the state root exists but cannot hold containers.
func TestSaveReportsAnUnusableStore(t *testing.T) {
	root := t.TempDir()
	store, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}

	// A file where the containers directory belongs.
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state", "containers"), []byte("in the way\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.Save(running("7f3c9a1b2d04")); err == nil {
		t.Fatal("Save = nil, want an error")
	}

	// LoadAll reports it too, rather than claiming the host has no
	// containers: an unreadable store and an empty one are different answers.
	if _, errs := store.LoadAll(); len(errs) != 1 {
		t.Fatalf("LoadAll errors = %v, want exactly one", errs)
	}
}

func TestLoadNotFound(t *testing.T) {
	store, _ := newStore(t)

	if err := store.Save(sample("7f3c9a1b2d04")); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}

	if _, err := store.Load("0000deadbeef"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Load of an unknown container = %v, want ErrNotFound", err)
	}
	if _, err := store.Load("../escape"); !errors.Is(err, state.ErrInvalidID) {
		t.Fatalf("Load of an invalid id = %v, want ErrInvalidID", err)
	}
}

func TestLoadAll(t *testing.T) {
	store, _ := newStore(t)

	ids := []string{"cccccccccccc", "aaaaaaaaaaaa", "bbbbbbbbbbbb"}
	for i, id := range ids {
		m := running(id)
		// Distinct creation times, deliberately out of insertion order.
		m.CreatedAt = created.Add(time.Duration(len(ids)-i) * time.Minute)
		m.StartedAt = nil
		if err := store.Save(m); err != nil {
			t.Fatalf("Save(%s) = %v, want nil", id, err)
		}
	}

	records, errs := store.LoadAll()
	if len(errs) != 0 {
		t.Fatalf("LoadAll errors = %v, want none", errs)
	}
	if len(records) != len(ids) {
		t.Fatalf("LoadAll returned %d records, want %d", len(records), len(ids))
	}

	// Oldest first: the record saved last was created first.
	want := []string{"bbbbbbbbbbbb", "aaaaaaaaaaaa", "cccccccccccc"}
	for i, id := range want {
		if records[i].ID != id {
			t.Errorf("records[%d].ID = %q, want %q", i, records[i].ID, id)
		}
	}
}

// TestLoadAllIgnoresForeignEntries covers the residue a crash or a curious
// user leaves in the state directory. None of it is a container, and none of
// it may stop ps from listing the containers that are.
func TestLoadAllIgnoresForeignEntries(t *testing.T) {
	store, root := newStore(t)
	containers := filepath.Join(root, "state", "containers")

	if err := store.Save(running("7f3c9a1b2d04")); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}

	// A directory that never got its record: a Save that died between the
	// mkdir and the rename.
	if err := os.MkdirAll(filepath.Join(containers, "0000deadbeef"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A stray file where a directory belongs.
	if err := os.WriteFile(filepath.Join(containers, "README"), []byte("not a container\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A dot-prefixed directory, which no container ID can ever be.
	if err := os.MkdirAll(filepath.Join(containers, ".tmp-work"), 0o700); err != nil {
		t.Fatal(err)
	}

	records, errs := store.LoadAll()
	if len(errs) != 0 {
		t.Fatalf("LoadAll errors = %v, want none", errs)
	}
	if len(records) != 1 || records[0].ID != "7f3c9a1b2d04" {
		t.Fatalf("LoadAll = %v, want exactly the one real container", records)
	}
}

func TestUpdate(t *testing.T) {
	store, _ := newStore(t)
	m := running("7f3c9a1b2d04")

	if err := store.Save(m); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}

	finished := created.Add(30 * time.Second)
	code := 0
	err := store.Update(m.ID, func(rec *state.Metadata) error {
		if rec.Status != state.StatusRunning {
			t.Errorf("Update saw status %q, want %q", rec.Status, state.StatusRunning)
		}
		rec.Status = state.StatusExited
		rec.ExitCode = &code
		rec.FinishedAt = &finished
		return nil
	})
	if err != nil {
		t.Fatalf("Update = %v, want nil", err)
	}

	got, err := store.Load(m.ID)
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if got.Status != state.StatusExited {
		t.Errorf("Status = %q, want %q", got.Status, state.StatusExited)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", fmtIntPtr(got.ExitCode))
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Errorf("FinishedAt = %v, want %v", got.FinishedAt, finished)
	}
	// Untouched fields survive a partial update.
	if got.Image != m.Image || got.PID != m.PID {
		t.Errorf("Update disturbed fields it was not given: %+v", got)
	}
}

func TestUpdateFailures(t *testing.T) {
	store, _ := newStore(t)
	m := running("7f3c9a1b2d04")

	if err := store.Save(m); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}

	t.Run("unknown container", func(t *testing.T) {
		err := store.Update("0000deadbeef", func(*state.Metadata) error { return nil })
		if !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("Update = %v, want ErrNotFound", err)
		}
	})

	t.Run("mutation refuses", func(t *testing.T) {
		sentinel := errors.New("not a legal transition")

		err := store.Update(m.ID, func(rec *state.Metadata) error {
			rec.Status = state.StatusExited
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("Update = %v, want the mutation's error", err)
		}

		got, err := store.Load(m.ID)
		if err != nil {
			t.Fatalf("Load = %v, want nil", err)
		}
		if got.Status != state.StatusRunning {
			t.Fatalf("Status = %q, want the record left untouched at %q", got.Status, state.StatusRunning)
		}
	})

	t.Run("mutation produces an invalid record", func(t *testing.T) {
		err := store.Update(m.ID, func(rec *state.Metadata) error {
			rec.Status = "wedged"
			return nil
		})
		if !errors.Is(err, state.ErrInvalid) {
			t.Fatalf("Update = %v, want ErrInvalid", err)
		}

		got, err := store.Load(m.ID)
		if err != nil {
			t.Fatalf("Load = %v, want nil", err)
		}
		if got.Status != state.StatusRunning {
			t.Fatalf("Status = %q, want the record left untouched", got.Status)
		}
	})

	t.Run("mutation changes the id", func(t *testing.T) {
		err := store.Update(m.ID, func(rec *state.Metadata) error {
			rec.ID = "0000deadbeef"
			return nil
		})
		if !errors.Is(err, state.ErrInvalid) {
			t.Fatalf("Update = %v, want ErrInvalid", err)
		}

		if _, err := store.Load(m.ID); err != nil {
			t.Fatalf("Load = %v, want the original record intact", err)
		}
	})

	t.Run("no mutation given", func(t *testing.T) {
		if err := store.Update(m.ID, nil); !errors.Is(err, state.ErrInvalid) {
			t.Fatalf("Update = %v, want ErrInvalid", err)
		}
	})
}

// TestUpdateDoesNotCreateDirectories keeps a typo'd container ID from
// littering the store with lock files for containers that never existed.
func TestUpdateDoesNotCreateDirectories(t *testing.T) {
	store, root := newStore(t)

	if err := store.Update("0000deadbeef", func(*state.Metadata) error { return nil }); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Update = %v, want ErrNotFound", err)
	}

	if _, err := os.Stat(filepath.Join(root, "state", "containers", "0000deadbeef")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Update created a directory for a container with no record")
	}
}

// TestRemoveIsIdempotent is SSOT §13.3 for this package: cleanup paths call
// Remove unconditionally, including for containers that were never saved and
// ones already removed.
func TestRemoveIsIdempotent(t *testing.T) {
	store, root := newStore(t)
	m := sample("7f3c9a1b2d04")

	if err := store.Save(m); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}

	for i := range 3 {
		if err := store.Remove(m.ID); err != nil {
			t.Fatalf("Remove call %d = %v, want nil", i+1, err)
		}
	}
	if err := store.Remove("0000deadbeef"); err != nil {
		t.Fatalf("Remove of a container that never existed = %v, want nil", err)
	}

	if _, err := store.Load(m.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("Load after Remove = %v, want ErrNotFound", err)
	}

	// The whole directory goes, lock file included.
	if _, err := os.Stat(filepath.Join(root, "state", "containers", m.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove left the container directory behind")
	}

	// And the ID is free again afterwards.
	if err := store.Save(m); err != nil {
		t.Fatalf("Save after Remove = %v, want nil", err)
	}
}

func TestRemoveRejectsInvalidID(t *testing.T) {
	store, _ := newStore(t)

	if err := store.Remove("../escape"); !errors.Is(err, state.ErrInvalidID) {
		t.Fatalf("Remove = %v, want ErrInvalidID", err)
	}
}

// TestRestartRecovery is the point of the package: a record written by one
// process is read back, whole, by another. A fresh Store over the same
// directory stands in for the fresh process.
func TestRestartRecovery(t *testing.T) {
	root := t.TempDir()

	before, err := state.New(root)
	if err != nil {
		t.Fatalf("New = %v, want nil", err)
	}

	want := map[string]state.Metadata{
		"7f3c9a1b2d04": sample("7f3c9a1b2d04"),
		"0000deadbeef": running("0000deadbeef"),
	}
	// A container with no image and no network, which is what Stages 1 and 2
	// produce and what the omitempty fields have to survive.
	minimal := state.Metadata{
		ID:        "1111222233ff",
		Command:   []string{"/bin/true"},
		Status:    state.StatusCreating,
		CreatedAt: created,
	}
	want[minimal.ID] = minimal

	for _, m := range want {
		if err := before.Save(m); err != nil {
			t.Fatalf("Save(%s) = %v, want nil", m.ID, err)
		}
	}

	// The process ends here. Everything below knows only the directory.
	after, err := state.New(root)
	if err != nil {
		t.Fatalf("New = %v, want nil", err)
	}

	records, errs := after.LoadAll()
	if len(errs) != 0 {
		t.Fatalf("LoadAll errors = %v, want none", errs)
	}
	if len(records) != len(want) {
		t.Fatalf("LoadAll returned %d records, want %d", len(records), len(want))
	}

	for _, got := range records {
		assertEqual(t, got, want[got.ID])
	}

	// And a record recovered after a restart is still writable: recovery is
	// not a read-only mode.
	if err := after.Update("0000deadbeef", func(rec *state.Metadata) error {
		rec.Status = state.StatusExited
		return nil
	}); err != nil {
		t.Fatalf("Update after restart = %v, want nil", err)
	}
}

// TestCorruptedMetadata covers every way a record can be unreadable. In all of
// them Load reports a sentinel the caller can act on, the file is left exactly
// as it was, and LoadAll returns the containers that are still fine.
func TestCorruptedMetadata(t *testing.T) {
	newer := `{"schema":99,"id":"cccccccccccc","status":"running","created_at":"2026-08-07T18:22:01Z"}`

	tests := []struct {
		name    string
		content string
		want    error
	}{
		{name: "empty file", content: "", want: state.ErrCorrupt},
		{
			name:    "truncated mid-write",
			content: `{"schema":1,"id":"cccccccccccc","status":"run`,
			want:    state.ErrCorrupt,
		},
		{name: "not json at all", content: "\x00\x00\x00\x00", want: state.ErrCorrupt},
		{name: "json but not an object", content: `["nope"]`, want: state.ErrCorrupt},
		{
			name:    "no schema version",
			content: `{"id":"cccccccccccc","status":"running","created_at":"2026-08-07T18:22:01Z"}`,
			want:    state.ErrCorrupt,
		},
		{
			name:    "unknown status",
			content: `{"schema":1,"id":"cccccccccccc","status":"wedged","created_at":"2026-08-07T18:22:01Z"}`,
			want:    state.ErrCorrupt,
		},
		{
			name:    "no created time",
			content: `{"schema":1,"id":"cccccccccccc","status":"running"}`,
			want:    state.ErrCorrupt,
		},
		{
			name:    "record belongs to another container",
			content: `{"schema":1,"id":"someone-else","status":"running","created_at":"2026-08-07T18:22:01Z"}`,
			want:    state.ErrCorrupt,
		},
		{name: "written by a newer forge", content: newer, want: state.ErrSchema},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, root := newStore(t)

			// One healthy container, so the test can assert that the bad
			// record does not take it down with it.
			healthy := running("7f3c9a1b2d04")
			if err := store.Save(healthy); err != nil {
				t.Fatalf("Save = %v, want nil", err)
			}

			const id = "cccccccccccc"
			dir := filepath.Join(root, "state", "containers", id)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "metadata.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := store.Load(id); !errors.Is(err, tc.want) {
				t.Fatalf("Load = %v, want %v", err, tc.want)
			}

			// The file is left exactly as it was: this package never repairs,
			// because a repair is a guess about what the bytes meant.
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile = %v, want nil", err)
			}
			if string(after) != tc.content {
				t.Errorf("Load rewrote the record:\n got %q\nwant %q", after, tc.content)
			}

			// One bad record must not hide the good ones.
			records, errs := store.LoadAll()
			if len(records) != 1 || records[0].ID != healthy.ID {
				t.Errorf("LoadAll = %v, want the one healthy container", records)
			}
			if len(errs) != 1 {
				t.Fatalf("LoadAll errors = %v, want exactly one", errs)
			}
			if !errors.Is(errs[0], tc.want) {
				t.Errorf("LoadAll error = %v, want %v", errs[0], tc.want)
			}

			// And the wreckage is still removable, which is how a user gets
			// out of this state without reaching for rm -rf.
			if err := store.Remove(id); err != nil {
				t.Fatalf("Remove = %v, want nil", err)
			}
		})
	}
}

// TestUpdateRefusesCorruptRecord keeps a bad record from being rewritten into
// a plausible-looking one. Update reads before it writes, and a read it cannot
// trust ends the update.
func TestUpdateRefusesCorruptRecord(t *testing.T) {
	store, root := newStore(t)

	const id = "cccccccccccc"
	dir := filepath.Join(root, "state", "containers", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"schema":1,`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := store.Update(id, func(rec *state.Metadata) error {
		rec.Status = state.StatusExited
		return nil
	})
	if !errors.Is(err, state.ErrCorrupt) {
		t.Fatalf("Update = %v, want ErrCorrupt", err)
	}
}

// TestConcurrentReads runs readers against a record that is being rewritten
// underneath them. Every read must return a whole, valid record — the old one
// or the new one, never a mixture and never a parse error. Run under -race,
// this also covers the store having no shared mutable state of its own.
func TestConcurrentReads(t *testing.T) {
	store, _ := newStore(t)
	m := running("7f3c9a1b2d04")

	if err := store.Save(m); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}

	const (
		readers = 8
		writes  = 60
	)

	var (
		start   sync.WaitGroup
		writing sync.WaitGroup
		reading sync.WaitGroup
	)
	start.Add(1)
	writing.Add(1)
	reading.Add(readers)

	// One writer, moving the PID through a known sequence. The writer being
	// finished is what tells the readers to stop, so there is no sleep and no
	// arbitrary deadline (SSOT §7).
	writeErrs := make(chan error, writes)
	go func() {
		defer writing.Done()

		start.Wait()
		for i := range writes {
			err := store.Update(m.ID, func(rec *state.Metadata) error {
				rec.PID = 1000 + i
				return nil
			})
			if err != nil {
				writeErrs <- err
				return
			}
		}
	}()

	done := make(chan struct{})
	readErrs := make(chan error, readers*4)
	counts := make([]int, readers)
	for r := range readers {
		go func() {
			defer reading.Done()

			start.Wait()
			for {
				select {
				case <-done:
					return
				default:
				}

				got, err := store.Load(m.ID)
				if err != nil {
					readErrs <- err
					return
				}
				// A torn read would fail one of these long before it failed
				// to parse: the record is whole or it is nothing.
				if got.ID != m.ID || got.Status != state.StatusRunning || got.Image != m.Image {
					readErrs <- errors.New("read a record that was never written: " + got.ID + " " + string(got.Status))
					return
				}
				if got.PID != m.PID && (got.PID < 1000 || got.PID >= 1000+writes) {
					readErrs <- errors.New("read a pid that was never written")
					return
				}
				counts[r]++
			}
		}()
	}

	start.Done()
	writing.Wait()
	close(done)
	reading.Wait()
	close(writeErrs)
	close(readErrs)

	for err := range writeErrs {
		t.Errorf("Update during concurrent reads = %v, want nil", err)
	}
	for err := range readErrs {
		t.Errorf("Load during concurrent writes = %v, want a whole record", err)
	}

	var total int
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		t.Fatal("no reads completed; the test proved nothing")
	}

	// The last write is the one that survives.
	got, err := store.Load(m.ID)
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if want := 1000 + writes - 1; got.PID != want {
		t.Errorf("final PID = %d, want %d", got.PID, want)
	}
}

// TestConcurrentUpdatesSerialise is the read-modify-write half of the same
// question. Without the lock, updates that read the same record and write
// different results would lose each other's changes; with it, every increment
// lands.
func TestConcurrentUpdatesSerialise(t *testing.T) {
	store, _ := newStore(t)
	m := running("7f3c9a1b2d04")
	m.PID = 0

	if err := store.Save(m); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}

	const (
		writers = 8
		each    = 25
	)

	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	done.Add(writers)

	errs := make(chan error, writers*each)
	for range writers {
		go func() {
			defer done.Done()

			start.Wait()
			for range each {
				err := store.Update(m.ID, func(rec *state.Metadata) error {
					rec.PID++
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	start.Done()
	done.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("Update = %v, want nil", err)
	}

	got, err := store.Load(m.ID)
	if err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
	if want := writers * each; got.PID != want {
		t.Fatalf("PID = %d, want %d: %d updates were lost", got.PID, want, want-got.PID)
	}
}

// TestConcurrentLoadAllAndRemove covers the other pair of racing verbs: a
// listing that overlaps a removal must return a consistent set, not an error.
func TestConcurrentLoadAllAndRemove(t *testing.T) {
	store, _ := newStore(t)

	ids := []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "cccccccccccc", "dddddddddddd"}
	for _, id := range ids {
		if err := store.Save(running(id)); err != nil {
			t.Fatalf("Save(%s) = %v, want nil", id, err)
		}
	}

	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	done.Add(2)

	var listErrs []error
	go func() {
		defer done.Done()

		start.Wait()
		for range 50 {
			_, errs := store.LoadAll()
			listErrs = append(listErrs, errs...)
		}
	}()

	removeErrs := make(chan error, len(ids))
	go func() {
		defer done.Done()

		start.Wait()
		for _, id := range ids {
			if err := store.Remove(id); err != nil {
				removeErrs <- err
			}
		}
	}()

	start.Done()
	done.Wait()
	close(removeErrs)

	for err := range removeErrs {
		t.Errorf("Remove = %v, want nil", err)
	}
	for _, err := range listErrs {
		t.Errorf("LoadAll during removals = %v, want none", err)
	}
}

func TestStatusTransitions(t *testing.T) {
	tests := []struct {
		from, to state.Status
		ok       bool
	}{
		{from: state.StatusCreating, to: state.StatusCreated, ok: true},
		{from: state.StatusCreating, to: state.StatusExited, ok: true},
		{from: state.StatusCreating, to: state.StatusRunning, ok: false},
		{from: state.StatusCreated, to: state.StatusRunning, ok: true},
		{from: state.StatusRunning, to: state.StatusStopping, ok: true},
		{from: state.StatusRunning, to: state.StatusExited, ok: true},
		{from: state.StatusRunning, to: state.StatusStopped, ok: true},
		{from: state.StatusRunning, to: state.StatusCreated, ok: false},
		{from: state.StatusStopping, to: state.StatusStopped, ok: true},
		{from: state.StatusStopping, to: state.StatusRunning, ok: false},
		// Nothing leaves a terminal state except removal: a supervisor
		// presumed dead cannot resurrect a container already reconciled.
		{from: state.StatusExited, to: state.StatusRemoving, ok: true},
		{from: state.StatusExited, to: state.StatusRunning, ok: false},
		{from: state.StatusStopped, to: state.StatusRunning, ok: false},
		{from: state.StatusRemoving, to: state.StatusExited, ok: false},
		// Re-asserting the current state is what an idempotent retry does.
		{from: state.StatusRunning, to: state.StatusRunning, ok: true},
		{from: "wedged", to: state.StatusRunning, ok: false},
		{from: state.StatusRunning, to: "wedged", ok: false},
	}

	for _, tc := range tests {
		t.Run(string(tc.from)+" to "+string(tc.to), func(t *testing.T) {
			err := tc.from.CanTransitionTo(tc.to)
			if tc.ok && err != nil {
				t.Fatalf("CanTransitionTo = %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("CanTransitionTo = nil, want an error")
			}
		})
	}
}

func TestStatusPredicates(t *testing.T) {
	tests := []struct {
		status                  state.Status
		valid, terminal, active bool
	}{
		{status: state.StatusCreating, valid: true},
		{status: state.StatusCreated, valid: true, active: true},
		{status: state.StatusRunning, valid: true, active: true},
		{status: state.StatusStopping, valid: true, active: true},
		{status: state.StatusExited, valid: true, terminal: true},
		{status: state.StatusStopped, valid: true, terminal: true},
		{status: state.StatusRemoving, valid: true},
		{status: "wedged"},
		{status: ""},
	}

	for _, tc := range tests {
		t.Run("status "+string(tc.status), func(t *testing.T) {
			if got := tc.status.Valid(); got != tc.valid {
				t.Errorf("Valid() = %v, want %v", got, tc.valid)
			}
			if got := tc.status.Terminal(); got != tc.terminal {
				t.Errorf("Terminal() = %v, want %v", got, tc.terminal)
			}
			m := state.Metadata{Status: tc.status}
			if got := m.Running(); got != tc.active {
				t.Errorf("Running() = %v, want %v", got, tc.active)
			}
		})
	}
}

// TestSavedRecordIsReadableJSON is the argument for JSON files over a database
// (ADR-0006), held to account: a record has to be legible to a person with a
// terminal and no Forge.
func TestSavedRecordIsReadableJSON(t *testing.T) {
	store, root := newStore(t)
	m := sample("7f3c9a1b2d04")

	if err := store.Save(m); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}

	data, err := os.ReadFile(recordPath(root, m.ID))
	if err != nil {
		t.Fatalf("ReadFile = %v, want nil", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("the record is not readable JSON: %v", err)
	}
	for _, key := range []string{
		"schema", "id", "image", "command", "pid", "status",
		"exit_code", "created_at", "started_at", "finished_at",
		"rootfs_path", "network_mode",
	} {
		if _, ok := generic[key]; !ok {
			t.Errorf("record is missing the %q field", key)
		}
	}

	if data[len(data)-1] != '\n' {
		t.Error("record does not end in a newline; it will not behave in a terminal")
	}
}

// TestUnknownFieldsAreNotPreserved documents a real limitation rather than
// asserting a feature: this build drops fields it does not know. It is why
// readRecord refuses a record whose schema is newer instead of rewriting it.
func TestUnknownFieldsAreNotPreserved(t *testing.T) {
	store, root := newStore(t)

	const id = "7f3c9a1b2d04"
	dir := filepath.Join(root, "state", "containers", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"schema":1,"id":"7f3c9a1b2d04","status":"running",` +
		`"created_at":"2026-08-07T18:22:01Z","hostname":"from-a-future-forge"}`
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.Update(id, func(rec *state.Metadata) error {
		rec.Status = state.StatusExited
		return nil
	}); err != nil {
		t.Fatalf("Update = %v, want nil", err)
	}

	data, err := os.ReadFile(recordPath(root, id))
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatal(err)
	}
	if _, ok := generic["hostname"]; ok {
		t.Fatal("an unknown field survived a rewrite; update this test and the schema rules if that becomes intentional")
	}
}
