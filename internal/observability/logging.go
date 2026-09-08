// Package observability provides structured logging and the Prometheus
// compatible /metrics and /status endpoints.
package observability

import (
	"log/slog"
	"os"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// NewLogger builds the structured logger described by the configuration and
// installs it as the default, so that every package logs through it.
func NewLogger(cfg config.Logging) *slog.Logger {
	options := &slog.HandlerOptions{Level: levelOf(cfg.Level)}

	var handler slog.Handler
	if cfg.Format == config.FormatText {
		handler = slog.NewTextHandler(os.Stdout, options)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, options)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

// levelOf maps a configured level onto its slog counterpart.
func levelOf(level config.LogLevel) slog.Level {
	switch level {
	case config.LevelDebug:
		return slog.LevelDebug
	case config.LevelWarn:
		return slog.LevelWarn
	case config.LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
