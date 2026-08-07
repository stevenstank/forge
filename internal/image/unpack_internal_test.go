package image

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Extraction runs as root over paths that came from the internet, so
// containment is the most security-relevant code in this package and gets the
// same treatment mount.resolveDestination got in Stage 2: a table, and one case
// per way out of the tree anyone has thought of.

func TestEntryPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "an ordinary file", input: "etc/passwd", want: filepath.Join("etc", "passwd")},
		{name: "a leading ./ is cleaned", input: "./etc/passwd", want: filepath.Join("etc", "passwd")},
		{name: "a redundant component is cleaned", input: "etc/./passwd", want: filepath.Join("etc", "passwd")},
		{name: "an interior .. that stays inside is cleaned", input: "etc/../etc/passwd", want: filepath.Join("etc", "passwd")},
		{name: "a trailing slash on a directory", input: "etc/", want: "etc"},
		{name: "the archive root", input: "./", want: ""},
		{name: "the archive root as a dot", input: ".", want: ""},

		{name: "an absolute path", input: "/etc/shadow", wantErr: ErrEscapesRoot},
		{name: "a parent reference", input: "../etc/shadow", wantErr: ErrEscapesRoot},
		{name: "a deep parent reference", input: "a/../../etc/shadow", wantErr: ErrEscapesRoot},
		{name: "a bare parent reference", input: "..", wantErr: ErrEscapesRoot},
		{name: "a NUL byte", input: "etc/pass\x00wd", wantErr: ErrEscapesRoot},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := entryPath(tt.input)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("entryPath(%q) = (%q, %v), want %v", tt.input, got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("entryPath(%q) = %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("entryPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// A symlink inside the tree must be rebased against the destination, never
// followed to where it points on the host. That is what makes a layer
// containing "etc -> /etc" harmless: a later write to "etc/passwd" lands in the
// tree, exactly as it would for a process that had already pivoted.
func TestResolveWithinRebasesSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "etc"))
	mustSymlink(t, "/etc", filepath.Join(root, "link-to-absolute"))
	mustSymlink(t, "etc", filepath.Join(root, "link-to-relative"))
	mustSymlink(t, "../../..", filepath.Join(root, "link-that-escapes"))

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{
			name:  "an absolute symlink resolves inside the tree",
			input: filepath.Join("link-to-absolute", "passwd"),
			want:  filepath.Join(root, "etc", "passwd"),
		},
		{
			name:  "a relative symlink resolves inside the tree",
			input: filepath.Join("link-to-relative", "passwd"),
			want:  filepath.Join(root, "etc", "passwd"),
		},
		{
			name:  "the final component is not followed",
			input: "link-to-absolute",
			want:  filepath.Join(root, "link-to-absolute"),
		},
		{
			name:    "a symlink climbing out is refused",
			input:   filepath.Join("link-that-escapes", "etc", "shadow"),
			wantErr: ErrEscapesRoot,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveWithin(root, tt.input)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("resolveWithin(%q) = (%q, %v), want %v", tt.input, got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWithin(%q) = %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("resolveWithin(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if !strings.HasPrefix(got, root) {
				t.Errorf("resolveWithin(%q) = %q, which is outside %q", tt.input, got, root)
			}
		})
	}
}

// A symlink loop must fail the extraction rather than hang it.
func TestResolveWithinRefusesASymlinkLoop(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustSymlink(t, "b", filepath.Join(root, "a"))
	mustSymlink(t, "a", filepath.Join(root, "b"))

	_, err := resolveWithin(root, filepath.Join("a", "file"))
	if err == nil || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("resolveWithin() = %v, want a symlink-loop error", err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s = %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, path string) {
	t.Helper()

	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink %s -> %s = %v", path, target, err)
	}
}

// Timestamp restoration must not follow a symlink.
//
// This is the metadata half of containment, and it went unnoticed for a stage
// because its usual symptom is a log line rather than a failure. An image the
// size of alpine is several hundred busybox links, every one of them absolute;
// restoring their timestamps through os.Chtimes resolved each link against the
// *host*, which either failed loudly (the target does not exist there) or
// succeeded quietly on a host file while running as root.
func TestApplyTimesDoesNotFollowSymlinks(t *testing.T) {
	t.Parallel()

	dest := t.TempDir()

	// The host file an absolute link inside the layer would resolve to if the
	// link were followed. It stands in for "/etc/hostname" and friends.
	outside := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(outside, []byte("the host's"), 0o644); err != nil {
		t.Fatalf("writing %s = %v", outside, err)
	}
	before, err := os.Lstat(outside)
	if err != nil {
		t.Fatalf("lstat %s = %v", outside, err)
	}

	want := time.Unix(1600000000, 0)

	links := map[string]string{
		// Dangling within the tree, which is what a busybox link is at the
		// moment it is written.
		"bin/sh": "/bin/busybox",
		"bin/ls": "busybox",
		// Resolvable on the host, which is the case that silently wrote there.
		"bin/escape": outside,
	}

	layer := gzipTar(t, func(w *tar.Writer) {
		if err := w.WriteHeader(&tar.Header{
			Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: want,
		}); err != nil {
			t.Fatalf("writing bin/: %v", err)
		}
		for name, target := range links {
			if err := w.WriteHeader(&tar.Header{
				Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777, ModTime: want,
			}); err != nil {
				t.Fatalf("writing %s: %v", name, err)
			}
		}
	})

	if _, err := applyLayer(context.Background(), bytes.NewReader(layer), dest, nil); err != nil {
		t.Fatalf("applyLayer() = %v", err)
	}

	for name := range links {
		info, err := os.Lstat(filepath.Join(dest, name))
		if err != nil {
			t.Fatalf("lstat %s = %v", name, err)
		}
		if got := info.ModTime(); !got.Equal(want) {
			t.Errorf("%s mtime = %v, want the layer's %v", name, got, want)
		}
	}

	after, err := os.Lstat(outside)
	if err != nil {
		t.Fatalf("lstat %s = %v", outside, err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the host file's mtime changed from %v to %v; a symlink in the layer was followed out of the tree",
			before.ModTime(), after.ModTime())
	}
}
