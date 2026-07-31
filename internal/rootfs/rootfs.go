// Package rootfs owns the on-disk layout of container root filesystems.
//
// It answers two questions and nothing else: where a container's files live,
// and whether a directory the caller offered can serve as one. Per SSOT §2 it
// performs no mount operations — it produces paths, and internal/mount is told
// what to do with them by internal/runtime.
//
// The layout it owns, per FR-2.4:
//
//	<root>/                     the storage root, from --root
//	└── <container-id>/         one container's tree
//	    └── rootfs/             becomes the container's "/"
//
// The intermediate directory exists so later stages have somewhere to put a
// container's other on-disk state beside its filesystem.
//
// In Stage 2 the content of rootfs/ comes from a host directory the user names
// with --rootfs, bind-mounted over it inside the container's mount namespace.
// Stage 5 replaces that bind with unpacked image layers written into the same
// directory; nothing in this package changes when it does.
package rootfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Sentinel errors callers may branch on.
var (
	// ErrSourceNotFound reports a source root filesystem that does not exist.
	ErrSourceNotFound = errors.New("root filesystem not found")

	// ErrSourceNotADirectory reports a source that exists but is not a
	// directory, such as an image tarball someone passed without unpacking it.
	ErrSourceNotADirectory = errors.New("root filesystem is not a directory")

	// ErrSourceIsHostRoot reports "/" offered as a container's root filesystem.
	// Forge refuses it: pivoting the host's own root into a container is never
	// what the caller meant.
	ErrSourceIsHostRoot = errors.New("the host root directory cannot be used as a container root filesystem")

	// ErrInvalidID reports a container ID that could name a directory outside
	// the store.
	ErrInvalidID = errors.New("invalid container id")

	// ErrAlreadyPrepared reports a container directory that already exists.
	ErrAlreadyPrepared = errors.New("container directory already exists")

	// ErrNotPrepared reports a container that has no directory in the store.
	ErrNotPrepared = errors.New("container directory does not exist")

	// ErrStillMounted reports a removal refused because something is still
	// mounted under the container's tree. See Store.Remove.
	ErrStillMounted = errors.New("container directory still has mounts under it")
)

// ValidateSource reports whether path can serve as a container's root
// filesystem, returning the path with symlinks resolved.
//
// The result is what callers should use from then on: a mount source that is
// re-resolved later is a mount source that can change between the check and the
// mount.
func ValidateSource(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%w: no path given", ErrSourceNotFound)
	}

	// Resolving symlinks first means the checks below describe the directory
	// that would actually be mounted, not the name it was reached by.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %q", ErrSourceNotFound, path)
		}
		return "", fmt.Errorf("resolving root filesystem %q: %w", path, err)
	}
	resolved = filepath.Clean(resolved)

	if resolved == string(filepath.Separator) {
		return "", fmt.Errorf("%w: %q", ErrSourceIsHostRoot, path)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %q", ErrSourceNotFound, path)
		}
		return "", fmt.Errorf("inspecting root filesystem %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %q", ErrSourceNotADirectory, path)
	}

	return resolved, nil
}

// validateID rejects any container ID that could name a directory outside the
// store. IDs come from runtime.NewID today (ADR-0005), so this is defence in
// depth rather than input validation: the store joins the ID onto its own root,
// and a "../" in that position would put a container's files anywhere on the
// host.
//
// It deliberately does not check the *format* of an ID. That belongs to
// whoever generates them.
func validateID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: empty", ErrInvalidID)
	case id == "." || id == "..":
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	case strings.ContainsRune(id, os.PathSeparator):
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidID, id)
	case strings.ContainsRune(id, 0):
		return fmt.Errorf("%w: %q contains a NUL byte", ErrInvalidID, id)
	}
	return nil
}
