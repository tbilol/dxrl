// Package log builds auth-gateway's structured logger (log/slog).
package log

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns a logger writing JSON (default) or text at the given level
// ("debug", "info", "warn", "error").
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	switch strings.ToLower(format) {
	case "", "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("log format %q: want json or text", format)
	}
	return slog.New(h).With("service", "auth-gateway"), nil
}

// Discard is a logger that drops everything (tests).
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
