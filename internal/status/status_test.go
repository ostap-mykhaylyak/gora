package status

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestServeAndQuery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets on Windows hit the path length limit of the temp dir")
	}

	socket := filepath.Join(t.TempDir(), "status.sock")
	want := Snapshot{Version: "test", PID: 42, UptimeSeconds: 7, Listen: "0.0.0.0:3306"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		errc <- Serve(ctx, socket, func() Snapshot { return want }, testLogger())
	}()

	snap, err := queryWhenReady(socket)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if snap.Version != want.Version || snap.PID != want.PID ||
		snap.UptimeSeconds != want.UptimeSeconds || snap.Listen != want.Listen {
		t.Fatalf("snapshot = %+v, want %+v", snap, want)
	}

	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("Serve returned %v, want nil on shutdown", err)
	}
}

// queryWhenReady retries until the listener is up: Serve runs in its own
// goroutine, so the first dial can legitimately lose the race.
func queryWhenReady(socket string) (Snapshot, error) {
	var (
		snap Snapshot
		err  error
	)
	for i := 0; i < 50; i++ {
		if snap, err = Query(socket); err == nil {
			return snap, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return snap, err
}

func TestQueryNotRunning(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "absent.sock")
	if _, err := Query(socket); err == nil {
		t.Fatal("Query on a missing socket returned nil error")
	}
}
