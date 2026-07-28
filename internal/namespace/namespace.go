// Package namespace computes and applies the Linux namespaces that isolate a
// container.
//
// The package has two halves, which run in two different processes:
//
//   - Config.CloneFlags is evaluated in the *parent*. Its result is handed to
//     clone(2) so the child is born inside fresh namespaces.
//   - Apply runs in the *child*, after clone(2) has placed it in those
//     namespaces but before the container's binary is exec'd. It performs the
//     setup that can only be done from inside a namespace.
//
// Per SSOT §2 this package only creates and enters namespaces; it never decides
// what a container should mount or how it should be networked.
package namespace

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
)

// MaxHostnameLen is the longest hostname the kernel accepts. It mirrors
// __NEW_UTS_LEN in include/uapi/linux/utsname.h; sethostname(2) rejects
// anything longer with EINVAL.
const MaxHostnameLen = 64

// Sentinel errors callers may branch on.
var (
	// ErrHostnameWithoutUTS reports a Config that sets a hostname without
	// requesting the UTS namespace that would contain it. Applying it would
	// rename the host.
	ErrHostnameWithoutUTS = errors.New("hostname requires a UTS namespace")

	// ErrInvalidHostname reports a hostname the kernel would reject.
	ErrInvalidHostname = errors.New("invalid hostname")

	// ErrPermission reports that the calling process lacks CAP_SYS_ADMIN,
	// which every namespace operation in Forge requires.
	ErrPermission = errors.New("operation requires CAP_SYS_ADMIN (run forge as root)")
)

// Config describes the namespaces a container is isolated by.
//
// The zero Config requests no isolation at all, which is valid: it produces
// zero clone flags and an Apply that does nothing. Callers choose the
// isolation they want rather than opting out of a default.
type Config struct {
	// PID requests a private process-ID namespace, so the container's init
	// process sees itself as PID 1 and cannot see host processes.
	PID bool

	// UTS requests a private hostname/domainname namespace.
	UTS bool

	// Mount requests a private mount namespace, so mounts made inside the
	// container are invisible to the host.
	Mount bool

	// Hostname is the name Apply assigns inside the UTS namespace. Empty
	// means "leave whatever was inherited". Setting it requires UTS.
	Hostname string
}

// CloneFlags returns the clone(2) flags that create the requested namespaces.
//
// This is pure computation and is deliberately separated from Apply so the
// mapping from Config to kernel flags is unit-testable without root.
func (c Config) CloneFlags() uintptr {
	var flags uintptr
	if c.PID {
		flags |= syscall.CLONE_NEWPID
	}
	if c.UTS {
		flags |= syscall.CLONE_NEWUTS
	}
	if c.Mount {
		flags |= syscall.CLONE_NEWNS
	}
	return flags
}

// Validate reports whether the configuration is coherent. It is pure and
// performs no syscalls, so callers can reject bad input before forking.
func (c Config) Validate() error {
	if c.Hostname == "" {
		return nil
	}
	if !c.UTS {
		return ErrHostnameWithoutUTS
	}
	return validateHostname(c.Hostname)
}

// validateHostname rejects names sethostname(2) would refuse, so the failure
// surfaces in the parent with a clear message rather than as a bare EINVAL
// inside the container's init.
func validateHostname(name string) error {
	if len(name) > MaxHostnameLen {
		return fmt.Errorf("%w: %q is %d bytes, limit is %d", ErrInvalidHostname, name, len(name), MaxHostnameLen)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("%w: %q must not start or end with a hyphen", ErrInvalidHostname, name)
	}
	for _, r := range name {
		isAlphanumeric := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !isAlphanumeric && r != '-' && r != '.' {
			return fmt.Errorf("%w: %q contains %q; only letters, digits, hyphen and dot are allowed", ErrInvalidHostname, name, r)
		}
	}
	return nil
}

// Apply performs the namespace setup that must happen from inside the new
// namespaces, after clone(2) and before the container's binary is exec'd.
//
// It must be called in the child process. Calling it in the parent would
// reconfigure the host.
func Apply(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}

	if c.Mount {
		if err := makeMountTreePrivate(); err != nil {
			return err
		}
	}

	if c.Hostname != "" {
		if err := syscall.Sethostname([]byte(c.Hostname)); err != nil {
			return fmt.Errorf("setting hostname to %q: %w", c.Hostname, translatePermission(err))
		}
	}

	return nil
}

// makeMountTreePrivate detaches the container's mount table from the host's.
//
// CLONE_NEWNS alone is not enough to satisfy FR-1.3. A new mount namespace is a
// *copy* of its parent's, and it inherits each mount's propagation type. On any
// systemd host "/" is MS_SHARED, so a mount made inside the container would
// still propagate back out to the host. Recursively marking the tree private is
// what actually delivers the isolation the requirement asks for.
func makeMountTreePrivate() error {
	// Source and filesystem type are ignored for a propagation-only change;
	// "none" is the conventional placeholder.
	const (
		source = "none"
		target = "/"
		fstype = ""
		data   = ""
	)
	if err := syscall.Mount(source, target, fstype, syscall.MS_REC|syscall.MS_PRIVATE, data); err != nil {
		return fmt.Errorf("making mount tree private: %w", translatePermission(err))
	}
	return nil
}

// translatePermission replaces a bare EPERM with Forge's sentinel, so callers
// can branch on it and users get an actionable message instead of
// "operation not permitted".
func translatePermission(err error) error {
	if errors.Is(err, syscall.EPERM) {
		return ErrPermission
	}
	return err
}
