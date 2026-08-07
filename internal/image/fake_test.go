package image_test

// A synthetic image builder and a fake registry.
//
// Every test in this package is built on these two helpers, so the same bytes
// exercise the reference parser, the client, the cache and the extractor. The
// registry is a real HTTP server on loopback implementing the slice of the
// Distribution Spec Forge uses, which means image.Client runs against it
// completely unmodified — no injected fake, and therefore no interface
// introduced for testability alone (SSOT §2).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/image"
	"github.com/stevenstank/forge/internal/logging"
)

const (
	ociManifestType = "application/vnd.oci.image.manifest.v1+json"
	ociIndexType    = "application/vnd.oci.image.index.v1+json"
	ociConfigType   = "application/vnd.oci.image.config.v1+json"
	ociLayerType    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

func discardLogger() *slog.Logger { return logging.New(io.Discard, slog.LevelError) }

// digestOf returns the digest naming b, in the form the cache and the
// manifests use.
func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// entry is one member of a synthetic layer.
type entry struct {
	name string
	typ  byte
	body string
	link string
	mode int64
	uid  int
	gid  int
	// major and minor apply to device entries only.
	major, minor int64
}

func file(name, body string) entry {
	return entry{name: name, typ: tar.TypeReg, body: body, mode: 0o644}
}

func dir(name string) entry {
	return entry{name: name, typ: tar.TypeDir, mode: 0o755}
}

func symlink(name, target string) entry {
	return entry{name: name, typ: tar.TypeSymlink, link: target, mode: 0o777}
}

func hardlink(name, target string) entry {
	return entry{name: name, typ: tar.TypeLink, link: target, mode: 0o644}
}

// whiteout returns the marker that deletes name from the layers below. The
// marker lives in the deleted entry's own directory, which is why this is a
// helper rather than a string concatenation at each call site.
func whiteout(name string) entry {
	return file(path.Join(path.Dir(name), ".wh."+path.Base(name)), "")
}

// buildTar renders entries as an uncompressed tar archive.
func buildTar(t *testing.T, entries ...entry) []byte {
	t.Helper()

	var buf bytes.Buffer
	archive := tar.NewWriter(&buf)

	for _, e := range entries {
		header := &tar.Header{
			Name:     e.name,
			Typeflag: e.typ,
			Mode:     e.mode,
			Linkname: e.link,
			Uid:      e.uid,
			Gid:      e.gid,
			Devmajor: e.major,
			Devminor: e.minor,
			ModTime:  time.Unix(1600000000, 0),
		}
		if e.typ == tar.TypeReg {
			header.Size = int64(len(e.body))
		}

		if err := archive.WriteHeader(header); err != nil {
			t.Fatalf("writing the header for %q: %v", e.name, err)
		}
		if e.typ == tar.TypeReg {
			if _, err := archive.Write([]byte(e.body)); err != nil {
				t.Fatalf("writing the body of %q: %v", e.name, err)
			}
		}
	}

	if err := archive.Close(); err != nil {
		t.Fatalf("closing the tar writer: %v", err)
	}

	return buf.Bytes()
}

// buildLayer renders entries as a gzipped tar archive, which is what a layer
// blob actually is.
func buildLayer(t *testing.T, entries ...entry) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(buildTar(t, entries...)); err != nil {
		t.Fatalf("compressing a layer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing the gzip writer: %v", err)
	}

	return buf.Bytes()
}

// buildConfig renders a minimal image config blob.
func buildConfig(t *testing.T, cfg map[string]any) []byte {
	t.Helper()

	b, err := json.Marshal(map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"config":       cfg,
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{}},
	})
	if err != nil {
		t.Fatalf("marshalling an image config: %v", err)
	}

	return b
}

// published is what a fake registry hands back after an image is added to it.
type published struct {
	Tag            string
	ManifestDigest string
	ConfigDigest   string
	LayerDigests   []string
}

// fakeRegistry serves the part of the OCI Distribution Spec Forge reads.
type fakeRegistry struct {
	server *httptest.Server

	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte
	types     map[string]string

	// requireToken makes the registry answer an unauthenticated request with a
	// 401 and a Bearer challenge, as Docker Hub does.
	requireToken bool

	// truncate cuts a blob response short, without changing its digest, which
	// is how an interrupted or lying registry is simulated.
	truncate map[string]int

	// corrupt swaps a blob's bytes for others of the same length, so the
	// length check passes and only the digest catches it.
	corrupt map[string]bool

	// block holds a blob response open until the channel is closed, which is
	// how a cancelled download is simulated deterministically.
	block map[string]chan struct{}

	// omitContentDigest suppresses the Docker-Content-Digest header.
	omitContentDigest bool

	// misreport makes the registry claim a manifest has a digest it does not,
	// which is how a tampered response is simulated for a tagged pull.
	misreport map[string]string

	// failNext makes that many manifest requests fail with a retryable status
	// before the registry starts answering normally.
	failNext atomic.Int64

	ManifestRequests atomic.Int64
	BlobRequests     atomic.Int64
	TokenRequests    atomic.Int64
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()

	f := &fakeRegistry{
		blobs:     make(map[string][]byte),
		manifests: make(map[string][]byte),
		types:     make(map[string]string),
		truncate:  make(map[string]int),
		corrupt:   make(map[string]bool),
		block:     make(map[string]chan struct{}),
		misreport: make(map[string]string),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)

	return f
}

// Host returns the registry's host:port, which is also the first component of
// every reference pointing at it.
func (f *fakeRegistry) Host() string {
	return strings.TrimPrefix(f.server.URL, "http://")
}

// Reference parses a reference against this registry.
func (f *fakeRegistry) Reference(t *testing.T, nameAndTag string) image.Reference {
	t.Helper()

	ref, err := image.ParseReference(f.Host() + "/" + nameAndTag)
	if err != nil {
		t.Fatalf("ParseReference() = %v", err)
	}

	return ref
}

// Client returns a client pointed at this registry.
func (f *fakeRegistry) Client(t *testing.T) *image.Client {
	t.Helper()

	client, err := image.New(discardLogger(), image.ClientConfig{
		HTTPClient: f.server.Client(),
		PlainHTTP:  true,
		// One attempt keeps a test that expects a failure fast, and the retry
		// policy has its own test.
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	return client
}

// AddBlob stores a blob and returns the descriptor naming it.
func (f *fakeRegistry) AddBlob(b []byte, mediaType string) image.Descriptor {
	f.mu.Lock()
	defer f.mu.Unlock()

	digest := digestOf(b)
	f.blobs[digest] = b

	return image.Descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(b))}
}

// AddImage publishes a single-platform image under a tag.
func (f *fakeRegistry) AddImage(t *testing.T, tag string, config []byte, layers ...[]byte) published {
	t.Helper()

	configDesc := f.AddBlob(config, ociConfigType)

	descriptors := make([]image.Descriptor, 0, len(layers))
	digests := make([]string, 0, len(layers))
	for _, layer := range layers {
		d := f.AddBlob(layer, ociLayerType)
		descriptors = append(descriptors, d)
		digests = append(digests, d.Digest)
	}

	manifest := f.marshal(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociManifestType,
		"config":        configDesc,
		"layers":        descriptors,
	})

	digest := f.addManifest(tag, manifest, ociManifestType)

	return published{Tag: tag, ManifestDigest: digest, ConfigDigest: configDesc.Digest, LayerDigests: digests}
}

// indexEntry is one platform's manifest within an index.
type indexEntry struct {
	OS, Architecture, Variant string
	Digest                    string
	Size                      int64
}

// AddIndex publishes a multi-platform index under a tag.
func (f *fakeRegistry) AddIndex(t *testing.T, tag string, entries ...indexEntry) string {
	t.Helper()

	manifests := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		platform := map[string]any{"os": e.OS, "architecture": e.Architecture}
		if e.Variant != "" {
			platform["variant"] = e.Variant
		}
		manifests = append(manifests, map[string]any{
			"mediaType": ociManifestType,
			"digest":    e.Digest,
			"size":      e.Size,
			"platform":  platform,
		})
	}

	index := f.marshal(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociIndexType,
		"manifests":     manifests,
	})

	return f.addManifest(tag, index, ociIndexType)
}

// AddRawManifest publishes bytes under a tag without interpreting them, for the
// tests that need a malformed or oversized document.
func (f *fakeRegistry) AddRawManifest(tag string, body []byte, mediaType string) string {
	return f.addManifest(tag, body, mediaType)
}

func (f *fakeRegistry) addManifest(tag string, body []byte, mediaType string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	digest := digestOf(body)
	if tag != "" {
		f.manifests[tag] = body
		f.types[tag] = mediaType
	}
	f.manifests[digest] = body
	f.types[digest] = mediaType

	return digest
}

func (f *fakeRegistry) marshal(t *testing.T, v any) []byte {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling a registry document: %v", err)
	}

	return b
}

// Truncate makes the registry send only n bytes of a blob.
func (f *fakeRegistry) Truncate(digest string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.truncate[digest] = n
}

// Corrupt makes the registry send different bytes of the same length.
func (f *fakeRegistry) Corrupt(digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.corrupt[digest] = true
}

// Block makes a blob request hang until the returned function is called.
func (f *fakeRegistry) Block(digest string) (release func()) {
	gate := make(chan struct{})

	f.mu.Lock()
	f.block[digest] = gate
	f.mu.Unlock()

	return sync.OnceFunc(func() { close(gate) })
}

// RequireToken turns on the anonymous Bearer challenge.
func (f *fakeRegistry) RequireToken() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requireToken = true
}

// OmitContentDigest suppresses the Docker-Content-Digest header.
func (f *fakeRegistry) OmitContentDigest() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.omitContentDigest = true
}

// Misreport makes the registry answer target with a Docker-Content-Digest
// header naming content it is not sending.
func (f *fakeRegistry) Misreport(target, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.misreport[target] = digest
}

func (f *fakeRegistry) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v2/":
		w.WriteHeader(http.StatusOK)

	case r.URL.Path == "/token":
		f.TokenRequests.Add(1)
		writeJSON(w, map[string]string{"token": "anonymous-token"})

	case strings.Contains(r.URL.Path, "/manifests/"):
		f.serveManifest(w, r)

	case strings.Contains(r.URL.Path, "/blobs/"):
		f.serveBlob(w, r)

	default:
		http.NotFound(w, r)
	}
}

// challenged reports whether the request must be answered with a 401.
func (f *fakeRegistry) challenged(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	required := f.requireToken
	f.mu.Unlock()

	if !required || r.Header.Get("Authorization") != "" {
		return false
	}

	w.Header().Set("WWW-Authenticate",
		fmt.Sprintf(`Bearer realm="%s/token",service="fake",scope="repository:test/img:pull"`, f.server.URL))
	writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")

	return true
}

// FailNext makes the next n manifest requests answer with a retryable 503.
func (f *fakeRegistry) FailNext(n int64) { f.failNext.Store(n) }

func (f *fakeRegistry) serveManifest(w http.ResponseWriter, r *http.Request) {
	f.ManifestRequests.Add(1)

	if remaining := f.failNext.Load(); remaining > 0 {
		f.failNext.Add(-1)
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "try again")
		return
	}

	if f.challenged(w, r) {
		return
	}

	target := r.URL.Path[strings.LastIndex(r.URL.Path, "/manifests/")+len("/manifests/"):]

	f.mu.Lock()
	body, ok := f.manifests[target]
	mediaType := f.types[target]
	omit := f.omitContentDigest
	claimed, lying := f.misreport[target]
	f.mu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}

	w.Header().Set("Content-Type", mediaType)
	switch {
	case lying:
		w.Header().Set("Docker-Content-Digest", claimed)
	case !omit:
		w.Header().Set("Docker-Content-Digest", digestOf(body))
	}
	if _, err := w.Write(body); err != nil {
		panic(err)
	}
}

func (f *fakeRegistry) serveBlob(w http.ResponseWriter, r *http.Request) {
	f.BlobRequests.Add(1)

	if f.challenged(w, r) {
		return
	}

	digest := r.URL.Path[strings.LastIndex(r.URL.Path, "/blobs/")+len("/blobs/"):]

	f.mu.Lock()
	body, ok := f.blobs[digest]
	limit, truncated := f.truncate[digest]
	corrupt := f.corrupt[digest]
	gate := f.block[digest]
	f.mu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown")
		return
	}

	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}

	if corrupt {
		body = bytes.Repeat([]byte{'x'}, len(body))
	}
	if truncated {
		body = body[:min(limit, len(body))]
		// The advertised length stays honest so the client's own length check
		// is what catches the short read.
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := w.Write(body); err != nil {
		panic(err)
	}
}

// waitFor blocks until condition holds, failing the test if it never does.
//
// It exists so the interrupted-download test can cancel a pull at a known
// point — after the request has reached the server — rather than at a moment
// chosen by a sleep.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the registry to receive the request")
		}
		time.Sleep(time.Millisecond)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	})
}
