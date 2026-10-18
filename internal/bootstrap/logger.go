package bootstrap

import (
	"io"
	"log/slog"
)

// NewLogger returns a logger writing to w in cfg.LogFormat, dropping records
// below cfg.LogLevel.
func NewLogger(w io.Writer, cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == LogJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
