package runtime

import (
	"errors"
	"slices"
	"testing"

	"github.com/stevenstank/forge/internal/image"
)

// The rule apply implements is one sentence — the image supplies the default,
// the caller overrides it — and every row below is one way of getting that
// backwards.

func testImage(config image.Config) *containerImage {
	ref, err := image.ParseReference("alpine:3.20")
	if err != nil {
		panic(err)
	}
	return &containerImage{ref: ref, config: config}
}

func TestApplyResolvesTheCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config image.Config
		spec   Spec
		want   []string
	}{
		{
			name:   "the image supplies entrypoint and cmd",
			config: image.Config{Entrypoint: []string{"/bin/ls"}, Cmd: []string{"-l", "/etc"}},
			want:   []string{"/bin/ls", "-l", "/etc"},
		},
		{
			name:   "positional arguments replace cmd and keep entrypoint",
			config: image.Config{Entrypoint: []string{"/bin/ls"}, Cmd: []string{"-l"}},
			spec:   Spec{Command: []string{"-a", "/srv"}},
			want:   []string{"/bin/ls", "-a", "/srv"},
		},
		{
			name:   "an image with only a cmd",
			config: image.Config{Cmd: []string{"/bin/sh"}},
			want:   []string{"/bin/sh"},
		},
		{
			name:   "the caller's command with no entrypoint to prepend",
			config: image.Config{Cmd: []string{"/bin/sh"}},
			spec:   Spec{Command: []string{"/bin/echo", "hi"}},
			want:   []string{"/bin/echo", "hi"},
		},
		{
			name:   "a bare name survives to be resolved child-side",
			config: image.Config{},
			spec:   Spec{Command: []string{"ls"}},
			want:   []string{"ls"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := testImage(tt.config).apply(tt.spec)
			if err != nil {
				t.Fatalf("apply() = %v", err)
			}
			if !slices.Equal(got.Command, tt.want) {
				t.Errorf("Command = %v, want %v", got.Command, tt.want)
			}
		})
	}
}

// An image with no entrypoint and no cmd, run with no command, has nothing to
// execute. The error has to say what to type instead.
func TestApplyReportsAnImageWithNoCommand(t *testing.T) {
	t.Parallel()

	_, err := testImage(image.Config{}).apply(Spec{})
	if !errors.Is(err, ErrNoCommand) {
		t.Fatalf("apply() = %v, want %v", err, ErrNoCommand)
	}
}

func TestApplyMergesTheEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config image.Config
		spec   Spec
		want   []string
	}{
		{
			name:   "the image's environment is used as-is",
			config: image.Config{Cmd: []string{"/bin/sh"}, Env: []string{"PATH=/usr/bin", "LANG=C"}},
			want:   []string{"PATH=/usr/bin", "LANG=C"},
		},
		{
			name:   "the caller wins per variable, and order is stable",
			config: image.Config{Cmd: []string{"/bin/sh"}, Env: []string{"PATH=/usr/bin", "LANG=C"}},
			spec:   Spec{Env: []string{"PATH=/opt/bin", "EXTRA=1"}},
			want:   []string{"PATH=/opt/bin", "LANG=C", "EXTRA=1"},
		},
		{
			name:   "an image with no environment falls back to forge's default",
			config: image.Config{Cmd: []string{"/bin/sh"}},
			want:   DefaultEnv(),
		},
		{
			name:   "the caller's environment stands when the image has none",
			config: image.Config{Cmd: []string{"/bin/sh"}},
			spec:   Spec{Env: []string{"ONLY=mine"}},
			want:   []string{"ONLY=mine"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := testImage(tt.config).apply(tt.spec)
			if err != nil {
				t.Fatalf("apply() = %v", err)
			}
			if !slices.Equal(got.Env, tt.want) {
				t.Errorf("Env = %v, want %v", got.Env, tt.want)
			}
		})
	}
}

func TestApplyWorkingDir(t *testing.T) {
	t.Parallel()

	config := image.Config{Cmd: []string{"/bin/sh"}, WorkingDir: "/srv"}

	fromImage, err := testImage(config).apply(Spec{})
	if err != nil {
		t.Fatalf("apply() = %v", err)
	}
	if fromImage.WorkingDir != "/srv" {
		t.Errorf("WorkingDir = %q, want the image's", fromImage.WorkingDir)
	}

	overridden, err := testImage(config).apply(Spec{WorkingDir: "/opt"})
	if err != nil {
		t.Fatalf("apply() = %v", err)
	}
	if overridden.WorkingDir != "/opt" {
		t.Errorf("WorkingDir = %q, want the caller's", overridden.WorkingDir)
	}
}

// Nothing in an OCI config feeds a hostname, a limit or a network mode, so
// apply must leave all three exactly as the caller set them.
func TestApplyTouchesNothingElse(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Command:      []string{"/bin/sh"},
		Hostname:     "chosen",
		Network:      "none",
		NetworkMTU:   1400,
		ReadonlyRoot: true,
	}

	got, err := testImage(image.Config{Cmd: []string{"/bin/false"}, WorkingDir: "/srv"}).apply(spec)
	if err != nil {
		t.Fatalf("apply() = %v", err)
	}

	if got.Hostname != "chosen" || got.Network != "none" || got.NetworkMTU != 1400 || !got.ReadonlyRoot {
		t.Errorf("apply() changed something outside its remit: %+v", got)
	}
}

// resolveImage is a no-op for a container with no image, and must not touch the
// network or the cache to establish that.
func TestResolveImageIgnoresASpecWithNoImage(t *testing.T) {
	t.Parallel()

	var r Runner // no cache and no client: using either would panic

	img, err := r.resolveImage(t.Context(), nil, Spec{Command: []string{"/bin/sh"}})
	if err != nil {
		t.Fatalf("resolveImage() = %v", err)
	}
	if img != nil {
		t.Errorf("resolveImage() = %+v, want nil for a spec with no image", img)
	}
}
