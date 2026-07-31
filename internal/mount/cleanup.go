package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// mountInfoPath is the kernel's view of the calling process's mount table.
const mountInfoPath = "/proc/self/mountinfo"

// maxCleanupPasses bounds Cleanup's retries. Unmounting can reveal another
// mount stacked on the same point, so one pass is not always enough; a mount
// that survives this many passes is not going to yield to another.
const maxCleanupPasses = 10

// Cleanup unmounts everything mounted at or under dir, deepest first.
//
// It is idempotent: a directory with no mounts under it, or no such directory
// at all, is not an error (SSOT §13.3).
//
// This is a reconciliation path, not the ordinary one. Forge makes its mounts
// inside the container's mount namespace, and the kernel destroys that
// namespace — with every mount in it — when the container's last process
// exits. Cleanup exists for the case that mechanism cannot cover: a forge
// process killed with SIGKILL, which leaves the container running and its
// directory behind. It is also what rootfs.Store.Remove's refusal to delete
// through a live mount points the operator at.
func Cleanup(ctx context.Context, dir string) error {
	dir = filepath.Clean(dir)

	for range maxCleanupPasses {
		if err := ctx.Err(); err != nil {
			return err
		}

		points, err := mountPointsUnder(dir)
		if err != nil {
			return err
		}
		if len(points) == 0 {
			return nil
		}

		// Deepest first: a parent mount cannot be unmounted while one of its
		// children is still mounted.
		slices.SortFunc(points, func(a, b string) int { return len(b) - len(a) })

		for _, point := range points {
			// MNT_DETACH removes the mount from the tree immediately and lets
			// the kernel release it once the last reference goes, so a mount
			// something still has open does not block cleanup.
			if err := unix.Unmount(point, unix.MNT_DETACH); err != nil {
				// EINVAL means it is not a mount point and ENOENT that it is
				// gone: both mean the work is already done, which is what
				// makes repeated calls safe.
				if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
					continue
				}
				return fmt.Errorf("unmounting %q: %w", point, translatePermission(err))
			}
		}
	}

	remaining, err := mountPointsUnder(dir)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return fmt.Errorf("%q still has mounts under it after %d passes: %s",
			dir, maxCleanupPasses, strings.Join(remaining, ", "))
	}

	return nil
}

// IsMountPoint reports whether path is a mount point in the calling process's
// mount namespace.
//
// It reads the mount table rather than comparing a directory's device number
// with its parent's, because that comparison misses the case Stage 2 depends
// on most: a bind mount within one filesystem leaves both device numbers
// identical, so the container's own root would not be recognised as a mount
// point and pivot_root(2) would be attempted anyway.
func IsMountPoint(path string) (bool, error) {
	path = filepath.Clean(path)

	points, err := mountPoints()
	if err != nil {
		return false, err
	}
	return slices.Contains(points, path), nil
}

// mountPointsUnder returns the mount points at or under dir.
func mountPointsUnder(dir string) ([]string, error) {
	points, err := mountPoints()
	if err != nil {
		return nil, err
	}

	prefix := strings.TrimSuffix(dir, string(filepath.Separator)) + string(filepath.Separator)

	var found []string
	for _, point := range points {
		if point == dir || strings.HasPrefix(point, prefix) {
			found = append(found, point)
		}
	}
	return found, nil
}

// mountPoints returns every mount point the calling process can see.
//
// internal/rootfs reads the same file for its own guard. The duplication is
// deliberate: SSOT §2 forbids one primitive package from importing another, and
// twenty lines of parsing in each is a smaller price than the dependency.
func mountPoints() ([]string, error) {
	data, err := os.ReadFile(mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", mountInfoPath, err)
	}

	var points []string
	for line := range strings.Lines(string(data)) {
		// mountinfo fields: id parent major:minor root mount-point ...
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		points = append(points, unescapeMountPoint(fields[4]))
	}

	return points, nil
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
