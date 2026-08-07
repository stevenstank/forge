package image

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
)

// Media types Forge reads. Both the OCI names and Docker's older equivalents
// appear because Docker Hub still serves the Docker ones for many images, and
// an image that Docker can run is an image Forge should be able to run.
const (
	mediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// Layer media types. The gzip and uncompressed forms are supported; the rest
// are named so they can be refused with an explanation rather than a parse
// failure fifty lines later.
const (
	mediaTypeOCILayerGzip    = "application/vnd.oci.image.layer.v1.tar+gzip"
	mediaTypeOCILayer        = "application/vnd.oci.image.layer.v1.tar"
	mediaTypeDockerLayerGzip = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	mediaTypeDockerLayer     = "application/vnd.docker.image.rootfs.diff.tar"

	mediaTypeOCILayerZstd  = "application/vnd.oci.image.layer.v1.tar+zstd"
	mediaTypeForeignLayer  = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
	mediaTypeOCINondistrib = "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip"
)

// manifestAccept is the Accept header sent with a manifest request. Listing
// every form Forge can read is what makes a registry serve the OCI variant when
// it has one and fall back to Docker's when it does not.
var manifestAccept = []string{
	mediaTypeOCIIndex,
	mediaTypeOCIManifest,
	mediaTypeDockerList,
	mediaTypeDockerManifest,
}

// Descriptor points at content by digest. It is the one type both halves of
// this package share (ADR-0020).
type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`

	// Platform is set only on the entries of an index.
	Platform *Platform `json:"platform,omitempty"`
}

// Platform is an OS and architecture an image was built for.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// HostPlatform returns the platform of the machine Forge is running on.
//
// The arm variant matters: arm64 images are commonly published as
// linux/arm64/v8, and an index entry for plain linux/arm64 and one for v8 must
// both match a host that can run either.
func HostPlatform() Platform {
	p := Platform{OS: "linux", Architecture: runtime.GOARCH}
	if p.Architecture == "arm64" {
		p.Variant = "v8"
	}
	return p
}

// String renders a platform in the conventional os/arch[/variant] form.
func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Matches reports whether an image built for other can run on the host
// platform.
//
// Variant matching is asymmetric on purpose: an image that names no variant
// runs on a host that has one, but an image that names a variant the host does
// not have does not run. Being liberal in the first direction is what makes
// plain linux/arm64 images work on a v8 host, which is nearly all of them.
//
// It is exported because a platform is matched in two places against two
// sources — the descriptors in an index, and the image config itself — and the
// second of those is the caller's to check. Which platform is acceptable is a
// policy decision, and per SSOT §2 policy lives in internal/runtime.
func (host Platform) Matches(other Platform) bool {
	if other.OS != host.OS || other.Architecture != host.Architecture {
		return false
	}
	return other.Variant == "" || other.Variant == host.Variant
}

// IsZero reports whether a platform declares nothing at all.
//
// A config with neither field set is unusual rather than wrong, and refusing it
// would invent a requirement the image spec does not state. Callers use this to
// tell "declares a platform that does not match" from "declares nothing".
func (p Platform) IsZero() bool {
	return p.OS == "" && p.Architecture == ""
}

// Manifest is a single-platform image: the config blob, and the layers that
// build its filesystem.
type Manifest struct {
	// Digest is the manifest's own digest, verified on arrival. It is what
	// makes a tagged pull reproducible after the fact — the caller can record
	// it and ask for exactly these bytes again.
	Digest string

	// Config describes the image config blob.
	Config Descriptor

	// Layers are in application order, base layer first.
	Layers []Descriptor
}

// manifestDocument is the wire form of a manifest.
type manifestDocument struct {
	MediaType string       `json:"mediaType"`
	Config    Descriptor   `json:"config"`
	Layers    []Descriptor `json:"layers"`
}

// indexDocument is the wire form of an index, or of Docker's manifest list.
type indexDocument struct {
	MediaType string       `json:"mediaType"`
	Manifests []Descriptor `json:"manifests"`
}

// Resolve turns a reference into a single-platform manifest (FR-5.1, FR-5.2).
//
// It follows an index to the manifest matching p, and verifies every document
// it reads against the digest that named it, before parsing it. That order is
// the point: a manifest is the root of trust for every layer beneath it, so
// acting on an unverified one would make every later verification circular.
//
// A tag is resolved to an immutable digest here and nowhere else. From this
// point on the pull is expressed entirely in digests, so a tag that moves
// mid-pull cannot produce a rootfs assembled from two different images.
func (c *Client) Resolve(ctx context.Context, ref Reference, p Platform) (Manifest, error) {
	target := ref.Target()

	for depth := 0; depth <= maxIndexDepth; depth++ {
		body, digest, err := c.fetchManifest(ctx, ref, target)
		if err != nil {
			return Manifest{}, err
		}

		// Only the media type is read before the type is known, which is safe:
		// the bytes have already been verified against their digest.
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			return Manifest{}, fmt.Errorf("parsing the manifest for %s: %w", ref, err)
		}

		switch probe.MediaType {
		case mediaTypeOCIIndex, mediaTypeDockerList:
			selected, err := selectPlatform(body, p, ref)
			if err != nil {
				return Manifest{}, err
			}
			target = selected
			continue

		case mediaTypeOCIManifest, mediaTypeDockerManifest, "":
			// An empty mediaType is legal in older manifests, which are only
			// ever single-platform, so it is read as one.
			return parseManifest(body, digest, ref)

		default:
			return Manifest{}, fmt.Errorf("%w: %s is a %q, which forge cannot read",
				ErrUnsupportedMediaType, ref, probe.MediaType)
		}
	}

	return Manifest{}, fmt.Errorf("%w: %s: more than %d levels of index indirection",
		ErrUnsupportedMediaType, ref, maxIndexDepth)
}

// fetchManifest downloads one manifest document and verifies it (FR-5.2).
//
// Verification differs by how the document was addressed, and both cases are
// covered:
//
//   - Addressed by digest, the computed hash must equal the digest asked for.
//     Anything else means the registry served different content than requested.
//   - Addressed by tag, the computed hash is compared against the registry's
//     Docker-Content-Digest header when it sends one. A tag names mutable
//     content, so there is no digest to check *against* — the header is the
//     registry's own claim about what it sent, and a mismatch means the body
//     was altered between the registry and Forge.
//
// Either way the computed digest is what is returned and used from then on.
func (c *Client) fetchManifest(ctx context.Context, ref Reference, target string) ([]byte, string, error) {
	rawURL := c.endpoint(ref, "manifests", target)

	resp, err := c.get(ctx, ref, rawURL, manifestAccept)
	if err != nil {
		return nil, "", err
	}
	defer drain(resp, c.logger)

	// One byte over the cap is read so the difference between "at the limit"
	// and "over it" is detectable.
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxManifestBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("%w: reading the manifest for %s: %w", ErrRegistryUnavailable, ref, err)
	}
	if int64(len(body)) > c.maxManifestBytes {
		return nil, "", fmt.Errorf("%w: %s sent more than %d bytes for %s",
			ErrManifestTooLarge, ref.Host(), c.maxManifestBytes, target)
	}

	computed := digestBytes(body)

	expected := resp.Header.Get("Docker-Content-Digest")
	if isDigestReference(target) {
		expected = target
	}
	if expected != "" && expected != computed {
		return nil, "", fmt.Errorf("%w: the manifest for %s hashes to %s, but %s named it %s",
			ErrDigestMismatch, ref, computed, ref.Host(), expected)
	}

	return body, computed, nil
}

// isDigestReference reports whether a manifest target is a digest rather than
// a tag.
func isDigestReference(target string) bool {
	return validateDigest(target) == nil
}

// selectPlatform picks the manifest in an index that runs on p (FR-5.1).
func selectPlatform(body []byte, p Platform, ref Reference) (string, error) {
	var index indexDocument
	if err := json.Unmarshal(body, &index); err != nil {
		return "", fmt.Errorf("parsing the image index for %s: %w", ref, err)
	}

	var offered []string
	for _, entry := range index.Manifests {
		if entry.Platform == nil {
			continue
		}
		// Attestation and SBOM manifests are published as unknown/unknown and
		// are not runnable images. Skipping them by name is more honest than
		// relying on them never matching the host.
		if entry.Platform.OS == "unknown" || entry.Platform.Architecture == "unknown" {
			continue
		}

		offered = append(offered, entry.Platform.String())

		if p.Matches(*entry.Platform) {
			if err := validateDigest(entry.Digest); err != nil {
				return "", fmt.Errorf("the index for %s names a manifest with an unusable digest: %w", ref, err)
			}
			return entry.Digest, nil
		}
	}

	if len(offered) == 0 {
		return "", fmt.Errorf("%w: the index for %s lists no runnable image", ErrNoMatchingPlatform, ref)
	}
	return "", fmt.Errorf("%w: %s is not published for %s; the index offers %s",
		ErrNoMatchingPlatform, ref, p, strings.Join(offered, ", "))
}

// parseManifest reads a verified single-platform manifest, refusing layer
// formats Forge cannot unpack.
//
// The refusal happens here rather than at unpack time so a run fails before it
// downloads several hundred megabytes it will not be able to use.
func parseManifest(body []byte, digest string, ref Reference) (Manifest, error) {
	var doc manifestDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return Manifest{}, fmt.Errorf("parsing the manifest for %s: %w", ref, err)
	}

	if err := validateDigest(doc.Config.Digest); err != nil {
		return Manifest{}, fmt.Errorf("the manifest for %s names an unusable config digest: %w", ref, err)
	}
	if len(doc.Layers) == 0 {
		return Manifest{}, fmt.Errorf("%w: the manifest for %s has no layers", ErrUnsupportedMediaType, ref)
	}

	for i, layer := range doc.Layers {
		if err := validateDigest(layer.Digest); err != nil {
			return Manifest{}, fmt.Errorf("layer %d of %s has an unusable digest: %w", i, ref, err)
		}
		if err := checkLayerMediaType(layer.MediaType); err != nil {
			return Manifest{}, fmt.Errorf("layer %d of %s: %w", i, ref, err)
		}
	}

	return Manifest{Digest: digest, Config: doc.Config, Layers: doc.Layers}, nil
}

// checkLayerMediaType reports whether a layer is in a format Forge can unpack.
func checkLayerMediaType(mediaType string) error {
	switch mediaType {
	case mediaTypeOCILayerGzip, mediaTypeOCILayer, mediaTypeDockerLayerGzip, mediaTypeDockerLayer, "":
		return nil
	case mediaTypeOCILayerZstd:
		return fmt.Errorf("%w: %s (the standard library has no zstd decompressor)",
			ErrUnsupportedMediaType, mediaType)
	case mediaTypeForeignLayer, mediaTypeOCINondistrib:
		return fmt.Errorf("%w: %s (this layer is hosted outside the registry)",
			ErrUnsupportedMediaType, mediaType)
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedMediaType, mediaType)
	}
}

// Config is the part of an OCI image config Forge acts on.
//
// The spec's config carries more — User, Volumes, ExposedPorts, StopSignal,
// healthchecks — and Forge reads none of it. That is a decision, not an
// oversight: each of those needs a mechanism Forge does not have yet, and
// silently ignoring a field the user set is worse than not claiming to support
// it.
type Config struct {
	Env        []string
	Cmd        []string
	Entrypoint []string
	WorkingDir string

	// Platform is the OS and architecture the image declares it was built for.
	//
	// It is the only statement of platform a single-platform manifest carries:
	// a descriptor's Platform field exists on the entries of an index and
	// nowhere else, so a tag pointing straight at a manifest has nothing else
	// to check. Callers that care must check this, and internal/runtime does.
	// The zero value means the config declared nothing.
	Platform Platform
}

// configDocument is the wire form of an image config blob.
type configDocument struct {
	// The platform fields sit at the top level of the config, not inside the
	// "config" object, which holds the runtime settings below.
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`

	Config struct {
		Env        []string `json:"Env"`
		Cmd        []string `json:"Cmd"`
		Entrypoint []string `json:"Entrypoint"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"config"`
}

// ParseConfig reads an image config blob. It is pure.
func ParseConfig(b []byte) (Config, error) {
	var doc configDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return Config{}, fmt.Errorf("parsing the image config: %w", err)
	}

	return Config{
		Env:        doc.Config.Env,
		Cmd:        doc.Config.Cmd,
		Entrypoint: doc.Config.Entrypoint,
		WorkingDir: doc.Config.WorkingDir,
		Platform: Platform{
			OS:           doc.OS,
			Architecture: doc.Architecture,
			Variant:      doc.Variant,
		},
	}, nil
}

// Command resolves the command a container should run, given whatever the
// caller supplied on the command line.
//
// The rule is the one Docker established and users expect: Entrypoint is the
// program, Cmd is its default arguments, and arguments given on the command
// line replace Cmd while leaving Entrypoint in place. An image with neither,
// and a caller who supplied nothing, returns nil — the caller reports that as
// a usage error, because only the caller knows what to tell the user to type.
func (c Config) Command(override []string) []string {
	args := c.Cmd
	if len(override) > 0 {
		args = override
	}

	command := make([]string, 0, len(c.Entrypoint)+len(args))
	command = append(command, c.Entrypoint...)
	command = append(command, args...)
	if len(command) == 0 {
		return nil
	}
	return command
}

// Environ merges the image's environment with the caller's, per variable, with
// the caller winning.
//
// Merging per key rather than wholesale is what lets a caller override PATH
// without discarding the half-dozen other variables an image needs to work.
// Order is stable: the image's variables keep their order, and new ones from
// the caller follow in the order given.
func (c Config) Environ(override []string) []string {
	merged := make([]string, 0, len(c.Env)+len(override))
	position := make(map[string]int, len(c.Env)+len(override))

	for _, entry := range append(append([]string{}, c.Env...), override...) {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			// An entry with no "=" cannot be overridden by key, so it is kept
			// as-is rather than dropped.
			merged = append(merged, entry)
			continue
		}
		if at, seen := position[key]; seen {
			merged[at] = entry
			continue
		}
		position[key] = len(merged)
		merged = append(merged, entry)
	}

	return merged
}
