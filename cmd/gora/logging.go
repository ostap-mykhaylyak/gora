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

// logLevel is the level in force, held in a variable the handler consults
// on every record: turning the log up to debug during an incident is the
// least welcome moment to restart the thing you are trying to observe.
var logLevel = new(slog.LevelVar)

func newLogger(w io.Writer, cfg config.Log) *slog.Logger {
	logLevel.Set(levelOf(cfg.Level))

	opts := &slog.HandlerOptions{Level: logLevel}
	if cfg.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func levelOf(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
