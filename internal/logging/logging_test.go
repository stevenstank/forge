package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    slog.Level
		wantErr bool
	}{
		{name: "debug", input: "debug", want: slog.LevelDebug},
		{name: "info", input: "info", want: slog.LevelInfo},
		{name: "warn", input: "warn", want: slog.LevelWarn},
		{name: "error", input: "error", want: slog.LevelError},
		{name: "mixed case is accepted", input: "WaRn", want: slog.LevelWarn},
		{name: "surrounding space is trimmed", input: "  info  ", want: slog.LevelInfo},
		{name: "empty is rejected", input: "", wantErr: true},
		{name: "unknown name is rejected", input: "verbose", wantErr: true},
		{name: "offset syntax is rejected", input: "INFO+2", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := logging.ParseLevel(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseLevel(%q) = %v, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLevel(%q) returned unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseLevelErrorNamesInput keeps the failure actionable without a
// debugger, per PRD NFR-7.
func TestParseLevelErrorNamesInput(t *testing.T) {
	t.Parallel()

	_, err := logging.ParseLevel("verbose")
	if err == nil {
		t.Fatal(`ParseLevel("verbose") = nil error, want error`)
	}
	if !strings.Contains(err.Error(), "verbose") {
		t.Errorf("error %q does not mention the offending input", err)
	}
}

func TestNewHonoursLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelWarn)

	log.Debug("debug message")
	log.Info("info message")
	if buf.Len() != 0 {
		t.Fatalf("records below the configured level were emitted: %q", buf.String())
	}

	log.Warn("warn message")
	if got := buf.String(); !strings.Contains(got, "warn message") {
		t.Errorf("output %q does not contain the warn record", got)
	}
}

func TestNewEmitsStructuredAttributes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelInfo)
	log.Info("container created", "container_id", "0123456789ab")

	if got := buf.String(); !strings.Contains(got, "container_id=0123456789ab") {
		t.Errorf("output %q does not contain the structured attribute", got)
	}
}
