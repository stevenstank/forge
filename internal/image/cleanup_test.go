package image_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/image"
)

// NFR-5: every teardown path is idempotent, so a caller can run it
// unconditionally and run it twice.

func TestDiscardIsIdempotent(t *testing.T) {
	t.Parallel()

	cache := newCache(t)

	staging, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	if _, err := staging.Write([]byte("abandoned")); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	name := staging.Name()

	for i := range 3 {
		if err := staging.Discard(); err != nil {
			t.Fatalf("Discard() call %d = %v", i+1, err)
		}
	}

	assertGone(t, name)
	assertStagingEmpty(t, cache)
}

// Discard after a successful Commit must do nothing: the deferred call in the
// download loop runs on that path too, and the blob it just published must
// survive it.
func TestDiscardAfterCommitKeepsTheBlob(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	body := []byte("committed bytes")
	digest := digestOf(body)

	staging, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	if _, err := staging.Write(body); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := staging.Commit(digest); err != nil {
		t.Fatalf("Commit() = %v", err)
	}

	if err := staging.Discard(); err != nil {
		t.Fatalf("Discard() after Commit = %v", err)
	}

	if err := cache.Verify(t.Context(), digest); err != nil {
		t.Errorf("Verify() after a post-commit discard = %v", err)
	}
	assertStagingEmpty(t, cache)
}

func TestRemoveIsIdempotent(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	digest := store(t, cache, []byte("a blob to quarantine"))

	for i := range 3 {
		if err := cache.Remove(digest); err != nil {
			t.Fatalf("Remove() call %d = %v", i+1, err)
		}
	}

	has, err := cache.Has(digest)
	if err != nil {
		t.Fatalf("Has() = %v", err)
	}
	if has {
		t.Error("the blob survived Remove")
	}

	// Removing from a cache that was never written to is also success.
	empty, err := image.NewCache(filepath.Join(t.TempDir(), "images"), discardLogger())
	if err != nil {
		t.Fatalf("NewCache() = %v", err)
	}
	if err := empty.Remove(digest); err != nil {
		t.Errorf("Remove() on an untouched cache = %v", err)
	}
}

func TestRemoveRejectsAnUnusableDigest(t *testing.T) {
	t.Parallel()

	if err := newCache(t).Remove("sha256:../../etc/passwd"); err == nil {
		t.Fatal("Remove() = nil, want a rejected digest")
	}
}

func TestPruneStagingIsIdempotent(t *testing.T) {
	t.Parallel()

	cache := newCache(t)

	// A cache whose staging directory does not exist yet.
	if err := cache.PruneStaging(t.Context(), time.Hour); err != nil {
		t.Fatalf("PruneStaging() on an untouched cache = %v", err)
	}

	staging, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(staging.Name(), stale, stale); err != nil {
		t.Fatalf("ageing the staging file: %v", err)
	}

	for i := range 3 {
		if err := cache.PruneStaging(t.Context(), time.Minute); err != nil {
			t.Fatalf("PruneStaging() call %d = %v", i+1, err)
		}
	}

	assertGone(t, staging.Name())
}

// A staging file that vanishes between the listing and the removal is another
// run finishing normally, not an error.
func TestPruneStagingToleratesAConcurrentRemoval(t *testing.T) {
	t.Parallel()

	cache := newCache(t)

	staging, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	if err := staging.Discard(); err != nil {
		t.Fatalf("Discard() = %v", err)
	}

	if err := cache.PruneStaging(t.Context(), 0); err != nil {
		t.Errorf("PruneStaging() = %v", err)
	}
}

func TestPruneStagingHonoursCancellation(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	if _, err := cache.Stage(); err != nil {
		t.Fatalf("Stage() = %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := cache.PruneStaging(ctx, 0); err == nil {
		t.Error("PruneStaging() = nil on a cancelled context, want an error")
	}
}

func assertGone(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("lstat %s = %v, want the file removed", path, err)
	}
}
