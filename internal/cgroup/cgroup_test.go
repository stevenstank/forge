package cgroup_test

import (
	"errors"
	"testing"

	"github.com/stevenstank/forge/internal/cgroup"
)

// These tests cover the pure half of internal/cgroup: the rendering of a Limits
// value into the exact bytes each controller file receives, the validation that
// keeps an unrunnable container from being created, and the containment check
// on container IDs. None of it touches a filesystem (SSOT §7).
//
// Limits.Files is the most important of them. A units bug there is a silently
// wrong limit — the worst failure mode in the stage, because the container
// still starts — so every field is asserted as an exact string.

func bytes(b cgroup.Bytes) *cgroup.Bytes    { return &b }
func weight(w cgroup.Weight) *cgroup.Weight { return &w }
func pids(p int64) *int64                   { return &p }
func quota(q, period int64) *cgroup.CPUQuota {
	return &cgroup.CPUQuota{Quota: q, Period: period}
}

func TestLimitsFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		limits cgroup.Limits
		want   []cgroup.File
	}{
		{
			name:   "no limits writes nothing",
			limits: cgroup.Limits{},
			want:   nil,
		},
		{
			// A memory limit always carries a swap limit: memory.max caps
			// resident memory only, and a container left free to swap can
			// exceed it indefinitely without ever being killed.
			name:   "memory.max is bytes in decimal, and caps swap with it",
			limits: cgroup.Limits{MemoryMax: bytes(128 << 20)},
			want: []cgroup.File{
				{Name: "memory.max", Value: "134217728"},
				{Name: "memory.swap.max", Value: "0", Optional: true},
			},
		},
		{
			// Asking for no memory limit must not be turned into a swap
			// restriction that was never requested.
			name:   "a negative memory limit is max, and leaves swap unlimited",
			limits: cgroup.Limits{MemoryMax: bytes(cgroup.UnlimitedBytes)},
			want: []cgroup.File{
				{Name: "memory.max", Value: "max"},
				{Name: "memory.swap.max", Value: "max", Optional: true},
			},
		},
		{
			name:   "cpu.max is quota and period",
			limits: cgroup.Limits{CPU: quota(150_000, 100_000)},
			want:   []cgroup.File{{Name: "cpu.max", Value: "150000 100000"}},
		},
		{
			name:   "a zero period takes the kernel default",
			limits: cgroup.Limits{CPU: quota(50_000, 0)},
			want:   []cgroup.File{{Name: "cpu.max", Value: "50000 100000"}},
		},
		{
			name:   "a negative quota is max, with the period still written",
			limits: cgroup.Limits{CPU: quota(cgroup.UnlimitedQuota, 0)},
			want:   []cgroup.File{{Name: "cpu.max", Value: "max 100000"}},
		},
		{
			name:   "no memory limit means no swap limit either",
			limits: cgroup.Limits{PIDsMax: pids(64), CPUWeight: weight(100)},
			want: []cgroup.File{
				{Name: "cpu.weight", Value: "100"},
				{Name: "pids.max", Value: "64"},
			},
		},
		{
			name:   "cpu.weight",
			limits: cgroup.Limits{CPUWeight: weight(512)},
			want:   []cgroup.File{{Name: "cpu.weight", Value: "512"}},
		},
		{
			name:   "pids.max",
			limits: cgroup.Limits{PIDsMax: pids(64)},
			want:   []cgroup.File{{Name: "pids.max", Value: "64"}},
		},
		{
			name:   "a negative pids limit is max",
			limits: cgroup.Limits{PIDsMax: pids(cgroup.UnlimitedPIDs)},
			want:   []cgroup.File{{Name: "pids.max", Value: "max"}},
		},
		{
			name: "every limit at once, in a fixed order",
			limits: cgroup.Limits{
				MemoryMax: bytes(64 << 20),
				CPU:       quota(200_000, 100_000),
				CPUWeight: weight(200),
				PIDsMax:   pids(100),
			},
			want: []cgroup.File{
				{Name: "memory.max", Value: "67108864"},
				{Name: "memory.swap.max", Value: "0", Optional: true},
				{Name: "cpu.max", Value: "200000 100000"},
				{Name: "cpu.weight", Value: "200"},
				{Name: "pids.max", Value: "100"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.limits.Files()
			if len(got) != len(tt.want) {
				t.Fatalf("Files() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Files()[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestLimitsValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		limits  cgroup.Limits
		wantErr error
	}{
		{name: "no limits", limits: cgroup.Limits{}},
		{name: "ordinary limits", limits: cgroup.Limits{
			MemoryMax: bytes(1 << 20), CPU: quota(100_000, 100_000),
			CPUWeight: weight(cgroup.DefaultWeight), PIDsMax: pids(32),
		}},
		{name: "unlimited everything", limits: cgroup.Limits{
			MemoryMax: bytes(cgroup.UnlimitedBytes),
			CPU:       quota(cgroup.UnlimitedQuota, 0),
			PIDsMax:   pids(cgroup.UnlimitedPIDs),
		}},
		{
			name:    "zero memory leaves nothing to run in",
			limits:  cgroup.Limits{MemoryMax: bytes(0)},
			wantErr: cgroup.ErrInvalidLimit,
		},
		{
			name:    "zero pids leaves no process to run",
			limits:  cgroup.Limits{PIDsMax: pids(0)},
			wantErr: cgroup.ErrInvalidLimit,
		},
		{
			name:    "a zero quota is not the same as unlimited",
			limits:  cgroup.Limits{CPU: quota(0, 100_000)},
			wantErr: cgroup.ErrInvalidLimit,
		},
		{
			name:    "a quota below the kernel minimum",
			limits:  cgroup.Limits{CPU: quota(500, 100_000)},
			wantErr: cgroup.ErrInvalidLimit,
		},
		{
			name:    "a period outside the kernel range",
			limits:  cgroup.Limits{CPU: quota(100_000, 5_000_000)},
			wantErr: cgroup.ErrInvalidLimit,
		},
		{
			name:    "a weight above the maximum",
			limits:  cgroup.Limits{CPUWeight: weight(cgroup.MaxWeight + 1)},
			wantErr: cgroup.ErrInvalidLimit,
		},
		{
			name:    "a weight below the minimum",
			limits:  cgroup.Limits{CPUWeight: weight(0)},
			wantErr: cgroup.ErrInvalidLimit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.limits.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestLimitsControllers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		limits cgroup.Limits
		want   []cgroup.Controller
	}{
		{name: "none", limits: cgroup.Limits{}, want: nil},
		{
			name:   "memory only",
			limits: cgroup.Limits{MemoryMax: bytes(1 << 20)},
			want:   []cgroup.Controller{cgroup.ControllerMemory},
		},
		{
			name:   "a quota needs the cpu controller",
			limits: cgroup.Limits{CPU: quota(100_000, 0)},
			want:   []cgroup.Controller{cgroup.ControllerCPU},
		},
		{
			name:   "a weight needs the cpu controller too, and only once",
			limits: cgroup.Limits{CPU: quota(100_000, 0), CPUWeight: weight(100)},
			want:   []cgroup.Controller{cgroup.ControllerCPU},
		},
		{
			name: "all three, in a fixed order",
			limits: cgroup.Limits{
				PIDsMax: pids(10), MemoryMax: bytes(1 << 20), CPUWeight: weight(100),
			},
			want: []cgroup.Controller{
				cgroup.ControllerCPU, cgroup.ControllerMemory, cgroup.ControllerPIDs,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.limits.Controllers()
			if len(got) != len(tt.want) {
				t.Fatalf("Controllers() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Controllers()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestLimitsIsZero(t *testing.T) {
	t.Parallel()

	if !(cgroup.Limits{}).IsZero() {
		t.Error("an empty Limits should be zero")
	}
	// Explicitly unlimited is not the same as unset: the caller asked for
	// something, and Forge writes it.
	if (cgroup.Limits{MemoryMax: bytes(cgroup.UnlimitedBytes)}).IsZero() {
		t.Error("an explicit unlimited memory limit should not be zero")
	}
	if (cgroup.Limits{PIDsMax: pids(64)}).IsZero() {
		t.Error("a pids limit should not be zero")
	}
}
