package image_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stevenstank/forge/internal/image"
)

// resolve is the first half of every pull, and every test below needs it.
func resolve(t *testing.T, registry *fakeRegistry, client *image.Client, tag string) (image.Reference, image.Manifest) {
	t.Helper()

	ref := registry.Reference(t, "test/img:"+tag)

	manifest, err := client.Resolve(t.Context(), ref, testPlatform)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}

	return ref, manifest
}

func TestFetchBlob(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	body := []byte("the contents of a layer")
	descriptor := registry.AddBlob(body, ociLayerType)

	var out bytes.Buffer
	if err := registry.Client(t).FetchBlob(t.Context(), registry.Reference(t, "test/img:v1"), descriptor, &out); err != nil {
		t.Fatalf("FetchBlob() = %v", err)
	}

	if out.String() != string(body) {
		t.Errorf("FetchBlob() wrote %q, want %q", out.String(), body)
	}
}

// A truncated response is the commonest half-failure a registry produces, and
// the message has to say how many bytes arrived — "the hash was wrong" is not
// something an operator can act on.
func TestFetchBlobDetectsATruncatedResponse(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	body := bytes.Repeat([]byte("layer"), 100)
	descriptor := registry.AddBlob(body, ociLayerType)
	registry.Truncate(descriptor.Digest, 120)

	var out bytes.Buffer
	err := registry.Client(t).FetchBlob(t.Context(), registry.Reference(t, "test/img:v1"), descriptor, &out)
	if !errors.Is(err, image.ErrDigestMismatch) {
		t.Fatalf("FetchBlob() = %v, want %v", err, image.ErrDigestMismatch)
	}
	if !strings.Contains(err.Error(), "120") || !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not say how much of the blob arrived", err)
	}
}

// Content swapped for other content of the same length passes the length check
// and must still be caught.
func TestFetchBlobDetectsSwappedContent(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	descriptor := registry.AddBlob([]byte("the real layer"), ociLayerType)
	registry.Corrupt(descriptor.Digest)

	var out bytes.Buffer
	err := registry.Client(t).FetchBlob(t.Context(), registry.Reference(t, "test/img:v1"), descriptor, &out)
	if !errors.Is(err, image.ErrDigestMismatch) {
		t.Fatalf("FetchBlob() = %v, want %v", err, image.ErrDigestMismatch)
	}
}

func TestFetchBlobReportsAMissingBlob(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	descriptor := image.Descriptor{MediaType: ociLayerType, Digest: "sha256:" + exampleHex, Size: 10}

	err := registry.Client(t).FetchBlob(t.Context(), registry.Reference(t, "test/img:v1"), descriptor, io.Discard)
	if !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("FetchBlob() = %v, want %v", err, image.ErrNotFound)
	}
}

func TestPullCachesEveryBlob(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	config := buildConfig(t, nil)
	layer := buildLayer(t, file("hello", "world"))
	img := registry.AddImage(t, "v1", config, layer)

	client := registry.Client(t)
	cache := newCache(t)
	ref, manifest := resolve(t, registry, client, "v1")

	stats, err := image.Pull(t.Context(), client, cache, ref, manifest)
	if err != nil {
		t.Fatalf("Pull() = %v", err)
	}

	if stats.Fetched != 2 || stats.Cached != 0 {
		t.Errorf("stats = %+v, want 2 fetched and 0 cached (a config and a layer)", stats)
	}
	if want := int64(len(config) + len(layer)); stats.Bytes != want {
		t.Errorf("Bytes = %d, want %d", stats.Bytes, want)
	}

	for _, digest := range append([]string{img.ConfigDigest}, img.LayerDigests...) {
		if err := cache.Verify(t.Context(), digest); err != nil {
			t.Errorf("Verify(%s) = %v", digest, err)
		}
	}
	assertStagingEmpty(t, cache)
}

// FR-5.4: a second pull of the same image transfers nothing. The manifest is
// still requested, deliberately — a tag is mutable, and a stale one would be a
// bug the user cannot see.
func TestDuplicatePullTransfersNothing(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("hello", "world")))

	client := registry.Client(t)
	cache := newCache(t)

	ref, manifest := resolve(t, registry, client, "v1")
	if _, err := image.Pull(t.Context(), client, cache, ref, manifest); err != nil {
		t.Fatalf("first Pull() = %v", err)
	}

	blobsAfterFirst := registry.BlobRequests.Load()
	if blobsAfterFirst == 0 {
		t.Fatal("the first pull requested no blobs")
	}

	ref, manifest = resolve(t, registry, client, "v1")
	stats, err := image.Pull(t.Context(), client, cache, ref, manifest)
	if err != nil {
		t.Fatalf("second Pull() = %v", err)
	}

	if stats.Fetched != 0 {
		t.Errorf("Fetched = %d on a fully cached image, want 0", stats.Fetched)
	}
	if stats.Cached != 2 {
		t.Errorf("Cached = %d, want 2", stats.Cached)
	}
	if stats.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0", stats.Bytes)
	}
	if got := registry.BlobRequests.Load(); got != blobsAfterFirst {
		t.Errorf("BlobRequests = %d, want it unchanged at %d", got, blobsAfterFirst)
	}
	if got := registry.ManifestRequests.Load(); got != 2 {
		t.Errorf("ManifestRequests = %d, want 2 — a tag is resolved on every run", got)
	}
}

// An interrupted download must leave no residue: no partial blob, and no
// staging file. Cancelling mid-transfer is the deterministic version of a
// connection reset.
func TestInterruptedDownloadLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	config := buildConfig(t, nil)
	layer := buildLayer(t, file("hello", "world"))
	img := registry.AddImage(t, "v1", config, layer)

	client := registry.Client(t)
	cache := newCache(t)

	release := registry.Block(img.LayerDigests[0])
	defer release()

	ctx, cancel := context.WithCancel(t.Context())
	ref, manifest := resolve(t, registry, client, "v1")

	// The config downloads, then the layer hangs; cancelling then is a pull
	// interrupted partway through.
	done := make(chan error, 1)
	go func() { _, err := image.Pull(ctx, client, cache, ref, manifest); done <- err }()

	// Wait until the blocked layer request has actually been received, so the
	// cancellation lands mid-download rather than before it starts.
	waitFor(t, func() bool { return registry.BlobRequests.Load() >= 2 })
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Pull() = %v, want %v", err, context.Canceled)
	}

	has, err := cache.Has(img.LayerDigests[0])
	if err != nil {
		t.Fatalf("Has() = %v", err)
	}
	if has {
		t.Error("the interrupted layer is in the cache")
	}
	assertStagingEmpty(t, cache)

	// The blobs that did complete stay cached, which is what makes a retry
	// cheap.
	if has, err := cache.Has(img.ConfigDigest); err != nil || !has {
		t.Errorf("Has(config) = (%v, %v), want the completed blob kept", has, err)
	}

	// And the retry works.
	release()
	if _, err := image.Pull(t.Context(), client, cache, ref, manifest); err != nil {
		t.Fatalf("Pull() after an interruption = %v", err)
	}
	if err := cache.Verify(t.Context(), img.LayerDigests[0]); err != nil {
		t.Errorf("Verify() after a retry = %v", err)
	}
}

// A registry that serves a layer whose bytes do not match its digest must not
// get those bytes into the cache.
func TestPullRefusesToCacheABlobThatDoesNotVerify(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	img := registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("hello", "world")))
	registry.Truncate(img.LayerDigests[0], 4)

	client := registry.Client(t)
	cache := newCache(t)
	ref, manifest := resolve(t, registry, client, "v1")

	_, err := image.Pull(t.Context(), client, cache, ref, manifest)
	if !errors.Is(err, image.ErrDigestMismatch) {
		t.Fatalf("Pull() = %v, want %v", err, image.ErrDigestMismatch)
	}

	has, hasErr := cache.Has(img.LayerDigests[0])
	if hasErr != nil {
		t.Fatalf("Has() = %v", hasErr)
	}
	if has {
		t.Error("a blob that failed verification reached the cache")
	}
	assertStagingEmpty(t, cache)
}

// Two forge runs pulling the same uncached image at once. Both must succeed,
// every blob must verify, and nothing may be left in staging — this is the
// property that link(2)'s EEXIST-is-success rule buys (ADR-0021).
func TestConcurrentPullsOfTheSameImage(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	img := registry.AddImage(t, "v1", buildConfig(t, nil),
		buildLayer(t, file("one", "1")),
		buildLayer(t, file("two", "2")),
		buildLayer(t, file("three", "3")),
	)

	// One cache shared by every runner, which is the point: it stands in for
	// /var/lib/forge/images shared by concurrent processes.
	cache := newCache(t)

	const runners = 8
	var wg sync.WaitGroup
	errs := make([]error, runners)

	for i := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()

			client := registry.Client(t)
			ref := registry.Reference(t, "test/img:v1")

			manifest, err := client.Resolve(t.Context(), ref, testPlatform)
			if err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = image.Pull(t.Context(), client, cache, ref, manifest)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("runner %d: %v", i, err)
		}
	}

	for _, digest := range append([]string{img.ConfigDigest}, img.LayerDigests...) {
		if err := cache.Verify(t.Context(), digest); err != nil {
			t.Errorf("Verify(%s) = %v", digest, err)
		}
	}
	assertStagingEmpty(t, cache)
}

func TestPullRequiresAClientAndACache(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("a", "a")))

	client := registry.Client(t)
	ref, manifest := resolve(t, registry, client, "v1")

	if _, err := image.Pull(t.Context(), nil, newCache(t), ref, manifest); err == nil {
		t.Error("Pull() with no client = nil, want an error")
	}
	if _, err := image.Pull(t.Context(), client, nil, ref, manifest); err == nil {
		t.Error("Pull() with no cache = nil, want an error")
	}
}

// A .part file in staging/ is not evidence of a leak. It is what a download in
// progress looks like, and the cache is deliberately shared between concurrent
// pulls — so "staging/ is empty" is an invariant of a *quiescent* cache only.
//
// This is the failure mode that made TestCleanupAfterContainerExit in the
// integration suite report `staging file 350529619.part left behind`: it
// asserted over the run-wide cache while other parallel tests were still
// pulling into it. Nothing had leaked. The assertion could not tell an
// abandoned download from a live one, because from the outside they are the
// same file.
//
// Pinning that distinction here is what stops the next reader from "fixing" the
// staging lifecycle to make such an assertion pass — the lifecycle is correct,
// and TestConcurrentPullsOfTheSameImage and
// TestInterruptedDownloadLeavesNothingBehind already prove it.
func TestStagingHoldsALiveDownloadUntilItCommits(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	img := registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("hello", "world")))

	client := registry.Client(t)
	cache := newCache(t)
	ref, manifest := resolve(t, registry, client, "v1")

	release := registry.Block(img.LayerDigests[0])
	defer release()

	done := make(chan error, 1)
	go func() { _, err := image.Pull(t.Context(), client, cache, ref, manifest); done <- err }()

	// The config lands, then the layer hangs with its staging file open.
	waitFor(t, func() bool { return registry.BlobRequests.Load() >= 2 })
	waitFor(t, func() bool { return len(stagingFiles(t, cache)) == 1 })

	// The file an outside observer would call a leak, mid-download.
	if got := stagingFiles(t, cache); len(got) != 1 || !strings.HasSuffix(got[0], ".part") {
		t.Fatalf("staging during a download = %v, want exactly one .part file", got)
	}

	release()
	if err := <-done; err != nil {
		t.Fatalf("Pull() = %v", err)
	}

	// And once the pull is quiescent, the same directory is empty. That is the
	// only moment the invariant holds, and the only moment it may be asserted.
	assertStagingEmpty(t, cache)
}

// stagingFiles lists the cache's in-progress downloads.
func stagingFiles(t *testing.T, cache *image.Cache) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(cache.Root(), "staging"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the staging directory: %v", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
