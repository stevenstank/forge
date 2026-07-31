package mount_test

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/stevenstank/forge/internal/mount"
)

// These tests cover the half of internal/mount that is pure computation: the
// mapping from Forge's typed options to kernel flags, the parsing of a --mount
// argument, and the validation and ordering of a plan. None of it touches the
// kernel, so all of it runs without root (SSOT §7).
//
// Apply, PivotRoot and Cleanup are exercised by test/integration.

func TestMountFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		m    mount.Mount
		want uintptr
	}{
		{
			name: "bind with no options is still a bind",
			m:    mount.Mount{Source: "/src", Destination: "/dst", Type: mount.TypeBind},
			want: syscall.MS_BIND,
		},
		{
			name: "read-only bind",
			m: mount.Mount{
				Source: "/src", Destination: "/dst", Type: mount.TypeBind,
				Options: []mount.Option{mount.OptionReadOnly},
			},
			want: syscall.MS_BIND | syscall.MS_RDONLY,
		},
		{
			name: "recursive bind",
			m: mount.Mount{
				Source: "/src", Destination: "/dst", Type: mount.TypeBind,
				Options: []mount.Option{mount.OptionRecursive},
			},
			want: syscall.MS_BIND | syscall.MS_REC,
		},
		{
			name: "the hardening trio",
			m: mount.Mount{
				Destination: "/proc", Type: mount.TypeProc,
				Options: []mount.Option{mount.OptionNoSuid, mount.OptionNoDev, mount.OptionNoExec},
			},
			want: syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC,
		},
		{
			name: "tmpfs with no options",
			m:    mount.Mount{Destination: "/dev", Type: mount.TypeTmpfs},
			want: 0,
		},
		{
			name: "read-only sysfs",
			m: mount.Mount{
				Destination: "/sys", Type: mount.TypeSysfs,
				Options: []mount.Option{mount.OptionReadOnly, mount.OptionNoSuid, mount.OptionNoDev, mount.OptionNoExec},
			},
			want: syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC,
		},
		{
			name: "duplicate options do not double-set a bit",
			m: mount.Mount{
				Source: "/src", Destination: "/dst", Type: mount.TypeBind,
				Options: []mount.Option{mount.OptionReadOnly, mount.OptionReadOnly},
			},
			want: syscall.MS_BIND | syscall.MS_RDONLY,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.m.Flags(); got != tt.want {
				t.Errorf("Flags() = %#x, want %#x", got, tt.want)
			}
		})
	}
}

// TestFlagsExcludesPropagationChanges guards the boundary in SSOT §2 and
// ADR-0008: making the mount tree private belongs to internal/namespace, and no
// per-mount option may quietly re-set propagation.
func TestFlagsExcludesPropagationChanges(t *testing.T) {
	t.Parallel()

	m := mount.Mount{
		Source: "/src", Destination: "/dst", Type: mount.TypeBind,
		Options: []mount.Option{
			mount.OptionReadOnly, mount.OptionNoSuid, mount.OptionNoDev,
			mount.OptionNoExec, mount.OptionRecursive,
		},
	}

	for _, forbidden := range []struct {
		name string
		flag uintptr
	}{
		{"MS_SHARED", syscall.MS_SHARED},
		{"MS_PRIVATE", syscall.MS_PRIVATE},
		{"MS_SLAVE", syscall.MS_SLAVE},
		{"MS_UNBINDABLE", syscall.MS_UNBINDABLE},
		{"MS_MOVE", syscall.MS_MOVE},
	} {
		if got := m.Flags(); got&forbidden.flag != 0 {
			t.Errorf("Flags() = %#x, must not include %s (%#x)", got, forbidden.name, forbidden.flag)
		}
	}
}

// TestNeedsRemount pins the kernel behaviour that costs everyone a day the
// first time: a bind mount ignores MS_RDONLY and the other per-mount flags on
// the first mount(2) call. They only take effect on a second, MS_REMOUNT call.
// If this ever returns false for a read-only bind, --mount ro silently gives
// the container a writable mount.
func TestNeedsRemount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		m    mount.Mount
		want bool
	}{
		{
			name: "read-only bind needs a remount",
			m: mount.Mount{
				Source: "/src", Destination: "/dst", Type: mount.TypeBind,
				Options: []mount.Option{mount.OptionReadOnly},
			},
			want: true,
		},
		{
			name: "nosuid bind needs a remount",
			m: mount.Mount{
				Source: "/src", Destination: "/dst", Type: mount.TypeBind,
				Options: []mount.Option{mount.OptionNoSuid},
			},
			want: true,
		},
		{
			name: "nodev bind needs a remount",
			m: mount.Mount{
				Source: "/src", Destination: "/dst", Type: mount.TypeBind,
				Options: []mount.Option{mount.OptionNoDev},
			},
			want: true,
		},
		{
			name: "noexec bind needs a remount",
			m: mount.Mount{
				Source: "/src", Destination: "/dst", Type: mount.TypeBind,
				Options: []mount.Option{mount.OptionNoExec},
			},
			want: true,
		},
		{
			name: "plain bind does not",
			m:    mount.Mount{Source: "/src", Destination: "/dst", Type: mount.TypeBind},
			want: false,
		},
		{
			name: "recursive-only bind does not",
			m: mount.Mount{
				Source: "/src", Destination: "/dst", Type: mount.TypeBind,
				Options: []mount.Option{mount.OptionRecursive},
			},
			want: false,
		},
		{
			name: "read-only tmpfs does not: a first mount honours its flags",
			m: mount.Mount{
				Destination: "/dev", Type: mount.TypeTmpfs,
				Options: []mount.Option{mount.OptionReadOnly},
			},
			want: false,
		},
		{
			name: "read-only sysfs does not",
			m: mount.Mount{
				Destination: "/sys", Type: mount.TypeSysfs,
				Options: []mount.Option{mount.OptionReadOnly},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.m.NeedsRemount(); got != tt.want {
				t.Errorf("NeedsRemount() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMountValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		m       mount.Mount
		wantErr error
	}{
		{
			name: "bind mount is valid",
			m:    mount.Mount{Source: "/host/data", Destination: "/data", Type: mount.TypeBind},
		},
		{
			name: "proc mount needs no source",
			m:    mount.Mount{Destination: "/proc", Type: mount.TypeProc},
		},
		{
			name: "tmpfs with data is valid",
			m:    mount.Mount{Destination: "/dev", Type: mount.TypeTmpfs, Data: "mode=755"},
		},
		{
			name:    "bind without a source is rejected",
			m:       mount.Mount{Destination: "/data", Type: mount.TypeBind},
			wantErr: mount.ErrInvalidMountSpec,
		},
		{
			name:    "bind with a relative source is rejected",
			m:       mount.Mount{Source: "host/data", Destination: "/data", Type: mount.TypeBind},
			wantErr: mount.ErrInvalidMountSpec,
		},
		{
			name:    "empty destination is rejected",
			m:       mount.Mount{Source: "/host/data", Type: mount.TypeBind},
			wantErr: mount.ErrInvalidMountSpec,
		},
		{
			name:    "relative destination is rejected",
			m:       mount.Mount{Source: "/host/data", Destination: "data", Type: mount.TypeBind},
			wantErr: mount.ErrInvalidMountSpec,
		},
		{
			name: "unknown option is rejected",
			m: mount.Mount{
				Source: "/host/data", Destination: "/data", Type: mount.TypeBind,
				Options: []mount.Option{"rw-ish"},
			},
			wantErr: mount.ErrInvalidOption,
		},
		{
			name: "recursive on a non-bind is rejected",
			m: mount.Mount{
				Destination: "/proc", Type: mount.TypeProc,
				Options: []mount.Option{mount.OptionRecursive},
			},
			wantErr: mount.ErrInvalidOption,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.m.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseMountSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec string
		want mount.Mount
	}{
		{
			name: "source and destination",
			spec: "/host/data:/data",
			want: mount.Mount{Source: "/host/data", Destination: "/data", Type: mount.TypeBind},
		},
		{
			name: "single option",
			spec: "/host/data:/data:ro",
			want: mount.Mount{
				Source: "/host/data", Destination: "/data", Type: mount.TypeBind,
				Options: []mount.Option{mount.OptionReadOnly},
			},
		},
		{
			name: "several options",
			spec: "/host/data:/data:ro,nosuid,nodev",
			want: mount.Mount{
				Source: "/host/data", Destination: "/data", Type: mount.TypeBind,
				Options: []mount.Option{mount.OptionReadOnly, mount.OptionNoSuid, mount.OptionNoDev},
			},
		},
		{
			name: "paths are cleaned",
			spec: "/host//data/:/data/./sub",
			want: mount.Mount{Source: "/host/data", Destination: "/data/sub", Type: mount.TypeBind},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := mount.ParseMountSpec(tt.spec)
			if err != nil {
				t.Fatalf("ParseMountSpec(%q) = %v", tt.spec, err)
			}
			if got.Source != tt.want.Source {
				t.Errorf("Source = %q, want %q", got.Source, tt.want.Source)
			}
			if got.Destination != tt.want.Destination {
				t.Errorf("Destination = %q, want %q", got.Destination, tt.want.Destination)
			}
			if got.Type != tt.want.Type {
				t.Errorf("Type = %q, want %q", got.Type, tt.want.Type)
			}
			if !sameOptions(got.Options, tt.want.Options) {
				t.Errorf("Options = %v, want %v", got.Options, tt.want.Options)
			}
		})
	}
}

func TestParseMountSpecRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    string
		wantErr error
	}{
		{name: "empty", spec: "", wantErr: mount.ErrInvalidMountSpec},
		{name: "no separator", spec: "/host/data", wantErr: mount.ErrInvalidMountSpec},
		{name: "empty source", spec: ":/data", wantErr: mount.ErrInvalidMountSpec},
		{name: "empty destination", spec: "/host/data:", wantErr: mount.ErrInvalidMountSpec},
		{name: "relative source", spec: "host/data:/data", wantErr: mount.ErrInvalidMountSpec},
		{name: "relative destination", spec: "/host/data:data", wantErr: mount.ErrInvalidMountSpec},
		{name: "too many fields", spec: "/host/data:/data:ro:extra", wantErr: mount.ErrInvalidMountSpec},
		{name: "unknown option", spec: "/host/data:/data:rw", wantErr: mount.ErrInvalidOption},
		{name: "empty option", spec: "/host/data:/data:ro,", wantErr: mount.ErrInvalidOption},
		{name: "destination escapes with ..", spec: "/host/data:/../etc", wantErr: mount.ErrEscapesRoot},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := mount.ParseMountSpec(tt.spec)
			if err == nil {
				t.Fatalf("ParseMountSpec(%q) = nil, want an error", tt.spec)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseMountSpec(%q) = %v, want %v", tt.spec, err, tt.wantErr)
			}
		})
	}
}

// TestParseMountSpecErrorNamesTheSpec keeps the failure actionable: a user who
// typo'd one of several --mount flags needs to know which one.
func TestParseMountSpecErrorNamesTheSpec(t *testing.T) {
	t.Parallel()

	const spec = "/host/data:/data:rw"

	_, err := mount.ParseMountSpec(spec)
	if err == nil {
		t.Fatal("ParseMountSpec() = nil, want an error")
	}
	if !strings.Contains(err.Error(), spec) {
		t.Errorf("error %q does not quote the offending spec", err)
	}
}

func TestPlanValidate(t *testing.T) {
	t.Parallel()

	bind := func(src, dst string) mount.Mount {
		return mount.Mount{Source: src, Destination: dst, Type: mount.TypeBind}
	}

	// A plan's Source is the host tree bind-mounted onto Root. That bind is
	// what makes Root a mount point, which pivot_root(2) requires. In Stage 5,
	// when layers are unpacked into Root directly, Source equals Root and the
	// bind becomes a self-bind that does nothing but satisfy that requirement.
	const (
		source = "/srv/images/alpine"
		root   = "/var/lib/forge/containers/abc/rootfs"
	)

	tests := []struct {
		name    string
		plan    mount.Plan
		wantErr error
	}{
		{
			name: "a plan with no mounts is valid",
			plan: mount.Plan{Source: source, Root: root},
		},
		{
			name: "typical plan",
			plan: mount.Plan{
				Source: source,
				Root:   root,
				Mounts: []mount.Mount{
					{Destination: "/proc", Type: mount.TypeProc},
					bind("/host/data", "/data"),
				},
			},
		},
		{
			name: "a self-bind is valid: that is what stage 5 produces",
			plan: mount.Plan{Source: root, Root: root},
		},
		{
			name:    "empty root is rejected",
			plan:    mount.Plan{Source: source, Mounts: []mount.Mount{bind("/host/data", "/data")}},
			wantErr: mount.ErrInvalidMountSpec,
		},
		{
			name:    "relative root is rejected",
			plan:    mount.Plan{Source: source, Root: "containers/abc/rootfs"},
			wantErr: mount.ErrInvalidMountSpec,
		},
		{
			name:    "empty source is rejected",
			plan:    mount.Plan{Root: root},
			wantErr: mount.ErrInvalidMountSpec,
		},
		{
			name:    "relative source is rejected",
			plan:    mount.Plan{Source: "images/alpine", Root: root},
			wantErr: mount.ErrInvalidMountSpec,
		},
		{
			name:    "the host root is rejected as a container root",
			plan:    mount.Plan{Source: source, Root: "/"},
			wantErr: mount.ErrEscapesRoot,
		},
		{
			name:    "the host root is rejected as a source",
			plan:    mount.Plan{Source: "/", Root: root},
			wantErr: mount.ErrEscapesRoot,
		},
		{
			name: "duplicate destinations are rejected rather than last-wins",
			plan: mount.Plan{
				Source: source,
				Root:   root,
				Mounts: []mount.Mount{
					bind("/host/one", "/data"),
					bind("/host/two", "/data"),
				},
			},
			wantErr: mount.ErrDuplicateDestination,
		},
		{
			name: "a destination equal after cleaning is still a duplicate",
			plan: mount.Plan{
				Source: source,
				Root:   root,
				Mounts: []mount.Mount{
					bind("/host/one", "/data"),
					bind("/host/two", "/data/"),
				},
			},
			wantErr: mount.ErrDuplicateDestination,
		},
		{
			name: "an invalid member invalidates the plan",
			plan: mount.Plan{
				Source: source,
				Root:   root,
				Mounts: []mount.Mount{bind("", "/data")},
			},
			wantErr: mount.ErrInvalidMountSpec,
		},
		{
			name: "a destination escaping the root is rejected",
			plan: mount.Plan{
				Source: source,
				Root:   root,
				Mounts: []mount.Mount{bind("/host/data", "/../../etc")},
			},
			wantErr: mount.ErrEscapesRoot,
		},
		{
			name: "a mount over the container root itself is rejected",
			plan: mount.Plan{
				Source: source,
				Root:   root,
				Mounts: []mount.Mount{bind("/host/data", "/")},
			},
			wantErr: mount.ErrEscapesRoot,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.plan.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestPlanOrdered covers the ordering rule that keeps a nested mount from being
// swallowed: mounting /var after /var/log would hide the inner mount entirely.
func TestPlanOrdered(t *testing.T) {
	t.Parallel()

	bind := func(dst string) mount.Mount {
		return mount.Mount{Source: "/host" + dst, Destination: dst, Type: mount.TypeBind}
	}

	tests := []struct {
		name  string
		input []mount.Mount
		want  []string
	}{
		{
			name:  "nested destinations are ordered shallowest first",
			input: []mount.Mount{bind("/var/log"), bind("/var")},
			want:  []string{"/var", "/var/log"},
		},
		{
			name:  "three levels",
			input: []mount.Mount{bind("/a/b/c"), bind("/a"), bind("/a/b")},
			want:  []string{"/a", "/a/b", "/a/b/c"},
		},
		{
			name:  "already ordered input is unchanged",
			input: []mount.Mount{bind("/a"), bind("/a/b")},
			want:  []string{"/a", "/a/b"},
		},
		{
			name:  "siblings keep their input order",
			input: []mount.Mount{bind("/z"), bind("/a"), bind("/m")},
			want:  []string{"/z", "/a", "/m"},
		},
		{
			name:  "no mounts",
			input: nil,
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			plan := mount.Plan{
				Source: "/srv/images/alpine",
				Root:   "/var/lib/forge/containers/abc/rootfs",
				Mounts: tt.input,
			}

			got := destinations(plan.Ordered())
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("Ordered() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestOrderedDoesNotMutateThePlan guards against an in-place sort surprising a
// caller that logs the plan after applying it.
func TestOrderedDoesNotMutateThePlan(t *testing.T) {
	t.Parallel()

	plan := mount.Plan{
		Source: "/srv/images/alpine",
		Root:   "/var/lib/forge/containers/abc/rootfs",
		Mounts: []mount.Mount{
			{Source: "/host/var/log", Destination: "/var/log", Type: mount.TypeBind},
			{Source: "/host/var", Destination: "/var", Type: mount.TypeBind},
		},
	}
	before := destinations(plan.Mounts)

	_ = plan.Ordered()

	if after := destinations(plan.Mounts); strings.Join(after, " ") != strings.Join(before, " ") {
		t.Errorf("Ordered() reordered the plan in place: %v, want %v", after, before)
	}
}

// destinations extracts the destination of each mount, for readable assertions.
func destinations(mounts []mount.Mount) []string {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]string, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, m.Destination)
	}
	return out
}

// sameOptions compares two option lists by value and order.
func sameOptions(got, want []mount.Option) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
