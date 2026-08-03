package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These tests drive the writes themselves, against a directory holding exactly
// the interface files a real cgroup would have.
//
// That fidelity is the point. Forge opens interface files O_WRONLY and never
// creates them, because the kernel creates them with the cgroup and a missing
// one means the controller was never delegated. A test hierarchy therefore has
// to create them too — and because a leaf's files appear at mkdir time, which
// is inside Manager.Create, the writes are exercised here rather than through
// Create. What Create sends to a *real* kernel is asserted by the Stage 3
// integration tests.

// newLeaf returns a directory holding the named interface files, empty, as the
// kernel would have left them.
func newLeaf(t *testing.T, files ...string) string {
	t.Helper()

	dir := t.TempDir()
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	return dir
}

// readLeafFile returns the content of an interface file.
func readLeafFile(t *testing.T, dir, name string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}

	return string(content)
}

func TestApplyLimitsWritesExactBytes(t *testing.T) {
	t.Parallel()

	memory := Bytes(128 << 20)
	unlimited := UnlimitedBytes
	weight := Weight(512)
	procs := int64(64)

	tests := []struct {
		name   string
		limits Limits
		want   map[string]string
	}{
		{
			name:   "no limits writes nothing",
			limits: Limits{},
			want:   map[string]string{},
		},
		{
			name:   "a memory limit caps swap too",
			limits: Limits{MemoryMax: &memory},
			want:   map[string]string{"memory.max": "134217728", "memory.swap.max": "0"},
		},
		{
			// Asking for no memory limit must not be turned into a swap
			// restriction the caller never asked for.
			name:   "an explicitly unlimited memory limit leaves swap unlimited",
			limits: Limits{MemoryMax: &unlimited},
			want:   map[string]string{"memory.max": "max", "memory.swap.max": "max"},
		},
		{
			name:   "cpu quota and weight",
			limits: Limits{CPU: &CPUQuota{Quota: 150_000}, CPUWeight: &weight},
			want:   map[string]string{"cpu.max": "150000 100000", "cpu.weight": "512"},
		},
		{
			name: "everything at once",
			limits: Limits{
				MemoryMax: &memory,
				CPU:       &CPUQuota{Quota: 150_000},
				CPUWeight: &weight,
				PIDsMax:   &procs,
			},
			want: map[string]string{
				"memory.max":      "134217728",
				"memory.swap.max": "0",
				"cpu.max":         "150000 100000",
				"cpu.weight":      "512",
				"pids.max":        "64",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := newLeaf(t, "memory.max", "memory.swap.max", "cpu.max", "cpu.weight", "pids.max")

			if err := applyLimits(dir, tt.limits); err != nil {
				t.Fatalf("applyLimits() = %v", err)
			}

			for name, want := range tt.want {
				if got := readLeafFile(t, dir, name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}

			// Nothing beyond what was asked for: a file left empty is a limit
			// the caller did not set and the cgroup still inherits.
			for _, name := range []string{"memory.max", "memory.swap.max", "cpu.max", "cpu.weight", "pids.max"} {
				if _, expected := tt.want[name]; expected {
					continue
				}
				if got := readLeafFile(t, dir, name); got != "" {
					t.Errorf("%s = %q, want it untouched", name, got)
				}
			}
		})
	}
}

// A kernel built without swap accounting has no memory.swap.max at all. There
// is no per-cgroup swap interface for anyone to set on such a kernel, so the
// memory limit is applied and the run continues.
func TestApplyLimitsToleratesAKernelWithoutSwapAccounting(t *testing.T) {
	t.Parallel()

	dir := newLeaf(t, "memory.max") // no memory.swap.max

	memory := Bytes(64 << 20)
	if err := applyLimits(dir, Limits{MemoryMax: &memory}); err != nil {
		t.Fatalf("applyLimits() = %v, want the missing swap file to be tolerated", err)
	}

	if got := readLeafFile(t, dir, "memory.max"); got != "67108864" {
		t.Errorf("memory.max = %q, want the limit to still have been applied", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory.swap.max")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("memory.swap.max was created: %v", err)
	}
}

// A missing *required* interface file is the kernel saying the controller was
// never delegated to this cgroup. It must be an error, not a silently skipped
// limit.
func TestApplyLimitsReportsAnUndelegatedController(t *testing.T) {
	t.Parallel()

	dir := newLeaf(t, "cpu.max") // the memory controller is not delegated

	memory := Bytes(64 << 20)
	err := applyLimits(dir, Limits{MemoryMax: &memory})
	if !errors.Is(err, ErrControllerUnavailable) {
		t.Fatalf("applyLimits() = %v, want ErrControllerUnavailable", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("applyLimits() = %v, want the underlying cause to survive wrapping", err)
	}
}

// The regression this guards: os.WriteFile would have created a regular file
// here and reported success, leaving a "limit" nothing will ever enforce.
func TestWriteControlFileNeverCreatesAFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := writeControlFile(dir, "memory.max", "134217728"); err == nil {
		t.Fatal("writeControlFile() to a missing file = nil, want a failure")
	}
	if _, err := os.Stat(filepath.Join(dir, "memory.max")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("memory.max was created: %v", err)
	}
}

func TestAddProcWritesThePID(t *testing.T) {
	t.Parallel()

	dir := newLeaf(t, fileProcs)

	if err := addProc(dir, 4242); err != nil {
		t.Fatalf("addProc() = %v", err)
	}
	if got := readLeafFile(t, dir, fileProcs); got != "4242" {
		t.Errorf("%s = %q, want %q", fileProcs, got, "4242")
	}
}

func TestReadProcs(t *testing.T) {
	t.Parallel()

	dir := newLeaf(t, fileProcs)
	if err := os.WriteFile(filepath.Join(dir, fileProcs), []byte("101\n202\n303\n"), 0o644); err != nil {
		t.Fatalf("seeding %s: %v", fileProcs, err)
	}

	pids, err := readProcs(dir)
	if err != nil {
		t.Fatalf("readProcs() = %v", err)
	}
	if len(pids) != 3 || pids[0] != 101 || pids[2] != 303 {
		t.Errorf("readProcs() = %v, want [101 202 303]", pids)
	}

	// A cgroup that is already gone has no members, which is not an error to a
	// caller trying to empty it.
	empty, err := readProcs(filepath.Join(dir, "gone"))
	if err != nil || empty != nil {
		t.Errorf("readProcs(missing) = %v, %v, want nil, nil", empty, err)
	}
}
