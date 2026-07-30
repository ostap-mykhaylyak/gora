package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/mysqltest"
)

// Everything the command wires together, exercised the way a WordPress
// installation would: a client connects to the address from config.yaml,
// runs a query and gets the backend's answer.
func TestServeProxiesTraffic(t *testing.T) {
	backend := mysqltest.Start(t, "gora", "backend-secret")
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := fmt.Sprintf(`
listen:
  address: %q
backend:
  address: %q
  username: "gora"
  password: "backend-secret"
users:
  - username: "wordpress"
    password: "client-secret"
status:
  socket: ""
log:
  path: "stdout"
`, addr, backend.Addr)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, path, slog.New(slog.DiscardHandler)) }()

	c := connectWhenReady(t, addr)
	r, err := c.Execute("SELECT 1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Values) != 1 {
		t.Fatalf("got %d rows, want 1", len(r.Values))
	}
	if backend.Count("SELECT 1") != 1 {
		t.Fatalf("the backend did not receive the query: %q", backend.Queries())
	}
	_ = c.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil on shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after the context was cancelled")
	}
}

// A backend that refuses connections must not stop gora from starting: the
// listener has to be up when MySQL comes back.
func TestServeStartsWithTheBackendDown(t *testing.T) {
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	cfg := config.Default()
	cfg.Listen.Address = addr
	cfg.Backend.Address = "127.0.0.1:1" // nothing listens there
	cfg.Backend.Username = "gora"
	cfg.Users = []config.User{{Username: "wordpress", Password: "client-secret"}}
	cfg.Status.Socket = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the test configuration is invalid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, "test", slog.New(slog.DiscardHandler)) }()

	waitForPort(t, addr)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil on shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after the context was cancelled")
	}
}

// freePort returns a port nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func waitForPort(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}

func connectWhenReady(t *testing.T, addr string) *client.Conn {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := client.Connect(addr, "wordpress", "client-secret", "wordpress")
		if err == nil {
			return c
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("could not connect to the proxy on %s: %v", addr, lastErr)
	return nil
}
