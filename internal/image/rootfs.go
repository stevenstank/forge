package image

import (
	"context"
	"fmt"
	"os"
)

// BuildRootfs applies an image's layers to dest, base layer first (FR-5.3).
//
// This is the whole of ADR-0003's decision in one function: layers are merged
// at unpack time into an ordinary directory tree, rather than at mount time by
// the kernel. When a later stage adds OverlayFS it replaces this call and
// nothing else — not the reference parser, not the client, not the cache.
//
// Order is not negotiable. A layer expresses changes relative to everything
// below it, so applying them out of order does not produce a differently-merged
// tree, it produces a wrong one: a file deleted by layer 3 and recreated by
// layer 4 exists, and the same two layers applied in the other order do not
// leave it there.
//
// # Ownership
//
// dest must already exist and is not this package's to create or destroy —
// internal/rootfs owns the directory, internal/image owns its contents
// (ADR-0020). What BuildRootfs does own is the invariant that a failure leaves
// no half-merged tree behind: on any error it empties dest, in reverse order of
// what it did, and returns. The directory itself survives, still owned by the
// caller, still empty enough to retry into or to remove.
//
// It does not undo a failed extraction file by file. Partial removal of a
// partial extraction is strictly more code and strictly less certain than
// clearing the tree, and the caller's own cleanup stack removes the directory
// outright soon afterwards anyway.
func BuildRootfs(ctx context.Context, cache *Cache, layers []Descriptor, dest string) (Stats, error) {
	var total Stats

	if cache == nil {
		return total, fmt.Errorf("building a root filesystem in %q: a cache is required", dest)
	}
	if len(layers) == 0 {
		return total, fmt.Errorf("building a root filesystem in %q: the image has no layers", dest)
	}

	info, err := os.Stat(dest)
	switch {
	case err != nil:
		return total, fmt.Errorf("inspecting the destination %q: %w", dest, err)
	case !info.IsDir():
		return total, fmt.Errorf("the destination %q is not a directory", dest)
	}

	// The rollback below empties dest, and dest belongs to the caller. Refusing
	// a destination that already has something in it is what makes that safe:
	// without this check, a layer failing part-way through would delete data
	// this function never wrote. internal/rootfs hands over a directory it has
	// just created, so nothing in Forge is constrained by this.
	entries, err := os.ReadDir(dest)
	if err != nil {
		return total, fmt.Errorf("reading the destination %q: %w", dest, err)
	}
	if len(entries) > 0 {
		return total, fmt.Errorf("%w: %q already contains %d entries",
			ErrDestinationNotEmpty, dest, len(entries))
	}

	// The rollback is registered before the first byte is written, never after.
	// That single placement is what makes a partial extraction safe: there is
	// no window in which files exist and nothing is responsible for them.
	rollback := newCleanupStack(cache.logger)
	defer rollback.unwind()

	rollback.push("emptying a partially built root filesystem", func() error {
		return clearDirectory(dest)
	})

	for i, layer := range layers {
		if err := ctx.Err(); err != nil {
			return total, err
		}

		stats, err := cache.UnpackLayer(ctx, layer.Digest, dest)
		total.add(stats)
		if err != nil {
			return total, fmt.Errorf("applying layer %d of %d to %q: %w", i+1, len(layers), dest, err)
		}
	}

	// Every layer landed, so there is nothing to roll back. Cancelling rather
	// than skipping the deferred unwind keeps the success and failure paths on
	// the same code, which is what makes the failure path get exercised.
	rollback.cancel()

	cache.logger.Debug("built a root filesystem from an image",
		"dest", dest, "layers", len(layers),
		"files", total.Files, "dirs", total.Dirs, "bytes", total.Bytes)

	return total, nil
}
