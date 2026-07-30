// Package status exposes gora's runtime state on a local unix socket.
//
// The socket is read-only and answers one JSON snapshot per connection: no
// commands, no mutation, nothing that can change how the proxy behaves.
// `gora status` is only a client of this socket, so asking a running
// instance how it is doing never disturbs the traffic it is serving.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/pool"
	"github.com/ostap-mykhaylyak/gora/internal/proxy"
)

// Snapshot is the state of a running instance. It grows with every
// milestone: the cache and topology sections land with the features they
// describe.
type Snapshot struct {
	Version       string `json:"version"`
	PID           int    `json:"pid"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	ConfigPath    string `json:"config_path"`
	Listen        string `json:"listen"`
	Backend       string `json:"backend"`

	Clients proxy.Stats `json:"clients"`
	Pool    pool.Stats  `json:"pool"`
}

// Uptime returns the uptime as a duration.
func (s Snapshot) Uptime() time.Duration {
	return time.Duration(s.UptimeSeconds) * time.Second
}

// Serve listens on the unix socket until ctx is cancelled, answering every
// connection with the snapshot returned by collect.
//
// A leftover socket file from a crashed instance is removed first: refusing
// to start because of a stale file would turn a crash into an outage.
func Serve(ctx context.Context, socket string, collect func() Snapshot, log *slog.Logger) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o750); err != nil {
		return fmt.Errorf("creating the status socket directory: %w", err)
	}
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the stale status socket: %w", err)
	}

	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socket, err)
	}
	// 0660: the gora user writes it, an administrator in the same group
	// reads it. It carries no credentials, but it does describe the traffic.
	if err := os.Chmod(socket, 0o660); err != nil {
		log.Warn("could not set the status socket permissions", "socket", socket, "error", err)
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = os.Remove(socket)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // listener closed on shutdown
			}
			return fmt.Errorf("accepting on %s: %w", socket, err)
		}
		go answer(conn, collect(), log)
	}
}

func answer(conn net.Conn, snap Snapshot, log *slog.Logger) {
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(snap); err != nil {
		log.Debug("status client went away", "error", err)
	}
}

// Query connects to a running instance and reads its snapshot.
func Query(socket string) (Snapshot, error) {
	var snap Snapshot

	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return snap, fmt.Errorf("connecting to %s: %w", socket, err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewDecoder(conn).Decode(&snap); err != nil {
		return snap, fmt.Errorf("reading the status snapshot: %w", err)
	}
	return snap, nil
}
