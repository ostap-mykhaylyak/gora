package pool

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/mysqltest"
)

const (
	testUser     = "gora"
	testPassword = "secret"
)

func testConfig() config.Pool {
	return config.Pool{
		MaxOpen:        4,
		MaxIdle:        4,
		PingInterval:   config.Duration(time.Second),
		AcquireTimeout: config.Duration(2 * time.Second),
		Multiplexing:   true,
	}
}

func newPool(t *testing.T, addr string, cfg config.Pool) *Pool {
	t.Helper()
	backend := config.Backend{
		Address:        addr,
		Username:       testUser,
		Password:       testPassword,
		ConnectTimeout: config.Duration(2 * time.Second),
	}
	p, err := New(backend, cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func acquire(t *testing.T, p *Pool) *Conn {
	t.Helper()
	c, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return c
}

// A released connection must serve the next Acquire instead of a new dial:
// this is the whole point of the pool.
func TestAcquireReusesAParkedConnection(t *testing.T) {
	backend := mysqltest.Start(t, testUser, testPassword)
	p := newPool(t, backend.Addr, testConfig())

	c := acquire(t, p)
	p.Release(c)
	c2 := acquire(t, p)
	p.Release(c2)

	if n := backend.Accepted.Load(); n != 1 {
		t.Fatalf("backend accepted %d connections, want 1", n)
	}
	if got := p.Stat().Dials; got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
}

// Release must clear whatever the previous session left behind;
// ReleaseClean is the multiplexing path and skips the roundtrip because the
// caller has already established there is nothing to clear.
func TestReleaseResetsButReleaseCleanDoesNot(t *testing.T) {
	backend := mysqltest.Start(t, testUser, testPassword)
	p := newPool(t, backend.Addr, testConfig())

	p.Release(acquire(t, p))
	if n := backend.Resets.Load(); n != 1 {
		t.Fatalf("Release sent %d resets, want 1", n)
	}

	p.ReleaseClean(acquire(t, p))
	if n := backend.Resets.Load(); n != 1 {
		t.Fatalf("ReleaseClean sent a reset (total %d), want it to stay at 1", n)
	}
}

// When the cap is reached, a client waits and then gives up with an error
// naming max_open — and the wait is counted, because that counter is how
// you find out the cap is too low.
func TestAcquireWaitsThenTimesOut(t *testing.T) {
	backend := mysqltest.Start(t, testUser, testPassword)
	cfg := testConfig()
	cfg.MaxOpen = 1
	cfg.AcquireTimeout = config.Duration(150 * time.Millisecond)
	p := newPool(t, backend.Addr, cfg)

	held := acquire(t, p)
	defer p.Release(held)

	start := time.Now()
	_, err := p.Acquire(context.Background())
	if err == nil {
		t.Fatal("Acquire succeeded past max_open")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("Acquire gave up after %s, before the acquire timeout", elapsed)
	}

	st := p.Stat()
	if st.Waits != 1 || st.WaitTimeouts != 1 {
		t.Fatalf("waits = %d, timeouts = %d, want 1 and 1", st.Waits, st.WaitTimeouts)
	}
	if st.AvgWaitMillis <= 0 {
		t.Fatalf("avg wait = %v, want a positive duration", st.AvgWaitMillis)
	}
}

// A connection waiting in the pool is handed over as soon as it is released.
func TestAcquireGetsTheReleasedConnection(t *testing.T) {
	backend := mysqltest.Start(t, testUser, testPassword)
	cfg := testConfig()
	cfg.MaxOpen = 1
	p := newPool(t, backend.Addr, cfg)

	held := acquire(t, p)
	go func() {
		time.Sleep(50 * time.Millisecond)
		p.ReleaseClean(held)
	}()

	c, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	p.Release(c)
	if n := backend.Accepted.Load(); n != 1 {
		t.Fatalf("backend accepted %d connections, want 1", n)
	}
}

// max_lifetime is what makes a restarted or failed-over MySQL heal on its
// own: connections older than the limit are retired instead of reused.
func TestMaxLifetimeRetiresConnections(t *testing.T) {
	backend := mysqltest.Start(t, testUser, testPassword)
	cfg := testConfig()
	cfg.MaxLifetime = config.Duration(20 * time.Millisecond)
	p := newPool(t, backend.Addr, cfg)

	p.ReleaseClean(acquire(t, p))
	time.Sleep(40 * time.Millisecond)

	c := acquire(t, p)
	p.Release(c)

	if n := backend.Accepted.Load(); n != 2 {
		t.Fatalf("backend accepted %d connections, want 2 (the first one retired)", n)
	}
	if got := p.Stat().Retired; got != 1 {
		t.Fatalf("retired = %d, want 1", got)
	}
}

// An expired connection must not be parked either, or it would be handed
// out again by the next Acquire.
func TestExpiredConnectionIsNotParked(t *testing.T) {
	backend := mysqltest.Start(t, testUser, testPassword)
	cfg := testConfig()
	cfg.MaxLifetime = config.Duration(20 * time.Millisecond)
	p := newPool(t, backend.Addr, cfg)

	c := acquire(t, p)
	time.Sleep(40 * time.Millisecond)
	p.ReleaseClean(c)

	if idle := p.Stat().Idle; idle != 0 {
		t.Fatalf("idle = %d, want 0: an expired connection was parked", idle)
	}
}

// min_idle opens connections up front so the first visitors do not pay for
// the handshake.
func TestPrewarmOpensMinIdle(t *testing.T) {
	backend := mysqltest.Start(t, testUser, testPassword)
	cfg := testConfig()
	cfg.MinIdle = 2
	p := newPool(t, backend.Addr, cfg)

	waitFor(t, time.Second, func() bool { return p.Stat().Idle == 2 })
	if n := backend.Accepted.Load(); n != 2 {
		t.Fatalf("backend accepted %d connections, want 2", n)
	}
}

// With the backend down, clients must fail immediately instead of each
// waiting for its own timeout: that pile-up is what takes PHP-FPM with it.
func TestBreakerFailsFast(t *testing.T) {
	cfg := testConfig()
	cfg.AcquireTimeout = config.Duration(200 * time.Millisecond)
	cfg.Breaker = config.Breaker{
		Failures:      2,
		ProbeInterval: config.Duration(time.Hour), // no recovery during the test
	}
	// Port 1 on the loopback interface: nothing listens there.
	p := newPool(t, "127.0.0.1:1", cfg)

	for i := 0; i < 2; i++ {
		if _, err := p.Acquire(context.Background()); err == nil {
			t.Fatal("Acquire succeeded against a dead backend")
		}
	}

	start := time.Now()
	_, err := p.Acquire(context.Background())
	if !errors.Is(err, ErrBackendDown) {
		t.Fatalf("error = %v, want ErrBackendDown", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("the breaker took %s to refuse, want it immediate", elapsed)
	}
	if !p.Stat().BreakerOpen {
		t.Fatal("BreakerOpen = false, want true")
	}
}

// KILL QUERY runs on its own connection, so it works even when the pool is
// saturated by the very queries that need killing.
func TestKillQueryUsesAnotherConnection(t *testing.T) {
	backend := mysqltest.Start(t, testUser, testPassword)
	cfg := testConfig()
	cfg.MaxOpen = 1
	p := newPool(t, backend.Addr, cfg)

	held := acquire(t, p) // the pool is now saturated
	defer p.Release(held)

	if err := p.KillQuery(42); err != nil {
		t.Fatalf("KillQuery: %v", err)
	}
	if n := backend.Kills.Load(); n != 1 {
		t.Fatalf("backend saw %d kills, want 1", n)
	}
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition still false after %s", limit)
}
