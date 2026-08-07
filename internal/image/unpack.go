package image

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Whiteout markers, from the OCI image layer specification. They are how a
// layer expresses deletion, and under explicit extraction (ADR-0003) they are
// the whole of the semantics: a whiteout is executed as a deletion at the
// moment it is read, and nothing is left behind to represent it.
const (
	// whiteoutPrefix marks a single deleted entry: ".wh.foo" deletes "foo".
	whiteoutPrefix = ".wh."

	// whiteoutOpaque empties the directory containing it, hiding everything
	// the lower layers put there.
	whiteoutOpaque = ".wh..wh..opq"
)

// maxSymlinkDepth bounds how many symlinks one path may traverse, mirroring
// the kernel's own limit. Without it, a layer containing a symlink loop would
// hang forge instead of failing it.
const maxSymlinkDepth = 40

// Directory permissions used while extracting.
//
// Directories are created world-traversable regardless of what the layer says,
// and their real mode is applied in a second pass once the layer is fully
// written (see applyDeferredMetadata). A layer that contains a 0500 directory
// followed by entries inside it is legal and common, and honouring the mode
// immediately would make writing those entries fail.
const extractDirPerm = 0o755

// Bounds on what one layer may expand into.
//
// A manifest's Size is the *compressed* size, and it is the only size Forge
// verifies. Without the two bounds below it therefore knows exactly how many
// bytes it agreed to download and nothing at all about how many it will write.
// Both halves of that gap are exploitable, and both were measured rather than
// assumed:
//
//   - 512 MiB of zeros compresses to 510 KB, a ratio of 1029:1. A 10 MB blob
//     can fill 10 GB of disk.
//   - 100,000 empty entries compress to 700 KB and take 85 seconds to extract,
//     at roughly 1.2 ms per entry. A 7 MB blob is a quarter of an hour and a
//     million inodes.
//
// The byte bound alone does not catch the second — those entries write no bytes
// at all — which is why there are two.
//
// This is the same decision defaultMaxManifestBytes already made for the other
// document a registry can make Forge read, applied to the much larger surface.
// The numbers are deliberately far above anything Forge targets, where a whole
// image is single-digit megabytes: they exist to make a hostile layer fail
// rather than to express a policy about image size.
//
// They are variables rather than constants for one reason: a test cannot
// demonstrate a bound it would have to write 8 GiB to reach. This is the same
// injection rootfs.Store.mountsUnder uses, and for the same reason — the branch
// is too consequential to leave unexercised.
var (
	// maxLayerBytes bounds the uncompressed size of one layer.
	maxLayerBytes int64 = 8 << 30

	// maxLayerEntries bounds how many members one layer may contain.
	maxLayerEntries = 500_000
)

// gzipMagic is the two-byte header of a gzip stream.
//
// The layer's media type also says whether it is compressed, but sniffing is
// what makes the extractor correct for a manifest whose media type is missing
// or wrong — and it costs two bytes.
var gzipMagic = []byte{0x1f, 0x8b}

// Stats reports what applying one layer did. It exists so tests and logs can
// assert on the shape of an extraction rather than on its side effects alone.
type Stats struct {
	Files     int
	Dirs      int
	Symlinks  int
	Hardlinks int
	Devices   int
	Whiteouts int

	// Bytes is the uncompressed size of the regular files written.
	Bytes int64

	// UnownedEntries counts entries whose uid/gid could not be applied because
	// the process is not root. It is never silently zero: an unprivileged
	// unpack logs a warning as well, because the resulting tree is not the tree
	// the image describes.
	UnownedEntries int
}

// add accumulates one layer's stats into a running total.
func (s *Stats) add(other Stats) {
	s.Files += other.Files
	s.Dirs += other.Dirs
	s.Symlinks += other.Symlinks
	s.Hardlinks += other.Hardlinks
	s.Devices += other.Devices
	s.Whiteouts += other.Whiteouts
	s.Bytes += other.Bytes
	s.UnownedEntries += other.UnownedEntries
}

// UnpackLayer applies one cached layer to dest (FR-5.3).
//
// The blob is verified while it is decompressed — the third and last of this
// package's verifications (ADR-0021). Decompression already reads every byte,
// so the check costs one hash pass and no extra I/O, and it is what turns bit
// rot in the cache from a mysterious container failure into a named error.
//
// A blob that fails verification here is quarantined by Remove: bytes that do
// not hash to their own name are definitively wrong, not merely suspect, so
// deleting them is safe and the next run re-downloads them. A layer that fails
// for any other reason — a malformed tar, an unsupported entry — leaves the
// cache alone, because those bytes may be perfectly good and simply beyond what
// Forge implements.
//
// dest must already exist. UnpackLayer never creates or removes it, so a
// partial extraction leaves a directory whose *contents* are incomplete and
// whose ownership is unchanged; BuildRootfs and the caller's cleanup stack
// decide what happens to it.
func (c *Cache) UnpackLayer(ctx context.Context, digest, dest string) (Stats, error) {
	hasher, err := newHasher(digest)
	if err != nil {
		return Stats{}, err
	}

	rc, err := c.Open(digest)
	if err != nil {
		return Stats{}, err
	}
	defer func() {
		if err := rc.Close(); err != nil {
			c.logger.Warn("closing a layer blob", "digest", digest, "error", err)
		}
	}()

	// The hash sees the compressed bytes, because the digest in a manifest
	// names the blob as stored, not as decompressed.
	verified := io.TeeReader(&contextReader{ctx: ctx, r: rc}, hasher)

	stats, unpackErr := applyLayer(ctx, verified, dest, c)

	// The tar reader stops at the end-of-archive marker, which for a gzipped
	// layer is usually before the end of the file. The remaining bytes have to
	// be read for the digest to mean anything.
	if _, err := io.Copy(io.Discard, verified); err != nil && unpackErr == nil {
		return stats, fmt.Errorf("%w: reading the rest of layer %s: %w", ErrCorruptLayer, digest, err)
	}

	if computed := formatDigest(hasher); computed != digest {
		if err := c.Remove(digest); err != nil {
			c.logger.Warn("quarantining a corrupt blob", "digest", digest, "error", err)
		}
		return stats, fmt.Errorf("%w: cached layer %s hashes to %s; it has been removed and will be downloaded again",
			ErrDigestMismatch, digest, computed)
	}

	if unpackErr != nil {
		return stats, fmt.Errorf("unpacking layer %s: %w", digest, unpackErr)
	}

	unownedWarning(c, digest, stats)

	c.logger.Debug("unpacked a layer",
		"digest", digest, "dest", dest,
		"files", stats.Files, "dirs", stats.Dirs, "whiteouts", stats.Whiteouts, "bytes", stats.Bytes)

	return stats, nil
}

// deferredMetadata is a directory's mode and timestamps, applied after the
// whole layer is written. See extractDirPerm.
type deferredMetadata struct {
	path    string
	mode    os.FileMode
	modTime time.Time
}

// applyLayer applies a tar stream, gzipped or not, to dest.
//
// It is separated from UnpackLayer so the extractor can be tested against an
// in-memory tar with no cache, no digest and no temporary blob — which is what
// makes the containment table in unpack_test.go cheap enough to be exhaustive.
func applyLayer(ctx context.Context, r io.Reader, dest string, c *Cache) (Stats, error) {
	var stats Stats

	info, err := os.Stat(dest)
	switch {
	case err != nil:
		return stats, fmt.Errorf("inspecting the destination %q: %w", dest, err)
	case !info.IsDir():
		return stats, fmt.Errorf("the destination %q is not a directory", dest)
	}
	dest = filepath.Clean(dest)

	buffered := bufio.NewReader(r)
	magic, err := buffered.Peek(len(gzipMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		return stats, fmt.Errorf("%w: reading the start of the layer: %w", ErrCorruptLayer, err)
	}

	var source io.Reader = buffered
	if len(magic) == len(gzipMagic) && magic[0] == gzipMagic[0] && magic[1] == gzipMagic[1] {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			return stats, fmt.Errorf("%w: %w", ErrCorruptLayer, err)
		}
		defer func() {
			if err := gz.Close(); err != nil && c != nil {
				c.logger.Debug("closing a gzip reader", "error", err)
			}
		}()
		source = gz
	}

	var deferred []deferredMetadata

	// The budget is applied to the decompressed stream rather than to the files
	// written, so a single enormous member is stopped while it is being read
	// instead of after it has landed on the disk.
	budget := &budgetReader{r: source, remaining: maxLayerBytes}

	entries := 0

	archive := tar.NewReader(budget)
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// An exhausted budget reaches the tar reader as a truncated
			// archive, so it has to be reported before the truncation is.
			if budget.exceeded {
				return stats, fmt.Errorf("%w: the layer expands to more than %d bytes",
					ErrLayerTooLarge, maxLayerBytes)
			}
			// A truncated layer surfaces here, as an unexpected EOF partway
			// through a member.
			return stats, fmt.Errorf("%w: %w", ErrCorruptLayer, err)
		}

		entries++
		if entries > maxLayerEntries {
			return stats, fmt.Errorf("%w: the layer contains more than %d entries",
				ErrLayerTooLarge, maxLayerEntries)
		}

		meta, err := applyEntry(archive, header, dest, &stats, c)
		if err != nil {
			// A budget exhausted part-way through a member surfaces here, as a
			// short read inside the write. It has to be reported as what it is
			// rather than as the truncation it looks like.
			if budget.exceeded {
				return stats, fmt.Errorf("%w: the layer expands to more than %d bytes",
					ErrLayerTooLarge, maxLayerBytes)
			}
			return stats, err
		}
		if meta != nil {
			deferred = append(deferred, *meta)
		}
	}

	// A budget exhausted exactly on a member boundary ends the archive cleanly
	// rather than mid-member, so the loop above never sees an error.
	if budget.exceeded {
		return stats, fmt.Errorf("%w: the layer expands to more than %d bytes",
			ErrLayerTooLarge, maxLayerBytes)
	}

	applyDeferredMetadata(deferred, c)

	return stats, nil
}

// budgetReader stops a reader after a fixed number of bytes and records that it
// did, so an exhausted budget can be told apart from a stream that genuinely
// ended.
//
// It bounds the *decompressed* stream, which is the only place the bound means
// anything: the compressed size is already known from the descriptor and is
// already verified.
type budgetReader struct {
	r         io.Reader
	remaining int64
	exceeded  bool
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		b.exceeded = true
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}

	n, err := b.r.Read(p)
	b.remaining -= int64(n)

	return n, err
}

// applyEntry writes one tar member. It returns directory metadata to be applied
// after the layer is complete, or nil.
func applyEntry(archive *tar.Reader, header *tar.Header, dest string, stats *Stats, c *Cache) (*deferredMetadata, error) {
	name, err := entryPath(header.Name)
	if err != nil {
		return nil, err
	}
	if name == "" {
		// An entry naming the root of the layer, such as "./". There is nothing
		// to create and its mode is not the container's to set.
		return nil, nil
	}

	if base := filepath.Base(name); strings.HasPrefix(base, whiteoutPrefix) {
		if err := applyWhiteout(dest, name, base, stats); err != nil {
			return nil, err
		}
		return nil, nil
	}

	target, err := resolveWithin(dest, name)
	if err != nil {
		return nil, err
	}

	if err := makeParents(dest, name); err != nil {
		return nil, err
	}

	mode := entryMode(header)

	switch header.Typeflag {
	case tar.TypeDir:
		// A directory entry follows an existing symlink rather than replacing
		// it, which is what keeps a merged-/usr layout intact.
		if target, err = resolveDir(dest, name); err != nil {
			return nil, err
		}
		if err := makeDir(target); err != nil {
			return nil, err
		}
		stats.Dirs++
		// Ownership is applied now; the mode and timestamps wait until the
		// layer is complete, so entries can still be written inside.
		applyOwnership(target, header, stats, c)
		return &deferredMetadata{path: target, mode: mode, modTime: header.ModTime}, nil

	case tar.TypeReg:
		written, err := writeFile(archive, target, mode, header.Size)
		if err != nil {
			return nil, err
		}
		stats.Files++
		stats.Bytes += written

	case tar.TypeSymlink:
		// The link's target is written verbatim and never validated. A symlink
		// to "/etc" is legal and common inside an image, and it is harmless
		// because nothing here follows it: resolveWithin rebases an absolute
		// link against dest, exactly as it would resolve for a process that had
		// already pivoted into this filesystem.
		if err := replace(target); err != nil {
			return nil, err
		}
		if err := os.Symlink(header.Linkname, target); err != nil {
			return nil, fmt.Errorf("creating symlink %q: %w", name, err)
		}
		stats.Symlinks++

	case tar.TypeLink:
		linkName, err := entryPath(header.Linkname)
		if err != nil {
			return nil, err
		}
		source, err := resolveWithin(dest, linkName)
		if err != nil {
			return nil, err
		}
		if err := replace(target); err != nil {
			return nil, err
		}
		if err := os.Link(source, target); err != nil {
			return nil, fmt.Errorf("creating hard link %q: %w", name, err)
		}
		stats.Hardlinks++

	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		if err := makeDevice(header, target, mode); err != nil {
			return nil, err
		}
		stats.Devices++

	case tar.TypeXGlobalHeader, tar.TypeXHeader:
		// Metadata for the following entries, already consumed by the tar
		// reader. Nothing to create.
		return nil, nil

	default:
		return nil, fmt.Errorf("%w: %q is a tar entry of type %q",
			ErrUnsupportedMediaType, name, string(header.Typeflag))
	}

	applyOwnership(target, header, stats, c)
	applyTimes(target, header, c)

	return nil, nil
}

// entryPath turns a tar member's name into a path relative to the destination,
// refusing anything that would leave it.
//
// This is the extractor's most consequential branch. Root is writing files
// whose names came from the internet, so an absolute name or one climbing out
// with ".." is refused outright rather than cleaned into something plausible —
// a layer that tried to escape is a layer to reject, not one to reinterpret.
func entryPath(name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: a layer entry name contains a NUL byte", ErrEscapesRoot)
	}

	cleaned := filepath.Clean(filepath.FromSlash(name))

	switch {
	case cleaned == "." || cleaned == string(filepath.Separator):
		return "", nil
	case filepath.IsAbs(cleaned):
		return "", fmt.Errorf("%w: %q is an absolute path", ErrEscapesRoot, name)
	case cleaned == "..", strings.HasPrefix(cleaned, ".."+string(filepath.Separator)):
		return "", fmt.Errorf("%w: %q climbs above the destination", ErrEscapesRoot, name)
	}

	return cleaned, nil
}

// resolveWithin resolves a layer-relative path to the host path it names,
// guaranteeing the result is inside root.
//
// It resolves every component itself, under the same two rules as
// mount.resolveDestination in Stage 2, and for the same reason: a tree Forge
// did not create may contain symlinks, and resolving them the way the host
// would is how a layer's "etc/passwd" ends up writing to the host's /etc.
//
//   - An absolute symlink points at the *layer's* root, not the host's. A link
//     to "/etc" inside the tree means <root>/etc, exactly as it would to a
//     process that had already pivoted. That is why a symlink to a host path
//     cannot be used to reach that path: it is rebased, not followed.
//   - A path that would climb above the root is refused rather than clamped.
//
// The final component is deliberately not resolved. It is the thing being
// created, and a layer that writes over a symlink from a lower layer must
// replace that symlink, not write through it.
func resolveWithin(root, name string) (string, error) {
	return resolvePath(root, name, false)
}

// resolveDir is resolveWithin for a path that is being traversed rather than
// created, so its final component is resolved too.
//
// It is what makes a layer's "etc/passwd" land inside the tree when a lower
// layer left "etc" as a symlink, and what stops a directory entry from
// destroying such a symlink: images built with a merged /usr rely on
// "lib -> usr/lib" surviving a later layer that declares "lib/".
func resolveDir(root, name string) (string, error) {
	return resolvePath(root, name, true)
}

// resolvePath is the shared implementation of resolveWithin and resolveDir.
func resolvePath(root, name string, followLast bool) (string, error) {
	components := strings.Split(name, string(filepath.Separator))

	var current string
	links := 0

	for len(components) > 0 {
		component := components[0]
		components = components[1:]

		switch component {
		case "", ".":
			continue
		case "..":
			if current == "" {
				return "", fmt.Errorf("%w: %q", ErrEscapesRoot, name)
			}
			if current = filepath.Dir(current); current == "." {
				current = ""
			}
			continue
		}

		candidate := filepath.Join(current, component)

		// The last component is the entry itself: unless the caller is
		// traversing it, it is created or replaced rather than followed.
		if len(components) == 0 && !followLast {
			return filepath.Join(root, candidate), nil
		}

		info, err := os.Lstat(filepath.Join(root, candidate))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			current = candidate
			continue
		}

		links++
		if links > maxSymlinkDepth {
			return "", fmt.Errorf("resolving %q: more than %d symlinks traversed, the layer may contain a loop",
				name, maxSymlinkDepth)
		}

		linkTarget, err := os.Readlink(filepath.Join(root, candidate))
		if err != nil {
			return "", fmt.Errorf("reading symlink %q while unpacking: %w", candidate, err)
		}
		if filepath.IsAbs(linkTarget) {
			current = ""
		}
		components = append(strings.Split(filepath.Clean(filepath.FromSlash(linkTarget)),
			string(filepath.Separator)), components...)
	}

	return filepath.Join(root, current), nil
}

// makeParents creates the directories leading to an entry.
//
// A tar is not required to contain an entry for every directory it writes into,
// so the parents are created on demand. The path is resolved first and created
// second: resolveDir rebases any symlink along the way, so what gets created is
// the real location inside the tree rather than wherever a symlink pointed —
// and the symlink itself survives, because it was followed rather than
// replaced.
func makeParents(dest, name string) error {
	parent := filepath.Dir(name)
	if parent == "." || parent == string(filepath.Separator) {
		return nil
	}

	resolved, err := resolveDir(dest, parent)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(resolved, extractDirPerm); err != nil {
		return fmt.Errorf("creating the directories leading to %q: %w", name, err)
	}

	return nil
}

// makeDir creates one directory, tolerating one that is already there and
// replacing anything that is not a directory.
func makeDir(target string) error {
	info, err := os.Lstat(target)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		// A later layer may legitimately replace a file or a symlink with a
		// directory.
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("replacing %q with a directory: %w", target, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspecting %q: %w", target, err)
	}

	if err := os.Mkdir(target, extractDirPerm); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("creating directory %q: %w", target, err)
	}

	return nil
}

// replace removes whatever is at target so a new entry can take its place.
//
// A layer overwriting a path from a lower layer is ordinary, and the entry may
// be a different type than what is there. Removing first is also what stops a
// write from following a symlink a lower layer left behind.
func replace(target string) error {
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("replacing %q: %w", target, err)
	}
	return nil
}

// writeFile writes a regular file's contents from the tar stream.
func writeFile(archive *tar.Reader, target string, mode os.FileMode, size int64) (int64, error) {
	if err := replace(target); err != nil {
		return 0, err
	}

	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode.Perm())
	if err != nil {
		return 0, fmt.Errorf("creating %q: %w", target, err)
	}

	written, copyErr := io.Copy(f, io.LimitReader(archive, size))
	closeErr := f.Close()

	switch {
	case copyErr != nil:
		return written, fmt.Errorf("%w: writing %q: %w", ErrCorruptLayer, target, copyErr)
	case closeErr != nil:
		return written, fmt.Errorf("closing %q: %w", target, closeErr)
	case written != size:
		return written, fmt.Errorf("%w: %q ended after %d of %d bytes",
			ErrCorruptLayer, target, written, size)
	}

	// The mode is set explicitly because OpenFile's is subject to the umask,
	// and because setuid and sticky bits do not survive it.
	if err := os.Chmod(target, mode); err != nil {
		return written, fmt.Errorf("setting the mode of %q: %w", target, err)
	}

	return written, nil
}

// makeDevice creates a character device, block device or FIFO.
//
// This needs CAP_MKNOD, which means root. It fails rather than skipping,
// because a rootfs missing the device nodes its image declared is a rootfs that
// will fail later, somewhere less obvious.
func makeDevice(header *tar.Header, target string, mode os.FileMode) error {
	if err := replace(target); err != nil {
		return err
	}

	var kind uint32
	switch header.Typeflag {
	case tar.TypeChar:
		kind = unix.S_IFCHR
	case tar.TypeBlock:
		kind = unix.S_IFBLK
	default:
		kind = unix.S_IFIFO
	}

	device := int(unix.Mkdev(uint32(header.Devmajor), uint32(header.Devminor)))
	if err := unix.Mknod(target, kind|uint32(mode.Perm()), device); err != nil {
		if errors.Is(err, unix.EPERM) {
			return fmt.Errorf("creating device node %q: %w (unpacking an image with device nodes needs root)",
				target, err)
		}
		return fmt.Errorf("creating device node %q: %w", target, err)
	}

	return os.Chmod(target, mode)
}

// entryMode returns the permission and setuid/setgid/sticky bits of an entry.
func entryMode(header *tar.Header) os.FileMode {
	mode := header.FileInfo().Mode()
	return mode.Perm() | (mode & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky))
}

// applyWhiteout executes a deletion marker (ADR-0003).
//
// Both forms act on the tree built so far, at the moment the marker is read, so
// ordering within a layer is preserved: a layer that deletes a file and then
// recreates it ends with the file present.
func applyWhiteout(dest, name, base string, stats *Stats) error {
	parent := filepath.Dir(name)

	if base == whiteoutOpaque {
		dir, err := resolveWithin(dest, parent)
		if err != nil {
			return err
		}
		if err := clearDirectory(dir); err != nil {
			return err
		}
		stats.Whiteouts++
		return nil
	}

	// A whiteout has to name something. Without this check ".wh." trims to the
	// empty string and ".wh.." trims to ".", and joining either onto the parent
	// yields the parent itself — so the RemoveAll below would delete the
	// directory the marker sits in, and at the root of a layer that is the
	// whole destination. It would do it silently, returning nil, which is worse
	// than the deletion: a later layer recreating the tree would leave an
	// extraction that looks successful and has quietly dropped everything
	// beneath it.
	deletedName := strings.TrimPrefix(base, whiteoutPrefix)
	switch deletedName {
	case "", ".", "..":
		return fmt.Errorf("%w: %q in %q is a whiteout that names no entry",
			ErrCorruptLayer, base, name)
	}

	deleted := filepath.Join(parent, deletedName)
	target, err := resolveWithin(dest, deleted)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("applying whiteout %q: %w", name, err)
	}

	stats.Whiteouts++

	return nil
}

// clearDirectory empties a directory without removing it, which is what an
// opaque whiteout means: the directory exists in this layer, and nothing the
// lower layers put in it does.
func clearDirectory(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The lower layers never created it, so there is nothing to hide.
			return nil
		}
		return fmt.Errorf("reading %q to apply an opaque whiteout: %w", dir, err)
	}

	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("applying an opaque whiteout in %q: %w", dir, err)
		}
	}

	return nil
}

// applyOwnership sets an entry's uid and gid.
//
// Without root this cannot be done, and the tree is then not the one the image
// describes: every file belongs to whoever ran forge. That is a legitimate mode
// for inspecting an image and a broken one for running a container, so it is
// counted in Stats and logged rather than either failing the unpack or passing
// unremarked (SSOT §13.7).
func applyOwnership(target string, header *tar.Header, stats *Stats, c *Cache) {
	if err := os.Lchown(target, header.Uid, header.Gid); err != nil {
		if errors.Is(err, os.ErrPermission) {
			stats.UnownedEntries++
			return
		}
		if c != nil {
			c.logger.Warn("setting the owner of an unpacked entry",
				"path", target, "uid", header.Uid, "gid", header.Gid, "error", err)
		}
	}
}

// applyTimes restores an entry's modification time.
//
// It does not follow symlinks, for the same reason applyOwnership uses Lchown
// rather than Chown. os.Chtimes resolves its path, and the entry it is called
// on is very often a symlink the layer has just created — an image the size of
// alpine is several hundred links into busybox alone. Following them is wrong
// twice over:
//
//   - The link's own timestamp is never set, and whatever it points at has its
//     timestamp overwritten instead.
//   - The link points into the *layer's* namespace, not the host's, and nothing
//     has pivoted yet. An absolute link to "/bin/busybox" resolves against the
//     host while this runs as root, so following it writes to the host's tree —
//     the one escape resolveWithin exists to prevent, arriving through the
//     metadata pass instead of through the path. Links whose targets do not
//     exist on the host merely fail loudly, which is what made this visible.
//
// It is otherwise best effort: a wrong timestamp is cosmetic, and failing an
// extraction over one would trade a working container for a tidy `ls -l`.
func applyTimes(target string, header *tar.Header, c *Cache) {
	if header.ModTime.IsZero() {
		return
	}
	if err := lutimes(target, header.ModTime); err != nil && c != nil {
		c.logger.Debug("setting the timestamps of an unpacked entry", "path", target, "error", err)
	}
}

// lutimes sets a path's access and modification times without following a
// symlink at the final component.
func lutimes(target string, when time.Time) error {
	times := []unix.Timespec{
		unix.NsecToTimespec(when.UnixNano()),
		unix.NsecToTimespec(when.UnixNano()),
	}

	if err := unix.UtimesNanoAt(unix.AT_FDCWD, target, times, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "lutimes", Path: target, Err: err}
	}

	return nil
}

// applyDeferredMetadata applies directory modes and timestamps once the layer
// is fully written.
//
// Deepest first, so that tightening a parent's permissions never prevents
// tightening a child's.
func applyDeferredMetadata(deferred []deferredMetadata, c *Cache) {
	for i := len(deferred) - 1; i >= 0; i-- {
		meta := deferred[i]
		if err := os.Chmod(meta.path, meta.mode); err != nil && c != nil {
			c.logger.Warn("setting the mode of an unpacked directory", "path", meta.path, "error", err)
		}
		if meta.modTime.IsZero() {
			continue
		}
		if err := lutimes(meta.path, meta.modTime); err != nil && c != nil {
			c.logger.Debug("setting the timestamps of an unpacked directory", "path", meta.path, "error", err)
		}
	}
}

// unownedWarning logs once per layer when ownership could not be applied.
func unownedWarning(c *Cache, digest string, stats Stats) {
	if stats.UnownedEntries == 0 || c == nil {
		return
	}
	c.logger.Warn("unpacked a layer without applying file ownership",
		"digest", digest, "entries", stats.UnownedEntries,
		"reason", "forge is not running as root; every unpacked file belongs to the current user")
}
