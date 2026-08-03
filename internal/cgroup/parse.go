package cgroup

import (
	"fmt"
	"strconv"
	"strings"
)

// Value parsers for the quantities Limits carries.
//
// These are pure string→value functions. They know nothing about flags, flag
// names, or where the string came from — a caller that reads "128m" from a
// config file or an image manifest uses the same function the CLI does. The
// conversion rules live here, next to the types that define the units, rather
// than in a CLI package that is supposed to hold no logic (SSOT §13.6). This
// mirrors mount.ParseMountSpec from Stage 2.

// byteSuffixes maps a memory suffix to its multiplier.
//
// Both the binary spellings (ki, mi) and the short ones (k, m) mean 1024-based
// units, which is what every container runtime means by "512m" and what an
// operator expects when comparing the number against memory.max.
var byteSuffixes = []struct {
	suffix     string
	multiplier int64
}{
	// Longest first: "kib" must be tested before "k", or "k" matches its tail.
	{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
	{"kb", 1 << 10}, {"mb", 1 << 20}, {"gb", 1 << 30}, {"tb", 1 << 40},
	{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40},
	{"b", 1},
}

// ParseBytes parses a memory quantity such as "128m", "1GiB", "1048576" or
// "max". A bare number is bytes.
//
// Fractional quantities ("1.5g") are rejected. They are ambiguous at the byte
// level and the caller can always express the same limit exactly in a smaller
// unit, so refusing is better than rounding silently.
func ParseBytes(s string) (Bytes, error) {
	text := strings.ToLower(strings.TrimSpace(s))
	if text == "" {
		return 0, fmt.Errorf("%w: empty memory limit", ErrInvalidLimit)
	}
	if text == Unlimited {
		return UnlimitedBytes, nil
	}

	digits, multiplier := text, int64(1)
	for _, unit := range byteSuffixes {
		if rest, found := strings.CutSuffix(text, unit.suffix); found {
			digits, multiplier = rest, unit.multiplier
			break
		}
	}

	value, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a memory quantity such as 128m, 1g or 1048576", ErrInvalidLimit, s)
	}
	if value < 0 {
		return 0, fmt.Errorf("%w: memory limit %q is negative; use %q for no limit", ErrInvalidLimit, s, Unlimited)
	}
	// Detect the overflow before it wraps into a plausible-looking small limit.
	if value != 0 && value > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("%w: memory limit %q overflows int64", ErrInvalidLimit, s)
	}

	return Bytes(value * multiplier), nil
}

// ParseCPUs parses a CPU count such as "1.5" — one and a half cores — into the
// quota and period cpu.max expects, using the default 100ms period. "max"
// yields an unlimited quota.
//
// The decimal is parsed digit by digit rather than through a float: cpu.max is
// integer microseconds, and float rounding here would produce quotas that are
// off by one microsecond from the obvious answer for common inputs.
func ParseCPUs(s string) (CPUQuota, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return CPUQuota{}, fmt.Errorf("%w: empty cpu limit", ErrInvalidLimit)
	}
	if strings.EqualFold(text, Unlimited) {
		return CPUQuota{Quota: UnlimitedQuota, Period: DefaultCPUPeriod}, nil
	}

	whole, frac, _ := strings.Cut(text, ".")
	if whole == "" {
		whole = "0" // ".5" is a legitimate way to write half a core.
	}

	cores, err := strconv.ParseInt(whole, 10, 32)
	if err != nil || cores < 0 {
		return CPUQuota{}, fmt.Errorf("%w: %q is not a cpu count such as 0.5, 1 or 2.5", ErrInvalidLimit, s)
	}

	// Scale the fraction to microseconds of a whole core, so 1.5 with the
	// default period is 100000 + 50000 = 150000us per 100000us.
	micros := int64(0)
	if frac != "" {
		const precision = 6
		if len(frac) > precision {
			frac = frac[:precision] // sub-microsecond digits cannot be expressed
		}
		padded := frac + strings.Repeat("0", precision-len(frac))
		if micros, err = strconv.ParseInt(padded, 10, 64); err != nil {
			return CPUQuota{}, fmt.Errorf("%w: %q is not a cpu count such as 0.5, 1 or 2.5", ErrInvalidLimit, s)
		}
	}

	quota := cores*DefaultCPUPeriod + micros*DefaultCPUPeriod/1_000_000
	q := CPUQuota{Quota: quota, Period: DefaultCPUPeriod}
	if err := q.Validate(); err != nil {
		return CPUQuota{}, fmt.Errorf("%w (from %q)", err, s)
	}

	return q, nil
}

// ParseWeight parses a cpu.weight value.
func ParseWeight(s string) (Weight, error) {
	text := strings.TrimSpace(s)

	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a cpu weight between %d and %d",
			ErrInvalidLimit, s, MinWeight, MaxWeight)
	}

	weight := Weight(value)
	if weight < MinWeight || weight > MaxWeight {
		return 0, fmt.Errorf("%w: cpu weight %d is outside the kernel's range %d-%d",
			ErrInvalidLimit, value, MinWeight, MaxWeight)
	}

	return weight, nil
}

// ParsePIDs parses a pids.max value. "max" yields an unlimited count.
func ParsePIDs(s string) (int64, error) {
	text := strings.TrimSpace(s)
	if strings.EqualFold(text, Unlimited) {
		return UnlimitedPIDs, nil
	}

	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a process count such as 64 or %q", ErrInvalidLimit, s, Unlimited)
	}
	if value < 0 {
		return 0, fmt.Errorf("%w: process limit %q is negative; use %q for no limit", ErrInvalidLimit, s, Unlimited)
	}
	if value == 0 {
		return 0, fmt.Errorf("%w: a pids limit of 0 leaves no process to run", ErrInvalidLimit)
	}

	return value, nil
}
