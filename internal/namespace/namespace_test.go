package namespace_test

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/stevenstank/forge/internal/namespace"
)

func TestConfigCloneFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  namespace.Config
		want uintptr
	}{
		{
			name: "zero config requests no namespaces",
			cfg:  namespace.Config{},
			want: 0,
		},
		{
			name: "pid only",
			cfg:  namespace.Config{PID: true},
			want: syscall.CLONE_NEWPID,
		},
		{
			name: "uts only",
			cfg:  namespace.Config{UTS: true},
			want: syscall.CLONE_NEWUTS,
		},
		{
			name: "mount only",
			cfg:  namespace.Config{Mount: true},
			want: syscall.CLONE_NEWNS,
		},
		{
			name: "stage 1 combination",
			cfg:  namespace.Config{PID: true, UTS: true, Mount: true},
			want: syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS,
		},
		{
			name: "hostname does not affect flags",
			cfg:  namespace.Config{UTS: true, Hostname: "forge"},
			want: syscall.CLONE_NEWUTS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.cfg.CloneFlags(); got != tt.want {
				t.Errorf("CloneFlags() = %#x, want %#x", got, tt.want)
			}
		})
	}
}

// TestCloneFlagsExcludesUnrequestedNamespaces guards SSOT §13.5: Stage 1 must
// not quietly create namespaces belonging to later stages, such as CLONE_NEWNET.
func TestCloneFlagsExcludesUnrequestedNamespaces(t *testing.T) {
	t.Parallel()

	got := namespace.Config{PID: true, UTS: true, Mount: true}.CloneFlags()

	for _, forbidden := range []struct {
		name string
		flag uintptr
	}{
		{"CLONE_NEWNET", syscall.CLONE_NEWNET},
		{"CLONE_NEWUSER", syscall.CLONE_NEWUSER},
		{"CLONE_NEWIPC", syscall.CLONE_NEWIPC},
	} {
		if got&forbidden.flag != 0 {
			t.Errorf("CloneFlags() = %#x, must not include %s (%#x)", got, forbidden.name, forbidden.flag)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     namespace.Config
		wantErr error
	}{
		{
			name: "zero config is valid",
			cfg:  namespace.Config{},
		},
		{
			name: "namespaces without a hostname are valid",
			cfg:  namespace.Config{PID: true, UTS: true, Mount: true},
		},
		{
			name: "hostname with uts is valid",
			cfg:  namespace.Config{UTS: true, Hostname: "forge-a1b2c3"},
		},
		{
			name: "dotted hostname is valid",
			cfg:  namespace.Config{UTS: true, Hostname: "node1.forge.local"},
		},
		{
			name:    "hostname without uts is rejected",
			cfg:     namespace.Config{Hostname: "forge"},
			wantErr: namespace.ErrHostnameWithoutUTS,
		},
		{
			name:    "hostname without uts is rejected even with other namespaces",
			cfg:     namespace.Config{PID: true, Mount: true, Hostname: "forge"},
			wantErr: namespace.ErrHostnameWithoutUTS,
		},
		{
			name:    "over-long hostname is rejected",
			cfg:     namespace.Config{UTS: true, Hostname: strings.Repeat("a", namespace.MaxHostnameLen+1)},
			wantErr: namespace.ErrInvalidHostname,
		},
		{
			name: "hostname at the limit is accepted",
			cfg:  namespace.Config{UTS: true, Hostname: strings.Repeat("a", namespace.MaxHostnameLen)},
		},
		{
			name:    "hostname with a space is rejected",
			cfg:     namespace.Config{UTS: true, Hostname: "forge box"},
			wantErr: namespace.ErrInvalidHostname,
		},
		{
			name:    "hostname with a slash is rejected",
			cfg:     namespace.Config{UTS: true, Hostname: "forge/box"},
			wantErr: namespace.ErrInvalidHostname,
		},
		{
			name:    "hostname with a NUL byte is rejected",
			cfg:     namespace.Config{UTS: true, Hostname: "forge\x00box"},
			wantErr: namespace.ErrInvalidHostname,
		},
		{
			name:    "leading hyphen is rejected",
			cfg:     namespace.Config{UTS: true, Hostname: "-forge"},
			wantErr: namespace.ErrInvalidHostname,
		},
		{
			name:    "trailing hyphen is rejected",
			cfg:     namespace.Config{UTS: true, Hostname: "forge-"},
			wantErr: namespace.ErrInvalidHostname,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.cfg.Validate()
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

// TestApplyRejectsInvalidConfig confirms Apply validates before touching the
// kernel, so a bad hostname cannot rename the host on a config that omits UTS.
// This runs without root precisely because validation short-circuits first.
func TestApplyRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	err := namespace.Apply(namespace.Config{Hostname: "forge"})
	if !errors.Is(err, namespace.ErrHostnameWithoutUTS) {
		t.Fatalf("Apply() = %v, want %v", err, namespace.ErrHostnameWithoutUTS)
	}
}

// TestApplyZeroConfigIsANoOp ensures Apply performs no privileged operation
// when nothing was requested; it must therefore succeed as an ordinary user.
func TestApplyZeroConfigIsANoOp(t *testing.T) {
	t.Parallel()

	if err := namespace.Apply(namespace.Config{}); err != nil {
		t.Fatalf("Apply(zero) = %v, want nil", err)
	}
}
