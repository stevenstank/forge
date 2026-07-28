// Package logging constructs the structured loggers used throughout Forge.
//
// It is a leaf package: it must not import any other internal package, so that
// every layer of Forge can depend on it without creating a cycle.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns a logger that writes structured records at or above level to w.
//
// Forge uses slog's text handler rather than its JSON handler because Forge's
// logs are read by a human at a terminal while debugging a container, not
// shipped to a log aggregator.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// ParseLevel maps a case-insensitive level name onto a slog.Level.
//
// Only the four names documented in SSOT §6 are accepted. slog's own
// UnmarshalText additionally understands offset syntax such as "INFO+2", which
// Forge deliberately does not expose: the log level is a user-facing CLI flag
// and its accepted values should be enumerable in one line of help text.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q: want one of debug, info, warn, error", name)
	}
}
