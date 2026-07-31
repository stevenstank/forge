package mount

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxSymlinkDepth bounds how many symlinks one destination may traverse,
// mirroring the kernel's own limit. Without it, a rootfs containing a symlink
// loop would hang Forge instead of failing it.
const maxSymlinkDepth = 40

// resolveDestination resolves a container path to the host path it names,
// guaranteeing the result is inside root.
//
// This is the most security-relevant function in Stage 2. A mount destination
// is a path *inside* the container, resolved against a root filesystem the user
// supplied — a tree that may contain symlinks Forge did not create. Resolving
// "/etc/hosts" the way the host would is how a bind mount ends up writing to
// the host's /etc.
//
// It therefore resolves every component itself, under two rules:
//
//   - An absolute symlink points at the *container's* root, not the host's.
//     A link to "/etc" inside the rootfs means <root>/etc, exactly as it would
//     to a process that had already pivoted. This is why a symlink to a host
//     path cannot be used to reach that path: it is rebased, not followed.
//   - A path that would climb above the root is refused rather than clamped.
//     Clamping ".." at the root is what the kernel does, but Forge is building
//     a mount plan, not walking one: a destination that tried to escape is a
//     mistake worth reporting, not one worth silently reinterpreting.
//
// Components that do not exist are fine — they are created by Apply.
func resolveDestination(root, destination string) (string, error) {
	if !filepath.IsAbs(destination) {
		return "", fmt.Errorf("%w: destination %q must be an absolute path inside the container",
			ErrInvalidMountSpec, destination)
	}
	root = filepath.Clean(root)

	// current is the resolved path so far, relative to root.
	var current string
	remaining := splitPath(destination)
	links := 0

	for len(remaining) > 0 {
		component := remaining[0]
		remaining = remaining[1:]

		switch component {
		case ".":
			continue
		case "..":
			if current == "" {
				return "", fmt.Errorf("%w: %q", ErrEscapesRoot, destination)
			}
			if current = filepath.Dir(current); current == "." {
				current = ""
			}
			continue
		}

		candidate := filepath.Join(current, component)
		info, err := os.Lstat(filepath.Join(root, candidate))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			// Missing, or not a symlink: an ordinary component either way.
			current = candidate
			continue
		}

		links++
		if links > maxSymlinkDepth {
			return "", fmt.Errorf("resolving %q: more than %d symlinks traversed, the root filesystem may contain a loop",
				destination, maxSymlinkDepth)
		}

		target, err := os.Readlink(filepath.Join(root, candidate))
		if err != nil {
			return "", fmt.Errorf("reading symlink %q in the container root filesystem: %w", candidate, err)
		}
		if filepath.IsAbs(target) {
			current = ""
		}
		remaining = append(splitPath(target), remaining...)
	}

	return filepath.Join(root, current), nil
}

// splitPath breaks a path into its components, dropping the empty strings that
// leading, trailing and repeated separators produce.
func splitPath(path string) []string {
	parts := strings.Split(path, string(filepath.Separator))

	components := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			components = append(components, part)
		}
	}
	return components
}
