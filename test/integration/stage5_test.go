//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/image"
	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/network"
	"github.com/stevenstank/forge/internal/runtime"
)

// Stage 5 against real registries.
//
// Every manifest and every byte of every layer in this file comes from Docker
// Hub over the real Distribution protocol: the real anonymous token dance, the
// real 307 to a CDN, the real gzipped tars. Nothing is mocked, because the
// failures worth catching here are the ones a mock cannot have — a registry
// that answers a HEAD differently from a GET, an index whose platform list is
// not what the spec examples show, a layer whose tar contains something the
// extractor has never seen.
//
// # The two proxies, and why they are not mocks
//
// Three of the requirements — detecting a digest mismatch in flight, proving no
// duplicate download happens, and interrupting a transfer — need something the
// registry will not do on request. Each is met by a transport or a proxy that
// sits between Forge and the *real* Docker Hub and either counts what passes or
// tampers with it. The bytes are genuine registry bytes; what is synthetic is
// only the interference, which is exactly the thing being defended against.
// Substituting a fake registry would test Forge against Forge's idea of a
// registry, which is what these tests exist to avoid.
//
// # Network
//
// A host that cannot reach Docker Hub skips rather than fails: an integration
// suite that goes red on a train is a suite people learn to ignore.
//
// # Rate limits
//
// Anonymous Docker Hub allows 100 pulls per six hours per IP. The tests that
// only need an image to exist share one cache for the whole run, so alpine is
// transferred once no matter how many tests read it. The tests that assert on
// download *behaviour* each get a private cache, because a shared one would
// make their subject a cache hit.

const (
	// registryProbe is the endpoint whose reachability decides whether this
	// file runs at all.
	registryProbe = "https://registry-1.docker.io/v2/"

	// The images under test. Both are official, tiny, and stable.
	alpineLatest = "alpine:latest"
	alpine320    = "alpine:3.20"

	// multiLayerImage is an official image built on top of alpine, so its
	// manifest has layers from more than one build step and its rootfs can only
	// be correct if they were applied in order.
	multiLayerImage = "redis:alpine"

	// probeTimeout bounds the reachability check. It is short because its only
	// job is to tell "no network" apart from "slow network".
	probeTimeout = 10 * time.Second

	// pullTimeout bounds a single image pull.
	pullTimeout = 5 * time.Minute

	// largePullTimeout bounds the multi-layer image, which is several times
	// alpine's size. The tests run in parallel and share one uplink, so the
	// budget has to cover the slowest transfer competing with every other.
	largePullTimeout = 20 * time.Minute
)

// Files the tests assert on inside a real alpine root filesystem. Each is
// chosen because it is present in every alpine release and because it is a
// different *kind* of entry.
const (
	alpineRelease  = "/etc/alpine-release" // a regular file, in a directory
	alpineBusybox  = "/bin/busybox"        // the executable everything links to
	alpineShell    = "/bin/sh"             // a symlink to busybox
	alpineHosts    = "/etc/hosts"          // a file the container's mounts do not cover
	alpineAPKTools = "/sbin/apk"           // deep enough to prove nested dirs
)

// requireRegistry skips a test that cannot reach Docker Hub.
func requireRegistry(t *testing.T) {
	t.Helper()

	registryOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryProbe, nil)
		if err != nil {
			registryErr = err
			return
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			registryErr = err
			return
		}
		defer resp.Body.Close()

		// A 401 is the correct answer to an anonymous /v2/ and is what Docker
		// Hub sends; anything in the 2xx or 4xx range means the registry is
		// there and talking.
		if resp.StatusCode >= 500 {
			registryErr = fmt.Errorf("registry returned %s", resp.Status)
		}
	})

	if registryErr != nil {
		t.Skipf("cannot reach %s (%v); set up networking or run the hermetic unit tests instead",
			registryProbe, registryErr)
	}
}

var (
	registryOnce sync.Once
	registryErr  error
)

// --- caches ----------------------------------------------------------------

// sharedImageCache is one blob cache for the whole run, used by every test that
// only needs an image to be available rather than to observe it arriving.
//
// It is removed by stage5CleanupSharedCache, which TestMain calls after the
// suite finishes. Nothing here uses t.TempDir, because the point is to outlive
// any single test.
var (
	sharedCacheOnce sync.Once
	sharedCacheDir  string
	sharedCacheErr  error
)

func sharedCache(t *testing.T) *image.Cache {
	t.Helper()

	sharedCacheOnce.Do(func() {
		sharedCacheDir, sharedCacheErr = os.MkdirTemp("", "forge-stage5-shared-cache-")
	})
	if sharedCacheErr != nil {
		t.Fatalf("creating the shared image cache: %v", sharedCacheErr)
	}

	cache, err := image.NewCache(sharedCacheDir, testLogger())
	if err != nil {
		t.Fatalf("NewCache() = %v", err)
	}
	return cache
}

// stage5CleanupSharedCache removes the run-wide blob cache. TestMain calls it
// once the suite is done, which is the only moment at which no test can still
// be reading from it.
func stage5CleanupSharedCache() {
	if sharedCacheDir != "" {
		_ = os.RemoveAll(sharedCacheDir)
	}
}

// privateCache returns a cache nothing else touches, for the tests whose
// subject is what gets downloaded.
func privateCache(t *testing.T) *image.Cache {
	t.Helper()

	cache, err := image.NewCache(filepath.Join(t.TempDir(), "images"), testLogger())
	if err != nil {
		t.Fatalf("NewCache() = %v", err)
	}
	return cache
}

func testLogger() *slog.Logger { return logging.New(io.Discard, slog.LevelError) }

// --- pulling ----------------------------------------------------------------

// newRegistryClient returns a client that talks to the real Docker Hub, with an
// optional transport wrapped around the real one.
func newRegistryClient(t *testing.T, transport http.RoundTripper) *image.Client {
	t.Helper()

	httpClient := &http.Client{Timeout: pullTimeout}
	if transport != nil {
		httpClient.Transport = transport
	}

	client, err := image.New(testLogger(), image.ClientConfig{HTTPClient: httpClient})
	if err != nil {
		t.Fatalf("image.New() = %v", err)
	}
	return client
}

// newProxyClient returns a client for a registry served over plain HTTP on
// loopback. Only the tampering proxy needs it; talking to Docker Hub itself is
// always TLS.
func newProxyClient(t *testing.T) *image.Client {
	t.Helper()

	client, err := image.New(testLogger(), image.ClientConfig{
		HTTPClient: &http.Client{Timeout: pullTimeout},
		PlainHTTP:  true,
	})
	if err != nil {
		t.Fatalf("image.New() = %v", err)
	}
	return client
}

// pulled is the result of resolving and pulling one image.
type pulled struct {
	ref      image.Reference
	manifest image.Manifest
	stats    image.PullStats
}

// pull resolves reference and downloads whatever cache is missing.
func pull(t *testing.T, cache *image.Cache, client *image.Client, reference string) pulled {
	t.Helper()

	return pullWithin(t, cache, client, reference, pullTimeout)
}

// pullWithin is pull with an explicit budget, for images that are not alpine.
func pullWithin(
	t *testing.T,
	cache *image.Cache,
	client *image.Client,
	reference string,
	budget time.Duration,
) pulled {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), budget)
	defer cancel()

	ref, err := image.ParseReference(reference)
	if err != nil {
		t.Fatalf("ParseReference(%q) = %v", reference, err)
	}

	manifest, err := client.Resolve(ctx, ref, image.HostPlatform())
	if err != nil {
		t.Fatalf("Resolve(%s) = %v", reference, err)
	}

	stats, err := image.Pull(ctx, client, cache, ref, manifest)
	if err != nil {
		t.Fatalf("Pull(%s) = %v", reference, err)
	}

	return pulled{ref: ref, manifest: manifest, stats: stats}
}

// --- 1 and 2. Pulling real images -------------------------------------------

// The headline requirement: FR-5.1 against a registry Forge did not write.
func TestPullAlpineLatest(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	cache := privateCache(t)
	got := pull(t, cache, newRegistryClient(t, nil), alpineLatest)

	assertManifestIsSane(t, got.manifest)

	if got.stats.Fetched == 0 {
		t.Error("Fetched = 0 on a cold cache; nothing was downloaded")
	}
	if got.stats.Bytes == 0 {
		t.Error("Bytes = 0 on a cold cache")
	}
	assertCacheIntact(t, cache, got.manifest)
	assertNoStaging(t, cache)
}

func TestPullAlpine320(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	cache := privateCache(t)
	got := pull(t, cache, newRegistryClient(t, nil), alpine320)

	assertManifestIsSane(t, got.manifest)
	assertCacheIntact(t, cache, got.manifest)
	assertNoStaging(t, cache)

	// A pinned tag must resolve to the same immutable content every time. This
	// is what makes a digest reference reproducible, and it is the one property
	// a moving :latest does not have.
	again := pull(t, cache, newRegistryClient(t, nil), alpine320)
	if again.manifest.Digest != got.manifest.Digest {
		t.Errorf("alpine:3.20 resolved to %s and then %s", got.manifest.Digest, again.manifest.Digest)
	}
}

// assertManifestIsSane checks the shape of anything a registry hands back.
func assertManifestIsSane(t *testing.T, manifest image.Manifest) {
	t.Helper()

	if !strings.HasPrefix(manifest.Digest, "sha256:") || len(manifest.Digest) != 71 {
		t.Errorf("manifest digest = %q, want a sha256 digest", manifest.Digest)
	}
	if manifest.Config.Digest == "" || manifest.Config.Size <= 0 {
		t.Errorf("config descriptor = %+v, want a digest and a size", manifest.Config)
	}
	if len(manifest.Layers) == 0 {
		t.Fatal("manifest has no layers")
	}
	for i, layer := range manifest.Layers {
		if layer.Digest == "" || layer.Size <= 0 {
			t.Errorf("layer %d = %+v, want a digest and a size", i, layer)
		}
	}
}

// --- 3. Running a command from an image -------------------------------------

// FR-5.5 end to end, with a real image and a real container: alpine's own
// /bin/sh, from alpine's own layers, in its own namespaces.
func TestRunCommandFromImage(t *testing.T) {
	t.Parallel()
	requireRegistry(t)
	requireRoot(t)

	const marker = "hello from inside the image"

	got := runImageContainer(t, runtime.Spec{
		Image:   alpine320,
		Command: []string{"/bin/sh", "-c", "echo " + marker},
		Network: network.ModeHost,
	})

	if got.stdout != marker {
		t.Errorf("stdout = %q, want %q", got.stdout, marker)
	}
	if got.status.Code != 0 {
		t.Errorf("exit code = %d, want 0 (stderr: %q)", got.status.Code, got.stderr)
	}
}

// A bare command name is resolved against the container's own PATH, after the
// pivot, which is the rule Stage 5 narrowed rather than deleted (§7.3).
func TestRunBareCommandNameFromImage(t *testing.T) {
	t.Parallel()
	requireRegistry(t)
	requireRoot(t)

	got := runImageContainer(t, runtime.Spec{
		Image:   alpine320,
		Command: []string{"cat", alpineRelease},
		Network: network.ModeHost,
	})

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", got.status.Code, got.stderr)
	}
	// The file holds a version string such as "3.20.3".
	if !strings.HasPrefix(got.stdout, "3.20") {
		t.Errorf("stdout = %q, want the alpine 3.20 release string", got.stdout)
	}
}

// An image with no command given runs its own entrypoint and cmd.
func TestRunUsesTheImageCommand(t *testing.T) {
	t.Parallel()
	requireRegistry(t)
	requireRoot(t)

	// alpine's config declares CMD ["/bin/sh"]. With stdin closed it reads EOF
	// and exits zero, which is enough to prove the command came from the image.
	got := runImageContainer(t, runtime.Spec{
		Image:   alpine320,
		Network: network.ModeHost,
	})

	if got.status.Code != 0 {
		t.Errorf("exit code = %d, want 0 — the image's own command should have run (stderr: %q)",
			got.status.Code, got.stderr)
	}
}

// --- 4 and 5. Digest verification -------------------------------------------

// FR-5.2 against a real registry: a tag resolves to a digest, that digest
// resolves to the same manifest, and every blob the pull cached hashes to the
// name the manifest gave it.
func TestManifestAndBlobDigestsVerify(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	// The run-wide cache is enough here: this test asserts that a tag and the
	// digest it resolves to name the same content, which is true of a warm
	// cache as much as a cold one — and it saves a transfer.
	cache := sharedCache(t)
	client := newRegistryClient(t, nil)

	byTag := pull(t, cache, client, alpine320)

	// Re-resolving by the digest the tag produced must return the identical
	// manifest. If Forge's own hashing disagreed with the registry's, these two
	// would differ.
	byDigest := pull(t, cache, client, "alpine@"+byTag.manifest.Digest)

	if byDigest.manifest.Digest != byTag.manifest.Digest {
		t.Errorf("digest reference resolved to %s, want %s", byDigest.manifest.Digest, byTag.manifest.Digest)
	}
	if !slices.EqualFunc(byTag.manifest.Layers, byDigest.manifest.Layers,
		func(a, b image.Descriptor) bool { return a.Digest == b.Digest }) {
		t.Error("the same manifest resolved to different layers by tag and by digest")
	}

	// Nothing was transferred the second time: the blobs were already cached
	// under the digests the first pull verified.
	if byDigest.stats.Fetched != 0 {
		t.Errorf("Fetched = %d resolving the same image by digest, want 0", byDigest.stats.Fetched)
	}

	assertCacheIntact(t, cache, byTag.manifest)
}

// Detecting tampering in flight (FR-5.2). The registry is the real Docker Hub;
// what is synthetic is a proxy in the middle that flips one byte of one layer,
// which is precisely the attack a digest defends against.
func TestDigestMismatchIsDetectedInFlight(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	proxy := newTamperingProxy(t)
	cache := privateCache(t)
	client := newProxyClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), pullTimeout)
	defer cancel()

	ref, err := image.ParseReference(proxy.reference("library/alpine:3.20"))
	if err != nil {
		t.Fatalf("ParseReference() = %v", err)
	}

	// The manifest is passed through untouched, so resolution succeeds against
	// the real registry's real bytes.
	manifest, err := client.Resolve(ctx, ref, image.HostPlatform())
	if err != nil {
		t.Fatalf("Resolve() through the proxy = %v", err)
	}
	assertManifestIsSane(t, manifest)

	// The layer is not.
	proxy.tamperWithBlobs()

	_, err = image.Pull(ctx, client, cache, ref, manifest)
	if !errors.Is(err, image.ErrDigestMismatch) {
		t.Fatalf("Pull() with a tampered layer = %v, want %v", err, image.ErrDigestMismatch)
	}

	// Nothing corrupt reached the cache, and nothing was left half-written.
	for _, layer := range manifest.Layers {
		has, err := cache.Has(layer.Digest)
		if err != nil {
			t.Fatalf("Has() = %v", err)
		}
		if has {
			t.Errorf("tampered layer %s was cached", layer.Digest)
		}
	}
	assertNoStaging(t, cache)
}

// A digest that names nothing is a 404, not a mismatch. The distinction matters
// because only one of them means "someone changed the bytes".
func TestUnknownDigestIsNotFound(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	ctx, cancel := context.WithTimeout(t.Context(), pullTimeout)
	defer cancel()

	ref, err := image.ParseReference("alpine@sha256:" + strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("ParseReference() = %v", err)
	}

	_, err = newRegistryClient(t, nil).Resolve(ctx, ref, image.HostPlatform())
	if !errors.Is(err, image.ErrNotFound) && !errors.Is(err, image.ErrUnauthorized) {
		// Docker Hub answers an unknown digest with either, depending on the
		// scope the token was issued for. Both are correct; neither is a
		// mismatch, which is the point.
		t.Fatalf("Resolve() of an unknown digest = %v, want not-found or unauthorized", err)
	}
}

// --- 6 and 7. Cache reuse and duplicate downloads ---------------------------

// FR-5.4. The second pull of an image must transfer nothing.
func TestCachedLayersAreReused(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	cache := privateCache(t)
	client := newRegistryClient(t, nil)

	first := pull(t, cache, client, alpine320)
	if first.stats.Fetched == 0 {
		t.Fatal("the first pull fetched nothing; the cache was not cold")
	}

	// The blobs must be reused, not rewritten: a cache that re-downloaded and
	// re-linked would look identical through PullStats alone.
	before := blobIdentities(t, cache, first.manifest)

	second := pull(t, cache, client, alpine320)

	if second.stats.Fetched != 0 {
		t.Errorf("Fetched = %d on a warm cache, want 0", second.stats.Fetched)
	}
	if second.stats.Bytes != 0 {
		t.Errorf("Bytes = %d on a warm cache, want 0", second.stats.Bytes)
	}
	if second.stats.Cached != len(first.manifest.Layers)+1 {
		t.Errorf("Cached = %d, want %d (every layer and the config)",
			second.stats.Cached, len(first.manifest.Layers)+1)
	}

	after := blobIdentities(t, cache, first.manifest)
	for digest, id := range before {
		if after[digest] != id {
			t.Errorf("blob %s was rewritten by a pull that should have transferred nothing", digest)
		}
	}
}

// The same guarantee, measured at the socket rather than inferred from stats:
// a warm pull makes no blob request at all.
func TestNoDuplicateDownloads(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	counter := &countingTransport{base: http.DefaultTransport}
	cache := privateCache(t)
	client := newRegistryClient(t, counter)

	first := pull(t, cache, client, alpine320)

	blobsAfterFirst := counter.blobRequests.Load()
	bytesAfterFirst := counter.blobBytes.Load()
	if blobsAfterFirst == 0 {
		t.Fatal("the first pull made no blob request")
	}

	pull(t, cache, client, alpine320)

	if got := counter.blobRequests.Load(); got != blobsAfterFirst {
		t.Errorf("blob requests = %d after a warm pull, want it unchanged at %d", got, blobsAfterFirst)
	}
	if got := counter.blobBytes.Load(); got != bytesAfterFirst {
		t.Errorf("blob bytes = %d after a warm pull, want it unchanged at %d", got, bytesAfterFirst)
	}

	// The manifest *is* requested again, deliberately: a tag is mutable, and a
	// stale one would be a bug the user cannot see.
	if counter.manifestRequests.Load() < 2 {
		t.Errorf("manifest requests = %d, want one per pull — a tag must be re-resolved every run",
			counter.manifestRequests.Load())
	}

	_ = first
}

// --- 8. Concurrent pulls ----------------------------------------------------

// Two forge runs pulling the same uncached image at once, against the real
// registry, through one shared cache. Every caller must succeed and every blob
// must verify; the losers of the link(2) race discard their copies (ADR-0021).
func TestConcurrentPulls(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	const runners = 6

	cache := privateCache(t)

	var wg sync.WaitGroup
	errs := make([]error, runners)
	manifests := make([]image.Manifest, runners)

	for i := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(t.Context(), pullTimeout)
			defer cancel()

			client := newRegistryClient(t, nil)

			ref, err := image.ParseReference(alpine320)
			if err != nil {
				errs[i] = err
				return
			}

			manifest, err := client.Resolve(ctx, ref, image.HostPlatform())
			if err != nil {
				errs[i] = err
				return
			}
			manifests[i] = manifest

			_, errs[i] = image.Pull(ctx, client, cache, ref, manifest)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("runner %d: %v", i, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// Every runner saw the same image, and the cache holds exactly one good
	// copy of each of its blobs.
	for i, manifest := range manifests {
		if manifest.Digest != manifests[0].Digest {
			t.Errorf("runner %d resolved %s, runner 0 resolved %s", i, manifest.Digest, manifests[0].Digest)
		}
	}
	assertCacheIntact(t, cache, manifests[0])
	assertNoStaging(t, cache)
}

// --- 9 and 10. Multi-layer images and rootfs correctness ---------------------

// A real image whose filesystem is built from more than one layer. Applying
// them out of order, or skipping one, produces a tree that fails these
// assertions rather than one that merely looks different.
func TestMultiLayerImageRootfs(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	cache := sharedCache(t)
	got := pullWithin(t, cache, newRegistryClient(t, nil), multiLayerImage, largePullTimeout)

	if len(got.manifest.Layers) < 2 {
		t.Fatalf("%s has %d layers, want a multi-layer image", multiLayerImage, len(got.manifest.Layers))
	}

	dest := buildRootfs(t, cache, got.manifest)

	// From the base layer, which is alpine.
	assertRootfsFile(t, dest, alpineRelease)
	assertRootfsFile(t, dest, alpineBusybox)

	// From a layer above it. If layer order were wrong, or an upper layer were
	// skipped, this would be missing.
	assertRootfsFile(t, dest, "/usr/local/bin/redis-server")
}

// The unpacked tree must be the tree the image describes: regular files,
// symlinks that are still symlinks, nested directories, and executables that
// kept their mode.
func TestRootfsCorrectness(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	cache := sharedCache(t)
	got := pull(t, cache, newRegistryClient(t, nil), alpine320)

	dest := buildRootfs(t, cache, got.manifest)

	for _, path := range []string{alpineRelease, alpineBusybox, alpineHosts, alpineAPKTools} {
		assertRootfsFile(t, dest, path)
	}

	// /bin/sh is a symlink to busybox in every alpine image, and it must still
	// be one: an extractor that dereferenced links would produce a working tree
	// that is quietly the wrong size and has the wrong semantics.
	shell := filepath.Join(dest, strings.TrimPrefix(alpineShell, "/"))
	info, err := os.Lstat(shell)
	if err != nil {
		t.Fatalf("lstat %s = %v", alpineShell, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is a %v, want a symlink", alpineShell, info.Mode().Type())
	}
	target, err := os.Readlink(shell)
	if err != nil {
		t.Fatalf("readlink %s = %v", alpineShell, err)
	}
	if !strings.Contains(target, "busybox") {
		t.Errorf("%s -> %q, want it to point at busybox", alpineShell, target)
	}

	// busybox must still be executable, or nothing in the container runs.
	busybox, err := os.Stat(filepath.Join(dest, strings.TrimPrefix(alpineBusybox, "/")))
	if err != nil {
		t.Fatalf("stat %s = %v", alpineBusybox, err)
	}
	if busybox.Mode()&0o111 == 0 {
		t.Errorf("%s has mode %v, want it executable", alpineBusybox, busybox.Mode())
	}

	// A tree with no nesting would pass the checks above by accident.
	if entries, err := os.ReadDir(filepath.Join(dest, "etc")); err != nil || len(entries) < 5 {
		t.Errorf("/etc has %d entries (err %v), want a populated directory", len(entries), err)
	}
}

// The same tree, asserted from inside the container rather than from the host —
// which is the only view that proves the pivot landed on the unpacked layers.
func TestRootfsCorrectnessFromInsideTheContainer(t *testing.T) {
	t.Parallel()
	requireRegistry(t)
	requireRoot(t)

	got := runImageContainer(t, runtime.Spec{
		Image: alpine320,
		Command: []string{
			"/bin/sh", "-c",
			"test -f " + alpineRelease + " && test -x " + alpineBusybox +
				" && test -L " + alpineShell + " && echo correct",
		},
		Network: network.ModeHost,
	})

	if got.stdout != "correct" {
		t.Errorf("stdout = %q, want %q (stderr: %q)", got.stdout, "correct", got.stderr)
	}
}

// --- 11. Cleanup ------------------------------------------------------------

// PRD §10.4 and FR-2.3, now with an image in the picture: the container's tree,
// including everything unpacked into it, is gone once it exits — and the blob
// cache, which is not this container's to delete, is untouched.
//
// The cache is private to this test even though a warm shared one would save a
// transfer, because the two assertions at the end are about the *whole* cache
// directory rather than about this container. assertNoStaging in particular
// fails on any .part file in staging/, and a .part file is what a download in
// progress looks like — so run against the shared cache it reports the live
// downloads of whichever parallel test happened to be mid-pull, most reliably
// the multi-layer image in TestMultiLayerImageRootfs. Nothing has leaked in
// that case; the assertion simply cannot tell "abandoned" from "in flight",
// which is exactly why every other assertNoStaging call site already owns its
// cache.
func TestCleanupAfterContainerExit(t *testing.T) {
	t.Parallel()
	requireRegistry(t)
	requireRoot(t)

	cache := privateCache(t)
	got := pull(t, cache, newRegistryClient(t, nil), alpine320)

	root := filepath.Join(t.TempDir(), "containers")

	result := runImageContainerIn(t, root, cache.Root(), runtime.Spec{
		Image:   alpine320,
		Command: []string{"/bin/sh", "-c", "echo done"},
		Network: network.ModeHost,
	})
	if result.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", result.status.Code, result.stderr)
	}

	// No container directory survives.
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading %s = %v", root, err)
	}
	for _, entry := range entries {
		t.Errorf("container directory %s left behind after exit", entry.Name())
	}

	// No mount is left pointing into it. Mount teardown follows the namespace's
	// last process, so this is the one assertion worth polling for.
	pollUntil(t, 5*time.Second, "mounts under the container root to be gone", func() bool {
		return len(mountsUnder(t, root)) == 0
	})

	// The cache is shared and must survive: a run deletes only blobs it has
	// proved corrupt.
	assertCacheIntact(t, cache, got.manifest)
	assertNoStaging(t, cache)
}

// --- 12. Corrupt cache recovery ---------------------------------------------

// Bit rot in the cache is caught at use, the blob is quarantined, and the next
// run repairs itself by downloading it again (ADR-0021).
func TestCorruptCacheRecovery(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	cache := privateCache(t)
	client := newRegistryClient(t, nil)

	got := pull(t, cache, client, alpine320)
	layer := got.manifest.Layers[0]

	// Overwrite a cached layer with something the same shape and different
	// bytes, which is what a failing disk produces.
	path, err := cache.Path(layer.Digest)
	if err != nil {
		t.Fatalf("Path() = %v", err)
	}
	if err := os.WriteFile(path, []byte("this is not a gzipped tar"), 0o600); err != nil {
		t.Fatalf("corrupting %s: %v", path, err)
	}

	dest := emptyDir(t)
	_, err = image.BuildRootfs(t.Context(), cache, got.manifest.Layers, dest)
	if !errors.Is(err, image.ErrDigestMismatch) {
		t.Fatalf("BuildRootfs() over a corrupt layer = %v, want %v", err, image.ErrDigestMismatch)
	}

	// Quarantined: the bad bytes are definitively wrong, so deleting them is
	// safe and is what makes the next run work.
	has, err := cache.Has(layer.Digest)
	if err != nil {
		t.Fatalf("Has() = %v", err)
	}
	if has {
		t.Error("the corrupt layer is still cached; it should have been quarantined")
	}

	// And the partial tree it left is gone.
	assertEmptyDir(t, dest)

	// The repair: a second pull re-downloads exactly the quarantined blob.
	repair := pull(t, cache, client, alpine320)
	if repair.stats.Fetched != 1 {
		t.Errorf("Fetched = %d on the repairing pull, want exactly the quarantined layer", repair.stats.Fetched)
	}

	if _, err := image.BuildRootfs(t.Context(), cache, got.manifest.Layers, emptyDir(t)); err != nil {
		t.Errorf("BuildRootfs() after the repair = %v", err)
	}
	assertCacheIntact(t, cache, got.manifest)
}

// --- 13. Interrupted pull recovery ------------------------------------------

// A transfer cut off part-way through must leave no partial blob and no staging
// file, and must not poison the retry. The interruption is real: the context is
// cancelled while the layer's body is still streaming from the CDN.
func TestInterruptedPullRecovery(t *testing.T) {
	t.Parallel()
	requireRegistry(t)

	cache := privateCache(t)

	ctx, cancel := context.WithTimeout(t.Context(), pullTimeout)
	defer cancel()

	// Resolve first, over an untouched transport, so the interruption lands on
	// a layer rather than on the manifest.
	ref, err := image.ParseReference(alpine320)
	if err != nil {
		t.Fatalf("ParseReference() = %v", err)
	}
	manifest, err := newRegistryClient(t, nil).Resolve(ctx, ref, image.HostPlatform())
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}

	interruptCtx, interrupt := context.WithCancel(ctx)
	defer interrupt()

	transport := &interruptingTransport{base: http.DefaultTransport, cancel: interrupt}
	client := newRegistryClient(t, transport)

	_, err = image.Pull(interruptCtx, client, cache, ref, manifest)
	if err == nil {
		t.Fatal("Pull() = nil, want the interrupted transfer to fail")
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, image.ErrRegistryUnavailable) {
		t.Fatalf("Pull() = %v, want a cancellation", err)
	}
	if !transport.interrupted.Load() {
		t.Fatal("the transport never interrupted a layer; the test proved nothing")
	}

	// No partial blob, and no staging file for the next run to trip over.
	for _, layer := range manifest.Layers {
		has, err := cache.Has(layer.Digest)
		if err != nil {
			t.Fatalf("Has() = %v", err)
		}
		if has {
			t.Errorf("interrupted layer %s is in the cache", layer.Digest)
		}
	}
	assertNoStaging(t, cache)

	// The retry, on an uninterrupted transport, succeeds and completes the
	// image.
	repaired := pull(t, cache, newRegistryClient(t, nil), alpine320)
	assertCacheIntact(t, cache, repaired.manifest)
	assertNoStaging(t, cache)
}

// --- transports and proxies -------------------------------------------------

// countingTransport records what actually crossed the socket. It wraps the real
// transport and changes nothing about the bytes.
type countingTransport struct {
	base http.RoundTripper

	blobRequests     atomic.Int64
	manifestRequests atomic.Int64
	blobBytes        atomic.Int64
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	isBlob := strings.Contains(req.URL.Path, "/blobs/")

	switch {
	case isBlob:
		c.blobRequests.Add(1)
	case strings.Contains(req.URL.Path, "/manifests/"):
		c.manifestRequests.Add(1)
	}

	resp, err := c.base.RoundTrip(req)
	if err != nil || !isBlob {
		return resp, err
	}

	resp.Body = &countingBody{ReadCloser: resp.Body, count: &c.blobBytes}
	return resp, nil
}

type countingBody struct {
	io.ReadCloser
	count *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.count.Add(int64(n))
	return n, err
}

// interruptingTransport cancels the pull once a layer body has begun arriving,
// which is a connection dropped mid-transfer as far as everything above it is
// concerned.
type interruptingTransport struct {
	base   http.RoundTripper
	cancel context.CancelFunc

	interrupted atomic.Bool
}

func (i *interruptingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := i.base.RoundTrip(req)
	if err != nil || !strings.Contains(req.URL.Path, "/blobs/") {
		return resp, err
	}

	// Only the layer is worth interrupting; the config blob is a couple of
	// kilobytes and would be over before the cancellation landed.
	if resp.ContentLength >= 0 && resp.ContentLength < 64<<10 {
		return resp, nil
	}

	resp.Body = &interruptingBody{ReadCloser: resp.Body, transport: i}
	return resp, nil
}

type interruptingBody struct {
	io.ReadCloser
	transport *interruptingTransport
	read      int64
}

func (b *interruptingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)

	// Let a little of the body through first, so the failure is genuinely a
	// transfer cut short rather than one that never started.
	if b.read > 0 && b.transport.interrupted.CompareAndSwap(false, true) {
		b.transport.cancel()
	}

	return n, err
}

// tamperingProxy forwards to the real Docker Hub and can corrupt what comes
// back. It is not a mock registry: every header, every redirect and every byte
// originates at the real registry, and the proxy's only contribution is to
// change one of those bytes on request.
type tamperingProxy struct {
	server *httptest.Server
	tamper atomic.Bool
}

func newTamperingProxy(t *testing.T) *tamperingProxy {
	t.Helper()

	p := &tamperingProxy{}

	// A client that follows the CDN redirect itself, so the blob body passes
	// through this process where it can be altered. Left to the caller, the
	// 307 would be followed straight to the CDN and the proxy would never see
	// the bytes.
	forwarder := &http.Client{Timeout: pullTimeout}

	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		upstream := "https://registry-1.docker.io" + req.URL.RequestURI()

		outbound, err := http.NewRequestWithContext(req.Context(), req.Method, upstream, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for _, header := range []string{"Accept", "Authorization", "User-Agent"} {
			for _, value := range req.Header.Values(header) {
				outbound.Header.Add(header, value)
			}
		}

		resp, err := forwarder.Do(outbound)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// The challenge has to reach the client verbatim, or it cannot find the
		// real token service.
		for _, header := range []string{"WWW-Authenticate", "Content-Type", "Docker-Content-Digest"} {
			if value := resp.Header.Get(header); value != "" {
				w.Header().Set(header, value)
			}
		}
		w.WriteHeader(resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return
		}
		if p.tamper.Load() && strings.Contains(req.URL.Path, "/blobs/") && len(body) > 0 {
			// One byte, in the middle, keeping the length identical — so the
			// length check cannot catch it and only the digest can.
			body[len(body)/2] ^= 0xff
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(p.server.Close)

	return p
}

// reference returns a reference pointing at the proxy instead of Docker Hub.
func (p *tamperingProxy) reference(repositoryAndTag string) string {
	return strings.TrimPrefix(p.server.URL, "http://") + "/" + repositoryAndTag
}

// tamperWithBlobs turns on corruption for every later blob response.
func (p *tamperingProxy) tamperWithBlobs() { p.tamper.Store(true) }

// --- helpers ----------------------------------------------------------------

// runImageContainer runs a container from an image, using the run-wide cache so
// the image is transferred once for the whole suite.
func runImageContainer(t *testing.T, spec runtime.Spec) result {
	t.Helper()

	return runImageContainerIn(t, filepath.Join(t.TempDir(), "containers"), sharedCache(t).Root(), spec)
}

// runImageContainerIn is runImageContainer with explicit roots, for the tests
// that inspect what Forge left in them.
//
// It exists rather than reusing runContainerIn because that helper predates
// Stage 5 and builds a Config with no ImageRoot, which would point every
// container at the real /var/lib/forge/images.
func runImageContainerIn(t *testing.T, root, imageRoot string, spec runtime.Spec) result {
	t.Helper()

	var stdout, stderr, logs strings.Builder
	spec.Stdout = &stdout
	spec.Stderr = &stderr

	runner, err := runtime.NewRunner(
		logging.New(&logs, slog.LevelDebug),
		runtime.Config{Root: root, ImageRoot: imageRoot},
	)
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), pullTimeout)
	defer cancel()

	status, err := runner.Run(ctx, spec)
	t.Logf("forge log:\n%s", logs.String())
	if err != nil {
		t.Fatalf("Run() = %v\ncontainer stderr: %s", err, stderr.String())
	}

	return result{
		status: status,
		stdout: strings.TrimSpace(stdout.String()),
		stderr: strings.TrimSpace(stderr.String()),
	}
}

// buildRootfs unpacks an image into a fresh directory and returns it.
func buildRootfs(t *testing.T, cache *image.Cache, manifest image.Manifest) string {
	t.Helper()

	dest := emptyDir(t)

	if _, err := image.BuildRootfs(t.Context(), cache, manifest.Layers, dest); err != nil {
		t.Fatalf("BuildRootfs() = %v", err)
	}

	return dest
}

// emptyDir returns a new empty directory, cleaned up with the test.
func emptyDir(t *testing.T) string {
	t.Helper()

	dest := filepath.Join(t.TempDir(), "rootfs")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatalf("mkdir %s = %v", dest, err)
	}
	return dest
}

// assertRootfsFile fails unless path exists inside an unpacked root filesystem.
func assertRootfsFile(t *testing.T, dest, path string) {
	t.Helper()

	full := filepath.Join(dest, strings.TrimPrefix(path, "/"))
	if _, err := os.Stat(full); err != nil {
		t.Errorf("stat %s in the unpacked rootfs = %v", path, err)
	}
}

// assertEmptyDir fails if a directory has anything in it.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s = %v", dir, err)
	}
	for _, entry := range entries {
		t.Errorf("%s left behind in what should be an empty directory", entry.Name())
	}
}

// assertCacheIntact re-verifies every blob of an image against its own digest.
func assertCacheIntact(t *testing.T, cache *image.Cache, manifest image.Manifest) {
	t.Helper()

	for _, descriptor := range append([]image.Descriptor{manifest.Config}, manifest.Layers...) {
		if err := cache.Verify(t.Context(), descriptor.Digest); err != nil {
			t.Errorf("Verify(%s) = %v", descriptor.Digest, err)
		}
	}
}

// assertNoStaging fails if a download was left half-written.
func assertNoStaging(t *testing.T, cache *image.Cache) {
	t.Helper()

	staging := filepath.Join(cache.Root(), "staging")
	entries, err := os.ReadDir(staging)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("reading %s = %v", staging, err)
	}
	for _, entry := range entries {
		t.Errorf("staging file %s left behind", entry.Name())
	}
}

// blobIdentities records each blob's inode and modification time, so a later
// pull that claims to have transferred nothing can be checked against the
// filesystem rather than taken at its word.
func blobIdentities(t *testing.T, cache *image.Cache, manifest image.Manifest) map[string]string {
	t.Helper()

	identities := make(map[string]string)
	for _, descriptor := range append([]image.Descriptor{manifest.Config}, manifest.Layers...) {
		path, err := cache.Path(descriptor.Digest)
		if err != nil {
			t.Fatalf("Path() = %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s = %v", path, err)
		}
		identities[descriptor.Digest] = fmt.Sprintf("%d/%d", info.Size(), info.ModTime().UnixNano())
	}
	return identities
}

// mountsUnder returns the mount points at or under dir, as the host sees them.
func mountsUnder(t *testing.T, dir string) []string {
	t.Helper()

	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("reading mountinfo: %v", err)
	}

	prefix := strings.TrimSuffix(dir, "/") + "/"

	var found []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if point := fields[4]; point == dir || strings.HasPrefix(point, prefix) {
			found = append(found, point)
		}
	}
	return found
}

// pollUntil waits for a condition, failing the test if it never holds.
//
// It exists for the assertions whose subject the kernel completes
// asynchronously — mount teardown following a namespace's last process is the
// one that matters here. Everything else in this file is synchronous and is
// asserted directly, because a poll around a synchronous condition only hides
// how long it really took (SSOT §7).
func pollUntil(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
