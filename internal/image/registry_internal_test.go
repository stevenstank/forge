package image

import (
	"testing"
	"time"
)

// The Bearer challenge is the only header forge parses by hand, and getting it
// wrong means every Docker Hub pull fails at the first request.
func TestParseChallenge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		header     string
		wantScheme string
		wantRealm  string
		wantScope  string
	}{
		{
			name:       "docker hub's challenge",
			header:     `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"`,
			wantScheme: "Bearer",
			wantRealm:  "https://auth.docker.io/token",
			wantScope:  "repository:library/alpine:pull",
		},
		{
			name:       "unquoted values",
			header:     `Bearer realm=https://auth.example/token,service=example`,
			wantScheme: "Bearer",
			wantRealm:  "https://auth.example/token",
		},
		{
			name:       "surrounding space",
			header:     `  Bearer  realm="https://auth.example/token" , service="example" `,
			wantScheme: "Bearer",
			wantRealm:  "https://auth.example/token",
		},
		{
			name:       "a scheme forge does not implement",
			header:     `Basic realm="registry"`,
			wantScheme: "Basic",
			wantRealm:  "registry",
		},
		{
			name:       "no parameters at all",
			header:     "Bearer",
			wantScheme: "Bearer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme, params := parseChallenge(tt.header)

			if scheme != tt.wantScheme {
				t.Errorf("scheme = %q, want %q", scheme, tt.wantScheme)
			}
			if params["realm"] != tt.wantRealm {
				t.Errorf("realm = %q, want %q", params["realm"], tt.wantRealm)
			}
			if tt.wantScope != "" && params["scope"] != tt.wantScope {
				t.Errorf("scope = %q, want %q", params["scope"], tt.wantScope)
			}
		})
	}
}

// Retry-After is honoured but clamped: an unbounded one would let a registry
// park a forge run indefinitely.
func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		header string
		want   time.Duration
	}{
		{header: "", want: 0},
		{header: "2", want: 2 * time.Second},
		{header: " 3 ", want: 3 * time.Second},
		{header: "600", want: maxRetryAfter},
		{header: "-1", want: 0},
		{header: "Wed, 21 Oct 2026 07:28:00 GMT", want: 0},
		{header: "soon", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			t.Parallel()

			if got := parseRetryAfter(tt.header); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

// The client defaults are what a caller gets from a zero ClientConfig, and
// every one of them is a decision rather than an accident.
func TestClientDefaults(t *testing.T) {
	t.Parallel()

	client, err := New(nil, ClientConfig{})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	if client.maxManifestBytes != defaultMaxManifestBytes {
		t.Errorf("maxManifestBytes = %d, want %d", client.maxManifestBytes, defaultMaxManifestBytes)
	}
	if client.maxAttempts != defaultMaxAttempts {
		t.Errorf("maxAttempts = %d, want %d", client.maxAttempts, defaultMaxAttempts)
	}
	if client.backoff != defaultBackoff {
		t.Errorf("backoff = %v, want %v", client.backoff, defaultBackoff)
	}
	if client.userAgent != defaultUserAgent {
		t.Errorf("userAgent = %q, want %q", client.userAgent, defaultUserAgent)
	}
	if client.plainHTTP {
		t.Error("plainHTTP = true by default; https must be the default")
	}
	if client.logger == nil {
		t.Error("logger = nil; New must supply one")
	}
}

// The endpoint is built from the reference, and Docker Hub's canonical name is
// not the host that serves its API.
func TestEndpoint(t *testing.T) {
	t.Parallel()

	client, err := New(nil, ClientConfig{})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	ref, err := ParseReference("alpine:3.20")
	if err != nil {
		t.Fatalf("ParseReference() = %v", err)
	}

	got := client.endpoint(ref, "manifests", ref.Target())
	want := "https://registry-1.docker.io/v2/library/alpine/manifests/3.20"
	if got != want {
		t.Errorf("endpoint() = %q, want %q", got, want)
	}
}
