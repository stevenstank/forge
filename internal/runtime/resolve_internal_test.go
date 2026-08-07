package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Child-side command resolution (Stage 5, §7.3).
//
// These run in the test process rather than in a container, so "the container's
// filesystem" is a temp directory. What they pin down is the logic; that it runs
// after pivot_root is a property of where Init calls it, which the integration
// suite covers.

func TestResolveCommandLeavesAPathAlone(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"/bin/sh", "./relative", "../up/one", "dir/binary"} {
		got, err := resolveCommand(name, []string{"PATH=/nowhere"})
		if err != nil {
			t.Fatalf("resolveCommand(%q) = %v", name, err)
		}
		if got != name {
			t.Errorf("resolveCommand(%q) = %q, want it untouched", name, got)
		}
	}
}

func TestResolveCommandSearchesPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")

	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s = %v", dir, err)
		}
	}

	// The same name in both directories: the earlier entry must win, as it
	// would for execvp.
	writeExecutable(t, filepath.Join(first, "tool"))
	writeExecutable(t, filepath.Join(second, "tool"))
	writeExecutable(t, filepath.Join(second, "only-later"))

	env := []string{"PATH=" + first + ":" + second}

	got, err := resolveCommand("tool", env)
	if err != nil {
		t.Fatalf("resolveCommand() = %v", err)
	}
	if want := filepath.Join(first, "tool"); got != want {
		t.Errorf("resolveCommand() = %q, want the first match %q", got, want)
	}

	got, err = resolveCommand("only-later", env)
	if err != nil {
		t.Fatalf("resolveCommand() = %v", err)
	}
	if want := filepath.Join(second, "only-later"); got != want {
		t.Errorf("resolveCommand() = %q, want %q", got, want)
	}
}

// A directory and a non-executable file both share a name with a binary
// someone might expect to run. Neither is a match.
func TestResolveCommandSkipsWhatItCannotExecute(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "tool"), 0o755); err != nil {
		t.Fatalf("mkdir = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write = %v", err)
	}

	for _, name := range []string{"tool", "data"} {
		_, err := resolveCommand(name, []string{"PATH=" + root})
		if !errors.Is(err, ErrCommandNotFound) {
			t.Errorf("resolveCommand(%q) = %v, want %v", name, err, ErrCommandNotFound)
		}
	}
}

// "not found" without saying where is the least useful error a container
// runtime can give.
func TestResolveCommandNamesTheSearchedPath(t *testing.T) {
	t.Parallel()

	_, err := resolveCommand("absent", []string{"PATH=/sbin:/usr/sbin"})
	if !errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("resolveCommand() = %v, want %v", err, ErrCommandNotFound)
	}
	for _, dir := range []string{"/sbin", "/usr/sbin"} {
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error %q does not name %s", err, dir)
		}
	}
}

func TestResolveCommandWithNoPathAtAll(t *testing.T) {
	t.Parallel()

	_, err := resolveCommand("ls", nil)
	if !errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("resolveCommand() = %v, want %v", err, ErrCommandNotFound)
	}
	if !strings.Contains(err.Error(), "no PATH") {
		t.Errorf("error %q does not say the container has no PATH", err)
	}
}

func TestPathFromEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{name: "nil", env: nil, want: nil},
		{name: "no PATH", env: []string{"HOME=/root"}, want: nil},
		{name: "one entry", env: []string{"PATH=/bin"}, want: []string{"/bin"}},
		{name: "several", env: []string{"PATH=/bin:/usr/bin"}, want: []string{"/bin", "/usr/bin"}},
		{
			// An empty element means "the current directory" to a shell.
			// Searching it would resolve against wherever -workdir left the
			// process, which nobody asked for.
			name: "empty elements are dropped",
			env:  []string{"PATH=/bin::/usr/bin:"},
			want: []string{"/bin", "/usr/bin"},
		},
		{
			// execve hands the program the last assignment, so the search must
			// agree with it or it would find a different binary.
			name: "the last assignment wins",
			env:  []string{"PATH=/first", "HOME=/root", "PATH=/second"},
			want: []string{"/second"},
		},
		{
			name: "a variable that merely ends in PATH is not one",
			env:  []string{"MANPATH=/usr/share/man"},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := pathFromEnv(tt.env); !slices.Equal(got, tt.want) {
				t.Errorf("pathFromEnv(%v) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

// The parent refuses this already. The child refuses it again because it is the
// process that would act on it: with no mount plan there is no pivot, so the
// search would run over the host's directories and execve the host's binary.
func TestDecodeInitPayloadRefusesAPathSearchWithNoFilesystem(t *testing.T) {
	t.Parallel()

	payload := `{"namespace":{},"command":["ls"],"env":["PATH=/bin"]}`

	_, err := decodeInitPayload(strings.NewReader(payload))
	if !errors.Is(err, ErrPathSearchWithoutRootfs) {
		t.Fatalf("decodeInitPayload() = %v, want %v", err, ErrPathSearchWithoutRootfs)
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()

	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing %s = %v", path, err)
	}
}
