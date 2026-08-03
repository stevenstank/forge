package runtime

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/stevenstank/forge/internal/cgroup"
)

// The Stage 3 half of the orchestration: deciding whether a container gets a
// cgroup, and making sure the one it gets is released.
//
// internal/cgroup performs cgroup operations and decides nothing; the policy
// below — when a missing hierarchy is fatal, when a partly-created leaf is
// removed, and where the teardown is registered — is a cross-package sequencing
// decision and so lives here (SSOT §2, §13.2).

// prepareCgroup creates the container's cgroup leaf and registers its removal.
//
// It returns the container ID to attach the container's init to, or an empty
// string for a container that has no cgroup — see the degradation rule below.
func (r *Runner) prepareCgroup(log *slog.Logger, id string, spec Spec, cleanup *cleanupStack) (string, error) {
	if err := r.cgroups.Create(id, spec.Limits); err != nil {
		if degraded := r.tolerateMissingCgroups(log, spec, err); degraded {
			return "", nil
		}

		// internal/cgroup rolls nothing back: a Create that failed after the
		// leaf existed leaves it behind, and there is no cleanup registered
		// for it yet. Remove it here, and let the original error stand — the
		// reason the run is failing is more useful than the reason the
		// tidy-up did (SSOT §5).
		if destroyErr := r.cgroups.Destroy(id); destroyErr != nil {
			log.Warn("removing the cgroup of a container that failed to start", "error", destroyErr)
		}
		return "", err
	}

	// Registered immediately, before anything else can fail: from here on
	// every path out of Run — a bad payload, a refused clone, a cancelled
	// context, a container that simply exits — unwinds through this
	// (SSOT §11.3). Destroy is idempotent, so running it against a cgroup the
	// kernel has already released is not an error (SSOT §13.3).
	cleanup.push("removing the container cgroup", func() error {
		return r.cgroups.Destroy(id)
	})

	log.Debug("created container cgroup",
		"cgroup_root", r.cgroups.Root(),
		"limits", limitSummary(spec.Limits),
	)

	return id, nil
}

// tolerateMissingCgroups reports whether a failure to create the container's
// cgroup can be survived.
//
// The rule is that a limit the caller asked for is never silently dropped: a
// container that was supposed to be capped at 128MB must not start uncapped,
// so a missing hierarchy is fatal. A container that asked for nothing loses
// only accounting, and refusing to start it would regress Stages 1 and 2 on
// every host still running cgroup v1 — which SSOT §13.5 forbids in the other
// direction and which is no more welcome in this one.
func (r *Runner) tolerateMissingCgroups(log *slog.Logger, spec Spec, err error) bool {
	if !spec.Limits.IsZero() || !errors.Is(err, cgroup.ErrUnifiedHierarchyNotMounted) {
		return false
	}

	log.Warn("no cgroup v2 hierarchy; the container runs unaccounted and unlimited",
		"cgroup_root", r.cgroups.Root(),
		"error", err,
	)

	return true
}

// limitSummary renders the limits for a log line. Only what was actually set
// appears, so the line says what Forge did rather than what it could have done.
func limitSummary(limits cgroup.Limits) string {
	if limits.IsZero() {
		return "none"
	}

	summary := ""
	for i, file := range limits.Files() {
		if i > 0 {
			summary += " "
		}
		summary += fmt.Sprintf("%s=%s", file.Name, file.Value)
	}

	return summary
}
