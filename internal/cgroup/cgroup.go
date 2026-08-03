// Package cgroup creates, configures and removes cgroup v2 leaves.
//
// One leaf per container (FR-3.1), at <root>/forge/<container-id>. Everything
// the kernel is told is a plain file write into the unified hierarchy — no
// syscall wrappers, no helper library (ADR-0002). cgroup v2 *is* a filesystem
// interface, so os and path/filepath are the whole toolkit.
//
// The package is deliberately ignorant of what a container is. It is handed a
// container ID and a typed Limits value and performs it; per SSOT §2 it never
// decides *what* a container should be limited to, and it never reads a flag,
// an environment variable, or any global state. Turning "128m" into a Bytes is
// value parsing (parse.go) and stays here next to the type that defines the
// unit; turning a -memory flag into that string is the CLI's job.
//
// # The two halves
//
// Like internal/mount, the package separates computation from kernel contact:
//
//   - cgroup.go and parse.go are pure. Limits.Files renders the exact bytes
//     each controller file will receive, and is where all the unit arithmetic
//     lives, so it can be tested exhaustively as data.
//   - manager.go, apply.go and cleanup.go perform those writes and remove the
//     leaf again. They touch the filesystem, but the filesystem they touch is
//     Manager.root — point it at a t.TempDir() and they run unprivileged.
//
// # Rollback
//
// There is none here. A Create that fails part-way leaves the leaf in place and
// says so; internal/runtime owns the cleanup stack and calls Destroy, which is
// idempotent and safe against a leaf that was never created (SSOT §11.3,
// §13.3).
package cgroup

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Layout of Forge's cgroups.
const (
	// DefaultRoot is the conventional mount point of the cgroup v2 unified
	// hierarchy. New uses it when given an empty root.
	DefaultRoot = "/sys/fs/cgroup"

	// ParentName is Forge's own subtree under the root. Every container leaf
	// is a child of it, which keeps Forge's cgroups identifiable and means the
	// controllers Forge needs are enabled in exactly one place.
	ParentName = "forge"
)

// CPU quota units. cpu.max is expressed in microseconds: "$QUOTA $PERIOD"
// means the cgroup may consume $QUOTA microseconds of CPU time in every
// $PERIOD microseconds of wall time.
const (
	// DefaultCPUPeriod is the kernel's own default period, 100ms.
	DefaultCPUPeriod int64 = 100_000

	// MinCPUPeriod and MaxCPUPeriod are the bounds the kernel accepts.
	MinCPUPeriod int64 = 1_000
	MaxCPUPeriod int64 = 1_000_000

	// MinCPUQuota is the smallest positive quota the kernel accepts. Anything
	// less is rejected here rather than by an opaque EINVAL from a write.
	MinCPUQuota int64 = 1_000
)

// cpu.weight bounds. Weight is relative: a cgroup with weight 200 gets twice
// the CPU of a sibling with weight 100 *when both are runnable*, and neither
// is capped. This is the share-based half of FR-3.3; CPUQuota is the hard cap.
const (
	MinWeight     Weight = 1
	MaxWeight     Weight = 10_000
	DefaultWeight Weight = 100
)

// Unlimited is what the kernel writes and accepts for "no limit".
const Unlimited = "max"

// Negative quantities mean "no limit". A pointer field distinguishes "the
// caller said nothing" (nil) from "the caller asked for a value"; within a
// value, a negative number distinguishes "explicitly unlimited" from a real
// number. Zero is neither — see Limits.Validate.
const (
	UnlimitedBytes Bytes = -1
	UnlimitedPIDs  int64 = -1
	UnlimitedQuota int64 = -1
)

// Sentinel errors callers may branch on (SSOT §5).
var (
	// ErrUnifiedHierarchyNotMounted reports a root that is not a cgroup v2
	// unified hierarchy — typically a host still running cgroup v1 or the
	// hybrid layout, where none of this package works.
	ErrUnifiedHierarchyNotMounted = errors.New("cgroup v2 unified hierarchy not mounted")

	// ErrControllerUnavailable reports a limit whose controller the kernel is
	// not offering at this level of the hierarchy.
	ErrControllerUnavailable = errors.New("cgroup controller unavailable")

	// ErrInvalidLimit reports a limit the kernel would reject, or that would
	// produce a container that cannot run at all.
	ErrInvalidLimit = errors.New("invalid resource limit")

	// ErrInvalidContainerID reports an ID that cannot be used as a single path
	// element. It is what stops an ID from escaping Forge's subtree.
	ErrInvalidContainerID = errors.New("invalid container id")

	// ErrExists reports a leaf that already exists, which means two containers
	// were given the same ID.
	ErrExists = errors.New("cgroup already exists")

	// ErrNotFound reports an operation on a leaf that does not exist.
	ErrNotFound = errors.New("cgroup not found")

	// ErrPermission reports that the caller cannot manage cgroups.
	ErrPermission = errors.New("managing cgroups requires root (or CAP_SYS_ADMIN)")

	// ErrNotEmpty reports a leaf that still had member processes after Destroy
	// tried to evict them.
	ErrNotEmpty = errors.New("cgroup still has member processes")
)

// Controller is a cgroup v2 controller, spelled as it appears in
// cgroup.controllers and cgroup.subtree_control.
type Controller string

// The controllers Stage 3 uses.
const (
	ControllerCPU    Controller = "cpu"
	ControllerMemory Controller = "memory"
	ControllerPIDs   Controller = "pids"
)

// Bytes is a memory quantity in bytes. A negative Bytes means unlimited.
type Bytes int64

// String renders the value as memory.max expects it.
func (b Bytes) String() string {
	if b < 0 {
		return Unlimited
	}
	return strconv.FormatInt(int64(b), 10)
}

// Weight is a cpu.weight value.
type Weight uint64

// String renders the value as cpu.weight expects it.
func (w Weight) String() string { return strconv.FormatUint(uint64(w), 10) }

// CPUQuota is a cpu.max setting: Quota microseconds of CPU time per Period
// microseconds of wall time. A negative Quota means unlimited; a zero Period
// means DefaultCPUPeriod.
//
// Both fields are microseconds rather than time.Duration because these are the
// kernel's own units, and a Duration would invite a rounding bug between the
// two representations for no gain.
type CPUQuota struct {
	Quota  int64
	Period int64
}

// normalized fills in the default period. It is applied by String and Validate
// so that CPUQuota{Quota: 50_000} is a complete, meaningful value.
func (q CPUQuota) normalized() CPUQuota {
	if q.Period == 0 {
		q.Period = DefaultCPUPeriod
	}
	return q
}

// String renders the value as cpu.max expects it: "$QUOTA $PERIOD".
func (q CPUQuota) String() string {
	n := q.normalized()
	quota := Unlimited
	if n.Quota >= 0 {
		quota = strconv.FormatInt(n.Quota, 10)
	}
	return quota + " " + strconv.FormatInt(n.Period, 10)
}

// Validate reports whether the kernel would accept the quota.
func (q CPUQuota) Validate() error {
	n := q.normalized()

	if n.Period < MinCPUPeriod || n.Period > MaxCPUPeriod {
		return fmt.Errorf("%w: cpu period %dus is outside the kernel's range %d-%dus",
			ErrInvalidLimit, n.Period, MinCPUPeriod, MaxCPUPeriod)
	}
	// Zero is the trap: it is not "unlimited", it is "no CPU at all", and a
	// container given it never makes progress. Unlimited is a negative quota.
	if n.Quota == 0 {
		return fmt.Errorf("%w: a cpu quota of 0 would give the container no CPU time; use a negative quota for %q",
			ErrInvalidLimit, Unlimited)
	}
	if n.Quota > 0 && n.Quota < MinCPUQuota {
		return fmt.Errorf("%w: cpu quota %dus is below the kernel's minimum of %dus",
			ErrInvalidLimit, n.Quota, MinCPUQuota)
	}

	return nil
}

// Limits is the complete set of constraints for one container.
//
// Every field is a pointer because nil — "the caller did not ask for this" —
// has to be distinguishable from a real value, and 0 is a real value for
// memory.max and pids.max. A nil field means Forge writes nothing to that
// controller file and whatever the leaf inherits stands.
type Limits struct {
	// MemoryMax is the memory.max limit (FR-3.2).
	MemoryMax *Bytes

	// CPU is the cpu.max limit: a hard ceiling on CPU time (FR-3.3).
	CPU *CPUQuota

	// CPUWeight is the cpu.weight share (FR-3.3). Orthogonal to CPU: a
	// container may have both a ceiling and a share of what is left over.
	CPUWeight *Weight

	// PIDsMax is the pids.max limit (FR-3.4).
	PIDsMax *int64
}

// IsZero reports whether the caller asked for no limits at all. Such a
// container still gets a leaf — FR-3.1 is unconditional, and an unlimited leaf
// still provides accounting — but no controller file is written.
func (l Limits) IsZero() bool {
	return l.MemoryMax == nil && l.CPU == nil && l.CPUWeight == nil && l.PIDsMax == nil
}

// Validate reports whether every limit set can be written and would leave a
// container that can actually run. It is pure and performs no syscalls, so bad
// input is refused before anything is created.
func (l Limits) Validate() error {
	if l.MemoryMax != nil && *l.MemoryMax == 0 {
		// The kernel accepts it, then OOM-kills the container's first
		// allocation. Refusing is more useful than obeying.
		return fmt.Errorf("%w: a memory limit of 0 leaves no memory to run in", ErrInvalidLimit)
	}
	if l.PIDsMax != nil && *l.PIDsMax == 0 {
		return fmt.Errorf("%w: a pids limit of 0 leaves no process to run", ErrInvalidLimit)
	}
	if l.CPU != nil {
		if err := l.CPU.Validate(); err != nil {
			return err
		}
	}
	if l.CPUWeight != nil && (*l.CPUWeight < MinWeight || *l.CPUWeight > MaxWeight) {
		return fmt.Errorf("%w: cpu weight %d is outside the kernel's range %d-%d",
			ErrInvalidLimit, *l.CPUWeight, MinWeight, MaxWeight)
	}

	return nil
}

// Controllers returns the controllers these limits require, in a stable order.
// It is what Manager delegates in cgroup.subtree_control: only what is asked
// for, so a host missing a controller Forge is not using stays usable.
func (l Limits) Controllers() []Controller {
	var controllers []Controller

	if l.CPU != nil || l.CPUWeight != nil {
		controllers = append(controllers, ControllerCPU)
	}
	if l.MemoryMax != nil {
		controllers = append(controllers, ControllerMemory)
	}
	if l.PIDsMax != nil {
		controllers = append(controllers, ControllerPIDs)
	}

	return controllers
}

// File is one controller file and the exact bytes to write to it. Name is
// relative to the leaf directory.
type File struct {
	Name  string
	Value string

	// Optional marks a file whose absence is not a failure. Interface files
	// are created by the kernel when the cgroup is made, so a missing one
	// normally means the controller was never delegated — an error. The
	// exception is memory.swap.max, which a kernel built without swap
	// accounting does not have at all; see Files.
	Optional bool
}

// Files renders the limits as the writes that implement them, in a fixed
// order. This is the whole of the package's unit arithmetic and the seam its
// tests are built on: what the kernel is told is a pure function of Limits,
// asserted as data rather than inferred from a filesystem afterwards.
func (l Limits) Files() []File {
	var files []File

	if l.MemoryMax != nil {
		files = append(files,
			File{Name: "memory.max", Value: l.MemoryMax.String()},
			// Swap is limited alongside memory, never independently. See
			// swapFor for why, and docs/stages/stage-3.md §9 for the
			// trade-off this policy makes.
			File{Name: "memory.swap.max", Value: swapFor(*l.MemoryMax), Optional: true},
		)
	}
	if l.CPU != nil {
		files = append(files, File{Name: "cpu.max", Value: l.CPU.String()})
	}
	if l.CPUWeight != nil {
		files = append(files, File{Name: "cpu.weight", Value: l.CPUWeight.String()})
	}
	if l.PIDsMax != nil {
		value := Unlimited
		if *l.PIDsMax >= 0 {
			value = strconv.FormatInt(*l.PIDsMax, 10)
		}
		files = append(files, File{Name: "pids.max", Value: value})
	}

	return files
}

// swapFor returns the memory.swap.max that accompanies a memory.max.
//
// memory.max caps a cgroup's *resident* memory. It says nothing about swap,
// which cgroup v2 accounts separately and leaves unlimited by default, so a
// container held at 32MB of RAM can page out indefinitely: it is never
// OOM-killed, it just gets slower and the host thrashes. A limit that can be
// escaped by being slow is not a limit, so Forge writes both.
//
// The policy is that swap mirrors memory rather than being configurable:
//
//   - A finite memory.max gets memory.swap.max=0. The container lives in RAM
//     or it dies, which is what "--memory 32m" plainly means.
//   - An explicitly unlimited memory.max gets memory.swap.max=max, so asking
//     for no limit is not quietly turned into a swap restriction.
//
// A container that asks for no memory limit at all has neither file written
// and inherits both, exactly as before.
func swapFor(memory Bytes) string {
	if memory < 0 {
		return Unlimited
	}
	return "0"
}

// validateContainerID reports whether id can be used as a single path element
// under Forge's parent cgroup.
//
// This is a containment check, not a style check: an ID containing a separator
// or ".." would place a container's cgroup outside <root>/forge, and Destroy
// would then remove a directory that is not Forge's.
func validateContainerID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: empty", ErrInvalidContainerID)
	case id == "." || id == "..":
		return fmt.Errorf("%w: %q is a path element with special meaning", ErrInvalidContainerID, id)
	case strings.ContainsAny(id, `/\`):
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidContainerID, id)
	case strings.ContainsRune(id, 0):
		return fmt.Errorf("%w: contains a NUL byte", ErrInvalidContainerID)
	}

	return nil
}
