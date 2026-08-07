package image_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/image"
)

// exampleHex is a syntactically valid SHA-256 body. It deliberately contains
// letters so the uppercase-rejection case below tests something.
const exampleHex = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// Reference parsing is where the familiar short forms are expanded into the
// address a registry request actually needs. Nearly every "image not found"
// a user will ever see starts here, so the table is exhaustive rather than
// representative.
func TestParseReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		input          string
		wantRegistry   string
		wantRepository string
		wantTag        string
		wantDigest     string
		wantString     string
	}{
		{
			name:           "a bare name is an official image with the latest tag",
			input:          "alpine",
			wantRegistry:   "docker.io",
			wantRepository: "library/alpine",
			wantTag:        "latest",
			wantString:     "docker.io/library/alpine:latest",
		},
		{
			name:           "a bare name with a tag",
			input:          "alpine:3.20",
			wantRegistry:   "docker.io",
			wantRepository: "library/alpine",
			wantTag:        "3.20",
			wantString:     "docker.io/library/alpine:3.20",
		},
		{
			name:           "a user repository keeps its own namespace",
			input:          "library/alpine:3.20",
			wantRegistry:   "docker.io",
			wantRepository: "library/alpine",
			wantTag:        "3.20",
			wantString:     "docker.io/library/alpine:3.20",
		},
		{
			name:           "a two-component name is not given the library prefix",
			input:          "grafana/grafana:11.0.0",
			wantRegistry:   "docker.io",
			wantRepository: "grafana/grafana",
			wantTag:        "11.0.0",
			wantString:     "docker.io/grafana/grafana:11.0.0",
		},
		{
			name:           "a fully qualified docker hub reference",
			input:          "docker.io/library/alpine:3.20",
			wantRegistry:   "docker.io",
			wantRepository: "library/alpine",
			wantTag:        "3.20",
			wantString:     "docker.io/library/alpine:3.20",
		},
		{
			name:           "another registry",
			input:          "ghcr.io/org/image:latest",
			wantRegistry:   "ghcr.io",
			wantRepository: "org/image",
			wantTag:        "latest",
			wantString:     "ghcr.io/org/image:latest",
		},
		{
			name:           "a deep repository path",
			input:          "quay.io/org/team/image:v1.2.3",
			wantRegistry:   "quay.io",
			wantRepository: "org/team/image",
			wantTag:        "v1.2.3",
			wantString:     "quay.io/org/team/image:v1.2.3",
		},
		{
			name:           "localhost is a registry even without a dot",
			input:          "localhost/img:v1",
			wantRegistry:   "localhost",
			wantRepository: "img",
			wantTag:        "v1",
			wantString:     "localhost/img:v1",
		},
		{
			name:           "a port makes the first component a registry",
			input:          "localhost:5000/img",
			wantRegistry:   "localhost:5000",
			wantRepository: "img",
			wantTag:        "latest",
			wantString:     "localhost:5000/img:latest",
		},
		{
			name:           "a colon after the last slash is a tag, not a port",
			input:          "127.0.0.1:5000/team/img:v2",
			wantRegistry:   "127.0.0.1:5000",
			wantRepository: "team/img",
			wantTag:        "v2",
			wantString:     "127.0.0.1:5000/team/img:v2",
		},
		{
			name:           "a digest reference",
			input:          "alpine@sha256:" + exampleHex,
			wantRegistry:   "docker.io",
			wantRepository: "library/alpine",
			wantDigest:     "sha256:" + exampleHex,
			wantString:     "docker.io/library/alpine@sha256:" + exampleHex,
		},
		{
			name:           "a tag and a digest keeps only the digest",
			input:          "alpine:3.20@sha256:" + exampleHex,
			wantRegistry:   "docker.io",
			wantRepository: "library/alpine",
			wantDigest:     "sha256:" + exampleHex,
			wantString:     "docker.io/library/alpine@sha256:" + exampleHex,
		},
		{
			name:           "a registry, a port and a digest",
			input:          "localhost:5000/img@sha256:" + exampleHex,
			wantRegistry:   "localhost:5000",
			wantRepository: "img",
			wantDigest:     "sha256:" + exampleHex,
			wantString:     "localhost:5000/img@sha256:" + exampleHex,
		},
		{
			name:           "separators inside a component are allowed",
			input:          "ghcr.io/my-org/my_image.v2:1.0",
			wantRegistry:   "ghcr.io",
			wantRepository: "my-org/my_image.v2",
			wantTag:        "1.0",
			wantString:     "ghcr.io/my-org/my_image.v2:1.0",
		},
		{
			name:           "an underscore-led tag is legal",
			input:          "alpine:_build.1-final",
			wantRegistry:   "docker.io",
			wantRepository: "library/alpine",
			wantTag:        "_build.1-final",
			wantString:     "docker.io/library/alpine:_build.1-final",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ref, err := image.ParseReference(tt.input)
			if err != nil {
				t.Fatalf("ParseReference(%q) = %v", tt.input, err)
			}

			if ref.Registry != tt.wantRegistry {
				t.Errorf("Registry = %q, want %q", ref.Registry, tt.wantRegistry)
			}
			if ref.Repository != tt.wantRepository {
				t.Errorf("Repository = %q, want %q", ref.Repository, tt.wantRepository)
			}
			if ref.Tag != tt.wantTag {
				t.Errorf("Tag = %q, want %q", ref.Tag, tt.wantTag)
			}
			if ref.Digest != tt.wantDigest {
				t.Errorf("Digest = %q, want %q", ref.Digest, tt.wantDigest)
			}
			if ref.Original != tt.input {
				t.Errorf("Original = %q, want %q", ref.Original, tt.input)
			}
			if got := ref.String(); got != tt.wantString {
				t.Errorf("String() = %q, want %q", got, tt.wantString)
			}
		})
	}
}

// A fully-qualified reference must survive a round trip, because String() is
// what makes two references written differently comparable.
func TestParseReferenceRoundTrips(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"alpine",
		"alpine:3.20",
		"ghcr.io/org/image:latest",
		"localhost:5000/img@sha256:" + exampleHex,
	}

	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			first, err := image.ParseReference(input)
			if err != nil {
				t.Fatalf("ParseReference(%q) = %v", input, err)
			}

			second, err := image.ParseReference(first.String())
			if err != nil {
				t.Fatalf("ParseReference(%q) = %v", first.String(), err)
			}

			if first.String() != second.String() {
				t.Errorf("round trip = %q, want %q", second.String(), first.String())
			}
		})
	}
}

func TestParseReferenceRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "a lone colon", input: ":"},
		{name: "a missing tag", input: "alpine:"},
		{name: "a missing repository", input: "ghcr.io/"},
		{name: "an empty path component", input: "ghcr.io/org//image"},
		{name: "an uppercase repository", input: "ghcr.io/Org/Image:latest"},
		{name: "a repository component that starts with a separator", input: "ghcr.io/-org/image"},
		{name: "a repository component that ends with a separator", input: "ghcr.io/org-/image"},
		{name: "a space", input: "ghcr.io/org/image :latest"},
		{name: "a NUL byte", input: "alpine\x00:3.20"},
		{name: "an unknown digest algorithm", input: "alpine@md5:" + strings.Repeat("a", 32)},
		{name: "a digest of the wrong length", input: "alpine@sha256:abcd"},
		{name: "an uppercase digest", input: "alpine@sha256:" + strings.ToUpper(exampleHex)},
		{name: "a digest with no algorithm", input: "alpine@" + exampleHex},
		{name: "a tag with a slash in it", input: "alpine:3.20/rc1"},
		{name: "a tag that is too long", input: "alpine:" + strings.Repeat("v", 129)},
		{name: "a tag starting with a dot", input: "alpine:.3.20"},

		// The Distribution Spec's separator grammar. Found by fuzzing: the
		// earlier approximation accepted every one of these, and no registry
		// would have served any of them.
		{name: "two dots in a row", input: "ghcr.io/org/a..b"},
		{name: "three underscores in a row", input: "ghcr.io/org/a___b"},
		{name: "a dot then a dash", input: "ghcr.io/org/a.-b"},
		{name: "a dash then a dot", input: "ghcr.io/org/a-.b"},
		{name: "a dot then an underscore", input: "ghcr.io/org/a._b"},
		{name: "a component that is only separators", input: "ghcr.io/org/---"},
		{name: "a doubled dot in what looks like a host", input: "a..b/c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ref, err := image.ParseReference(tt.input)
			if !errors.Is(err, image.ErrInvalidReference) {
				t.Fatalf("ParseReference(%q) = (%+v, %v), want %v", tt.input, ref, err, image.ErrInvalidReference)
			}
			if !strings.Contains(err.Error(), "invalid image reference") {
				t.Errorf("error %q does not name the problem", err)
			}
		})
	}
}

// Host is the one place the canonical registry name and the host serving its
// API differ, and getting it wrong means every Docker Hub pull fails.
func TestReferenceHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{input: "alpine", want: "registry-1.docker.io"},
		{input: "docker.io/library/alpine", want: "registry-1.docker.io"},
		{input: "ghcr.io/org/image", want: "ghcr.io"},
		{input: "localhost:5000/img", want: "localhost:5000"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()

			ref, err := image.ParseReference(tt.input)
			if err != nil {
				t.Fatalf("ParseReference() = %v", err)
			}
			if got := ref.Host(); got != tt.want {
				t.Errorf("Host() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReferenceTarget(t *testing.T) {
	t.Parallel()

	tagged, err := image.ParseReference("alpine:3.20")
	if err != nil {
		t.Fatalf("ParseReference() = %v", err)
	}
	if tagged.IsDigest() {
		t.Error("IsDigest() = true for a tagged reference")
	}
	if got := tagged.Target(); got != "3.20" {
		t.Errorf("Target() = %q, want the tag", got)
	}

	pinned, err := image.ParseReference("alpine@sha256:" + exampleHex)
	if err != nil {
		t.Fatalf("ParseReference() = %v", err)
	}
	if !pinned.IsDigest() {
		t.Error("IsDigest() = false for a digest reference")
	}
	if got := pinned.Target(); got != "sha256:"+exampleHex {
		t.Errorf("Target() = %q, want the digest", got)
	}
}

// The separator forms the spec does allow, which the stricter grammar must not
// have caught in the same net.
func TestParseReferenceAcceptsLegalSeparators(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"ghcr.io/org/a.b", "ghcr.io/org/a_b", "ghcr.io/org/a__b",
		"ghcr.io/org/a-b", "ghcr.io/org/a---b", "ghcr.io/my-org/my_image.v2",
		"ghcr.io/org/a1.b2_c3-d4",
	} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			if _, err := image.ParseReference(input); err != nil {
				t.Errorf("ParseReference(%q) = %v, want it accepted", input, err)
			}
		})
	}
}

// A registry host with an empty label is not a host. Found by fuzzing: "../b"
// was read as the repository "b" on a registry called "..", and "a..b/c" as
// "c" on a registry called "a..b". Both reached a DNS lookup before failing,
// and reported a network problem for what is a malformed reference.
func TestParseReferenceRejectsHostsWithEmptyLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "a parent-directory path", input: "../b"},
		{name: "a doubled dot in the host", input: "a..b/c"},
		{name: "a leading dot", input: ".example.com/img"},
		{name: "a trailing dot before the path", input: "example./img"},
		{name: "nothing but dots", input: "../../etc/passwd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ref, err := image.ParseReference(tt.input)
			if !errors.Is(err, image.ErrInvalidReference) {
				t.Fatalf("ParseReference(%q) = (%+v, %v), want %v",
					tt.input, ref, err, image.ErrInvalidReference)
			}
		})
	}
}

// The hosts that must keep working, so the check above cannot have been
// tightened into refusing a registry someone actually runs.
func TestParseReferenceAcceptsRealHosts(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"ghcr.io/org/image", "registry.example.co.uk/team/img:v1",
		"localhost/img", "localhost:5000/img", "127.0.0.1:5000/img",
		"my-registry.internal:8443/a/b", "docker.io/library/alpine",
	} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			if _, err := image.ParseReference(input); err != nil {
				t.Errorf("ParseReference(%q) = %v, want it accepted", input, err)
			}
		})
	}
}
