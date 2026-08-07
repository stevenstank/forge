package image

import (
	"context"
	"fmt"
	"io"
)

// FetchBlob streams one blob into w, verifying it as the bytes pass (FR-5.2).
//
// Verification is in flight rather than after the fact because that is the only
// moment the bytes exist as a stream: a layer is too large to buffer, and
// hashing a file after writing it would mean reading it twice. The cost is that
// a mismatch is discovered only once w has already been written to — so callers
// write to something they are prepared to throw away. Staging.Write is exactly
// that, and Pull is the caller that uses it.
//
// Both the digest and the length are checked. A hash mismatch alone would catch
// a truncated response, but "the registry sent 3 MB of the 5 MB it promised" is
// the sentence an operator can act on, and "the hash was wrong" is not.
func (c *Client) FetchBlob(ctx context.Context, ref Reference, d Descriptor, w io.Writer) error {
	hasher, err := newHasher(d.Digest)
	if err != nil {
		return err
	}

	rawURL := c.endpoint(ref, "blobs", d.Digest)

	// A blob request is commonly answered with a redirect to object storage.
	// http.Client follows it, and strips the Authorization header when the
	// redirect crosses to another host — which is what keeps a registry token
	// from being handed to a CDN.
	resp, err := c.get(ctx, ref, rawURL, nil)
	if err != nil {
		return err
	}
	defer drain(resp, c.logger)

	written, err := io.Copy(io.MultiWriter(w, hasher), resp.Body)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: downloading %s after %d bytes: %w",
			ErrRegistryUnavailable, d.Digest, written, err)
	}

	if d.Size > 0 && written != d.Size {
		return fmt.Errorf("%w: %s sent %d bytes for %s, but its descriptor says %d",
			ErrDigestMismatch, ref.Host(), written, d.Digest, d.Size)
	}

	if computed := formatDigest(hasher); computed != d.Digest {
		return fmt.Errorf("%w: %d bytes from %s hash to %s, but were requested as %s",
			ErrDigestMismatch, written, ref.Host(), computed, d.Digest)
	}

	c.logger.Debug("downloaded a blob", "digest", d.Digest, "bytes", written)

	return nil
}

// PullStats reports what a Pull actually did, which is how a caller — and the
// cache-reuse test — can tell a cache hit from a download.
type PullStats struct {
	// Fetched is the number of blobs downloaded.
	Fetched int

	// Cached is the number of blobs already present, and therefore not
	// downloaded. This is FR-5.4's whole observable effect.
	Cached int

	// Bytes is the total downloaded, excluding cache hits.
	Bytes int64
}

// Pull downloads every blob of a manifest that the cache does not already have
// (FR-5.1, FR-5.2, FR-5.4).
//
// It touches nothing but the network and the shared cache. No container
// directory exists yet and none is created, so every way this can fail — a
// registry that is down, a layer that does not verify, a full disk — leaves the
// host unchanged apart from a possibly larger cache.
//
// Blobs are fetched sequentially. Parallelism would help a large image and
// would turn this function into a scheduler; it is the first optimization to
// make once the stage is correct.
func Pull(ctx context.Context, client *Client, cache *Cache, ref Reference, m Manifest) (PullStats, error) {
	if client == nil || cache == nil {
		return PullStats{}, fmt.Errorf("pulling %s: a client and a cache are both required", ref)
	}

	// Stale staging files from a previous run that was killed mid-download are
	// collected here, once, rather than by a background task nothing starts.
	if err := cache.PruneStaging(ctx, defaultStagingMaxAge); err != nil {
		// A cache that cannot be tidied can still be pulled into, so this is
		// reported and not fatal (SSOT §13.7).
		cache.logger.Warn("pruning stale downloads before a pull", "error", err)
	}

	var stats PullStats

	// The config is fetched alongside the layers because it is a blob like any
	// other; treating it specially would mean a second code path to verify one
	// invariant.
	for _, d := range append([]Descriptor{m.Config}, m.Layers...) {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		has, err := cache.Has(d.Digest)
		if err != nil {
			return stats, err
		}
		if has {
			stats.Cached++
			continue
		}

		written, err := fetchInto(ctx, client, cache, ref, d)
		if err != nil {
			return stats, err
		}

		stats.Fetched++
		stats.Bytes += written
	}

	cache.logger.Debug("pulled an image",
		"reference", ref.String(), "manifest", m.Digest,
		"fetched", stats.Fetched, "cached", stats.Cached, "bytes", stats.Bytes)

	return stats, nil
}

// fetchInto downloads one blob into the cache.
//
// It exists as its own function so the staging file's deferred Discard is
// scoped to a single blob rather than to the whole pull. In the loop above,
// deferring would keep every staging file open until the pull finished and
// would leak them all on a failure partway through.
func fetchInto(ctx context.Context, client *Client, cache *Cache, ref Reference, d Descriptor) (int64, error) {
	staging, err := cache.Stage()
	if err != nil {
		return 0, err
	}
	// Idempotent, and a no-op after a successful Commit: the download's only
	// rollback, running on every path out of this function.
	defer func() {
		if err := staging.Discard(); err != nil {
			cache.logger.Warn("discarding a staging file", "path", staging.Name(), "error", err)
		}
	}()

	if err := client.FetchBlob(ctx, ref, d, staging); err != nil {
		return 0, err
	}

	if err := staging.Commit(d.Digest); err != nil {
		return 0, err
	}

	return staging.Written(), nil
}
