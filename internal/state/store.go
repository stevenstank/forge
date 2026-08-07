package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// The layout this package owns, relative to the store's root.
const (
	// stateDirName separates Forge's bookkeeping from the trees other
	// packages own under the same root — containers/ is internal/rootfs's,
	// images/ is internal/image's.
	stateDirName = "state"

	// containersDirName holds one directory per container.
	containersDirName = "containers"

	// metadataFileName is the record itself.
	metadataFileName = "metadata.json"

	// lockFileName is the flock target for a container's read-modify-write.
	// Its contents are never read; only the lock on it matters.
	lockFileName = ".lock"

	// tempPrefix marks a record being written. Dot-prefixed so LoadAll skips
	// it without needing to know the name, and so it can never collide with
	// a container ID (ValidateID refuses a leading dot).
	tempPrefix = ".metadata-"
)

// Permissions for what this package creates.
const (
	// dirPerm keeps one user's containers out of reach of another's. The
	// store is normally under a user's own data directory, but Forge also
	// runs as root, and a world-readable state directory would be one.
	dirPerm = 0o700

	// filePerm applies the same reasoning to a record, which holds the
	// container's command line.
	filePerm = 0o600
)

// Store is the on-disk container metadata store.
//
// Construct it with New. It holds no state of its own beyond its root: two
// Stores over the same directory are as consistent with each other as two
// processes are, which is the property the whole design turns on.
type Store struct {
	root string
	dir  string
}

// New returns a Store keeping records under root.
//
// root must be absolute. Every command resolves it independently, so a
// relative path would silently mean a different directory depending on where
// forge was started — and `forge ps` finding no containers because it was run
// from elsewhere is a bug that would take an afternoon to see.
//
// It performs no I/O. Nothing here can fail in a way that depends on the state
// of the host, so nothing here needs the host: a Forge that only ever runs
// `forge run --help` creates no directories. The store's directories are
// created by the first Save, which is also the first moment there is anything
// to put in them (SSOT §13, and the precedent image.NewCache set in Stage 5).
func New(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("state root is required")
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("state root must be an absolute path, got %q", root)
	}
	root = filepath.Clean(root)

	return &Store{
		root: root,
		dir:  filepath.Join(root, stateDirName, containersDirName),
	}, nil
}

// DefaultRoot returns the directory Forge keeps its state in when the user
// names none: $XDG_DATA_HOME/forge, or ~/.local/share/forge when
// XDG_DATA_HOME is unset, per the XDG Base Directory specification.
//
// It reads the environment, so it is the CLI's to call once and pass down;
// nothing below internal/cli should reach for it (SSOT §4, no global state).
func DefaultRoot() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "forge"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the home directory for the default state root: %w", err)
	}

	return filepath.Join(home, ".local", "share", "forge"), nil
}

// Root returns the directory the store was constructed with.
func (s *Store) Root() string { return s.root }

// Dir returns the directory holding a container's record. It touches nothing,
// and does not imply the directory exists.
func (s *Store) Dir(id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, id), nil
}

// path returns a container's record path.
func (s *Store) path(id string) (string, error) {
	dir, err := s.Dir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, metadataFileName), nil
}

// Save writes the first record for a container.
//
// It refuses a container that already has one, with ErrExists. Save is how an
// ID is claimed, and two containers sharing an ID would go on to share a root
// filesystem, a cgroup and an interface name — so the collision has to be
// caught here, where it is still one failed command rather than two containers
// deleting each other's resources. Changing a record that exists is Update's
// job, and the split is what makes the claim unambiguous.
//
// The check and the write happen under the container's lock, so two callers
// racing for the same ID produce one record and one ErrExists rather than two
// writes where the last one wins.
func (s *Store) Save(m Metadata) (err error) {
	if m.Schema == 0 {
		m.Schema = Schema
	}
	if err := m.Validate(); err != nil {
		return err
	}

	dir, err := s.Dir(m.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating the state directory %q: %w", dir, err)
	}

	lock, err := lockDir(dir)
	if err != nil {
		return err
	}
	// Joined rather than discarded: a lock that failed to release is not the
	// caller's problem to handle, but it is nobody's to hide (SSOT §13.7).
	defer func() { err = errors.Join(err, lock.unlock()) }()

	path := filepath.Join(dir, metadataFileName)
	switch _, err := os.Stat(path); {
	case err == nil:
		return fmt.Errorf("%w: %s", ErrExists, m.ID)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("checking for an existing record %q: %w", path, err)
	}

	return writeRecord(path, m)
}

// Load returns a container's record.
//
// It takes no lock. Writes replace the file by rename rather than editing it
// in place, so a read either sees the record that was there when it opened the
// file or the one that replaced it — never a mixture, and never a file that is
// half-written. That is what makes concurrent readers safe without
// coordinating with anyone.
func (s *Store) Load(id string) (Metadata, error) {
	path, err := s.path(id)
	if err != nil {
		return Metadata{}, err
	}

	return readRecord(path, id)
}

// LoadAll returns every record in the store, oldest first.
//
// It returns the records it could read *and* the errors it could not, rather
// than one or the other. A single corrupt file must not hide the other nine
// containers from `forge ps`: the caller prints what it got and warns about
// what it did not, which is the only behaviour that leaves a user able to act.
// A signature returning ([]Metadata, error) would let a caller write
// `if err != nil { return }` and lose the records — this one does not compile
// into that mistake.
//
// The store not existing yet is not an error; it is a host with no containers.
func (s *Store) LoadAll() ([]Metadata, []error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("reading the state directory %q: %w", s.dir, err)}
	}

	var (
		records []Metadata
		errs    []error
	)
	for _, entry := range entries {
		// Anything that is not a container directory is not this package's
		// to explain: a stray file here is somebody else's mistake, and
		// failing ps over it would be a worse one.
		if !entry.IsDir() || ValidateID(entry.Name()) != nil {
			continue
		}

		m, err := s.Load(entry.Name())
		switch {
		case errors.Is(err, ErrNotFound):
			// A directory with no record is a container that was removed, or
			// one whose Save never landed. Neither is a container.
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}

		records = append(records, m)
	}

	// Oldest first, ties broken by ID so the order is total. `forge ps`
	// reverses it; what matters here is that two calls agree.
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID < records[j].ID
		}
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})

	return records, errs
}

// Update applies mutate to a container's record and writes the result.
//
// The whole read-modify-write runs under the container's lock, so two callers
// changing a record at once take turns rather than interleaving — the second
// sees the first's result and mutates that, instead of overwriting it with a
// record it read before the first one wrote.
//
// It takes a function rather than a Metadata for the same reason. There is no
// way to express "read, change one field, write it back" that skips the lock,
// and no way for a caller to write back a record it read a minute ago and has
// been holding while somebody else moved the container to stopped.
//
// A mutate that returns an error abandons the update and leaves the record
// exactly as it was, which is how a caller refuses a transition it has decided
// is illegal.
func (s *Store) Update(id string, mutate func(*Metadata) error) (err error) {
	if mutate == nil {
		return fmt.Errorf("%w: no mutation given", ErrInvalid)
	}

	dir, err := s.Dir(id)
	if err != nil {
		return err
	}

	// Locking would otherwise create the directory for a container that does
	// not exist, leaving a lock file behind for every typo'd ID.
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return fmt.Errorf("inspecting the state directory %q: %w", dir, err)
	}

	lock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.unlock()) }()

	path := filepath.Join(dir, metadataFileName)
	m, err := readRecord(path, id)
	if err != nil {
		return err
	}

	if err := mutate(&m); err != nil {
		return err
	}

	// The ID is the record's own name on disk. Letting a mutation change it
	// would write one container's record into another's directory.
	if m.ID != id {
		return fmt.Errorf("%w: cannot change the id of %s to %q", ErrInvalid, id, m.ID)
	}
	m.Schema = Schema

	if err := m.Validate(); err != nil {
		return err
	}

	return writeRecord(path, m)
}

// Remove deletes a container's record and the directory holding it.
//
// It is idempotent: removing a container that has no record, or has already
// been removed, is not an error, so cleanup paths can call it unconditionally
// (SSOT §13.3). This is the last step of removing a container and it is
// deliberately the last: the record is the list of what to clean up, so
// deleting it before the things it describes would strand them with nothing
// left to name them.
//
// It takes the lock first, so a Remove racing an Update waits for that update
// to finish rather than deleting the file it is about to rename over.
func (s *Store) Remove(id string) (err error) {
	dir, err := s.Dir(id)
	if err != nil {
		return err
	}

	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspecting the state directory %q: %w", dir, err)
	}

	lock, err := lockDir(dir)
	if err != nil {
		return err
	}
	// The lock file is inside the tree being deleted. Releasing the lock
	// after the removal is still correct: flock lives on the open file
	// description, which stays valid after the name is gone, and the next
	// caller to lock this ID creates the file afresh.
	defer func() { err = errors.Join(err, lock.unlock()) }()

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing the state directory %q: %w", dir, err)
	}

	return nil
}

// readRecord loads and validates one record.
//
// Every way a file can be unusable is folded into two sentinels here —
// ErrNotFound for a container that has none, ErrCorrupt for one whose record
// cannot be trusted — because a caller can act on those and cannot act on
// "unexpected end of JSON input".
func readRecord(path, id string) (Metadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Metadata{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return Metadata{}, fmt.Errorf("reading the record %q: %w", path, err)
	}

	var m Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return Metadata{}, fmt.Errorf("%w: %s: %w", ErrCorrupt, id, err)
	}

	// The schema is checked before anything else is believed. A record from a
	// newer Forge may have fields this build would drop on the next write, so
	// it is refused rather than read — and refused without being rewritten.
	switch {
	case m.Schema == 0:
		return Metadata{}, fmt.Errorf("%w: %s: no schema version", ErrCorrupt, id)
	case m.Schema > Schema:
		return Metadata{}, fmt.Errorf("%w: %s was written with schema %d, this build understands %d",
			ErrSchema, id, m.Schema, Schema)
	}

	if err := m.Validate(); err != nil {
		return Metadata{}, fmt.Errorf("%w: %s: %w", ErrCorrupt, id, err)
	}

	// A record whose contents disagree with the directory it was found in has
	// been moved or hand-edited; either way it is not this container's.
	if m.ID != id {
		return Metadata{}, fmt.Errorf("%w: %s holds a record for %q", ErrCorrupt, id, m.ID)
	}

	return m, nil
}

// writeRecord encodes m and replaces path with it atomically.
func writeRecord(path string, m Metadata) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the record for %s: %w", m.ID, err)
	}
	// A trailing newline costs one byte and makes the file behave in a
	// terminal, which is most of the argument for JSON files over a database.
	data = append(data, '\n')

	return writeAtomic(path, data)
}
