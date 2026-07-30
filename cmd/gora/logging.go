package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

// logFileName is the file gora writes inside log.path.
const logFileName = "gora.log"

// openLog returns the log destination and the function that closes it.
// A file is opened in append mode and never truncated: logrotate owns
// rotation (copytruncate in the shipped configuration), gora only writes.
func openLog(cfg config.Log) (io.Writer, func(), error) {
	switch cfg.Path {
	case "stdout":
		return os.Stdout, func() {}, nil
	case "stderr":
		return os.Stderr, func() {}, nil
	}

	if err := os.MkdirAll(cfg.Path, 0o750); err != nil {
		return nil, nil, fmt.Errorf("creating the log directory %s: %w", cfg.Path, err)
	}
	path := filepath.Join(cfg.Path, logFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the log file %s: %w", path, err)
	}
	return f, func() { _ = f.Close() }, nil
}

func newLogger(w io.Writer, cfg config.Log) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
