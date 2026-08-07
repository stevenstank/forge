package image

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// The cache's layout on disk (ADR-0021).
//
//	<root>/                       0700
//	├── blobs/
//	│   └── sha256/
//	│       └── <64 hex chars>    every blob: manifests, configs, layers
//	└── staging/
//	    └── <random>.part         downloads in progress
const (
	blobsDirName   = "blobs"
	stagingDirName = "staging"

	// stagingPattern names an in-progress download. The random middle is what
	// makes two concurrent downloads of the same blob independent, and the
	// ".part" suffix makes a stray file obvious to a human looking at the
	// directory.
	stagingPattern = "*.part"
)

// Cache permissions. A layer can contain a setuid binary, so the cache is
// root's and nobody else's.
const (
	cacheDirPerm = 0o700
	blobPerm     = 0o600
)

// defaultStagingMaxAge is how old an abandoned download must be before Pull
// collects it.
//
// The bound only has to exceed the longest plausible download: anything shorter
// risks deleting a live one belonging to a concurrent run, which is the single
// way this design could corrupt a pull that was going to succeed.
const defaultStagingMaxAge = 24 * time.Hour

// Cache is a content-addressed blob store (FR-5.4).
//
// A blob's name is its digest, so presence in the cache is the whole of the
// cache-hit test — there is no index to consult and none to keep consistent.
// Bytes become visible under blobs/ only complete and verified, so a file being
// there means it is correct, and a reader never needs to coordinate with a
// writer.
//
// A Cache is safe for concurrent use by multiple goroutines and, deliberately,
// by multiple processes: the commit is a link(2), which is atomic and never
// replaces. There are no lock files, so a SIGKILLed forge cannot leave a lock
// behind for the next one to trip over.
type Cache struct {
	root   string
	logger *slog.Logger
}

// NewCache returns a Cache rooted at root.
//
// It performs no I/O — not even creating the root. That differs from
// rootfs.NewStore, which creates its root eagerly, and the difference is
// deliberate: a `forge run -rootfs …` invocation should not leave behind an
// image cache it will never put anything in. The directories are created on the
// first write, a few milliseconds before they are first needed.
//
// root must be absolute. Every later operation joins onto it, so a relative
// path would silently depend on the working directory forge was started in.
func NewCache(root string, logger *slog.Logger) (*Cache, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("image cache root must be an absolute path, got %q", root)
	}

	return &Cache{root: filepath.Clean(root), logger: logger}, nil
}

// Root returns the directory the cache manages.
func (c *Cache) Root() string { return c.root }

// Path returns where a blob lives, without touching the filesystem.
//
// The digest is validated first, and that check is load-bearing rather than
// cosmetic: the digest becomes a path component, so a value containing a
// separator or ".." would name a file outside the cache. parseDigest permits
// only lowercase hex of a fixed length, which cannot express either.
func (c *Cache) Path(digest string) (string, error) {
	algorithm, hex, err := parseDigest(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(c.root, blobsDirName, algorithm, hex), nil
}

// Has reports whether a blob is already cached (FR-5.4).
//
// It is a stat, and it is sufficient, because a file only ever appears under
// blobs/ complete and verified (ADR-0021).
func (c *Cache) Has(digest string) (bool, error) {
	path, err := c.Path(digest)
	if err != nil {
		return false, err
	}

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("looking for blob %s in the cache: %w", digest, err)
	case !info.Mode().IsRegular():
		// Something that is not a file where a blob should be is corruption of
		// a kind this package will not guess about.
		return false, fmt.Errorf("%w: %q is not a regular file", ErrDigestMismatch, path)
	}

	return true, nil
}

// Open returns a cached blob's contents.
//
// It does not verify. Callers that are about to consume every byte anyway —
// UnpackLayer is the one that matters — verify as they read, which costs one
// hash pass and no extra I/O. Verify exists for callers that want the check on
// its own.
func (c *Cache) Open(digest string) (io.ReadCloser, error) {
	path, err := c.Path(digest)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, digest)
		}
		return nil, fmt.Errorf("opening blob %s: %w", digest, err)
	}

	return f, nil
}

// ReadAll returns a cached blob's contents, verifying them (FR-5.2).
//
// It is for the small blobs — manifests and image configs — where buffering is
// free and the verification is therefore worth doing eagerly. A layer must not
// go through here.
func (c *Cache) ReadAll(digest string) ([]byte, error) {
	rc, err := c.Open(digest)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rc.Close(); err != nil {
			c.logger.Warn("closing a blob after reading it", "digest", digest, "error", err)
		}
	}()

	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("reading blob %s: %w", digest, err)
	}

	if computed := digestBytes(b); computed != digest {
		return nil, fmt.Errorf("%w: cached blob %s hashes to %s", ErrDigestMismatch, digest, computed)
	}

	return b, nil
}

// Verify re-reads a cached blob and checks it against its own name.
//
// It is the answer to "is this cache still good", and it is what the corrupt-
// cache path uses before deciding a blob is definitively wrong rather than
// merely suspect.
func (c *Cache) Verify(ctx context.Context, digest string) error {
	hasher, err := newHasher(digest)
	if err != nil {
		return err
	}

	rc, err := c.Open(digest)
	if err != nil {
		return err
	}
	defer func() {
		if err := rc.Close(); err != nil {
			c.logger.Warn("closing a blob after verifying it", "digest", digest, "error", err)
		}
	}()

	if _, err := io.Copy(hasher, &contextReader{ctx: ctx, r: rc}); err != nil {
		return fmt.Errorf("reading blob %s to verify it: %w", digest, err)
	}

	if computed := formatDigest(hasher); computed != digest {
		return fmt.Errorf("%w: cached blob %s hashes to %s", ErrDigestMismatch, digest, computed)
	}

	return nil
}

// Staging is a download in progress.
//
// It is an io.Writer, so Client.FetchBlob can stream into it without knowing
// that a cache exists. Its contents are never visible under blobs/ and are
// never read by anything but the download that created it, which is why an
// interrupted download cannot produce a corrupt cache entry: there is no path
// by which a partial file acquires a blob's name.
//
// A Staging belongs to one goroutine. Concurrency between *downloads* is safe
// because each has its own.
type Staging struct {
	file    *os.File
	cache   *Cache
	hasher  hash.Hash
	written int64
	closed  bool
}

// Stage begins a download, creating the cache directories if this is the first
// one.
//
// Everything that can fail without a file on disk is done before the file is
// created. A staging file is only ever released by the Discard its caller
// defers, so any error returned *after* CreateTemp and *before* the Staging
// exists leaks a .part nothing will collect for a day — there is no owner to
// defer anything. Ordering the work this way removes that window rather than
// adding a second cleanup branch to it.
func (c *Cache) Stage() (*Staging, error) {
	hasher, err := newHasher(digestAlgorithm + ":" + zeroHex)
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(c.root, stagingDirName)
	if err := c.ensureDir(dir); err != nil {
		return nil, err
	}

	f, err := os.CreateTemp(dir, stagingPattern)
	if err != nil {
		return nil, fmt.Errorf("creating a staging file in %q: %w", dir, err)
	}
	if err := f.Chmod(blobPerm); err != nil {
		// The file is unusable if its permissions are not what the cache
		// promises, and leaving it behind would be a leak.
		if rmErr := os.Remove(f.Name()); rmErr != nil {
			c.logger.Warn("removing a staging file after it could not be secured",
				"path", f.Name(), "error", rmErr)
		}
		return nil, fmt.Errorf("setting permissions on %q: %w", f.Name(), err)
	}

	return &Staging{file: f, cache: c, hasher: hasher}, nil
}

// ensureDir creates a cache directory with the permissions the cache promises.
func (c *Cache) ensureDir(dir string) error {
	if err := os.MkdirAll(dir, cacheDirPerm); err != nil {
		return fmt.Errorf("creating cache directory %q: %w", dir, err)
	}
	// MkdirAll is subject to the process umask, and these permissions are a
	// decision rather than a default.
	if err := os.Chmod(dir, cacheDirPerm); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", dir, err)
	}
	return nil
}

// Name returns the staging file's path, for log lines and error messages.
func (s *Staging) Name() string { return s.file.Name() }

// Written returns how many bytes have been staged. It stays valid after Commit,
// which is how a caller reports what a download cost.
func (s *Staging) Written() int64 { return s.written }

// Write implements io.Writer, hashing as it goes so Commit's verification costs
// no second pass over the data.
func (s *Staging) Write(p []byte) (int, error) {
	if s.closed {
		return 0, fmt.Errorf("%w: %s", ErrStagingCommitted, s.file.Name())
	}

	n, err := s.file.Write(p)
	s.written += int64(n)
	if n > 0 {
		// The hash must see exactly the bytes that reached the file, so it is
		// fed n and not len(p).
		s.hasher.Write(p[:n])
	}
	if err != nil {
		return n, fmt.Errorf("writing to %q: %w", s.file.Name(), err)
	}

	return n, nil
}

// Commit verifies the staged bytes and publishes them as a blob (FR-5.2).
//
// This is the second of the three verifications: FetchBlob checked the bytes on
// the wire, and this checks what actually reached the disk. Neither is trusted
// to cover the other's window.
//
// Publication is a link(2), which is atomic and never replaces an existing
// file. EEXIST therefore means another process won a race to cache the same
// blob, and that is success, not failure: both copies passed verification
// against the same digest, so they are byte-identical, and the loser simply
// uses the winner's. This is what makes concurrent pulls safe without a lock —
// and a lock is what a SIGKILLed forge would leak.
func (s *Staging) Commit(digest string) error {
	if s.closed {
		return fmt.Errorf("%w: %s", ErrStagingCommitted, s.file.Name())
	}

	blob, err := s.cache.Path(digest)
	if err != nil {
		return err
	}

	if computed := formatDigest(s.hasher); computed != digest {
		return fmt.Errorf("%w: %d staged bytes hash to %s, but were to be committed as %s",
			ErrDigestMismatch, s.written, computed, digest)
	}

	// The data must be on disk before another process can find the name: a
	// crash between the link and the flush would otherwise leave a
	// correctly-named blob with unwritten contents.
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("flushing %q: %w", s.file.Name(), err)
	}
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", s.file.Name(), err)
	}
	// From here the file is closed, so Discard must not close it again.
	s.closed = true

	if err := s.cache.ensureDir(filepath.Dir(blob)); err != nil {
		return err
	}

	staged := s.file.Name()
	if err := os.Link(staged, blob); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("publishing blob %s: %w", digest, err)
	}

	if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		// The blob is published; a leftover staging file is untidy, not
		// incorrect, and PruneStaging will collect it.
		s.cache.logger.Warn("removing a staging file after committing it",
			"path", staged, "digest", digest, "error", err)
	}

	s.cache.logger.Debug("cached a blob", "digest", digest, "bytes", s.written)

	return nil
}

// zeroHex is a placeholder digest body used only to select a hash algorithm
// before the real digest is known.
const zeroHex = "0000000000000000000000000000000000000000000000000000000000000000"

// contextReader makes a plain io.Reader cancellable, so a long read over a
// large blob answers a cancelled context promptly instead of at EOF.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
