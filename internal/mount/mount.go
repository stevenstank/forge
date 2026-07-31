// Package mount wraps the three kernel mechanisms that give a container its own
// filesystem: bind mounts, pivot_root(2), and unmounting.
//
// The package is deliberately ignorant of what a container is. It is handed a
// fully-formed Plan and performs it; per SSOT §2 it never decides *what* a
// container should mount. That decision is policy and lives in
// internal/runtime.
//
// Like internal/namespace, it has two halves that run in two different
// processes:
//
//   - Flags, Validate, Ordered and ParseMountSpec are pure computation. They
//     run in the parent, before anything is forked, so a bad mount is refused
//     with a clear error rather than an opaque failure inside a container.
//   - Apply and PivotRoot run in the *child*, after clone(2) has placed it in a
//     new mount namespace and internal/namespace has made that namespace's
//     mount tree private. Every mount they make therefore belongs to the
//     container, and the kernel destroys all of them when the namespace's last
//     process exits.
//
// Cleanup is the exception: it runs in the parent and exists to reconcile
// mounts left behind by a crash, not to perform ordinary teardown. Ordinary
// teardown is the kernel's, by the mechanism just described.
package mount

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// Type is the filesystem type passed to mount(2). The empty type means a bind
// mount, for which the kernel ignores the type entirely.
type Type string

// The filesystem types Stage 2 can mount.
const (
	// TypeBind rebinds an existing directory or file at a new location.
	TypeBind Type = ""
	// TypeProc is the process filesystem. A fresh instance reflects the mount
	// namespace's PID namespace, which is what makes CLONE_NEWPID observable
	// from inside a container.
	TypeProc Type = "proc"
	// TypeSysfs exposes kernel objects.
	TypeSysfs Type = "sysfs"
	// TypeTmpfs is an in-memory filesystem.
	TypeTmpfs Type = "tmpfs"
	// TypeDevpts is a pseudo-terminal filesystem.
	TypeDevpts Type = "devpts"
)

// Option is a per-mount modifier. Each maps to exactly one MS_* flag, so the
// set of options a reader sees in a log line is the set of flags the kernel
// was given.
type Option string

// The options a mount may carry.
const (
	// OptionReadOnly forbids writes through this mount.
	OptionReadOnly Option = "ro"
	// OptionNoSuid ignores set-user-ID and set-group-ID bits.
	OptionNoSuid Option = "nosuid"
	// OptionNoDev forbids access to device special files.
	OptionNoDev Option = "nodev"
	// OptionNoExec forbids execution of binaries.
	OptionNoExec Option = "noexec"
	// OptionRecursive carries a bind mount's own submounts with it. It is
	// meaningful only for a bind.
	OptionRecursive Option = "rec"
)

// optionFlags maps each option to its kernel flag. It is also the set of
// options Forge accepts: anything absent here is rejected by name.
var optionFlags = map[Option]uintptr{
	OptionReadOnly:  unix.MS_RDONLY,
	OptionNoSuid:    unix.MS_NOSUID,
	OptionNoDev:     unix.MS_NODEV,
	OptionNoExec:    unix.MS_NOEXEC,
	OptionRecursive: unix.MS_REC,
}

// remountFlags are the flags a bind mount silently ignores on its first
// mount(2) call. Applying them needs a second, MS_REMOUNT call — see
// Mount.NeedsRemount.
const remountFlags = unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC

// Sentinel errors callers may branch on.
var (
	// ErrInvalidMountSpec reports a mount Forge cannot make sense of: a
	// missing path, a relative path, or a malformed --mount argument.
	ErrInvalidMountSpec = errors.New("invalid mount")

	// ErrInvalidOption reports a mount option Forge does not implement.
	ErrInvalidOption = errors.New("invalid mount option")

	// ErrEscapesRoot reports a destination that resolves outside the
	// container's root filesystem. It is the refusal that keeps a bind mount
	// from writing to the host.
	ErrEscapesRoot = errors.New("mount destination escapes the container root")

	// ErrDuplicateDestination reports two mounts targeting one destination.
	ErrDuplicateDestination = errors.New("duplicate mount destination")

	// ErrNotADirectory reports a source and destination whose types disagree,
	// such as a directory bind-mounted onto a file.
	ErrNotADirectory = errors.New("mount source and destination types differ")

	// ErrRootNotMountPoint reports a pivot_root onto a directory that is not a
	// mount point, which the kernel refuses.
	ErrRootNotMountPoint = errors.New("the new root must be a mount point")

	// ErrPermission reports that the calling process lacks CAP_SYS_ADMIN.
	ErrPermission = errors.New("operation requires CAP_SYS_ADMIN (run forge as root)")
)

// Mount is one entry in a Plan.
type Mount struct {
	// Source is the host path for a bind mount. It is ignored for filesystem
	// types that have no backing device, such as proc and tmpfs.
	Source string `json:"source,omitempty"`

	// Destination is an absolute path *inside* the container.
	Destination string `json:"destination"`

	// Type is the filesystem type. The zero value is a bind mount.
	Type Type `json:"type"`

	// Options modify this mount. They are applied with a second, MS_REMOUNT
	// call where the kernel requires one.
	Options []Option `json:"options,omitempty"`

	// Data is filesystem-specific data passed verbatim to mount(2), such as
	// "mode=755" for a tmpfs.
	Data string `json:"data,omitempty"`
}

// Flags returns the mount(2) flags this mount is made with.
//
// It never returns a propagation flag. Propagation is a property of the
// namespace rather than of any one mount, and internal/namespace sets it once
// for the whole tree (ADR-0008); a per-mount option that could change it would
// be a way to undo that isolation.
func (m Mount) Flags() uintptr {
	var flags uintptr
	if m.Type == TypeBind {
		flags |= unix.MS_BIND
	}
	for _, option := range m.Options {
		flags |= optionFlags[option]
	}
	return flags
}

// NeedsRemount reports whether this mount requires a second, MS_REMOUNT call
// to apply its options.
//
// A first mount(2) call that carries MS_BIND ignores MS_RDONLY, MS_NOSUID,
// MS_NODEV and MS_NOEXEC: the new mount inherits them from the mount it was
// bound from. Without the second call, a mount asked for read-only is silently
// writable — a failure that looks like nothing at all until something writes.
func (m Mount) NeedsRemount() bool {
	return m.Type == TypeBind && m.Flags()&remountFlags != 0
}

// Validate reports whether the mount can be made. It is pure and performs no
// syscalls, so bad input is refused in the parent, before any fork.
func (m Mount) Validate() error {
	if m.Destination == "" {
		return fmt.Errorf("%w: no destination", ErrInvalidMountSpec)
	}
	if !filepath.IsAbs(m.Destination) {
		return fmt.Errorf("%w: destination %q must be an absolute path inside the container",
			ErrInvalidMountSpec, m.Destination)
	}
	// Checked before the destination is cleaned: filepath.Clean("/../etc") is
	// "/etc", so cleaning first would silently accept an escape.
	if err := checkNoEscape(m.Destination); err != nil {
		return err
	}

	if m.Type == TypeBind {
		if m.Source == "" {
			return fmt.Errorf("%w: bind mount at %q has no source", ErrInvalidMountSpec, m.Destination)
		}
		if !filepath.IsAbs(m.Source) {
			return fmt.Errorf("%w: source %q must be an absolute host path", ErrInvalidMountSpec, m.Source)
		}
	}

	for _, option := range m.Options {
		if _, ok := optionFlags[option]; !ok {
			return fmt.Errorf("%w: %q", ErrInvalidOption, option)
		}
		if option == OptionRecursive && m.Type != TypeBind {
			return fmt.Errorf("%w: %q applies to bind mounts only, not %q",
				ErrInvalidOption, OptionRecursive, m.Type)
		}
	}

	return nil
}

// depth is how deeply the destination is nested, used to order a plan.
func (m Mount) depth() int {
	return strings.Count(filepath.Clean(m.Destination), string(filepath.Separator))
}

// Plan is the complete set of mounts for one container.
type Plan struct {
	// Source is the host directory bind-mounted onto Root. That bind is what
	// makes Root a mount point, which pivot_root(2) requires.
	//
	// In Stage 2 it is the tree the caller named with --rootfs. From Stage 5,
	// when image layers are unpacked into Root itself, Source equals Root and
	// the bind becomes a self-bind that does nothing but satisfy that
	// requirement.
	Source string `json:"source"`

	// Root is the host path of the directory that becomes the container's "/".
	Root string `json:"root"`

	// ReadonlyRoot remounts the container's root filesystem read-only once
	// everything has been mounted into it.
	ReadonlyRoot bool `json:"readonly_root,omitempty"`

	// Mounts are the filesystems to mount inside Root.
	Mounts []Mount `json:"mounts,omitempty"`
}

// Validate reports whether the plan can be applied. It is pure and performs no
// syscalls.
func (p Plan) Validate() error {
	if err := validateRootPath("root", p.Root); err != nil {
		return err
	}
	if err := validateRootPath("source", p.Source); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(p.Mounts))
	for _, m := range p.Mounts {
		if err := m.Validate(); err != nil {
			return err
		}

		destination := filepath.Clean(m.Destination)
		if destination == string(filepath.Separator) {
			return fmt.Errorf("%w: %q would replace the container root itself",
				ErrEscapesRoot, m.Destination)
		}
		if _, duplicate := seen[destination]; duplicate {
			return fmt.Errorf("%w: %q", ErrDuplicateDestination, destination)
		}
		seen[destination] = struct{}{}
	}

	return nil
}

// validateRootPath checks one of the plan's two host paths.
func validateRootPath(name, path string) error {
	if path == "" {
		return fmt.Errorf("%w: no %s directory", ErrInvalidMountSpec, name)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: %s %q must be an absolute path", ErrInvalidMountSpec, name, path)
	}
	if filepath.Clean(path) == string(filepath.Separator) {
		return fmt.Errorf("%w: %s must not be the host root directory", ErrEscapesRoot, name)
	}
	return nil
}

// Ordered returns the plan's mounts shallowest destination first, leaving the
// plan itself untouched.
//
// Order matters because a mount hides whatever is beneath it: mounting /var
// after /var/log would bury the inner mount, and mounting a tmpfs on /dev after
// binding /dev/null would bury the device node. Mounts at the same depth keep
// the order they were given, so an explicit choice by the caller survives.
func (p Plan) Ordered() []Mount {
	if len(p.Mounts) == 0 {
		return nil
	}

	ordered := slices.Clone(p.Mounts)
	slices.SortStableFunc(ordered, func(a, b Mount) int {
		return a.depth() - b.depth()
	})
	return ordered
}

// ParseMountSpec parses a --mount argument of the form
//
//	<source>:<destination>[:<option>[,<option>...]]
//
// Both paths are absolute; the destination is a path inside the container.
func ParseMountSpec(spec string) (Mount, error) {
	fields := strings.Split(spec, ":")
	if len(fields) < 2 || len(fields) > 3 {
		return Mount{}, fmt.Errorf(
			"%w: %q, want <source>:<destination> or <source>:<destination>:<options>",
			ErrInvalidMountSpec, spec)
	}

	m := Mount{Source: fields[0], Destination: fields[1], Type: TypeBind}

	if len(fields) == 3 {
		for _, name := range strings.Split(fields[2], ",") {
			option := Option(name)
			if _, ok := optionFlags[option]; !ok {
				return Mount{}, fmt.Errorf("%w: %q in %q; valid options are %s",
					ErrInvalidOption, name, spec, optionNames())
			}
			m.Options = append(m.Options, option)
		}
	}

	if err := m.Validate(); err != nil {
		return Mount{}, fmt.Errorf("%w in %q", err, spec)
	}

	// Cleaning happens after validation so the checks see what the caller
	// actually typed.
	m.Source = filepath.Clean(m.Source)
	m.Destination = filepath.Clean(m.Destination)

	return m, nil
}

// optionNames lists the accepted options, for error messages.
func optionNames() string {
	names := make([]string, 0, len(optionFlags))
	for option := range optionFlags {
		names = append(names, string(option))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// checkNoEscape reports whether a container path stays within the container's
// root, judging by its ".." components alone.
//
// It is the cheap, pure half of containment. The expensive half —
// resolveDestination — additionally follows the symlinks in a rootfs Forge did
// not create.
func checkNoEscape(path string) error {
	depth := 0
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		switch component {
		case "", ".":
		case "..":
			depth--
			if depth < 0 {
				return fmt.Errorf("%w: %q", ErrEscapesRoot, path)
			}
		default:
			depth++
		}
	}
	return nil
}

// translatePermission replaces a bare EPERM with Forge's sentinel, so callers
// can branch on it and users get an actionable message rather than "operation
// not permitted" from a syscall they did not know was involved.
func translatePermission(err error) error {
	if errors.Is(err, unix.EPERM) {
		return ErrPermission
	}
	return err
}
