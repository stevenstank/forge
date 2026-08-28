package image_test

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/image"
)

// unpackInto stores a layer in the cache and applies it to a fresh directory,
// which is the shape of every extraction test below.
func unpackInto(t *testing.T, cache *image.Cache, dest string, layer []byte) (image.Stats, error) {
	t.Helper()

	digest := store(t, cache, layer)

	return cache.UnpackLayer(t.Context(), digest, dest)
}

// destDir returns an empty directory standing in for a container's rootfs.
func destDir(t *testing.T) string {
	t.Helper()

	dest := filepath.Join(t.TempDir(), "rootfs")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatalf("mkdir %s = %v", dest, err)
	}

	return dest
}

func TestUnpackLayerWritesEveryEntryType(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	layer := buildLayer(t,
		dir("etc"),
		file("etc/hostname", "forge"),
		dir("usr"),
		dir("usr/bin"),
		file("usr/bin/tool", "#!/bin/sh\n"),
		symlink("bin", "usr/bin"),
		hardlink("etc/hostname.bak", "etc/hostname"),
	)

	stats, err := unpackInto(t, cache, dest, layer)
	if err != nil {
		t.Fatalf("UnpackLayer() = %v", err)
	}

	if stats.Dirs != 3 || stats.Files != 2 || stats.Symlinks != 1 || stats.Hardlinks != 1 {
		t.Errorf("stats = %+v, want 3 dirs, 2 files, 1 symlink, 1 hard link", stats)
	}

	assertFile(t, filepath.Join(dest, "etc", "hostname"), "forge")
	assertFile(t, filepath.Join(dest, "etc", "hostname.bak"), "forge")

	target, err := os.Readlink(filepath.Join(dest, "bin"))
	if err != nil {
		t.Fatalf("readlink = %v", err)
	}
	if target != "usr/bin" {
		t.Errorf("bin -> %q, want usr/bin", target)
	}
}

// A tar is not required to contain an entry for every directory it writes
// into.
func TestUnpackLayerCreatesMissingParents(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	if _, err := unpackInto(t, cache, dest, buildLayer(t, file("a/b/c/deep", "value"))); err != nil {
		t.Fatalf("UnpackLayer() = %v", err)
	}

	assertFile(t, filepath.Join(dest, "a", "b", "c", "deep"), "value")
}

// An uncompressed layer is legal, and the extractor sniffs rather than trusting
// the media type.
func TestUnpackLayerAcceptsAnUncompressedTar(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	if _, err := unpackInto(t, cache, dest, buildTar(t, file("plain", "tar"))); err != nil {
		t.Fatalf("UnpackLayer() = %v", err)
	}

	assertFile(t, filepath.Join(dest, "plain"), "tar")
}

// Mode bits, including setuid, must survive extraction: a rootfs whose
// binaries lost their permissions is a rootfs that fails later, somewhere less
// obvious.
func TestUnpackLayerPreservesModes(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	layer := buildLayer(t,
		entry{name: "script", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o755},
		entry{name: "secret", typ: tar.TypeReg, body: "hidden", mode: 0o600},
		entry{name: "su", typ: tar.TypeReg, body: "elevate", mode: 0o4755},
		entry{name: "restricted", typ: tar.TypeDir, mode: 0o700},
	)

	if _, err := unpackInto(t, cache, dest, layer); err != nil {
		t.Fatalf("UnpackLayer() = %v", err)
	}

	assertMode(t, filepath.Join(dest, "script"), 0o755)
	assertMode(t, filepath.Join(dest, "secret"), 0o600)
	assertMode(t, filepath.Join(dest, "restricted"), 0o700)

	info, err := os.Stat(filepath.Join(dest, "su"))
	if err != nil {
		t.Fatalf("stat = %v", err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Errorf("mode = %v, want the setuid bit preserved", info.Mode())
	}
}

// A directory that the layer declares unwritable must still receive the entries
// that come after it. This is why modes are applied in a second pass.
func TestUnpackLayerWritesIntoARestrictedDirectory(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so this asserts nothing as root")
	}

	cache := newCache(t)
	dest := destDir(t)

	layer := buildLayer(t,
		entry{name: "locked", typ: tar.TypeDir, mode: 0o500},
		file("locked/inside", "written anyway"),
	)

	// The directory is left unwritable, which is the point of the test and
	// also more than TempDir's cleanup can remove.
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "locked"), 0o700) })

	if _, err := unpackInto(t, cache, dest, layer); err != nil {
		t.Fatalf("UnpackLayer() = %v", err)
	}

	assertMode(t, filepath.Join(dest, "locked"), 0o500)
	assertFile(t, filepath.Join(dest, "locked", "inside"), "written anyway")
}

// A FIFO needs no privilege, so this half of the device path runs everywhere.
func TestUnpackLayerCreatesAFifo(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	layer := buildLayer(t, entry{name: "pipe", typ: tar.TypeFifo, mode: 0o644})

	stats, err := unpackInto(t, cache, dest, layer)
	if err != nil {
		t.Fatalf("UnpackLayer() = %v", err)
	}
	if stats.Devices != 1 {
		t.Errorf("Devices = %d, want 1", stats.Devices)
	}

	info, err := os.Lstat(filepath.Join(dest, "pipe"))
	if err != nil {
		t.Fatalf("lstat = %v", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("mode = %v, want a named pipe", info.Mode())
	}
}

// A character device needs CAP_MKNOD. Unprivileged, the failure must say so
// rather than surfacing a bare EPERM — and it must fail rather than skip, since
// a rootfs missing a device node its image declared breaks later, somewhere
// less obvious.
func TestUnpackLayerReportsDeviceNodesNeedRoot(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	layer := buildLayer(t, entry{name: "dev/null", typ: tar.TypeChar, mode: 0o666, major: 1, minor: 3})

	stats, err := unpackInto(t, cache, dest, layer)

	if os.Geteuid() == 0 {
		if err != nil {
			t.Fatalf("UnpackLayer() as root = %v", err)
		}
		if stats.Devices != 1 {
			t.Errorf("Devices = %d, want 1", stats.Devices)
		}
		return
	}

	if err == nil {
		t.Fatal("UnpackLayer() = nil unprivileged, want a failure")
	}
	if !strings.Contains(err.Error(), "needs root") {
		t.Errorf("error %q does not explain that device nodes need root", err)
	}
}

// Without root, ownership cannot be applied and the resulting tree is not the
// one the image describes. That is counted rather than passed over in silence.
func TestUnpackLayerCountsEntriesItCouldNotOwn(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	// The builder's entries are owned by uid 0, which an unprivileged process
	// cannot set.
	stats, err := unpackInto(t, cache, dest, buildLayer(t, file("owned", "by root")))
	if err != nil {
		t.Fatalf("UnpackLayer() = %v", err)
	}

	if os.Geteuid() == 0 {
		if stats.UnownedEntries != 0 {
			t.Errorf("UnownedEntries = %d as root, want 0", stats.UnownedEntries)
		}
		return
	}
	if stats.UnownedEntries != 1 {
		t.Errorf("UnownedEntries = %d unprivileged, want 1", stats.UnownedEntries)
	}
}

// Layer order is the whole of the merge under explicit extraction (ADR-0003).
func TestLayersAreAppliedInOrder(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	base := store(t, cache, buildLayer(t,
		dir("etc"),
		file("etc/motd", "from the base layer"),
		file("etc/keep", "untouched"),
		dir("var"),
		file("var/cache", "stale"),
	))
	middle := store(t, cache, buildLayer(t,
		file("etc/motd", "overwritten by the middle layer"),
	))
	top := store(t, cache, buildLayer(t,
		whiteout("var/cache"),
		file("etc/added", "by the top layer"),
	))

	layers := []image.Descriptor{{Digest: base}, {Digest: middle}, {Digest: top}}

	stats, err := image.BuildRootfs(t.Context(), cache, layers, dest)
	if err != nil {
		t.Fatalf("BuildRootfs() = %v", err)
	}
	if stats.Whiteouts != 1 {
		t.Errorf("Whiteouts = %d, want 1", stats.Whiteouts)
	}

	assertFile(t, filepath.Join(dest, "etc", "motd"), "overwritten by the middle layer")
	assertFile(t, filepath.Join(dest, "etc", "keep"), "untouched")
	assertFile(t, filepath.Join(dest, "etc", "added"), "by the top layer")
	assertAbsent(t, filepath.Join(dest, "var", "cache"))

	// The whiteout deletes the file, not the directory holding it.
	if _, err := os.Stat(filepath.Join(dest, "var")); err != nil {
		t.Errorf("stat var = %v, want the directory kept", err)
	}
}

// A whiteout acts on the tree built so far, at the moment it is read, so a
// layer that deletes a path and then recreates it ends with the path present.
func TestWhiteoutsApplyInReadOrder(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	base := store(t, cache, buildLayer(t, dir("etc"), file("etc/conf", "old")))
	top := store(t, cache, buildLayer(t,
		whiteout("etc/conf"),
		file("etc/conf", "new"),
	))

	if _, err := image.BuildRootfs(t.Context(), cache,
		[]image.Descriptor{{Digest: base}, {Digest: top}}, dest); err != nil {
		t.Fatalf("BuildRootfs() = %v", err)
	}

	assertFile(t, filepath.Join(dest, "etc", "conf"), "new")
}

// An opaque whiteout empties the directory it marks without removing it.
func TestOpaqueWhiteout(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	base := store(t, cache, buildLayer(t,
		dir("data"),
		file("data/one", "1"),
		file("data/two", "2"),
		dir("data/nested"),
		file("data/nested/three", "3"),
		file("keep", "outside the opaque directory"),
	))
	top := store(t, cache, buildLayer(t,
		file("data/.wh..wh..opq", ""),
		file("data/fresh", "only this"),
	))

	if _, err := image.BuildRootfs(t.Context(), cache,
		[]image.Descriptor{{Digest: base}, {Digest: top}}, dest); err != nil {
		t.Fatalf("BuildRootfs() = %v", err)
	}

	assertAbsent(t, filepath.Join(dest, "data", "one"))
	assertAbsent(t, filepath.Join(dest, "data", "two"))
	assertAbsent(t, filepath.Join(dest, "data", "nested"))
	assertFile(t, filepath.Join(dest, "data", "fresh"), "only this")
	assertFile(t, filepath.Join(dest, "keep"), "outside the opaque directory")
}

// A layer may replace a file with a directory, a directory with a file, or
// anything with a symlink.
func TestLayersMayChangeAnEntrysType(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	base := store(t, cache, buildLayer(t,
		file("swap", "was a file"),
		dir("flip"),
		file("flip/child", "inside"),
	))
	top := store(t, cache, buildLayer(t,
		dir("swap"),
		file("swap/now", "a directory"),
		file("flip", "now a file"),
	))

	if _, err := image.BuildRootfs(t.Context(), cache,
		[]image.Descriptor{{Digest: base}, {Digest: top}}, dest); err != nil {
		t.Fatalf("BuildRootfs() = %v", err)
	}

	assertFile(t, filepath.Join(dest, "swap", "now"), "a directory")
	assertFile(t, filepath.Join(dest, "flip"), "now a file")
}

// The case the containment rules exist for: root is writing paths that came
// from the internet.
func TestUnpackLayerRefusesEntriesThatEscape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry entry
	}{
		{name: "a parent reference", entry: file("../escaped", "outside")},
		{name: "a deep parent reference", entry: file("a/../../escaped", "outside")},
		{name: "an absolute path", entry: file("/etc/escaped", "outside")},
		{name: "a hard link to outside", entry: hardlink("link", "../escaped")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cache := newCache(t)
			dest := destDir(t)
			outside := filepath.Join(filepath.Dir(dest), "escaped")

			_, err := unpackInto(t, cache, dest, buildLayer(t, tt.entry))
			if !errors.Is(err, image.ErrEscapesRoot) {
				t.Fatalf("UnpackLayer() = %v, want %v", err, image.ErrEscapesRoot)
			}
			assertAbsent(t, outside)
		})
	}
}

// A symlink out of the tree is legal to *create* — images contain them — and
// must never be written *through*. A later layer writing to a path under it
// lands inside the tree, as it would for a process that had pivoted.
func TestWritesThroughASymlinkStayInsideTheTree(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	outside := filepath.Join(filepath.Dir(dest), "host-etc")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatalf("mkdir %s = %v", outside, err)
	}
	if err := os.WriteFile(filepath.Join(outside, "passwd"), []byte("host's own file"), 0o644); err != nil {
		t.Fatalf("writing the host's file: %v", err)
	}

	base := store(t, cache, buildLayer(t, symlink("etc", outside)))
	top := store(t, cache, buildLayer(t, file("etc/passwd", "written by the layer")))

	if _, err := image.BuildRootfs(t.Context(), cache,
		[]image.Descriptor{{Digest: base}, {Digest: top}}, dest); err != nil {
		t.Fatalf("BuildRootfs() = %v", err)
	}

	// The host's file is untouched, and the write landed inside the tree.
	assertFile(t, filepath.Join(outside, "passwd"), "host's own file")
	assertFile(t, filepath.Join(dest, strings.TrimPrefix(outside, "/"), "passwd"), "written by the layer")
}

// A layer whose bytes do not match the digest naming it is definitively wrong,
// so it is quarantined and the next run downloads it again (ADR-0021).
func TestUnpackLayerQuarantinesACorruptBlob(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	digest := store(t, cache, buildLayer(t, file("hello", "world")))

	path, err := cache.Path(digest)
	if err != nil {
		t.Fatalf("Path() = %v", err)
	}
	if err := os.WriteFile(path, []byte("not a layer at all"), 0o600); err != nil {
		t.Fatalf("corrupting the blob: %v", err)
	}

	_, err = cache.UnpackLayer(t.Context(), digest, dest)
	if !errors.Is(err, image.ErrDigestMismatch) && !errors.Is(err, image.ErrCorruptLayer) {
		t.Fatalf("UnpackLayer() = %v, want a digest mismatch or a corrupt layer", err)
	}

	has, hasErr := cache.Has(digest)
	if hasErr != nil {
		t.Fatalf("Has() = %v", hasErr)
	}
	if has {
		t.Error("the corrupt blob is still in the cache; it must be quarantined")
	}
}

// A layer that is a valid gzip stream but a truncated tar fails as a corrupt
// layer, and its bytes are left alone: they may be exactly what the digest
// says, and forge simply could not read past the end.
func TestUnpackLayerReportsATruncatedTar(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	full := buildTar(t, file("big", strings.Repeat("x", 4096)))
	truncated := full[:len(full)/2]

	_, err := unpackInto(t, cache, dest, truncated)
	if !errors.Is(err, image.ErrCorruptLayer) {
		t.Fatalf("UnpackLayer() = %v, want %v", err, image.ErrCorruptLayer)
	}
}

func TestUnpackLayerReportsAMissingBlob(t *testing.T) {
	t.Parallel()

	cache := newCache(t)

	_, err := cache.UnpackLayer(t.Context(), "sha256:"+exampleHex, destDir(t))
	if !errors.Is(err, image.ErrBlobNotFound) {
		t.Errorf("UnpackLayer() = %v, want %v", err, image.ErrBlobNotFound)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s = %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lstat %s = %v, want the path to be absent", path, err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s = %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s has mode %v, want %v", path, got, want)
	}
}

// D5. A whiteout marker has to name something. ".wh." trims to the empty
// string and ".wh.." trims to ".", and either joined onto the marker's own
// directory yields that directory — so the deletion lands on the parent, and at
// the root of a layer that is the entire destination.
//
// The silence was worse than the deletion: the unpack returned nil, so a layer
// that wiped everything beneath it looked like a layer that had applied
// cleanly.
func TestMalformedWhiteoutsAreRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry entry
	}{
		{name: "a whiteout naming nothing at the layer root", entry: file(".wh.", "")},
		{name: "a whiteout naming nothing in a directory", entry: file("etc/.wh.", "")},
		{name: "a whiteout naming the current directory", entry: file(".wh..", "")},
		{name: "a whiteout naming the parent directory", entry: file("etc/.wh..", "")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cache := newCache(t)
			dest := destDir(t)

			base := store(t, cache, buildLayer(t,
				dir("etc"),
				file("etc/keep", "important"),
				file("root-file", "also important"),
			))
			if _, err := cache.UnpackLayer(t.Context(), base, dest); err != nil {
				t.Fatalf("applying the base layer: %v", err)
			}

			top := store(t, cache, buildLayer(t, dir("etc"), tt.entry))

			_, err := cache.UnpackLayer(t.Context(), top, dest)
			if !errors.Is(err, image.ErrCorruptLayer) {
				t.Fatalf("UnpackLayer() = %v, want %v", err, image.ErrCorruptLayer)
			}

			// Nothing may have been deleted on the way to that refusal.
			if _, statErr := os.Stat(dest); statErr != nil {
				t.Fatalf("the destination itself was removed: %v", statErr)
			}
			assertFile(t, filepath.Join(dest, "etc", "keep"), "important")
			assertFile(t, filepath.Join(dest, "root-file"), "also important")
		})
	}
}

// The well-formed markers must keep working, including the opaque one, whose
// prefix check runs before the branch the fix above guards.
func TestWellFormedWhiteoutsStillApply(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	base := store(t, cache, buildLayer(t,
		dir("etc"),
		file("etc/gone", "deleted by the next layer"),
		file("etc/kept", "survives"),
		dir("opaque"),
		file("opaque/hidden", "hidden by the next layer"),
	))
	top := store(t, cache, buildLayer(t,
		whiteout("etc/gone"),
		file("opaque/.wh..wh..opq", ""),
	))

	if _, err := image.BuildRootfs(t.Context(), cache,
		[]image.Descriptor{{Digest: base}, {Digest: top}}, destDirAt(t, dest)); err != nil {
		t.Fatalf("BuildRootfs() = %v", err)
	}

	assertAbsent(t, filepath.Join(dest, "etc", "gone"))
	assertFile(t, filepath.Join(dest, "etc", "kept"), "survives")
	assertAbsent(t, filepath.Join(dest, "opaque", "hidden"))
	if _, err := os.Stat(filepath.Join(dest, "opaque")); err != nil {
		t.Errorf("the opaque directory itself was removed: %v", err)
	}
}

// destDirAt returns dir unchanged; it exists to make the call above read as
// "build into this directory" without a second helper.
func destDirAt(t *testing.T, dir string) string {
	t.Helper()
	return dir
}

// layerModTime is what buildTar stamps on every entry.
var layerModTime = time.Unix(1600000000, 0)

// TestUnpackLayerRestoresModificationTimes checks the metadata pass that runs
// after the bytes are written.
//
// A directory's timestamp is the interesting one: writing an entry into a
// directory updates that directory's mtime, so restoring it eagerly would be
// undone by the very next entry. The deferred pass is what makes the extracted
// tree match the image rather than the moment it was unpacked.
func TestUnpackLayerRestoresModificationTimes(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	layer := buildLayer(t,
		dir("etc"),
		file("etc/hosts", "127.0.0.1 localhost\n"),
		file("etc/passwd", "root:x:0:0:root:/root:/bin/sh\n"),
		symlink("etc/hostname", "hosts"),
	)

	if _, err := unpackInto(t, cache, dest, layer); err != nil {
		t.Fatalf("UnpackLayer() = %v", err)
	}

	for _, path := range []string{"etc", "etc/hosts", "etc/passwd"} {
		info, err := os.Stat(filepath.Join(dest, path))
		if err != nil {
			t.Fatalf("stat %s = %v", path, err)
		}
		if !info.ModTime().Equal(layerModTime) {
			t.Errorf("%s mtime = %s, want %s", path, info.ModTime().UTC(), layerModTime.UTC())
		}
	}

	// The symlink's own timestamp, not its target's. Following it would set
	// the target's time instead and leave the link untouched.
	info, err := os.Lstat(filepath.Join(dest, "etc/hostname"))
	if err != nil {
		t.Fatalf("lstat = %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("etc/hostname is not a symlink")
	}
	if !info.ModTime().Equal(layerModTime) {
		t.Errorf("symlink mtime = %s, want %s", info.ModTime().UTC(), layerModTime.UTC())
	}
}

// TestUnpackLayerRestoresTimesOnARestrictedDirectory is the two deferred
// concerns together: a directory that is unwritable in the image still receives
// its contents, and still ends with both the mode and the timestamp the image
// declared.
func TestUnpackLayerRestoresTimesOnARestrictedDirectory(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so this asserts nothing as root")
	}

	cache := newCache(t)
	dest := destDir(t)

	layer := buildLayer(t,
		entry{name: "locked", typ: tar.TypeDir, mode: 0o500},
		file("locked/inside", "written anyway"),
	)
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "locked"), 0o700) })

	if _, err := unpackInto(t, cache, dest, layer); err != nil {
		t.Fatalf("UnpackLayer() = %v", err)
	}

	info, err := os.Stat(filepath.Join(dest, "locked"))
	if err != nil {
		t.Fatalf("stat = %v", err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Errorf("mode = %v, want 0500", info.Mode().Perm())
	}
	if !info.ModTime().Equal(layerModTime) {
		t.Errorf("mtime = %s, want %s", info.ModTime().UTC(), layerModTime.UTC())
	}
}

// TestLaterLayersReplaceMetadataToo checks that an entry rewritten by a later
// layer carries the later layer's metadata, not a merge of the two.
func TestLaterLayersReplaceMetadataToo(t *testing.T) {
	t.Parallel()

	cache := newCache(t)
	dest := destDir(t)

	first := buildLayer(t, entry{name: "tool", typ: tar.TypeReg, body: "v1", mode: 0o644})
	second := buildLayer(t, entry{name: "tool", typ: tar.TypeReg, body: "v2", mode: 0o755})

	if _, err := unpackInto(t, cache, dest, first); err != nil {
		t.Fatalf("UnpackLayer(first) = %v", err)
	}
	if _, err := unpackInto(t, cache, dest, second); err != nil {
		t.Fatalf("UnpackLayer(second) = %v", err)
	}

	assertFile(t, filepath.Join(dest, "tool"), "v2")
	assertMode(t, filepath.Join(dest, "tool"), 0o755)
}
