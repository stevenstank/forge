package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/namespace"
)

// The init payload crosses a process boundary, so its encoding is a contract
// between the parent and the container's init. These tests pin that contract
// without needing a real pipe or a real container.

func TestDecodeInitPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	want := initPayload{
		Namespace: namespace.Config{PID: true, UTS: true, Mount: true, Hostname: "forge-test"},
		Command:   []string{"/bin/echo", "hello", "world"},
		Env:       []string{"PATH=/bin", "HOME=/root"},
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshalling payload: %v", err)
	}

	got, err := decodeInitPayload(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("decodeInitPayload() = %v", err)
	}

	if got.Namespace != want.Namespace {
		t.Errorf("Namespace = %+v, want %+v", got.Namespace, want.Namespace)
	}
	if strings.Join(got.Command, "\x00") != strings.Join(want.Command, "\x00") {
		t.Errorf("Command = %q, want %q", got.Command, want.Command)
	}
	if strings.Join(got.Env, "\x00") != strings.Join(want.Env, "\x00") {
		t.Errorf("Env = %q, want %q", got.Env, want.Env)
	}
}

// TestDecodeInitPayloadPreservesNamespaceConfig is the specific guarantee that
// matters: if the namespace configuration did not survive the boundary, the
// container would silently run without the isolation the user asked for.
func TestDecodeInitPayloadPreservesNamespaceConfig(t *testing.T) {
	t.Parallel()

	configs := []namespace.Config{
		{},
		{PID: true},
		{UTS: true, Hostname: "h"},
		{Mount: true},
		{PID: true, UTS: true, Mount: true, Hostname: "forge-abc123"},
	}

	for _, want := range configs {
		encoded, err := json.Marshal(initPayload{Namespace: want, Command: []string{"/bin/true"}})
		if err != nil {
			t.Fatalf("marshalling %+v: %v", want, err)
		}

		got, err := decodeInitPayload(bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("decodeInitPayload() = %v", err)
		}
		if got.Namespace != want {
			t.Errorf("Namespace = %+v, want %+v", got.Namespace, want)
		}
		if got.Namespace.CloneFlags() != want.CloneFlags() {
			t.Errorf("CloneFlags = %#x, want %#x", got.Namespace.CloneFlags(), want.CloneFlags())
		}
	}
}

func TestDecodeInitPayloadRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr error
		wantMsg string
	}{
		{
			name:    "empty input means no payload arrived",
			input:   "",
			wantErr: ErrNoInitPayload,
		},
		{
			name:    "malformed json",
			input:   "{not json",
			wantMsg: "decoding init payload",
		},
		{
			name:    "no command",
			input:   `{"namespace":{"PID":true},"command":[]}`,
			wantErr: ErrNoCommand,
		},
		{
			name:    "null command",
			input:   `{"namespace":{"PID":true}}`,
			wantErr: ErrNoCommand,
		},
		{
			name:    "empty binary path",
			input:   `{"command":[""]}`,
			wantErr: ErrNoCommand,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := decodeInitPayload(strings.NewReader(tt.input))
			if err == nil {
				t.Fatalf("decodeInitPayload(%q) = nil, want an error", tt.input)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("decodeInitPayload(%q) = %v, want %v", tt.input, err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not contain %q", err, tt.wantMsg)
			}
		})
	}
}

// TestDecodeInitPayloadReportsReadFailure ensures a truncated pipe is reported
// rather than treated as an empty payload.
func TestDecodeInitPayloadReportsReadFailure(t *testing.T) {
	t.Parallel()

	_, err := decodeInitPayload(errReader{errors.New("pipe broken")})
	if err == nil {
		t.Fatal("decodeInitPayload() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "reading init payload") {
		t.Errorf("error %q does not describe the read failure", err)
	}
}

// errReader always fails, standing in for a broken pipe.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// --- Stage 2 --------------------------------------------------------------

// The mount plan crosses the same boundary as the namespace config, and with
// the same consequence if it does not survive intact: the container would run
// on the host's filesystem while Forge reported that it had isolated it.

func TestDecodeInitPayloadPreservesTheMountPlan(t *testing.T) {
	t.Parallel()

	want := initPayload{
		Namespace: namespace.Config{PID: true, UTS: true, Mount: true, Hostname: "forge-test"},
		Command:   []string{"/bin/sh"},
		Mount: &mount.Plan{
			Source:       "/srv/images/alpine",
			Root:         "/var/lib/forge/containers/a1b2c3d4e5f6/rootfs",
			ReadonlyRoot: true,
			Mounts: []mount.Mount{
				{Destination: "/proc", Type: mount.TypeProc, Options: []mount.Option{mount.OptionNoSuid}},
				{Source: "/host/data", Destination: "/data", Type: mount.TypeBind, Options: []mount.Option{mount.OptionReadOnly}},
				{Destination: "/dev", Type: mount.TypeTmpfs, Data: "mode=755"},
			},
		},
		WorkingDir: "/data",
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshalling payload: %v", err)
	}

	got, err := decodeInitPayload(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("decodeInitPayload() = %v", err)
	}

	if got.Mount == nil {
		t.Fatal("Mount = nil, want the plan to survive the boundary")
	}
	if got.Mount.Source != want.Mount.Source {
		t.Errorf("Source = %q, want %q", got.Mount.Source, want.Mount.Source)
	}
	if got.Mount.Root != want.Mount.Root {
		t.Errorf("Root = %q, want %q", got.Mount.Root, want.Mount.Root)
	}
	if got.Mount.ReadonlyRoot != want.Mount.ReadonlyRoot {
		t.Errorf("ReadonlyRoot = %v, want %v", got.Mount.ReadonlyRoot, want.Mount.ReadonlyRoot)
	}
	if got.WorkingDir != want.WorkingDir {
		t.Errorf("WorkingDir = %q, want %q", got.WorkingDir, want.WorkingDir)
	}

	if len(got.Mount.Mounts) != len(want.Mount.Mounts) {
		t.Fatalf("got %d mounts, want %d", len(got.Mount.Mounts), len(want.Mount.Mounts))
	}
	for i, wantMount := range want.Mount.Mounts {
		gotMount := got.Mount.Mounts[i]
		if gotMount.Source != wantMount.Source || gotMount.Destination != wantMount.Destination {
			t.Errorf("mount %d = %q→%q, want %q→%q", i,
				gotMount.Source, gotMount.Destination, wantMount.Source, wantMount.Destination)
		}
		if gotMount.Type != wantMount.Type || gotMount.Data != wantMount.Data {
			t.Errorf("mount %d type/data = %q/%q, want %q/%q", i,
				gotMount.Type, gotMount.Data, wantMount.Type, wantMount.Data)
		}
		// Flags are derived from Options rather than sent as a bitmask, so the
		// wire format stays readable in a debug log and verifiable here.
		if gotMount.Flags() != wantMount.Flags() {
			t.Errorf("mount %d flags = %#x, want %#x", i, gotMount.Flags(), wantMount.Flags())
		}
	}
}

// TestDecodeInitPayloadWithoutAMountPlan is what keeps Stage 1 working: no
// --rootfs means no plan, and the init must run the container against the
// host's filesystem exactly as it did before Stage 2.
func TestDecodeInitPayloadWithoutAMountPlan(t *testing.T) {
	t.Parallel()

	got, err := decodeInitPayload(strings.NewReader(`{"namespace":{"PID":true},"command":["/bin/echo","hi"]}`))
	if err != nil {
		t.Fatalf("decodeInitPayload() = %v", err)
	}
	if got.Mount != nil {
		t.Errorf("Mount = %+v, want nil for a container with no rootfs", got.Mount)
	}
	if got.WorkingDir != "" {
		t.Errorf("WorkingDir = %q, want empty", got.WorkingDir)
	}
}

// TestDecodeInitPayloadRejectsAnInvalidPlan makes the container's init the last
// line of defence. The parent validates the plan too, but the init is the
// process that would perform the mounts, and a plan that escapes the container
// root must never reach mount(2).
func TestDecodeInitPayloadRejectsAnInvalidPlan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "destination escapes the root",
			input: `{"command":["/bin/sh"],"mount":{"source":"/srv/alpine","root":"/var/lib/forge/containers/abc/rootfs","mounts":[{"source":"/host","destination":"/../../etc","type":""}]}}`,
		},
		{
			name:  "the container root is the host root",
			input: `{"command":["/bin/sh"],"mount":{"source":"/srv/alpine","root":"/"}}`,
		},
		{
			name:  "the root is relative",
			input: `{"command":["/bin/sh"],"mount":{"source":"/srv/alpine","root":"rootfs"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := decodeInitPayload(strings.NewReader(tt.input)); err == nil {
				t.Fatalf("decodeInitPayload(%s) = nil, want an error", tt.input)
			}
		})
	}
}
