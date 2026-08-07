package image_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/image"
)

// New() performs no I/O: no connection, no DNS lookup, no token. A Client can
// therefore be built before forge knows whether it will pull anything.
func TestNewClientTouchesNothing(t *testing.T) {
	t.Parallel()

	client, err := image.New(discardLogger(), image.ClientConfig{})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if client == nil {
		t.Fatal("New() = nil client")
	}
}

// A transient 5xx is exactly what retrying is for. A 404 is not.
func TestRetriesRecoverFromATransientFailure(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	img := registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("a", "a")))
	registry.FailNext(2)

	client, err := image.New(discardLogger(), image.ClientConfig{
		HTTPClient:  registry.server.Client(),
		PlainHTTP:   true,
		MaxAttempts: 3,
		Backoff:     time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	manifest, err := client.Resolve(t.Context(), registry.Reference(t, "test/img:v1"), testPlatform)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if manifest.Digest != img.ManifestDigest {
		t.Errorf("Digest = %s, want %s", manifest.Digest, img.ManifestDigest)
	}
	if got := registry.ManifestRequests.Load(); got != 3 {
		t.Errorf("ManifestRequests = %d, want 3 (two failures and one success)", got)
	}
}

// Retrying has to give up, and the error has to say the registry was the
// problem rather than the image.
func TestExhaustedRetriesReportTheRegistry(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	registry.AddImage(t, "v1", buildConfig(t, nil), buildLayer(t, file("a", "a")))
	registry.FailNext(10)

	client, err := image.New(discardLogger(), image.ClientConfig{
		HTTPClient:  registry.server.Client(),
		PlainHTTP:   true,
		MaxAttempts: 2,
		Backoff:     time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	_, err = client.Resolve(t.Context(), registry.Reference(t, "test/img:v1"), testPlatform)
	if !errors.Is(err, image.ErrRegistryUnavailable) {
		t.Fatalf("Resolve() = %v, want %v", err, image.ErrRegistryUnavailable)
	}
	if got := registry.ManifestRequests.Load(); got != 2 {
		t.Errorf("ManifestRequests = %d, want exactly the 2 attempts allowed", got)
	}
}

// A registry that is not there at all is unavailable, not "image not found".
func TestAnUnreachableRegistryIsUnavailable(t *testing.T) {
	t.Parallel()

	client, err := image.New(discardLogger(), image.ClientConfig{
		PlainHTTP:   true,
		MaxAttempts: 1,
		HTTPClient:  &http.Client{Timeout: 2 * time.Second},
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	// Port 1 on loopback refuses connections promptly.
	ref, err := image.ParseReference("127.0.0.1:1/test/img:v1")
	if err != nil {
		t.Fatalf("ParseReference() = %v", err)
	}

	if _, err := client.Resolve(t.Context(), ref, testPlatform); !errors.Is(err, image.ErrRegistryUnavailable) {
		t.Fatalf("Resolve() = %v, want %v", err, image.ErrRegistryUnavailable)
	}
}

// A 401 that a token does not fix is a private image, and the message has to
// say so — forge pulls anonymously by design, and a bare HTTP status would
// leave the user guessing.
func TestAPersistent401ReportsThatForgePullsAnonymously(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	registry.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			writeJSON(w, map[string]string{"token": "useless"})
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+registry.server.URL+`/token",service="fake"`)
		writeError(w, http.StatusUnauthorized, "DENIED", "requires authentication")
	})

	_, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "private/img:v1"), testPlatform)
	if !errors.Is(err, image.ErrUnauthorized) {
		t.Fatalf("Resolve() = %v, want %v", err, image.ErrUnauthorized)
	}
	if !strings.Contains(err.Error(), "anonymously") {
		t.Errorf("error %q does not explain that forge pulls anonymously", err)
	}
}

// An authentication scheme forge does not implement must be named, not
// silently treated as a failed token fetch.
func TestAnUnsupportedAuthSchemeIsReported(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t)
	registry.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		writeError(w, http.StatusUnauthorized, "DENIED", "requires authentication")
	})

	_, err := registry.Client(t).Resolve(t.Context(), registry.Reference(t, "private/img:v1"), testPlatform)
	if !errors.Is(err, image.ErrUnauthorized) {
		t.Fatalf("Resolve() = %v, want %v", err, image.ErrUnauthorized)
	}
	if !strings.Contains(err.Error(), "Basic") {
		t.Errorf("error %q does not name the scheme the registry asked for", err)
	}
}
