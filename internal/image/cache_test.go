package image_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/image"
)

// newCache returns a cache rooted in a temporary directory.
func newCache(t *testing.T) *image.Cache {
	t.Helper()

	cache, err := image.NewCache(filepath.Join(t.TempDir(), "images"), discardLogger())
	if err != nil {
		t.Fatalf("NewCache() = %v", err)
	}

	return cache
}

// store writes b into the cache the way a download would, and returns its
// digest.
func store(t *testing.T, cache *image.Cache, b []byte) string {
	t.Helper()

	digest := digestOf(b)

	staging, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	if _, err := staging.Write(b); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := staging.Commit(digest); err != nil {
		t.Fatalf("Commit() = %v", err)
	}

	return digest
}

// New() performs no I/O (SSOT §13, and the constraint that a forge run against
// a local rootfs must not create an image cache it will never use).
func TestNewCacheTouchesNothing(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "images")

	cache, err := image.NewCache(root, discardLogger())
	if err != nil {
		t.Fatalf("NewCache() = %v", err)
	}
	if cache.Root() != root {
		t.Errorf("Root() = %q, want %q", cache.Root(), root)
	}

	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s = %v, want the cache root not to have been created", root, err)
	}
}

func TestNewCacheRejectsARelativeRoot(t *testing.T) {
	t.Parallel()

	if _, err := image.NewCache("images", discardLogger()); err == nil {
		t.Fatal("NewCache() = nil, want an error for a relative root")
	}
}

// A digest becomes a path component, so a digest that could name a file
// outside the cache must never reach the filesystem.
func TestCachePathRejectsUnusableDigests(t *testing.T) {
	t.Parallel()

	cache := newCache(t)

	tests := []struct {
		name   string
		digest string
	}{
		{name: "empty", digest: ""},
		{name: "no algorithm", digest: exampleHex},
		{name: "an unknown algorithm", digest: "md5:" + strings.Repeat("a", 32)},
		{name: "a traversal in the hex", digest: "sha256:../../../etc/passwd"},
		{name: "a separator in the hex", digest: "sha256:aa/bb"},
		{name: "too short", digest: "sha256:abcd"},
		{name: "uppercase", digest: "sha256:" + strings.ToUpper(exampleHex)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := cache.Path(tt.digest); !errors.Is(err, image.ErrInvalidDigest) {
				t.Errorf("Path(%q) = %v, want %v", tt.digest, err, image.ErrInvalidDigest)
			}
			if _, err := cache.Has(tt.digest); !errors.Is(err, image.ErrInvalidDigest) {
				t.Errorf("Has(%q) = %v, want %v", tt.digest, err, image.ErrInvalidDigest)
			}
		})
	}
}

func TestCachePathIsContentAddressed(t *testing.T) {
	t.Parallel()

	cache := newCache(t)

	path, err := cache.Path("sha256:" + exampleHex)
	if err != nil {
		t.Fatalf("Path() = %v", err)
	}

	want := filepath.Join(cache.Root(), "blobs", "sha256", exampleHex)
	if path != want {
		t.Errorf("Path() = %q, want %q", path, want)
	}
}

// Has is FR-5.4's whole question, and the answer has to be no before a blob is
// stored and yes after.
func TestCacheHasMissThenHit(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	body := []byte("a layer's worth of bytes")
	digest := digestOf(body)

	has, err := cache.Has(digest)
	if err != nil {
		t.Fatalf("Has() = %v", err)
	}
	if has {
		t.Fatal("Has() = true before anything was stored")
	}

	if got := store(t, cache, body); got != digest {
		t.Fatalf("stored %s, want %s", got, digest)
	}

	has, err = cache.Has(digest)
	if err != nil {
		t.Fatalf("Has() = %v", err)
	}
	if !has {
		t.Error("Has() = false after a commit")
	}

	got, err := cache.ReadAll(digest)
	if err != nil {
		t.Fatalf("ReadAll() = %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("ReadAll() = %q, want %q", got, body)
	}
}

// Bytes must never be visible under blobs/ until they are complete and
// verified: presence is the cache-hit test, so a partial file with a blob's
// name would be silently served as a whole one.
func TestStagedBytesAreInvisibleUntilCommit(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	body := []byte("half-written")
	digest := digestOf(body)

	staging, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}

	if _, err := staging.Write(body[:4]); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	has, err := cache.Has(digest)
	if err != nil {
		t.Fatalf("Has() = %v", err)
	}
	if has {
		t.Error("Has() = true while the download was still in progress")
	}
	if !strings.HasPrefix(staging.Name(), filepath.Join(cache.Root(), "staging")) {
		t.Errorf("staging file %q is not under staging/", staging.Name())
	}

	if _, err := staging.Write(body[4:]); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := staging.Commit(digest); err != nil {
		t.Fatalf("Commit() = %v", err)
	}

	if has, err = cache.Has(digest); err != nil || !has {
		t.Errorf("Has() = (%v, %v), want true", has, err)
	}
}

// Committing under the wrong digest is the disk-side half of FR-5.2. It must
// fail, and it must leave the cache empty rather than storing bytes under a
// name they do not hash to.
func TestCommitRefusesTheWrongDigest(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	claimed := "sha256:" + exampleHex

	staging, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	if _, err := staging.Write([]byte("not what the digest says")); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	err = staging.Commit(claimed)
	if !errors.Is(err, image.ErrDigestMismatch) {
		t.Fatalf("Commit() = %v, want %v", err, image.ErrDigestMismatch)
	}

	has, hasErr := cache.Has(claimed)
	if hasErr != nil {
		t.Fatalf("Has() = %v", hasErr)
	}
	if has {
		t.Error("a blob that failed verification is in the cache")
	}

	// The staging file is still the caller's to discard, and doing so must
	// leave no residue.
	if err := staging.Discard(); err != nil {
		t.Fatalf("Discard() = %v", err)
	}
	assertStagingEmpty(t, cache)
}

// The commit is a link(2), which never replaces. A blob that is already there
// belongs to another process that verified the same digest, so the loser
// proceeds with the winner's copy — this is what makes concurrent pulls safe
// without a lock (ADR-0021).
func TestCommitAcceptsABlobThatAlreadyExists(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	body := []byte("contended bytes")
	digest := store(t, cache, body)

	path, err := cache.Path(digest)
	if err != nil {
		t.Fatalf("Path() = %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s = %v", path, err)
	}

	staging, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	if _, err := staging.Write(body); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := staging.Commit(digest); err != nil {
		t.Fatalf("Commit() over an existing blob = %v, want success", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s = %v", path, err)
	}
	if !os.SameFile(before, after) {
		t.Error("the existing blob was replaced; link(2) must never replace")
	}
	assertStagingEmpty(t, cache)
}

func TestStagingRefusesUseAfterCommit(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	body := []byte("done")

	staging, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	if _, err := staging.Write(body); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if err := staging.Commit(digestOf(body)); err != nil {
		t.Fatalf("Commit() = %v", err)
	}

	if _, err := staging.Write([]byte("more")); !errors.Is(err, image.ErrStagingCommitted) {
		t.Errorf("Write() after Commit = %v, want %v", err, image.ErrStagingCommitted)
	}
	if err := staging.Commit(digestOf(body)); !errors.Is(err, image.ErrStagingCommitted) {
		t.Errorf("Commit() twice = %v, want %v", err, image.ErrStagingCommitted)
	}
	if got := staging.Written(); got != int64(len(body)) {
		t.Errorf("Written() = %d, want %d after a commit", got, len(body))
	}
}

// Verify is the answer to "is this cache still good", and it has to notice bit
// rot that Has cannot.
func TestVerifyDetectsCorruption(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	digest := store(t, cache, []byte("original contents"))

	if err := cache.Verify(t.Context(), digest); err != nil {
		t.Fatalf("Verify() on a good blob = %v", err)
	}

	path, err := cache.Path(digest)
	if err != nil {
		t.Fatalf("Path() = %v", err)
	}
	if err := os.WriteFile(path, []byte("corrupted content!"), 0o600); err != nil {
		t.Fatalf("corrupting the blob: %v", err)
	}

	if err := cache.Verify(t.Context(), digest); !errors.Is(err, image.ErrDigestMismatch) {
		t.Errorf("Verify() on a corrupt blob = %v, want %v", err, image.ErrDigestMismatch)
	}
}

func TestOpenAndVerifyReportAMissingBlob(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	digest := "sha256:" + exampleHex

	if _, err := cache.Open(digest); !errors.Is(err, image.ErrBlobNotFound) {
		t.Errorf("Open() = %v, want %v", err, image.ErrBlobNotFound)
	}
	if err := cache.Verify(t.Context(), digest); !errors.Is(err, image.ErrBlobNotFound) {
		t.Errorf("Verify() = %v, want %v", err, image.ErrBlobNotFound)
	}
	if _, err := cache.ReadAll(digest); !errors.Is(err, image.ErrBlobNotFound) {
		t.Errorf("ReadAll() = %v, want %v", err, image.ErrBlobNotFound)
	}
}

// Many goroutines committing the same blob at once is the in-process shape of
// two forge runs racing. Exactly one inode must win and every caller must see
// success.
func TestConcurrentCommitsOfOneBlob(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	body := []byte("a blob two runs both want")
	digest := digestOf(body)

	const runners = 16
	var wg sync.WaitGroup
	errs := make([]error, runners)

	for i := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()

			staging, err := cache.Stage()
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = staging.Discard() }()

			if _, err := staging.Write(body); err != nil {
				errs[i] = err
				return
			}
			errs[i] = staging.Commit(digest)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("runner %d: %v", i, err)
		}
	}

	if err := cache.Verify(t.Context(), digest); err != nil {
		t.Errorf("Verify() after a contended commit = %v", err)
	}
	assertStagingEmpty(t, cache)
}

// PruneStaging collects what a SIGKILLed forge left behind, and must not touch
// a download that could still be live.
func TestPruneStagingRespectsTheAgeBound(t *testing.T) {
	t.Parallel()

	cache := newCache(t)

	old, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	if _, err := old.Write([]byte("abandoned")); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	recent, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}

	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old.Name(), stale, stale); err != nil {
		t.Fatalf("ageing the staging file: %v", err)
	}

	if err := cache.PruneStaging(t.Context(), 24*time.Hour); err != nil {
		t.Fatalf("PruneStaging() = %v", err)
	}

	if _, err := os.Stat(old.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s = %v, want the abandoned download removed", old.Name(), err)
	}
	if _, err := os.Stat(recent.Name()); err != nil {
		t.Errorf("stat %s = %v, want a live download left alone", recent.Name(), err)
	}
}

// assertStagingEmpty fails if any download is left behind. Every test that
// touches the cache ends with this, because a leaked staging file is the one
// residue this design is meant to make impossible.
func assertStagingEmpty(t *testing.T, cache *image.Cache) {
	t.Helper()

	dir := filepath.Join(cache.Root(), "staging")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("reading %s = %v", dir, err)
	}

	for _, entry := range entries {
		t.Errorf("staging file left behind: %s", entry.Name())
	}
}

// D1. Commit closes the staging file and marks the Staging spent *before*
// link(2) has succeeded. A publication that fails after that point — a full
// disk, a permission problem, a blobs/ directory that cannot be created —
// leaves the caller's deferred Discard with nothing to do, and the .part file
// survives until PruneStaging collects it a day later.
//
// The failure is provoked here by putting a regular file where blobs/sha256
// must be a directory, which is the cheapest deterministic way to make
// publication fail after the digest has already verified.
func TestCommitLeavesNoStagingFileWhenPublishingFails(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	body := []byte("bytes that verify perfectly well")
	digest := digestOf(body)

	staging, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	if _, err := staging.Write(body); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	blobs := filepath.Join(cache.Root(), "blobs")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatalf("mkdir %s = %v", blobs, err)
	}
	if err := os.WriteFile(filepath.Join(blobs, "sha256"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("blocking the blob directory: %v", err)
	}

	if err := staging.Commit(digest); err == nil {
		t.Fatal("Commit() = nil, want publication to fail")
	}

	// This is the rollback every download defers. After it, nothing this
	// attempt created may remain.
	if err := staging.Discard(); err != nil {
		t.Fatalf("Discard() after a failed Commit = %v", err)
	}

	assertStagingEmpty(t, cache)
}

// The same guarantee stated as the invariant it protects: a Discard is
// idempotent whatever happened before it, including a Commit that got
// half-way.
func TestDiscardIsIdempotentAfterAFailedCommit(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	body := []byte("more bytes that verify")

	staging, err := cache.Stage()
	if err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	if _, err := staging.Write(body); err != nil {
		t.Fatalf("Write() = %v", err)
	}

	blobs := filepath.Join(cache.Root(), "blobs")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatalf("mkdir %s = %v", blobs, err)
	}
	if err := os.WriteFile(filepath.Join(blobs, "sha256"), []byte("x"), 0o600); err != nil {
		t.Fatalf("blocking the blob directory: %v", err)
	}

	if err := staging.Commit(digestOf(body)); err == nil {
		t.Fatal("Commit() = nil, want publication to fail")
	}

	for i := range 3 {
		if err := staging.Discard(); err != nil {
			t.Fatalf("Discard() call %d = %v", i+1, err)
		}
	}
	assertStagingEmpty(t, cache)
}

// A successful Commit must survive a Discard: the deferred rollback in the
// download loop runs on that path too, and it must not unlink the blob it just
// published.
func TestDiscardAfterASuccessfulCommitKeepsTheBlobAndClearsStaging(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	body := []byte("published bytes")
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
		t.Fatalf("Discard() = %v", err)
	}

	if err := cache.Verify(t.Context(), digest); err != nil {
		t.Errorf("Verify() after a post-commit discard = %v", err)
	}
	assertStagingEmpty(t, cache)
}
