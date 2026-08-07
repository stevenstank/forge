package image_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevenstank/forge/internal/image"
)

func TestBuildRootfs(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	layers := []image.Descriptor{
		{Digest: store(t, cache, buildLayer(t, dir("bin"), file("bin/sh", "shell")))},
		{Digest: store(t, cache, buildLayer(t, dir("etc"), file("etc/hostname", "forge")))},
	}

	stats, err := image.BuildRootfs(t.Context(), cache, layers, dest)
	if err != nil {
		t.Fatalf("BuildRootfs() = %v", err)
	}

	if stats.Files != 2 || stats.Dirs != 2 {
		t.Errorf("stats = %+v, want 2 files and 2 dirs across both layers", stats)
	}

	assertFile(t, filepath.Join(dest, "bin", "sh"), "shell")
	assertFile(t, filepath.Join(dest, "etc", "hostname"), "forge")
}

// PRD §10.4: a partial failure leaves nothing half-built. The rollback is
// registered before the first byte is extracted, so there is no window in which
// files exist and nothing is responsible for them.
func TestBuildRootfsEmptiesTheDestinationOnFailure(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	good := store(t, cache, buildLayer(t, dir("bin"), file("bin/sh", "shell")))

	// The second layer's blob is in the cache but its bytes are not a layer, so
	// the failure happens after the first layer has already been written.
	bad := store(t, cache, []byte("not a tar stream at all"))

	layers := []image.Descriptor{{Digest: good}, {Digest: bad}}

	_, err := image.BuildRootfs(t.Context(), cache, layers, dest)
	if err == nil {
		t.Fatal("BuildRootfs() = nil, want an error")
	}
	// The message has to say which layer failed, or an operator cannot tell a
	// broken image from a broken cache.
	if !errors.Is(err, image.ErrCorruptLayer) {
		t.Errorf("BuildRootfs() = %v, want %v", err, image.ErrCorruptLayer)
	}

	entries, readErr := os.ReadDir(dest)
	if readErr != nil {
		t.Fatalf("reading %s = %v", dest, readErr)
	}
	for _, entry := range entries {
		t.Errorf("%s left behind after a failed build", entry.Name())
	}

	// The directory itself is the caller's and must survive: internal/rootfs
	// owns it, internal/image owns only its contents (ADR-0020).
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("stat %s = %v, want the destination directory kept", dest, err)
	}
}

// A blob that is missing from the cache fails before anything is written.
func TestBuildRootfsFailsOnAMissingLayer(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	layers := []image.Descriptor{{Digest: "sha256:" + exampleHex}}

	if _, err := image.BuildRootfs(t.Context(), cache, layers, dest); !errors.Is(err, image.ErrBlobNotFound) {
		t.Fatalf("BuildRootfs() = %v, want %v", err, image.ErrBlobNotFound)
	}
}

func TestBuildRootfsValidatesItsArguments(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)
	layers := []image.Descriptor{{Digest: store(t, cache, buildLayer(t, file("a", "a")))}}

	if _, err := image.BuildRootfs(t.Context(), nil, layers, dest); err == nil {
		t.Error("BuildRootfs() with no cache = nil, want an error")
	}
	if _, err := image.BuildRootfs(t.Context(), cache, nil, dest); err == nil {
		t.Error("BuildRootfs() with no layers = nil, want an error")
	}
	if _, err := image.BuildRootfs(t.Context(), cache, layers, filepath.Join(dest, "absent")); err == nil {
		t.Error("BuildRootfs() into a missing directory = nil, want an error")
	}

	notADirectory := filepath.Join(dest, "file")
	if err := os.WriteFile(notADirectory, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing %s = %v", notADirectory, err)
	}
	if _, err := image.BuildRootfs(t.Context(), cache, layers, notADirectory); err == nil {
		t.Error("BuildRootfs() into a file = nil, want an error")
	}
}

// A cancelled build stops and rolls back, like any other failure.
func TestBuildRootfsHonoursCancellation(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)
	layers := []image.Descriptor{{Digest: store(t, cache, buildLayer(t, file("a", "a")))}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := image.BuildRootfs(ctx, cache, layers, dest); !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildRootfs() = %v, want %v", err, context.Canceled)
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("reading %s = %v", dest, err)
	}
	if len(entries) != 0 {
		t.Errorf("%d entries left behind after a cancelled build", len(entries))
	}
}

// The end-to-end shape of the stage, with no runtime involved: resolve, pull,
// build. This is what internal/runtime will call in the following change.
func TestResolvePullBuild(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	config := buildConfig(t, map[string]any{
		"Env":        []string{"PATH=/usr/bin"},
		"Entrypoint": []string{"/bin/sh"},
		"Cmd":        []string{"-c", "echo hi"},
		"WorkingDir": "/srv",
	})
	registry.AddImage(t, "v1", config,
		buildLayer(t, dir("bin"), file("bin/sh", "#!/bin/sh\n")),
		buildLayer(t, dir("srv"), file("srv/data", "payload")),
	)

	client := registry.Client(t)
	cache := newCache(t)
	dest := destDir(t)

	ref := registry.Reference(t, "test/img:v1")

	manifest, err := client.Resolve(t.Context(), ref, testPlatform)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if _, err := image.Pull(t.Context(), client, cache, ref, manifest); err != nil {
		t.Fatalf("Pull() = %v", err)
	}
	if _, err := image.BuildRootfs(t.Context(), cache, manifest.Layers, dest); err != nil {
		t.Fatalf("BuildRootfs() = %v", err)
	}

	assertFile(t, filepath.Join(dest, "bin", "sh"), "#!/bin/sh\n")
	assertFile(t, filepath.Join(dest, "srv", "data"), "payload")

	raw, err := cache.ReadAll(manifest.Config.Digest)
	if err != nil {
		t.Fatalf("ReadAll(config) = %v", err)
	}
	parsed, err := image.ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig() = %v", err)
	}
	if got := parsed.Command(nil); len(got) != 3 || got[0] != "/bin/sh" {
		t.Errorf("Command(nil) = %v, want the image's entrypoint and cmd", got)
	}
	if parsed.WorkingDir != "/srv" {
		t.Errorf("WorkingDir = %q, want /srv", parsed.WorkingDir)
	}

	assertStagingEmpty(t, cache)
}

// D3. BuildRootfs's rollback empties the destination on failure, but the
// destination is the caller's — internal/rootfs owns the directory, this
// package owns only its contents. Handed a populated directory and a layer
// that fails, the rollback therefore deletes data BuildRootfs never wrote.
//
// The runtime never triggers this, because Store.Prepare hands over a fresh
// empty directory. As a package API it is a data-loss footgun, and the fix is
// to make the precondition the rollback already assumes into one the code
// enforces.
func TestBuildRootfsRefusesANonEmptyDestination(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	precious := filepath.Join(dest, "caller-data")
	if err := os.WriteFile(precious, []byte("not forge's to delete"), 0o644); err != nil {
		t.Fatalf("writing %s = %v", precious, err)
	}

	layers := []image.Descriptor{{Digest: store(t, cache, buildLayer(t, file("a", "a")))}}

	if _, err := image.BuildRootfs(t.Context(), cache, layers, dest); err == nil {
		t.Fatal("BuildRootfs() = nil, want a non-empty destination refused")
	}

	// Nothing the caller put there may have been touched.
	assertFile(t, precious, "not forge's to delete")
}

// The same precondition, proven against the rollback path it protects: a
// failing layer must not turn into deletion of the caller's tree.
func TestBuildRootfsDoesNotDeleteCallerDataOnFailure(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	precious := filepath.Join(dest, "caller-data")
	if err := os.WriteFile(precious, []byte("not forge's to delete"), 0o644); err != nil {
		t.Fatalf("writing %s = %v", precious, err)
	}

	layers := []image.Descriptor{
		{Digest: store(t, cache, buildLayer(t, file("a", "a")))},
		{Digest: store(t, cache, []byte("not a tar stream at all"))},
	}

	if _, err := image.BuildRootfs(t.Context(), cache, layers, dest); err == nil {
		t.Fatal("BuildRootfs() = nil, want an error")
	}

	assertFile(t, precious, "not forge's to delete")
}
