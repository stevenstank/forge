package runtime_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	runtimepkg "runtime"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/image"
	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/runtime"
)

// The Stage 5 half of the runtime's contract, tested without root and without a
// registry: everything below either fails before anything is created, or
// asserts a validation rule that is pure.

// newImageRunner builds a Runner whose container root, cgroup hierarchy and
// image cache all live in temporary directories, and returns the roots so a
// test can assert nothing was left in them.
func newImageRunner(t *testing.T) (*runtime.Runner, string, string) {
	t.Helper()

	root := filepath.Join(t.TempDir(), "containers")
	imageRoot := filepath.Join(t.TempDir(), "images")

	runner, err := runtime.NewRunner(
		logging.New(io.Discard, slog.LevelError),
		runtime.Config{
			Root:       root,
			ImageRoot:  imageRoot,
			CgroupRoot: fakeCgroupRoot(t),
			// One attempt, so a test that expects an unreachable registry does
			// not wait out the retry schedule.
			Registry: image.ClientConfig{MaxAttempts: 1, PlainHTTP: true},
		},
	)
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	return runner, root, imageRoot
}

// A runner that is never used with an image must not create a blob cache. This
// is the whole reason image.NewCache performs no I/O.
func TestNewRunnerCreatesNoImageCache(t *testing.T) {
	t.Parallel()

	_, _, imageRoot := newImageRunner(t)

	if _, err := os.Stat(imageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s = %v, want no image cache to have been created", imageRoot, err)
	}
}

func TestNewRunnerRejectsARelativeImageRoot(t *testing.T) {
	t.Parallel()

	_, err := runtime.NewRunner(
		logging.New(io.Discard, slog.LevelError),
		runtime.Config{Root: filepath.Join(t.TempDir(), "containers"), ImageRoot: "images"},
	)
	if err == nil {
		t.Fatal("NewRunner() = nil, want an error for a relative -image-root")
	}
}

// An image and a --rootfs are two answers to the same question. Refusing is
// better than picking one, because the caller more likely made a mistake than a
// choice.
func TestValidateRefusesAnImageAndARootfs(t *testing.T) {
	t.Parallel()

	spec := runtime.Spec{
		Command: []string{"/bin/sh"},
		Image:   "alpine:3.20",
		Rootfs:  "/srv/alpine",
	}

	if err := spec.Validate(); !errors.Is(err, runtime.ErrImageAndRootfs) {
		t.Fatalf("Validate() = %v, want %v", err, runtime.ErrImageAndRootfs)
	}
}

// A malformed reference is a usage error caught before anything is forked,
// because ParseReference performs no I/O.
func TestValidateRejectsAMalformedReference(t *testing.T) {
	t.Parallel()

	for _, ref := range []string{"ALPINE:3.20", "alpine:", "alpine@sha256:short", ":"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()

			spec := runtime.Spec{Command: []string{"/bin/sh"}, Image: ref}
			if err := spec.Validate(); !errors.Is(err, image.ErrInvalidReference) {
				t.Errorf("Validate() = %v, want %v", err, image.ErrInvalidReference)
			}
		})
	}
}

// The PATH rule narrowed in Stage 5 rather than disappearing: a bare name is
// accepted with an image and still refused without one.
func TestValidateBareCommandNames(t *testing.T) {
	t.Parallel()

	withImage := runtime.Spec{Command: []string{"ls"}, Image: "alpine:3.20"}
	if err := withImage.Validate(); err != nil {
		t.Errorf("Validate() with an image = %v, want a bare name accepted", err)
	}

	withRootfs := runtime.Spec{Command: []string{"ls"}, Rootfs: "/srv/alpine"}
	if err := withRootfs.Validate(); !errors.Is(err, runtime.ErrNotAPath) {
		t.Errorf("Validate() with a rootfs = %v, want %v", err, runtime.ErrNotAPath)
	}

	onHost := runtime.Spec{Command: []string{"ls"}}
	if err := onHost.Validate(); !errors.Is(err, runtime.ErrNotAPath) {
		t.Errorf("Validate() with no filesystem = %v, want %v", err, runtime.ErrNotAPath)
	}
}

// An image may supply the command from its entrypoint and cmd, so a spec with
// an image and no command is valid. Whether that image actually declares one is
// only knowable after the pull.
func TestValidateAllowsAnImageWithNoCommand(t *testing.T) {
	t.Parallel()

	if err := (runtime.Spec{Image: "alpine:3.20"}).Validate(); err != nil {
		t.Errorf("Validate() = %v, want an image with no command accepted", err)
	}

	if err := (runtime.Spec{}).Validate(); !errors.Is(err, runtime.ErrNoCommand) {
		t.Errorf("Validate() with neither = %v, want %v", err, runtime.ErrNoCommand)
	}
}

// The filesystem options needed a --rootfs before Stage 5. An image is now an
// equally good answer to "something to apply them to".
func TestValidateFilesystemOptionsAcceptAnImage(t *testing.T) {
	t.Parallel()

	spec := runtime.Spec{
		Command:      []string{"/bin/sh"},
		Image:        "alpine:3.20",
		Mounts:       []mount.Mount{{Source: "/srv/data", Destination: "/data", Type: mount.TypeBind}},
		ReadonlyRoot: true,
		WorkingDir:   "/srv",
	}

	if err := spec.Validate(); err != nil {
		t.Errorf("Validate() = %v, want the filesystem options accepted with an image", err)
	}
}

func TestValidateFilesystemOptionsStillNeedSomething(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec runtime.Spec
	}{
		{
			name: "a mount",
			spec: runtime.Spec{
				Command: []string{"/bin/sh"},
				Mounts:  []mount.Mount{{Source: "/srv/data", Destination: "/data", Type: mount.TypeBind}},
			},
		},
		{name: "read-only", spec: runtime.Spec{Command: []string{"/bin/sh"}, ReadonlyRoot: true}},
		{name: "a working directory", spec: runtime.Spec{Command: []string{"/bin/sh"}, WorkingDir: "/srv"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.spec.Validate()
			if !errors.Is(err, runtime.ErrMountWithoutRootfs) {
				t.Fatalf("Validate() = %v, want %v", err, runtime.ErrMountWithoutRootfs)
			}
			// The message has to mention both ways of answering it now.
			if !strings.Contains(err.Error(), "image") {
				t.Errorf("error %q does not mention that an image would satisfy it", err)
			}
		})
	}
}

// The point of pulling before creating anything: a registry that cannot be
// reached fails a run that has taken no container ID, made no directory and
// leased no address.
func TestRunLeavesNothingBehindWhenThePullFails(t *testing.T) {
	t.Parallel()

	// A server that refuses everything stands in for a registry that has no
	// such image.
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.NotFound(w, req)
	}))
	defer registry.Close()

	runner, root, imageRoot := newImageRunner(t)

	spec := runtime.Spec{
		Command: []string{"/bin/sh"},
		Image:   strings.TrimPrefix(registry.URL, "http://") + "/test/absent:v1",
		Network: "none",
	}

	_, err := runner.Run(t.Context(), spec)
	if !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("Run() = %v, want %v", err, image.ErrNotFound)
	}

	assertNoContainerDirectories(t, root)

	// The blob cache may exist — PruneStaging and Stage create it — but it must
	// hold no half-written download.
	assertNoStagedDownloads(t, imageRoot)
}

// The same guarantee for the failure that needs no server at all.
func TestRunRejectsABadReferenceBeforeCreatingAnything(t *testing.T) {
	t.Parallel()

	runner, root, imageRoot := newImageRunner(t)

	_, err := runner.Run(t.Context(), runtime.Spec{Command: []string{"/bin/sh"}, Image: "NOPE:latest"})
	if !errors.Is(err, image.ErrInvalidReference) {
		t.Fatalf("Run() = %v, want %v", err, image.ErrInvalidReference)
	}

	assertNoContainerDirectories(t, root)
	if _, err := os.Stat(imageRoot); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s = %v, want the cache untouched by a reference that never parsed", imageRoot, err)
	}
}

// The furthest an unprivileged test can follow a real image: steps 1 to 6 all
// run for real against a registry on loopback, and the run then fails at the
// clone(2) or cgroup step for want of root.
//
// That failure is what makes the test worth having. It lands *after* the
// container directory was created and the layers unpacked into it, so it
// exercises the exact partial state the cleanup stack exists for — and the
// assertions afterwards are the two halves of "partial failures leak nothing":
// the container tree is gone, and the shared cache it legitimately filled is
// intact and still verifies.
func TestRunPullsAndUnpacksThenUnwindsCleanly(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("this test asserts the unwind after an unprivileged failure; as root the run would proceed")
	}

	registry, img := serveSyntheticImage(t)

	runner, root, imageRoot := newImageRunner(t)

	spec := runtime.Spec{
		Image:   registry + "/test/img:v1",
		Command: []string{"/bin/sh"},
		Network: "none",
	}

	if _, err := runner.Run(t.Context(), spec); err == nil {
		t.Fatal("Run() = nil, want a failure for want of privilege")
	}

	// Steps 1 to 4 really happened: every blob is cached and verifies.
	cache, err := image.NewCache(imageRoot, logging.New(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatalf("NewCache() = %v", err)
	}
	for _, digest := range img.blobs {
		if err := cache.Verify(t.Context(), digest); err != nil {
			t.Errorf("Verify(%s) = %v, want the pulled blob cached and intact", digest, err)
		}
	}

	// Step 12 really happened: nothing this container created survives.
	assertNoContainerDirectories(t, root)
	assertNoStagedDownloads(t, imageRoot)
}

// syntheticImage records the digests a fake registry published, so a test can
// assert they were all cached.
type syntheticImage struct {
	blobs []string
}

// serveSyntheticImage starts a registry on loopback serving a one-layer image,
// and returns its host:port.
//
// It is a deliberately minimal reimplementation of what internal/image's own
// fake registry does. Sharing that one would mean exporting a test helper from
// a package whose tests are the only thing that should use it.
func serveSyntheticImage(t *testing.T) (string, syntheticImage) {
	t.Helper()

	return serveSyntheticImageWithConfig(t,
		[]byte(`{"architecture":"`+runtimepkg.GOARCH+`","os":"linux","config":{"Cmd":["/bin/sh"]}}`))
}

// serveSyntheticImageWithConfig is serveSyntheticImage with a caller-supplied
// image config, for the tests whose subject is what the config declares.
func serveSyntheticImageWithConfig(t *testing.T, config []byte) (string, syntheticImage) {
	t.Helper()

	layer := gzippedTar(t, map[string]string{"bin/sh": "#!/bin/sh\n"})

	blobs := map[string][]byte{
		digestOf(layer):  layer,
		digestOf(config): config,
	}

	manifest := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
			`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},`+
			`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
		digestOf(config), len(config), digestOf(layer), len(layer)))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.Contains(req.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digestOf(manifest))
			_, _ = w.Write(manifest)

		case strings.Contains(req.URL.Path, "/blobs/"):
			digest := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
			body, ok := blobs[digest]
			if !ok {
				http.NotFound(w, req)
				return
			}
			_, _ = w.Write(body)

		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)

	return strings.TrimPrefix(server.URL, "http://"),
		syntheticImage{blobs: []string{digestOf(config), digestOf(layer)}}
}

// gzippedTar builds a layer blob from a set of file contents.
func gzippedTar(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	archive := tar.NewWriter(gz)

	for name, body := range files {
		header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if _, err := archive.Write([]byte(body)); err != nil {
			t.Fatalf("writing tar body: %v", err)
		}
	}

	if err := archive.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}

	return buf.Bytes()
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// assertNoContainerDirectories fails if a run left a container tree behind.
func assertNoContainerDirectories(t *testing.T, root string) {
	t.Helper()

	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("reading %s = %v", root, err)
	}

	for _, entry := range entries {
		t.Errorf("container directory %s left behind by a failed run", entry.Name())
	}
}

// assertNoStagedDownloads fails if a run left a partial blob behind.
func assertNoStagedDownloads(t *testing.T, imageRoot string) {
	t.Helper()

	staging := filepath.Join(imageRoot, "staging")
	entries, err := os.ReadDir(staging)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("reading %s = %v", staging, err)
	}

	for _, entry := range entries {
		t.Errorf("staging file %s left behind by a failed run", entry.Name())
	}
}

// D2. A manifest that is not behind an index carries no platform descriptor, so
// Resolve has nothing to match against and accepts it unconditionally. The
// image's own config declares its OS and architecture and Forge already
// downloads and parses it — but discards both fields. Pulling an image built
// for another architecture therefore succeeds, unpacks, and fails only when
// execve returns ENOEXEC inside the container, which is the least debuggable
// place it could possibly surface.
func TestRunRejectsAnImageBuiltForAnotherArchitecture(t *testing.T) {
	t.Parallel()

	const foreign = "s390x"
	if runtimepkg.GOARCH == foreign {
		t.Skipf("this host is %s; the test needs an architecture it is not", foreign)
	}

	registry, _ := serveSyntheticImageWithConfig(t,
		[]byte(`{"architecture":"`+foreign+`","os":"linux","config":{"Cmd":["/bin/sh"]}}`))

	runner, root, _ := newImageRunner(t)

	_, err := runner.Run(t.Context(), runtime.Spec{
		Image:   registry + "/test/img:v1",
		Network: "none",
	})

	if !errors.Is(err, image.ErrNoMatchingPlatform) {
		t.Fatalf("Run() = %v, want %v", err, image.ErrNoMatchingPlatform)
	}
	// The refusal must come before anything is created, like every other
	// step-1-to-4 failure.
	assertNoContainerDirectories(t, root)
}

// An image built for another OS is the same defect wearing a different hat.
func TestRunRejectsAnImageBuiltForAnotherOS(t *testing.T) {
	t.Parallel()

	registry, _ := serveSyntheticImageWithConfig(t,
		[]byte(`{"architecture":"`+runtimepkg.GOARCH+`","os":"windows","config":{"Cmd":["/bin/sh"]}}`))

	runner, _, _ := newImageRunner(t)

	_, err := runner.Run(t.Context(), runtime.Spec{
		Image:   registry + "/test/img:v1",
		Network: "none",
	})

	if !errors.Is(err, image.ErrNoMatchingPlatform) {
		t.Fatalf("Run() = %v, want %v", err, image.ErrNoMatchingPlatform)
	}
}

// A config that declares nothing must not be rejected: an unusual image is not
// a wrong one, and refusing it would invent a requirement the spec does not
// state.
func TestRunAcceptsAnImageConfigWithNoPlatform(t *testing.T) {
	t.Parallel()

	registry, _ := serveSyntheticImageWithConfig(t, []byte(`{"config":{"Cmd":["/bin/sh"]}}`))

	runner, _, _ := newImageRunner(t)

	_, err := runner.Run(t.Context(), runtime.Spec{
		Image:   registry + "/test/img:v1",
		Network: "none",
	})

	// It gets as far as needing privilege, which is proof it was not refused
	// for its platform.
	if errors.Is(err, image.ErrNoMatchingPlatform) {
		t.Fatalf("Run() = %v, want a config that declares no platform to be accepted", err)
	}
}
