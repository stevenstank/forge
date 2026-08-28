package image_test

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/image"
)

// linux/amd64 is used throughout rather than HostPlatform() so the tests assert
// the same thing on every machine that runs them.
var testPlatform = image.Platform{OS: "linux", Architecture: "amd64"}

func TestResolveATag(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	layer := buildLayer(t, file("hello", "world"))
	img := registry.AddImage(t, "v1", buildConfig(t, nil), layer)

	manifest, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "test/img:v1"), testPlatform)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}

	if manifest.Digest != img.ManifestDigest {
		t.Errorf("Digest = %s, want %s", manifest.Digest, img.ManifestDigest)
	}
	if manifest.Config.Digest != img.ConfigDigest {
		t.Errorf("Config.Digest = %s, want %s", manifest.Config.Digest, img.ConfigDigest)
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].Digest != img.LayerDigests[0] {
		t.Errorf("Layers = %+v, want the one layer %s", manifest.Layers, img.LayerDigests[0])
	}
}

// A tag is resolved to an immutable digest here and nowhere else. Asking for
// that digest must return the same manifest.
func TestResolveADigestReference(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	img := registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("hello", "world")))

	ref := registry.Reference(t, "test/img@"+img.ManifestDigest)
	if !ref.IsDigest() {
		t.Fatal("the reference is not a digest reference")
	}

	manifest, err := registry.Client(t).Resolve(t.Context(), ref, testPlatform)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if manifest.Digest != img.ManifestDigest {
		t.Errorf("Digest = %s, want %s", manifest.Digest, img.ManifestDigest)
	}
}

func TestResolveFollowsAnIndex(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)

	amd64 := registry.AddImage(t, "", buildConfig(t, nil), buildLayer(t, file("amd64", "yes")))
	arm64 := registry.AddImage(t, "", buildConfig(t, nil), buildLayer(t, file("arm64", "yes")))

	registry.AddIndex(t, "multi",
		indexEntry{OS: "linux", Architecture: "arm64", Variant: "v8", Digest: arm64.ManifestDigest, Size: 1},
		indexEntry{OS: "linux", Architecture: "amd64", Digest: amd64.ManifestDigest, Size: 1},
	)

	manifest, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "test/img:multi"), testPlatform)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if manifest.Digest != amd64.ManifestDigest {
		t.Errorf("Digest = %s, want the amd64 manifest %s", manifest.Digest, amd64.ManifestDigest)
	}
}

// An arm64 host must be told its architecture is not published, not handed a
// manifest that will fail with ENOEXEC inside the container.
func TestResolveReportsAMissingPlatform(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	amd64 := registry.AddImage(t, "", buildConfig(t, nil), buildLayer(t, file("amd64", "yes")))
	registry.AddIndex(t, "multi",
		indexEntry{OS: "linux", Architecture: "amd64", Digest: amd64.ManifestDigest, Size: 1},
		indexEntry{OS: "windows", Architecture: "amd64", Digest: amd64.ManifestDigest, Size: 1},
	)

	wanted := image.Platform{OS: "linux", Architecture: "riscv64"}

	_, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "test/img:multi"), wanted)
	if !errors.Is(err, image.ErrNoMatchingPlatform) {
		t.Fatalf("Resolve() = %v, want %v", err, image.ErrNoMatchingPlatform)
	}
	// The operator needs to know what is on offer, not only that their
	// platform is absent.
	for _, offered := range []string{"linux/amd64", "windows/amd64"} {
		if !strings.Contains(err.Error(), offered) {
			t.Errorf("error %q does not list %s", err, offered)
		}
	}
}

// Attestation and SBOM manifests are published as unknown/unknown and are not
// runnable images.
func TestResolveSkipsAttestationManifests(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	img := registry.AddImage(t, "", buildConfig(t, nil), buildLayer(t, file("real", "image")))
	registry.AddIndex(t, "multi",
		indexEntry{OS: "unknown", Architecture: "unknown", Digest: img.ManifestDigest, Size: 1},
		indexEntry{OS: "linux", Architecture: "amd64", Digest: img.ManifestDigest, Size: 1},
	)

	manifest, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "test/img:multi"), testPlatform)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if manifest.Digest != img.ManifestDigest {
		t.Errorf("Digest = %s, want %s", manifest.Digest, img.ManifestDigest)
	}
	if strings.Contains(manifest.Config.MediaType, "unknown") {
		t.Error("an attestation manifest was selected")
	}
}

// A manifest is the root of trust for every layer beneath it, so a body that
// does not match the digest naming it must never be parsed, let alone acted on.
func TestResolveRejectsAManifestThatDoesNotMatchItsDigest(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	img := registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("hello", "world")))

	// A digest that names nothing is an ordinary 404.
	_, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "test/img@sha256:"+exampleHex), testPlatform)
	if !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("Resolve() of an unknown digest = %v, want %v", err, image.ErrNotFound)
	}

	// The case that matters: the registry answers a digest request with bytes
	// that are not the ones asked for. Serving a different manifest's body
	// under this digest is exactly the tampering digests exist to catch.
	other := buildConfig(t, map[string]any{"WorkingDir": "/elsewhere"})
	registry.AddRawManifest(img.ManifestDigest, other, ociManifestType)

	_, err = registry.Client(t).Resolve(t.Context(), registry.Reference(t, "test/img@"+img.ManifestDigest), testPlatform)
	if !errors.Is(err, image.ErrDigestMismatch) {
		t.Fatalf("Resolve() = %v, want %v", err, image.ErrDigestMismatch)
	}
	// The message has to carry both digests, or the operator cannot tell a
	// tampered response from a forge bug.
	if !strings.Contains(err.Error(), img.ManifestDigest) || !strings.Contains(err.Error(), digestOf(other)) {
		t.Errorf("error %q does not name both the expected and the computed digest", err)
	}
}

// A tag names mutable content, so there is no digest to check against. The
// registry's own Docker-Content-Digest header is its claim about what it sent,
// and a body that does not match it was altered in transit.
func TestResolveChecksTheContentDigestHeader(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("a", "a")))
	registry.Misreport("v1", "sha256:"+exampleHex)

	_, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "test/img:v1"), testPlatform)
	if !errors.Is(err, image.ErrDigestMismatch) {
		t.Fatalf("Resolve() = %v, want %v", err, image.ErrDigestMismatch)
	}
}

// A registry that sends no Docker-Content-Digest header at all is still
// usable: the header is a claim to check, not a requirement.
func TestResolveWorksWithoutAContentDigestHeader(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	img := registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("a", "a")))
	registry.OmitContentDigest()

	manifest, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "test/img:v1"), testPlatform)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if manifest.Digest != img.ManifestDigest {
		t.Errorf("Digest = %s, want the digest forge computed itself (%s)", manifest.Digest, img.ManifestDigest)
	}
}

func TestResolveReportsAnUnknownTag(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("a", "a")))

	_, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "test/img:absent"), testPlatform)
	if !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("Resolve() = %v, want %v", err, image.ErrNotFound)
	}
	// The registry's own words are more useful than a bare 404.
	if !strings.Contains(err.Error(), "manifest unknown") {
		t.Errorf("error %q does not relay the registry's message", err)
	}
}

// The anonymous Bearer flow: a 401 with a challenge, one token request, one
// retry. It is what public Docker Hub, GHCR and Quay all require.
func TestResolveCompletesTheAnonymousTokenFlow(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	img := registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("hello", "world")))
	registry.RequireToken()

	client := registry.Client(t)

	manifest, err := client.Resolve(t.Context(), registry.Reference(t, "test/img:v1"), testPlatform)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if manifest.Digest != img.ManifestDigest {
		t.Errorf("Digest = %s, want %s", manifest.Digest, img.ManifestDigest)
	}
	if got := registry.TokenRequests.Load(); got != 1 {
		t.Errorf("TokenRequests = %d, want exactly 1", got)
	}

	// The token is cached per repository, so a second call must not repeat the
	// dance.
	if _, err := client.Resolve(t.Context(), registry.Reference(t, "test/img:v1"), testPlatform); err != nil {
		t.Fatalf("second Resolve() = %v", err)
	}
	if got := registry.TokenRequests.Load(); got != 1 {
		t.Errorf("TokenRequests = %d after a second resolve, want the token to have been cached", got)
	}
}

// A manifest has to be buffered to be hashed, so a cap is what stops a hostile
// registry making forge allocate without bound.
func TestResolveRefusesAnOversizedManifest(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	registry.AddRawManifest("huge", bytes.Repeat([]byte("x"), 2048), ociManifestType)

	client, err := image.New(discardLogger(), image.ClientConfig{
		HTTPClient:       registry.server.Client(),
		PlainHTTP:        true,
		MaxAttempts:      1,
		MaxManifestBytes: 512,
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	_, err = client.Resolve(t.Context(), registry.Reference(t, "test/img:huge"), testPlatform)
	if !errors.Is(err, image.ErrManifestTooLarge) {
		t.Fatalf("Resolve() = %v, want %v", err, image.ErrManifestTooLarge)
	}
}

// Refusing a layer format at resolve time is what stops a run downloading
// several hundred megabytes it cannot use.
func TestResolveRefusesUnsupportedLayerTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mediaType string
		wantWord  string
	}{
		{name: "zstd", mediaType: "application/vnd.oci.image.layer.v1.tar+zstd", wantWord: "zstd"},
		{
			name:      "a foreign layer",
			mediaType: "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip",
			wantWord:  "hosted outside",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			registry := newFakeRegistry(t)
			config := registry.AddBlob(buildConfig(t, nil), ociConfigType)
			layer := registry.AddBlob([]byte("compressed somehow"), tt.mediaType)

			body := registry.marshal(t, map[string]any{
				"schemaVersion": 2,
				"mediaType":     ociManifestType,
				"config":        config,
				"layers":        []image.Descriptor{layer},
			})
			registry.AddRawManifest("odd", body, ociManifestType)

			_, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "test/img:odd"), testPlatform)
			if !errors.Is(err, image.ErrUnsupportedMediaType) {
				t.Fatalf("Resolve() = %v, want %v", err, image.ErrUnsupportedMediaType)
			}
			if !strings.Contains(err.Error(), tt.wantWord) {
				t.Errorf("error %q does not explain the refusal", err)
			}
		})
	}
}

func TestHostPlatformIsLinux(t *testing.T) {
	t.Parallel()

	if got := image.HostPlatform(); got.OS != "linux" {
		t.Errorf("HostPlatform().OS = %q, want linux", got.OS)
	}
}

// An image that names no variant must run on a host that has one — nearly
// every arm64 image is published that way.
func TestPlatformVariantMatching(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	plain := registry.AddImage(t, "", buildConfig(t, nil), buildLayer(t, file("a", "a")))
	registry.AddIndex(t, "multi",
		indexEntry{OS: "linux", Architecture: "arm64", Digest: plain.ManifestDigest, Size: 1},
	)

	host := image.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}

	manifest, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "test/img:multi"), host)
	if err != nil {
		t.Fatalf("Resolve() = %v, want a variantless arm64 image to match a v8 host", err)
	}
	if manifest.Digest != plain.ManifestDigest {
		t.Errorf("Digest = %s, want %s", manifest.Digest, plain.ManifestDigest)
	}
}

func TestParseConfig(t *testing.T) {
	t.Parallel()

	body := buildConfig(t, map[string]any{
		"Env":        []string{"PATH=/usr/bin", "LANG=C"},
		"Cmd":        []string{"-l"},
		"Entrypoint": []string{"/bin/ls"},
		"WorkingDir": "/srv",
	})

	config, err := image.ParseConfig(body)
	if err != nil {
		t.Fatalf("ParseConfig() = %v", err)
	}

	if !slices.Equal(config.Env, []string{"PATH=/usr/bin", "LANG=C"}) {
		t.Errorf("Env = %v", config.Env)
	}
	if !slices.Equal(config.Cmd, []string{"-l"}) {
		t.Errorf("Cmd = %v", config.Cmd)
	}
	if !slices.Equal(config.Entrypoint, []string{"/bin/ls"}) {
		t.Errorf("Entrypoint = %v", config.Entrypoint)
	}
	if config.WorkingDir != "/srv" {
		t.Errorf("WorkingDir = %q", config.WorkingDir)
	}

	if _, err := image.ParseConfig([]byte("not json")); err == nil {
		t.Error("ParseConfig() = nil for a malformed config")
	}
}

// Entrypoint is the program and Cmd is its default arguments; arguments given
// on the command line replace Cmd and leave Entrypoint alone. Getting this
// backwards is the classic container-runtime bug.
func TestConfigCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   image.Config
		override []string
		want     []string
	}{
		{
			name:   "entrypoint and cmd are concatenated",
			config: image.Config{Entrypoint: []string{"/bin/ls"}, Cmd: []string{"-l", "/etc"}},
			want:   []string{"/bin/ls", "-l", "/etc"},
		},
		{
			name:     "an override replaces cmd and keeps entrypoint",
			config:   image.Config{Entrypoint: []string{"/bin/ls"}, Cmd: []string{"-l"}},
			override: []string{"-a", "/srv"},
			want:     []string{"/bin/ls", "-a", "/srv"},
		},
		{
			name:   "cmd alone is the command",
			config: image.Config{Cmd: []string{"/bin/sh"}},
			want:   []string{"/bin/sh"},
		},
		{
			name:     "an override with no entrypoint is the command",
			config:   image.Config{Cmd: []string{"/bin/sh"}},
			override: []string{"/bin/echo", "hi"},
			want:     []string{"/bin/echo", "hi"},
		},
		{
			name:   "an image with neither has no command",
			config: image.Config{},
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.config.Command(tt.override); !slices.Equal(got, tt.want) {
				t.Errorf("Command(%v) = %v, want %v", tt.override, got, tt.want)
			}
		})
	}
}

// Merging per key rather than wholesale is what lets a caller override PATH
// without discarding everything else the image needs.
func TestConfigEnviron(t *testing.T) {
	t.Parallel()

	config := image.Config{Env: []string{"PATH=/usr/bin", "LANG=C", "TZ=UTC"}}

	got := config.Environ([]string{"PATH=/opt/bin", "EXTRA=1"})
	want := []string{"PATH=/opt/bin", "LANG=C", "TZ=UTC", "EXTRA=1"}

	if !slices.Equal(got, want) {
		t.Errorf("Environ() = %v, want %v", got, want)
	}

	if got := config.Environ(nil); !slices.Equal(got, config.Env) {
		t.Errorf("Environ(nil) = %v, want the image's own environment", got)
	}
}

// TestPlatformIsZero covers the distinction Stage 5 needs when an image config
// declares nothing: "declares a platform that does not match" is a refusal,
// "declares nothing" is not.
func TestPlatformIsZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    image.Platform
		want bool
	}{
		{name: "empty", p: image.Platform{}, want: true},
		{name: "a variant alone is not a platform", p: image.Platform{Variant: "v8"}, want: true},
		{name: "os only", p: image.Platform{OS: "linux"}, want: false},
		{name: "architecture only", p: image.Platform{Architecture: "arm64"}, want: false},
		{name: "both", p: image.Platform{OS: "linux", Architecture: "amd64"}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.p.IsZero(); got != tc.want {
				t.Errorf("Platform%+v.IsZero() = %t, want %t", tc.p, got, tc.want)
			}
		})
	}

	// The host's own platform is never zero, which is what makes the check
	// usable as "the image said nothing" rather than "something went wrong".
	if image.HostPlatform().IsZero() {
		t.Error("HostPlatform().IsZero() = true")
	}
}
