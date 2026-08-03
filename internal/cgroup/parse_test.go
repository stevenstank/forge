package cgroup_test

import (
	"errors"
	"testing"

	"github.com/stevenstank/forge/internal/cgroup"
)

func TestParseBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    cgroup.Bytes
		wantErr bool
	}{
		{name: "bare number is bytes", in: "1048576", want: 1 << 20},
		{name: "zero is accepted here and rejected by Validate", in: "0", want: 0},
		{name: "kilobytes are 1024 bytes", in: "512k", want: 512 << 10},
		{name: "megabytes", in: "128m", want: 128 << 20},
		{name: "gigabytes", in: "2g", want: 2 << 30},
		{name: "terabytes", in: "1t", want: 1 << 40},
		{name: "explicit binary units", in: "128MiB", want: 128 << 20},
		{name: "the b suffix is bytes", in: "4096b", want: 4096},
		{name: "case is irrelevant", in: "1G", want: 1 << 30},
		{name: "surrounding space is trimmed", in: "  64m  ", want: 64 << 20},
		{name: "max", in: "max", want: cgroup.UnlimitedBytes},

		{name: "empty", in: "", wantErr: true},
		{name: "not a number", in: "lots", wantErr: true},
		{name: "unknown suffix", in: "12x", wantErr: true},
		// Rejected rather than rounded: the caller can say 1536m and mean it
		// exactly.
		{name: "fractions are ambiguous at byte level", in: "1.5g", wantErr: true},
		{name: "negative", in: "-1", wantErr: true},
		{name: "negative with a suffix", in: "-1g", wantErr: true},
		{name: "overflows int64", in: "9999999999t", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := cgroup.ParseBytes(tt.in)
			if tt.wantErr {
				if !errors.Is(err, cgroup.ErrInvalidLimit) {
					t.Fatalf("ParseBytes(%q) error = %v, want ErrInvalidLimit", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBytes(%q) = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseBytes(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseCPUs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    cgroup.CPUQuota
		wantErr bool
	}{
		{name: "one core", in: "1", want: cgroup.CPUQuota{Quota: 100_000, Period: 100_000}},
		{name: "half a core", in: "0.5", want: cgroup.CPUQuota{Quota: 50_000, Period: 100_000}},
		{name: "a leading dot", in: ".5", want: cgroup.CPUQuota{Quota: 50_000, Period: 100_000}},
		{name: "one and a half cores", in: "1.5", want: cgroup.CPUQuota{Quota: 150_000, Period: 100_000}},
		{name: "two and a half cores", in: "2.5", want: cgroup.CPUQuota{Quota: 250_000, Period: 100_000}},
		{name: "more cores than most hosts have", in: "16", want: cgroup.CPUQuota{Quota: 1_600_000, Period: 100_000}},
		{name: "a thousandth of a core is still above the minimum quota",
			in: "0.01", want: cgroup.CPUQuota{Quota: 1_000, Period: 100_000}},
		{name: "max", in: "max", want: cgroup.CPUQuota{Quota: cgroup.UnlimitedQuota, Period: 100_000}},

		{name: "empty", in: "", wantErr: true},
		{name: "not a number", in: "many", wantErr: true},
		{name: "negative", in: "-1", wantErr: true},
		// 0.001 cores is 100us, below the kernel's 1000us minimum quota.
		{name: "too small to express", in: "0.001", wantErr: true},
		{name: "zero cores would never run", in: "0", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := cgroup.ParseCPUs(tt.in)
			if tt.wantErr {
				if !errors.Is(err, cgroup.ErrInvalidLimit) {
					t.Fatalf("ParseCPUs(%q) error = %v, want ErrInvalidLimit", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCPUs(%q) = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseCPUs(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseWeight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    cgroup.Weight
		wantErr bool
	}{
		{name: "the kernel default", in: "100", want: cgroup.DefaultWeight},
		{name: "the minimum", in: "1", want: cgroup.MinWeight},
		{name: "the maximum", in: "10000", want: cgroup.MaxWeight},

		{name: "zero is below the minimum", in: "0", wantErr: true},
		{name: "above the maximum", in: "10001", wantErr: true},
		{name: "negative", in: "-1", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "not a number", in: "heavy", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := cgroup.ParseWeight(tt.in)
			if tt.wantErr {
				if !errors.Is(err, cgroup.ErrInvalidLimit) {
					t.Fatalf("ParseWeight(%q) error = %v, want ErrInvalidLimit", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWeight(%q) = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseWeight(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParsePIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{name: "a count", in: "64", want: 64},
		{name: "one process", in: "1", want: 1},
		{name: "max", in: "max", want: cgroup.UnlimitedPIDs},
		{name: "MAX", in: "MAX", want: cgroup.UnlimitedPIDs},

		{name: "zero leaves no process to run", in: "0", wantErr: true},
		{name: "negative", in: "-5", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "not a number", in: "some", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := cgroup.ParsePIDs(tt.in)
			if tt.wantErr {
				if !errors.Is(err, cgroup.ErrInvalidLimit) {
					t.Fatalf("ParsePIDs(%q) error = %v, want ErrInvalidLimit", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePIDs(%q) = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParsePIDs(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// The parsers and the renderers are two halves of one mapping, and a limit that
// survives a round trip is one an operator can read back off the filesystem and
// recognise as what they asked for.
func TestParseAndRenderRoundTrip(t *testing.T) {
	t.Parallel()

	memory, err := cgroup.ParseBytes("128m")
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	cpus, err := cgroup.ParseCPUs("1.5")
	if err != nil {
		t.Fatalf("ParseCPUs: %v", err)
	}
	procs, err := cgroup.ParsePIDs("64")
	if err != nil {
		t.Fatalf("ParsePIDs: %v", err)
	}

	limits := cgroup.Limits{MemoryMax: &memory, CPU: &cpus, PIDsMax: &procs}
	want := []cgroup.File{
		{Name: "memory.max", Value: "134217728"},
		{Name: "memory.swap.max", Value: "0", Optional: true},
		{Name: "cpu.max", Value: "150000 100000"},
		{Name: "pids.max", Value: "64"},
	}

	got := limits.Files()
	if len(got) != len(want) {
		t.Fatalf("Files() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Files()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
