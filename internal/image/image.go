// Package image turns an OCI image reference into a populated directory.
//
// It owns everything between the two: parsing the reference, speaking the OCI
// Distribution Spec to a registry, verifying every byte against the digest that
// named it, caching those bytes on disk, and applying the image's layers to a
// destination directory (FR-5.1 … FR-5.4).
//
// Per SSOT §2 and ADR-0020 it is a leaf package. It imports no other Forge
// package, it knows nothing about containers, and it never decides *which*
// image to pull or *where* the rootfs goes — it is handed a reference and a
// destination. internal/runtime sequences it against internal/rootfs.
//
// # The two halves
//
// The network half — reference.go, registry.go, manifest.go, blob.go — speaks
// HTTP and knows names. The disk half — cache.go, unpack.go, rootfs.go,
// cleanup.go — speaks tar and knows paths. They share exactly one type,
// Descriptor, and one concept, the digest. ADR-0020 records why that is one
// package rather than two.
//
// # Digests are the whole trust model
//
// Every document and every blob is named by the SHA-256 of its own bytes, so
// verification is possible at every boundary the bytes cross, and Forge
// verifies at all of them (ADR-0021): in flight as the registry streams them,
// again on the write to the cache, and again when a layer is decompressed for
// use. The disk between ingest and use is not trusted. A verification you did
// not perform yourself is a verification you are taking on someone else's word.
//
// This is integrity, not authenticity. It proves the bytes are the bytes that
// digest names; it does not prove who made them. Signature and provenance
// verification (cosign, Notary) is out of scope.
//
// # What is deliberately not here
//
// Authenticated pulls (the anonymous Bearer flow only, which is what public
// Docker Hub, GHCR and Quay serve), pushing, image building, `docker save`
// tarballs, zstd-compressed layers (no stdlib decompressor — refused by media
// type with a clear error), foreign and encrypted layers, and cache garbage
// collection. The cache grows; `rm -rf <cache root>` is a safe, complete reset.
package image

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
)

// Sentinel errors callers may branch on.
var (
	// ErrInvalidReference reports an image reference that is not well formed.
	ErrInvalidReference = errors.New("invalid image reference")

	// ErrInvalidDigest reports a digest string Forge cannot parse or does not
	// implement the algorithm for.
	ErrInvalidDigest = errors.New("invalid digest")

	// ErrDigestMismatch reports content whose bytes do not hash to the digest
	// that named them. It is the failure this package exists to make impossible
	// to miss, and it is never recoverable by retrying the same bytes.
	ErrDigestMismatch = errors.New("digest mismatch")

	// ErrNotFound reports a manifest or blob the registry does not have.
	ErrNotFound = errors.New("not found in registry")

	// ErrUnauthorized reports content that anonymous access cannot reach.
	ErrUnauthorized = errors.New("registry denied anonymous access")

	// ErrRegistryUnavailable reports a registry that could not be reached, or
	// that failed in a way retrying did not fix.
	ErrRegistryUnavailable = errors.New("registry unavailable")

	// ErrUnsupportedMediaType reports a document or layer in a format Forge
	// does not implement. The bytes may be perfectly good; Forge cannot read
	// them. Distinguishing this from ErrDigestMismatch is what keeps the
	// quarantine rule in cleanup.go honest.
	ErrUnsupportedMediaType = errors.New("unsupported media type")

	// ErrNoMatchingPlatform reports an index with no manifest for the platform
	// asked for.
	ErrNoMatchingPlatform = errors.New("no image for this platform")

	// ErrManifestTooLarge reports a manifest above the size cap. A registry
	// Forge does not control must not be able to make it buffer a gigabyte of
	// "manifest".
	ErrManifestTooLarge = errors.New("manifest exceeds the size limit")

	// ErrBlobNotFound reports a digest that is not in the cache.
	ErrBlobNotFound = errors.New("blob not in cache")

	// ErrCorruptLayer reports a layer whose bytes are unreadable as a tar
	// stream, or which ended early.
	ErrCorruptLayer = errors.New("corrupt layer")

	// ErrEscapesRoot reports a tar member whose path would be written outside
	// the destination directory. Extraction runs as root over paths that came
	// from the internet; this is the extractor's most consequential branch.
	ErrEscapesRoot = errors.New("layer entry escapes the destination directory")

	// ErrStagingCommitted reports a write to a staging file that has already
	// been committed or discarded.
	ErrStagingCommitted = errors.New("staging file is no longer open")

	// ErrLayerTooLarge reports a layer that expands beyond what Forge will
	// write: too many bytes, or too many entries. The blob is not corrupt and
	// is not quarantined — a decompression bomb hashes to its own digest
	// perfectly well, which is exactly why the digest cannot be the thing that
	// catches it.
	ErrLayerTooLarge = errors.New("layer expands beyond the extraction limits")

	// ErrDestinationNotEmpty reports a root filesystem being built into a
	// directory that already has contents. BuildRootfs empties its destination
	// when a layer fails, and it must only ever do that to a directory whose
	// entire contents it wrote.
	ErrDestinationNotEmpty = errors.New("destination directory is not empty")
)

// digestAlgorithm is the only algorithm Forge implements.
//
// The OCI spec allows others, and a blob's path includes the algorithm so a
// future one cannot collide with this one (ADR-0021). Accepting an algorithm
// Forge cannot compute would mean storing content it cannot verify, so an
// unknown algorithm is an error rather than a skipped check.
const digestAlgorithm = "sha256"

// digestHexLength is the length of a SHA-256 digest in lowercase hex.
const digestHexLength = sha256.Size * 2

// parseDigest splits a digest into its algorithm and hex halves, rejecting
// anything Forge cannot verify.
//
// The validation is strict on purpose. A digest is used as a path component in
// the blob cache, so a value containing a separator or "." would name a file
// outside it — the same reasoning as rootfs.validateID, and the reason the hex
// half is checked character by character rather than with a length test alone.
func parseDigest(digest string) (algorithm, hex string, err error) {
	algorithm, hex, found := strings.Cut(digest, ":")
	switch {
	case !found:
		return "", "", fmt.Errorf("%w: %q has no algorithm prefix", ErrInvalidDigest, digest)
	case algorithm != digestAlgorithm:
		return "", "", fmt.Errorf("%w: %q uses %q, and Forge implements only %s",
			ErrInvalidDigest, digest, algorithm, digestAlgorithm)
	case len(hex) != digestHexLength:
		return "", "", fmt.Errorf("%w: %q has %d hex characters, want %d",
			ErrInvalidDigest, digest, len(hex), digestHexLength)
	}

	for i := 0; i < len(hex); i++ {
		c := hex[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", "", fmt.Errorf("%w: %q is not lowercase hexadecimal", ErrInvalidDigest, digest)
		}
	}

	return algorithm, hex, nil
}

// validateDigest reports whether digest is one Forge can verify against.
func validateDigest(digest string) error {
	_, _, err := parseDigest(digest)
	return err
}

// newHasher returns a hash for the algorithm named in digest.
func newHasher(digest string) (hash.Hash, error) {
	if err := validateDigest(digest); err != nil {
		return nil, err
	}
	return sha256.New(), nil
}

// formatDigest renders a hash's sum in the "algorithm:hex" form.
func formatDigest(h hash.Hash) string {
	return digestAlgorithm + ":" + hex.EncodeToString(h.Sum(nil))
}

// digestBytes returns the digest naming b.
func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return digestAlgorithm + ":" + hex.EncodeToString(sum[:])
}
