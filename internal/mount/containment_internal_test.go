package mount

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// resolveDestination is the single most security-relevant function in Stage 2.
// Every mount destination is a path *inside* the container, and it is resolved
// against a rootfs the user supplied — a tree that may contain symlinks Forge
// did not create. Resolving "/etc/hosts" against the host's "/" instead of the
// container's root is how a bind mount ends up writing to the host's /etc.
//
// So the rule it must enforce is: every component is resolved relative to the
// root, and the result is always inside the root. An absolute symlink inside
// the tree points at the container's root, not the host's — chroot semantics.
//
// These tests are deliberately adversarial and need no privileges: they build a
// tree in t.TempDir() and ask where a destination lands.

func TestResolveDestination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// build populates the rootfs before resolution.
		build func(t *testing.T, root string)
		dest  string
		// want is relative to the root; "" means the root itself.
		want string
	}{
		{
			name: "plain destination",
			dest: "/data",
			want: "data",
		},
		{
			name: "nested destination that does not exist yet",
			dest: "/var/log/forge",
			want: "var/log/forge",
		},
		{
			name: "root itself",
			dest: "/",
			want: "",
		},
		{
			name: "path is cleaned",
			dest: "/var/./log//forge",
			want: "var/log/forge",
		},
		{
			name: "interior dot-dot that stays inside is fine",
			dest: "/var/log/../lib",
			want: "var/lib",
		},
		{
			name: "existing directory",
			build: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, "data"))
			},
			dest: "/data",
			want: "data",
		},
		{
			name: "symlink inside the root is followed",
			build: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, "real"))
				symlink(t, "real", filepath.Join(root, "link"))
			},
			dest: "/link",
			want: "real",
		},
		{
			name: "a component that is a symlink is followed",
			build: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, "real", "sub"))
				symlink(t, "real", filepath.Join(root, "link"))
			},
			dest: "/link/sub",
			want: "real/sub",
		},
		{
			name: "an absolute symlink points at the container root, not the host",
			build: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, "etc"))
				symlink(t, "/etc", filepath.Join(root, "link"))
			},
			dest: "/link/hosts",
			want: "etc/hosts",
		},
		{
			name: "an absolute symlink to a host path that has no counterpart inside",
			build: func(t *testing.T, root string) {
				symlink(t, "/usr/lib/forge-nonexistent", filepath.Join(root, "link"))
			},
			dest: "/link",
			want: "usr/lib/forge-nonexistent",
		},
		{
			name: "a chain of symlinks",
			build: func(t *testing.T, root string) {
				mkdirAll(t, filepath.Join(root, "a"))
				symlink(t, "a", filepath.Join(root, "b"))
				symlink(t, "b", filepath.Join(root, "c"))
			},
			dest: "/c",
			want: "a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			if tt.build != nil {
				tt.build(t, root)
			}

			got, err := resolveDestination(root, tt.dest)
			if err != nil {
				t.Fatalf("resolveDestination(%q, %q) = %v", root, tt.dest, err)
			}

			want := filepath.Join(root, tt.want)
			if got != want {
				t.Errorf("resolveDestination(%q) = %q, want %q", tt.dest, got, want)
			}
		})
	}
}

// TestResolveDestinationRejectsEscapes is the test that must never be relaxed.
// Each case is a way a destination can end up outside the container's root; all
// of them must be refused, not clamped, not resolved.
func TestResolveDestinationRejectsEscapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func(t *testing.T, root string)
		dest  string
	}{
		{
			name: "leading dot-dot",
			dest: "/../etc",
		},
		{
			name: "dot-dot in the middle climbing out",
			dest: "/a/../../etc",
		},
		{
			name: "many dot-dots",
			dest: "/../../../../../../etc/shadow",
		},
		{
			name: "dot-dot alone",
			dest: "/..",
		},
		{
			name: "symlink pointing out of the root with dot-dot",
			build: func(t *testing.T, root string) {
				symlink(t, "../../..", filepath.Join(root, "escape"))
			},
			dest: "/escape/etc/shadow",
		},
		{
			name: "an intermediate component escapes even though the leaf looks safe",
			build: func(t *testing.T, root string) {
				symlink(t, "../..", filepath.Join(root, "up"))
			},
			dest: "/up/tmp/data",
		},
		{
			name: "a symlink chain whose last link escapes",
			build: func(t *testing.T, root string) {
				symlink(t, "../..", filepath.Join(root, "a"))
				symlink(t, "a", filepath.Join(root, "b"))
			},
			dest: "/b/etc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			if tt.build != nil {
				tt.build(t, root)
			}

			got, err := resolveDestination(root, tt.dest)
			if err == nil {
				t.Fatalf("resolveDestination(%q, %q) = %q, want an escape error", root, tt.dest, got)
			}
			if !errors.Is(err, ErrEscapesRoot) {
				t.Fatalf("resolveDestination(%q, %q) = %v, want %v", root, tt.dest, err, ErrEscapesRoot)
			}
		})
	}
}

// TestAbsoluteSymlinkToAHostPathIsRebasedNotFollowed is the rule that makes an
// absolute symlink harmless. A link inside the rootfs pointing at a real host
// directory is not an escape to be rejected — it is a container path, and it
// means the same directory *inside the container*. Following it to the host is
// the bug; rebasing it on the root is the fix, and it is why no absolute
// symlink can reach the host, whatever it names.
func TestAbsoluteSymlinkToAHostPathIsRebasedNotFollowed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	hostDir := t.TempDir()
	hostFile := filepath.Join(hostDir, "secret")
	if err := os.WriteFile(hostFile, []byte("host data"), 0o600); err != nil {
		t.Fatalf("writing the host file: %v", err)
	}

	symlink(t, hostDir, filepath.Join(root, "escape"))

	got, err := resolveDestination(root, "/escape/secret")
	if err != nil {
		t.Fatalf("resolveDestination() = %v", err)
	}

	if got == hostFile {
		t.Fatalf("resolveDestination() = %q, the host file itself", got)
	}
	if want := filepath.Join(root, hostDir, "secret"); got != want {
		t.Errorf("resolveDestination() = %q, want %q", got, want)
	}
}

// TestResolveDestinationRejectsSymlinkLoops keeps a malformed rootfs from
// hanging Forge in an unbounded resolution loop.
func TestResolveDestinationRejectsSymlinkLoops(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	symlink(t, "b", filepath.Join(root, "a"))
	symlink(t, "a", filepath.Join(root, "b"))

	if _, err := resolveDestination(root, "/a"); err == nil {
		t.Fatal("resolveDestination() = nil error on a symlink loop, want a failure")
	}
}

// TestResolveDestinationRequiresAnAbsoluteDestination keeps callers from
// accidentally resolving a container path against the process's cwd.
func TestResolveDestinationRequiresAnAbsoluteDestination(t *testing.T) {
	t.Parallel()

	if _, err := resolveDestination(t.TempDir(), "data"); err == nil {
		t.Fatal("resolveDestination() = nil error for a relative destination, want a failure")
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("linking %s -> %s: %v", link, target, err)
	}
}
