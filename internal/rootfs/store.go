package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Directory permissions for the layout this package owns.
const (
	// storePerm keeps one container's filesystem out of reach of another's
	// unprivileged processes: the store holds every container's files.
	storePerm = 0o700

	// containerPerm applies the same reasoning to a single container's tree.
	containerPerm = 0o700

	// rootfsPerm is what a root filesystem conventionally is. It is the
	// directory the container itself sees as "/", so it is not Forge's to
	// lock down.
	rootfsPerm = 0o755
)

// rootfsDirName is the child of a container's directory that becomes its "/".
const rootfsDirName = "rootfs"

// hostMountInfo is the kernel's view of the calling process's mount table.
const hostMountInfo = "/proc/self/mountinfo"

// Dir is one container's prepared directory tree.
type Dir struct {
	// ID is the container the tree belongs to.
	ID string

	// Base is the container's directory, <root>/<id>.
	Base string

	// Rootfs is the directory that becomes the container's "/", <root>/<id>/rootfs.
	Rootfs string
}

// Store owns the directory holding every container's root filesystem.
//
// Construct it with NewStore.
type Store struct {
	root   string
	logger *slog.Logger

	// mountsUnder reports the mount points at or under a directory. It is a
	// field so tests can supply the answer without needing root and a real
	// mount; Remove's refusal to delete through a live mount is the single
	// most consequential branch in this package.
	mountsUnder func(dir string) ([]string, error)
}

// NewStore returns a Store rooted at root, creating that directory if it does
// not exist. root must be absolute: every later stage resolves it against the
// host root, so a relative path would silently depend on the working directory
// forge happened to be started in.
func NewStore(root string, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("container storage root must be an absolute path, got %q", root)
	}
	root = filepath.Clean(root)

	info, err := os.Stat(root)
	switch {
	case err == nil:
		if !info.IsDir() {
			return nil, fmt.Errorf("container storage root %q is not a directory", root)
		}
	case os.IsNotExist(err):
		if err := os.MkdirAll(root, storePerm); err != nil {
			return nil, fmt.Errorf("creating container storage root %q: %w", root, err)
		}
		// MkdirAll is subject to the process umask, and these permissions are
		// a decision rather than a default.
		if err := os.Chmod(root, storePerm); err != nil {
			return nil, fmt.Errorf("setting permissions on %q: %w", root, err)
		}
	default:
		return nil, fmt.Errorf("inspecting container storage root %q: %w", root, err)
	}

	return &Store{root: root, logger: logger, mountsUnder: mountsUnder}, nil
}

// Root returns the directory the store manages.
func (s *Store) Root() string { return s.root }

// Prepare creates the directory tree for a container and returns its paths.
//
// It refuses a container that already has one rather than adopting a tree whose
// contents it does not know.
func (s *Store) Prepare(id string) (Dir, error) {
	dir, err := s.dir(id)
	if err != nil {
		return Dir{}, err
	}

	if err := os.Mkdir(dir.Base, containerPerm); err != nil {
		if os.IsExist(err) {
			return Dir{}, fmt.Errorf("%w: %q", ErrAlreadyPrepared, dir.Base)
		}
		return Dir{}, fmt.Errorf("creating container directory %q: %w", dir.Base, err)
	}

	if err := s.populate(dir); err != nil {
		// A half-built tree is worse than none: leave the store as it was
		// (PRD NFR-8).
		if rmErr := os.RemoveAll(dir.Base); rmErr != nil {
			s.logger.Warn("removing a partially prepared container directory",
				"container_id", id, "path", dir.Base, "error", rmErr)
		}
		return Dir{}, err
	}

	s.logger.Debug("prepared container directory", "container_id", id, "rootfs", dir.Rootfs)

	return dir, nil
}

// populate creates the directories inside a container's tree.
func (s *Store) populate(dir Dir) error {
	if err := os.Chmod(dir.Base, containerPerm); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", dir.Base, err)
	}
	if err := os.Mkdir(dir.Rootfs, rootfsPerm); err != nil {
		return fmt.Errorf("creating container root filesystem %q: %w", dir.Rootfs, err)
	}
	if err := os.Chmod(dir.Rootfs, rootfsPerm); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", dir.Rootfs, err)
	}
	return nil
}

// Lookup returns the paths of a container that has already been prepared.
func (s *Store) Lookup(id string) (Dir, error) {
	dir, err := s.dir(id)
	if err != nil {
		return Dir{}, err
	}

	info, err := os.Stat(dir.Base)
	switch {
	case os.IsNotExist(err):
		return Dir{}, fmt.Errorf("%w: %q", ErrNotPrepared, dir.Base)
	case err != nil:
		return Dir{}, fmt.Errorf("inspecting container directory %q: %w", dir.Base, err)
	case !info.IsDir():
		return Dir{}, fmt.Errorf("%w: %q is not a directory", ErrNotPrepared, dir.Base)
	}

	return dir, nil
}

// Remove deletes a container's directory tree.
//
// It is idempotent: removing a container that was never prepared, or has
// already been removed, is not an error, so cleanup paths can call it
// unconditionally (SSOT §13.3).
//
// Removal ends in os.RemoveAll, which follows the directory tree wherever it
// leads. If a bind mount into the host filesystem were still live underneath,
// that walk would delete the host's files — an unrecoverable outcome. Remove
// therefore consults the mount table first and refuses to proceed if anything
// is mounted under the tree, including when it cannot tell.
func (s *Store) Remove(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	dir, err := s.dir(id)
	if err != nil {
		return err
	}

	if _, err := os.Lstat(dir.Base); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspecting container directory %q: %w", dir.Base, err)
	}

	mounts, err := s.mountsUnder(dir.Base)
	if err != nil {
		// Fail closed. Not knowing whether something is mounted is exactly the
		// situation in which deleting would be catastrophic.
		return fmt.Errorf("checking for mounts under %q before removing it: %w", dir.Base, err)
	}
	if len(mounts) > 0 {
		return fmt.Errorf("%w: %s", ErrStillMounted, strings.Join(mounts, ", "))
	}

	if err := os.RemoveAll(dir.Base); err != nil {
		return fmt.Errorf("removing container directory %q: %w", dir.Base, err)
	}

	s.logger.Debug("removed container directory", "container_id", id, "path", dir.Base)

	return nil
}

// dir computes a container's paths without touching the filesystem.
func (s *Store) dir(id string) (Dir, error) {
	if err := validateID(id); err != nil {
		return Dir{}, err
	}

	base := filepath.Join(s.root, id)
	return Dir{ID: id, Base: base, Rootfs: filepath.Join(base, rootfsDirName)}, nil
}

// mountsUnder returns the mount points at or under dir, as the calling process
// sees them.
//
// Reading the mount table is not mount *logic*: this package performs no mount
// operation and makes no decision about what a container should mount. It reads
// the table for the same reason it stats a directory before deleting it — to
// find out what it is about to do.
func mountsUnder(dir string) ([]string, error) {
	data, err := os.ReadFile(hostMountInfo)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", hostMountInfo, err)
	}

	prefix := strings.TrimSuffix(dir, string(filepath.Separator)) + string(filepath.Separator)

	var found []string
	for line := range strings.Lines(string(data)) {
		// mountinfo fields: id parent major:minor root mount-point ...
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		point := unescapeMountPoint(fields[4])
		if point == dir || strings.HasPrefix(point, prefix) {
			found = append(found, point)
		}
	}

	return found, nil
}

// unescapeMountPoint decodes the octal escapes the kernel writes into
// mountinfo for characters that would otherwise break its field separation.
func unescapeMountPoint(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}

	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] != '\\' || i+3 >= len(field) {
			out.WriteByte(field[i])
			continue
		}

		value, err := strconv.ParseUint(field[i+1:i+4], 8, 8)
		if err != nil {
			out.WriteByte(field[i])
			continue
		}
		out.WriteByte(byte(value))
		i += 3
	}

	return out.String()
}
