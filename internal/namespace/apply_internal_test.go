package namespace

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// The child-side half of Stage 1.
//
// Apply runs after clone(2) and before execve, and both things it does —
// marking the mount tree private and setting the hostname — need CAP_SYS_ADMIN.
// Doing either successfully belongs to the privileged suite, and must not be
// attempted here: as root, makeMountTreePrivate would reconfigure the host's
// own mount propagation and Sethostname would rename the machine.
//
// What is testable without privileges is the other outcome, and it is the one
// an unprivileged user actually meets: the kernel's refusal, and whether Forge
// turns it into something that says what to do about it.

// requireUnprivileged skips a case whose whole point is that the kernel says no.
func requireUnprivileged(t *testing.T) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("running as root: this case would reconfigure the host rather than being refused")
	}
}

// TestTranslatePermission covers the sentinel swap by itself.
func TestTranslatePermission(t *testing.T) {
	t.Parallel()

	if got := translatePermission(syscall.EPERM); !errors.Is(got, ErrPermission) {
		t.Errorf("translatePermission(EPERM) = %v, want ErrPermission", got)
	}
	if got := translatePermission(&os.SyscallError{Syscall: "sethostname", Err: syscall.EPERM}); !errors.Is(got, ErrPermission) {
		t.Errorf("translatePermission(wrapped EPERM) = %v, want ErrPermission", got)
	}

	// Anything else is passed through, so the caller still learns which
	// syscall failed and why.
	if got := translatePermission(syscall.EINVAL); !errors.Is(got, syscall.EINVAL) {
		t.Errorf("translatePermission(EINVAL) = %v, want it left alone", got)
	}
	if got := translatePermission(nil); got != nil {
		t.Errorf("translatePermission(nil) = %v, want nil", got)
	}
}

// TestApplyIsRefusedWithoutPrivileges drives the refusal through Apply, which
// is where an unprivileged `forge run` finds out it needed sudo.
func TestApplyIsRefusedWithoutPrivileges(t *testing.T) {
	requireUnprivileged(t)
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "making the mount tree private", cfg: Config{Mount: true}},
		{name: "setting the hostname", cfg: Config{UTS: true, Hostname: "forge-test"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := Apply(tc.cfg)
			if err == nil {
				t.Fatalf("Apply(%+v) = nil without privileges", tc.cfg)
			}
			if !errors.Is(err, ErrPermission) {
				t.Errorf("Apply(%+v) = %v, want ErrPermission", tc.cfg, err)
			}
		})
	}
}

// TestApplyValidatesBeforeTouchingAnything checks the ordering: a config that
// cannot be applied is refused before the first syscall, so a bad hostname
// never leaves a container with a private mount tree and no name.
func TestApplyValidatesBeforeTouchingAnything(t *testing.T) {
	t.Parallel()

	// A hostname without a UTS namespace would rename the host, which is what
	// Validate exists to refuse.
	err := Apply(Config{Mount: true, Hostname: "forge-test"})
	if err == nil {
		t.Fatal("Apply() = nil for a hostname with no UTS namespace")
	}
	if errors.Is(err, ErrPermission) {
		t.Errorf("Apply() = %v, want the validation failure rather than a syscall's", err)
	}
}
