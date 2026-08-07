package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/rootfs"
	"github.com/stevenstank/forge/internal/state"
)

// logsDirName is the directory a container's captured output lives in, a
// sibling of the state and container trees under the state directory.
const logsDirName = "logs"

// RemoveOptions configures a removal.
type RemoveOptions struct {
	// Force stops the container first, rather than refusing to remove one
	// that is still running.
	Force bool
}

// Remove deletes a stopped container and everything Forge still holds for it
// (FR-6.6).
//
// It refuses a running container unless Force is set. That refusal is the
// point of the verb: removing a container's root filesystem out from under a
// process that is executing inside it does not stop the container, it corrupts
// it, and `rm` is the command a user reaches for when they think a container is
// already finished.
//
// # Order
//
// Removal unwinds the resources a container holds in reverse of the order they
// were acquired, which for a stopped container is the tail of the same stack
// its supervisor already unwound the head of:
//
//	acquired:   record → logs → filesystem → cgroup → network → process
//	stop:                                             ◀────────────────
//	rm:         ◀───────────────────────────
//
// The network and cgroup are released again here anyway. They should already
// be gone — Stop released them, and so did the supervisor as it exited — but
// "should" is doing too much work for a container whose supervisor may have
// been killed, and every Destroy is idempotent precisely so that this belt can
// be worn with the braces (SSOT §13.3).
//
// The record goes last, always. It is the list of what to remove, so deleting
// it first would strand everything it names with nothing left to attribute it
// to. Marking it `removing` before starting is what makes an interrupted
// removal finishable: a later `forge rm` picks it up and completes it rather
// than finding a record that claims to be a healthy stopped container while
// half its resources are gone.
func (r *Runner) Remove(ctx context.Context, id string, opts RemoveOptions) error {
	log := r.logger.With("container_id", id)

	m, err := r.state.Load(id)
	if err != nil {
		return translateStateError(id, err)
	}

	if m.Running() {
		if !opts.Force {
			return fmt.Errorf("%w: %s is %s; stop it first or use -f", ErrRunning, id, m.Status)
		}

		log.Debug("stopping the container before removing it", "status", string(m.Status))
		if err := r.Stop(ctx, id, StopOptions{}); err != nil {
			return err
		}

		// Re-read: the stop moved the container on, and what is removed below
		// is described by the record as it is now.
		if m, err = r.state.Load(id); err != nil {
			return translateStateError(id, err)
		}
	}

	// From here the container is being taken apart. A crash after this point
	// leaves a record that says so, which is a state a later rm can finish
	// rather than one it has to interpret.
	if m.Status != state.StatusRemoving {
		r.note(log, id, "the removal", func(rec *state.Metadata) error {
			rec.Status = state.StatusRemoving
			return nil
		})
	}

	if err := r.releaseRuntimeResources(log, id); err != nil {
		return err
	}

	var errs []error

	if err := r.removeFilesystem(ctx, id); err != nil {
		errs = append(errs, err)
	}
	if err := r.removeLogs(id); err != nil {
		errs = append(errs, err)
	}

	// Every step above must be given its chance before the record goes: the
	// record is the only thing that names them, so deleting it while one is
	// still on disk would leak it permanently. A failure therefore stops the
	// removal here, with the record intact and still saying `removing`.
	if len(errs) > 0 {
		return fmt.Errorf("container %s: %w", id, errors.Join(errs...))
	}

	if err := r.state.Remove(id); err != nil {
		return translateStateError(id, err)
	}

	log.Info("container removed")

	return nil
}

// removeFilesystem deletes the container's root filesystem.
//
// It is the same two steps the run's own cleanup performs, and for the same
// reasons: mount.Cleanup first because rootfs.Store.Remove refuses to delete a
// tree with mounts under it, and it refuses because os.RemoveAll walking
// through a live bind mount would delete the host's files rather than the
// container's (ADR-0012). A container removed long after the Forge that ran it
// died is exactly the case where residual mounts are possible, so the check is
// not theoretical here.
//
// The context is deliberately not the caller's cancellation to inherit: a
// removal interrupted half-way is what leaves residue, and the whole point of
// this function is that it does not.
func (r *Runner) removeFilesystem(ctx context.Context, id string) error {
	ctx = context.WithoutCancel(ctx)

	dir, err := r.store.Lookup(id)
	if err != nil {
		// A container with no directory of its own — Stage 1's
		// host-filesystem case — or one whose directory has already been
		// removed. Neither has anything here to release.
		if errors.Is(err, rootfs.ErrNotPrepared) {
			return nil
		}
		return err
	}

	if err := mount.Cleanup(ctx, dir.Base); err != nil {
		return err
	}

	return r.store.Remove(ctx, id)
}

// removeLogs deletes the container's captured output.
func (r *Runner) removeLogs(id string) error {
	return r.logs.Remove(id)
}
