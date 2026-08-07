package image

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fuzzing for the code that runs as root over bytes that came from the
// internet.
//
// The two defects this package shipped with — a whiteout naming no entry, which
// deleted its own parent, and a staging file that outlived a failed commit —
// were both found by reading the code adversarially rather than by any test.
// That is the wrong way round: the suite should be the thing that finds them.
//
// Each target below asserts a *property* rather than an expected output, which
// is what makes a random input meaningful:
//
//	entryPath        a name it accepts can never escape the destination
//	resolveWithin    a path it returns is always inside the root
//	applyLayer       whatever the archive contains, nothing outside the
//	                 destination is created, modified or removed
//	ParseReference   a reference it accepts round-trips through String()
//
// Seeds are the malicious shapes already known to matter, so a mutation engine
// starts from the interesting part of the space rather than from noise.

// fuzzMaxInput bounds an archive fuzz case. The extractor has no limit on the
// uncompressed size of a layer (see the audit), so an unbounded case could
// legitimately fill the disk and fail the run for a reason that is not a bug.
const fuzzMaxInput = 64 << 10

// FuzzEntryPath asserts the containment guarantee at the point it is decided:
// any name entryPath accepts, joined onto a root, stays under that root.
func FuzzEntryPath(f *testing.F) {
	for _, seed := range []string{
		"etc/passwd", "./etc/passwd", "etc/../etc/passwd", "../escape",
		"/absolute", "..", ".", "./", "a/../../b", "....//....//etc",
		"etc/pass\x00wd", "\\..\\..\\windows", ".wh.", ".wh..", ".wh..wh..opq",
		strings.Repeat("a/", 200) + "deep",
	} {
		f.Add(seed)
	}

	const root = "/container/rootfs"

	f.Fuzz(func(t *testing.T, name string) {
		got, err := entryPath(name)
		if err != nil {
			return
		}
		if got == "" {
			return
		}

		if filepath.IsAbs(got) {
			t.Fatalf("entryPath(%q) = %q, which is absolute", name, got)
		}

		joined := filepath.Clean(filepath.Join(root, got))
		if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
			t.Fatalf("entryPath(%q) = %q, which resolves to %q outside %q", name, got, joined, root)
		}
	})
}

// FuzzResolveWithin asserts the same guarantee against a real directory tree
// containing the symlinks an attacker would plant.
func FuzzResolveWithin(f *testing.F) {
	for _, seed := range []string{
		"etc/passwd", "up/etc/shadow", "abs/passwd", "rel/passwd",
		"loop/x", "deep/a/b/c", "..", "../..", "abs/../../../etc/shadow",
	} {
		f.Add(seed)
	}

	root := f.TempDir()
	for _, dir := range []string{"etc", "deep/a/b", "real"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			f.Fatalf("building the fuzz tree: %v", err)
		}
	}
	// The three shapes that matter: a link out of the tree, an absolute link,
	// and a loop.
	mustLink(f, "../../../../../../etc", filepath.Join(root, "up"))
	mustLink(f, "/etc", filepath.Join(root, "abs"))
	mustLink(f, "real", filepath.Join(root, "rel"))
	mustLink(f, "loop2", filepath.Join(root, "loop"))
	mustLink(f, "loop", filepath.Join(root, "loop2"))

	cleaned := filepath.Clean(root)

	f.Fuzz(func(t *testing.T, name string) {
		// resolveWithin's contract starts where entryPath's ends.
		relative, err := entryPath(name)
		if err != nil || relative == "" {
			return
		}

		got, err := resolveWithin(root, relative)
		if err != nil {
			return
		}

		got = filepath.Clean(got)
		if got != cleaned && !strings.HasPrefix(got, cleaned+string(filepath.Separator)) {
			t.Fatalf("resolveWithin(%q) = %q, which is outside %q", relative, got, cleaned)
		}
	})
}

// FuzzApplyLayer is the end-to-end property: whatever an archive contains,
// applying it touches nothing outside the destination.
//
// The assertion is made against a canary tree that sits beside the destination,
// exactly where a "../" would land. An extractor that escaped would modify or
// delete it.
func FuzzApplyLayer(f *testing.F) {
	f.Add(buildFuzzSeed(f, "etc/passwd", "root:x:0:0"))
	f.Add(buildFuzzSeed(f, "../escape", "outside"))
	f.Add(buildFuzzSeed(f, "/absolute", "outside"))
	f.Add(buildFuzzSeed(f, ".wh.", ""))
	f.Add(buildFuzzSeed(f, "etc/.wh..", ""))
	f.Add(buildFuzzSeed(f, ".wh..wh..opq", ""))
	f.Add([]byte{0x1f, 0x8b, 0x08, 0x00})
	f.Add([]byte("not an archive at all"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInput {
			t.Skip("input above the size bound; see fuzzMaxInput")
		}

		parent := t.TempDir()
		dest := filepath.Join(parent, "rootfs")
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatalf("mkdir %s = %v", dest, err)
		}

		// The canary sits where an escaping entry would land.
		canary := filepath.Join(parent, "canary")
		const canaryContent = "this file is outside the destination"
		if err := os.WriteFile(canary, []byte(canaryContent), 0o644); err != nil {
			t.Fatalf("writing the canary: %v", err)
		}

		// A nil cache is the extractor with no logger and no blob store, which
		// is what makes this callable without any I/O beyond the destination.
		_, _ = applyLayer(context.Background(), bytes.NewReader(data), dest, nil)

		// 1. The destination itself must still be there. A whiteout naming no
		//    entry used to remove it.
		info, err := os.Stat(dest)
		if err != nil {
			t.Fatalf("the destination was removed: %v", err)
		}
		if !info.IsDir() {
			t.Fatalf("the destination is no longer a directory: %v", info.Mode())
		}

		// 2. Nothing outside it may have been touched.
		got, err := os.ReadFile(canary)
		if err != nil {
			t.Fatalf("the canary outside the destination was removed: %v", err)
		}
		if string(got) != canaryContent {
			t.Fatalf("the canary outside the destination was modified: %q", got)
		}

		// 3. And nothing new may have appeared beside it.
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatalf("reading %s = %v", parent, err)
		}
		for _, entry := range entries {
			switch entry.Name() {
			case "rootfs", "canary":
			default:
				t.Fatalf("%q was created outside the destination", entry.Name())
			}
		}
	})
}

// buildFuzzSeed renders a one-entry uncompressed tar, for seeding FuzzApplyLayer
// with the shapes that are known to matter.
func buildFuzzSeed(f *testing.F, name, body string) []byte {
	f.Helper()

	var buf bytes.Buffer
	if err := writeSingleEntryTar(&buf, name, body); err != nil {
		f.Fatalf("building a fuzz seed: %v", err)
	}
	return buf.Bytes()
}

func mustLink(f *testing.F, target, path string) {
	f.Helper()

	if err := os.Symlink(target, path); err != nil {
		f.Fatalf("symlink %s -> %s = %v", path, target, err)
	}
}

// FuzzParseReference asserts the property that makes a reference usable: one
// the parser accepts renders to a string that parses back to the same thing.
//
// A reference is interpolated into a URL path, so the parser is also the only
// thing standing between a user's typo and a request to somewhere unintended.
// The round trip is what proves normalisation is total rather than
// approximately right.
func FuzzParseReference(f *testing.F) {
	for _, seed := range []string{
		"alpine", "alpine:3.20", "library/alpine", "docker.io/library/alpine:3.20",
		"ghcr.io/org/image:latest", "localhost:5000/img", "127.0.0.1:5000/a/b:v1",
		"alpine@sha256:" + strings.Repeat("a", 64),
		"alpine:3.20@sha256:" + strings.Repeat("b", 64),
		"", ":", "alpine:", "ALPINE", "a//b", "a/../b", "-x/y", "x-/y",
		"user@evil.com/repo", "alpine:3.20/rc1", "..", "/", "//",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		ref, err := ParseReference(s)
		if err != nil {
			return
		}

		// A repository reaches the URL path unescaped, so the grammar has to
		// have excluded everything that could change what that path means.
		//
		// The property is about path *segments*, not substrings: "0..0" is an
		// ordinary segment name that no resolver collapses, while a standalone
		// ".." segment is a traversal. An earlier version of this target
		// asserted the substring and reported "0..0" as a defect, which it is
		// not — the assertion was wrong, and the distinction is the whole point.
		for _, segment := range strings.Split(ref.Repository, "/") {
			switch segment {
			case "", ".", "..":
				t.Fatalf("ParseReference(%q) accepted repository %q with the segment %q",
					s, ref.Repository, segment)
			}
		}
		for _, forbidden := range []string{"//", "@", "?", "#", "%", " ", "\\"} {
			if strings.Contains(ref.Repository, forbidden) {
				t.Fatalf("ParseReference(%q) accepted repository %q containing %q",
					s, ref.Repository, forbidden)
			}
		}
		if strings.HasPrefix(ref.Repository, "/") {
			t.Fatalf("ParseReference(%q) accepted a repository starting with a separator: %q", s, ref.Repository)
		}

		// The host goes into the URL authority, where an empty label means the
		// reference names something that cannot exist.
		host, _, _ := strings.Cut(ref.Registry, ":")
		if host == "" || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") ||
			strings.Contains(host, "..") {
			t.Fatalf("ParseReference(%q) accepted the registry host %q", s, ref.Registry)
		}

		// Exactly one of the two ways of naming content is set.
		if (ref.Tag == "") == (ref.Digest == "") {
			t.Fatalf("ParseReference(%q) = tag %q and digest %q; exactly one must be set",
				s, ref.Tag, ref.Digest)
		}

		again, err := ParseReference(ref.String())
		if err != nil {
			t.Fatalf("ParseReference(%q) rendered %q, which does not parse: %v", s, ref.String(), err)
		}
		if again.String() != ref.String() {
			t.Fatalf("ParseReference(%q) does not round-trip: %q then %q", s, ref.String(), again.String())
		}
	})
}
