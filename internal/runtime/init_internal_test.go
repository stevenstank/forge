package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
