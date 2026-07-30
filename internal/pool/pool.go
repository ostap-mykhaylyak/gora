// Package pool manages gora's connections to the MySQL backend.
//
// Backend connections are authenticated once with gora's own credentials and
// reused across client sessions: when a session is done with one, it is
// reset and parked instead of being closed, so hundreds of short-lived PHP
// requests share a handful of real MySQL connections. A keepalive loop pings
// parked connections so MySQL's wait_timeout never closes them behind
// gora's back.
package pool

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

// Dialer opens the raw connection to the backend; tests replace it.
type Dialer = client.Dialer

// ErrBackendDown is returned by Acquire while the circuit breaker is open.
var ErrBackendDown = errors.New("backend unavailable (circuit breaker open)")

// Conn is a pooled backend connection.
type Conn struct {
	*client.Conn

	// BoundDB is the database the connection is currently bound to and
	// VarSig identifies the session variables applied to it. Both are
	// maintained by the proxy layer so a connection hopping between sessions
	// skips the COM_INIT_DB and SET roundtrips it does not need.
	BoundDB string
	VarSig  string

	// ThreadID is MySQL's connection id, used by KILL QUERY.
	ThreadID uint32

	createdAt time.Time // when it was dialed, for max_lifetime
	pooledAt  time.Time // when it was last parked, for idle_timeout
	lastPing  time.Time // last successful COM_PING or command
}

// Pool hands out backend connections up to a configured cap.
type Pool struct {
	backend config.Backend
	cfg     config.Pool
	dialer  Dialer
	tlsConf *tls.Config // nil = plaintext backend connections
	log     *slog.Logger

	// openSem holds one token per open backend connection (leased or idle),
	// so the cap is enforced without a mutex.
	openSem chan struct{}
	// idle holds parked connections ready for reuse.
	idle chan *Conn

	resetUnsupported atomic.Bool

	// Circuit breaker: after cfg.Breaker.Failures consecutive dial failures
	// Acquire fails immediately until a background probe reaches the backend
	// again.
	breakerFails atomic.Int32
	breakerOpen  atomic.Bool

	counters counters

	stop   chan struct{}
	closed atomic.Bool
}

// counters are the numbers you need to size a pool: how often clients had to
// wait, how often they gave up, how much churn there is.
type counters struct {
	dials        atomic.Uint64
	discards     atomic.Uint64
	retired      atomic.Uint64 // closed because max_lifetime elapsed
	waits        atomic.Uint64
	waitTimeouts atomic.Uint64
	waitNanos    atomic.Uint64
}

// New creates the pool, opens min_idle connections in the background and
// starts the keepalive loop. A nil dialer means plain TCP.
func New(backend config.Backend, cfg config.Pool, log *slog.Logger, dialer Dialer) (*Pool, error) {
	if dialer == nil {
		d := &net.Dialer{Timeout: backend.ConnectTimeout.Std()}
		dialer = d.DialContext
	}
	tlsConf, err := backendTLS(backend)
	if err != nil {
		return nil, err
	}

	p := &Pool{
		backend: backend,
		cfg:     cfg,
		dialer:  dialer,
		tlsConf: tlsConf,
		log:     log,
		openSem: make(chan struct{}, cfg.MaxOpen),
		idle:    make(chan *Conn, cfg.MaxOpen),
		stop:    make(chan struct{}),
	}

	go p.keepalive()
	if cfg.MinIdle > 0 {
		// In the background: a backend that is not up yet must delay
		// traffic, not startup.
		go p.prewarm()
	}
	return p, nil
}

// backendTLS builds the TLS configuration for backend connections.
func backendTLS(backend config.Backend) (*tls.Config, error) {
	if !backend.TLS.Enabled {
		return nil, nil
	}
	host, _, err := net.SplitHostPort(backend.Address)
	if err != nil {
		return nil, fmt.Errorf("backend.address: %w", err)
	}
	conf := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: backend.TLS.SkipVerify,
	}
	if backend.TLS.CA != "" {
		pem, err := os.ReadFile(backend.TLS.CA)
		if err != nil {
			return nil, fmt.Errorf("reading backend.tls.ca: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("backend.tls.ca %s: no certificates found", backend.TLS.CA)
		}
		conf.RootCAs = roots
	}
	return conf, nil
}

// prewarm opens min_idle connections so the first visitors do not pay for
// the TCP handshake and the MySQL authentication.
func (p *Pool) prewarm() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opened := 0
	for i := 0; i < p.cfg.MinIdle; i++ {
		select {
		case p.openSem <- struct{}{}:
		default:
			return
		}
		c, err := p.dial(ctx)
		if err != nil {
			<-p.openSem
			p.log.Warn("could not pre-open backend connections", "opened", opened, "error", err)
			return
		}
		p.park(c)
		opened++
	}
	p.log.Info("backend connections pre-opened", "count", opened)
}

// Acquire returns a backend connection: a parked one when available,
// otherwise a new one if the cap allows, otherwise it waits until a
// connection frees up or the acquire timeout expires.
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
	// Fail fast while the backend is down: without this, every PHP worker
	// piles up waiting for its own timeout and the web server dies before
	// the database comes back.
	if p.breakerOpen.Load() {
		return nil, ErrBackendDown
	}

	ctx, cancel := context.WithTimeout(ctx, p.cfg.AcquireTimeout.Std())
	defer cancel()

	// Fast path: something is immediately available.
	for {
		select {
		case c := <-p.idle:
			if p.revive(c) {
				return c, nil
			}
			continue // dead or retired, try the next one
		default:
		}
		select {
		case p.openSem <- struct{}{}:
			c, err := p.dial(ctx)
			if err != nil {
				<-p.openSem
				return nil, err
			}
			return c, nil
		default:
		}
		break
	}

	// Slow path: the pool is saturated. This is the number that tells you
	// max_open is too small, so it is worth counting separately.
	p.counters.waits.Add(1)
	start := time.Now()
	defer func() { p.counters.waitNanos.Add(uint64(time.Since(start))) }()

	for {
		select {
		case c := <-p.idle:
			if p.revive(c) {
				return c, nil
			}
		case p.openSem <- struct{}{}:
			c, err := p.dial(ctx)
			if err != nil {
				<-p.openSem
				return nil, err
			}
			return c, nil
		case <-ctx.Done():
			p.counters.waitTimeouts.Add(1)
			return nil, fmt.Errorf("waiting for a backend connection (pool.max_open=%d reached): %w",
				p.cfg.MaxOpen, ctx.Err())
		}
	}
}

// Release parks a connection whose session state is unknown. It is reset
// first so nothing leaks to the next client; if the reset fails the
// connection is closed instead of being handed over dirty.
func (p *Pool) Release(c *Conn) {
	if p.closed.Load() || p.expired(c) {
		p.Discard(c)
		return
	}
	if err := p.ResetConn(c); err != nil {
		p.log.Debug("closing backend connection, reset failed", "error", err)
		p.Discard(c)
		return
	}
	if len(p.idle) >= p.cfg.MaxIdle {
		p.Discard(c)
		return
	}
	p.park(c)
}

// ReleaseClean parks a connection whose state is known clean, which is the
// multiplexing path between two statements. No reset roundtrip, and no
// max_idle trimming: a burst of concurrently released connections must not
// be closed just to be dialed again a moment later — idle_timeout is what
// shrinks the pool, slowly.
func (p *Pool) ReleaseClean(c *Conn) {
	if p.closed.Load() || p.expired(c) {
		p.Discard(c)
		return
	}
	p.park(c)
}

// park puts the connection in the idle buffer.
func (p *Pool) park(c *Conn) {
	now := time.Now()
	c.pooledAt = now
	c.lastPing = now
	select {
	case p.idle <- c:
	default:
		p.Discard(c)
	}
}

// Discard closes a connection and frees its slot in the pool.
func (p *Pool) Discard(c *Conn) {
	p.counters.discards.Add(1)
	_ = c.Conn.Close()
	<-p.openSem
}

// Close shuts down the keepalive loop and closes the parked connections.
// Leased connections are closed by the sessions holding them.
func (p *Pool) Close() {
	if p.closed.Swap(true) {
		return
	}
	close(p.stop)
	for {
		select {
		case c := <-p.idle:
			_ = c.Conn.Close()
			<-p.openSem
		default:
			return
		}
	}
}

func (p *Pool) dial(ctx context.Context) (*Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, p.backend.ConnectTimeout.Std())
	defer cancel()

	c, err := client.ConnectWithDialer(ctx, "tcp", p.backend.Address,
		p.backend.Username, p.backend.Password, "", p.dialer, p.dialOptions()...)
	if err != nil {
		p.recordDialFailure()
		return nil, fmt.Errorf("connecting to backend %s: %w", p.backend.Address, err)
	}

	p.breakerFails.Store(0)
	p.counters.dials.Add(1)
	now := time.Now()
	return &Conn{
		Conn:      c,
		ThreadID:  c.GetConnectionID(),
		createdAt: now,
		pooledAt:  now,
		lastPing:  now,
	}, nil
}

func (p *Pool) dialOptions() []client.Option {
	if p.tlsConf == nil {
		return nil
	}
	return []client.Option{func(c *client.Conn) error {
		c.SetTLSConfig(p.tlsConf)
		return nil
	}}
}

// KillQuery interrupts a running backend statement. KILL QUERY leaves the
// connection alive, so the client gets an error and its session survives.
// It uses a throwaway connection on purpose: the pool may be saturated by
// exactly the queries that need killing.
func (p *Pool) KillQuery(threadID uint32) error {
	ctx, cancel := context.WithTimeout(context.Background(), p.backend.ConnectTimeout.Std())
	defer cancel()

	c, err := client.ConnectWithDialer(ctx, "tcp", p.backend.Address,
		p.backend.Username, p.backend.Password, "", p.dialer, p.dialOptions()...)
	if err != nil {
		return fmt.Errorf("connecting to kill a query: %w", err)
	}
	defer c.Close()

	_, err = c.Execute(fmt.Sprintf("KILL QUERY %d", threadID))
	return err
}

// Stats is a snapshot of the pool.
type Stats struct {
	Open        int  `json:"open"`
	Idle        int  `json:"idle"`
	MaxOpen     int  `json:"max_open"`
	BreakerOpen bool `json:"breaker_open"`

	Dials        uint64 `json:"dials"`
	Discards     uint64 `json:"discards"`
	Retired      uint64 `json:"retired"`
	Waits        uint64 `json:"waits"`
	WaitTimeouts uint64 `json:"wait_timeouts"`
	// AvgWaitMillis is the mean time spent waiting, over the waits that
	// happened; it is meaningless without Waits, and that is the point.
	AvgWaitMillis float64 `json:"avg_wait_millis"`
}

// Stat returns the current pool state.
func (p *Pool) Stat() Stats {
	waits := p.counters.waits.Load()
	var avg float64
	if waits > 0 {
		avg = float64(p.counters.waitNanos.Load()) / float64(waits) / float64(time.Millisecond)
	}
	return Stats{
		Open:          len(p.openSem),
		Idle:          len(p.idle),
		MaxOpen:       p.cfg.MaxOpen,
		BreakerOpen:   p.breakerOpen.Load(),
		Dials:         p.counters.dials.Load(),
		Discards:      p.counters.discards.Load(),
		Retired:       p.counters.retired.Load(),
		Waits:         waits,
		WaitTimeouts:  p.counters.waitTimeouts.Load(),
		AvgWaitMillis: avg,
	}
}

// recordDialFailure counts consecutive dial failures and opens the circuit
// when the threshold is reached.
func (p *Pool) recordDialFailure() {
	threshold := p.cfg.Breaker.Failures
	if threshold <= 0 {
		return
	}
	if p.breakerFails.Add(1) < int32(threshold) {
		return
	}
	if p.breakerOpen.Swap(true) {
		return // already open, a probe is running
	}
	p.log.Error("backend unreachable, circuit breaker open: failing fast",
		"backend", p.backend.Address, "failures", threshold)
	go p.probe()
}

// probe retries the backend until it answers, then closes the circuit.
func (p *Pool) probe() {
	ticker := time.NewTicker(p.cfg.Breaker.ProbeInterval.Std())
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
		}

		ctx, cancel := context.WithTimeout(context.Background(), p.backend.ConnectTimeout.Std())
		c, err := client.ConnectWithDialer(ctx, "tcp", p.backend.Address,
			p.backend.Username, p.backend.Password, "", p.dialer, p.dialOptions()...)
		cancel()
		if err != nil {
			p.log.Debug("backend probe failed", "error", err)
			continue
		}
		_ = c.Close()

		p.breakerFails.Store(0)
		p.breakerOpen.Store(false)
		p.log.Info("backend recovered, circuit breaker closed", "backend", p.backend.Address)
		return
	}
}

// expired reports whether a connection has reached max_lifetime. Retiring
// connections on a schedule is what lets a MySQL restart, a failover or a
// rotated certificate heal without anyone restarting gora.
func (p *Pool) expired(c *Conn) bool {
	return p.cfg.MaxLifetime > 0 && time.Since(c.createdAt) > p.cfg.MaxLifetime.Std()
}

// revive validates a connection taken from the idle buffer. Connections
// pinged recently by the keepalive loop are trusted; older ones get a fresh
// ping. It reports whether the connection is usable.
func (p *Pool) revive(c *Conn) bool {
	if p.expired(c) {
		p.counters.retired.Add(1)
		p.log.Debug("retiring backend connection, max_lifetime reached")
		p.Discard(c)
		return false
	}
	if time.Since(c.lastPing) < 2*p.cfg.PingInterval.Std() {
		return true
	}
	if err := c.Ping(); err != nil {
		p.log.Debug("dropping dead pooled connection", "error", err)
		p.Discard(c)
		return false
	}
	c.lastPing = time.Now()
	return true
}

// ResetConn clears session state with COM_RESET_CONNECTION: transactions
// rolled back, locks and temporary tables released, session variables back
// to their defaults. Servers without it (MySQL before 5.7) get a ROLLBACK as
// a best-effort fallback.
func (p *Pool) ResetConn(c *Conn) error {
	if err := p.reset(c); err != nil {
		return err
	}
	// The ROLLBACK fallback cannot clear session variables, so a connection
	// that had any applied is not safe to recycle at all.
	if p.resetUnsupported.Load() && c.VarSig != "" {
		return fmt.Errorf("backend lacks COM_RESET_CONNECTION, cannot clear session variables")
	}
	// The variables are gone for certain. The current database survives a
	// reset, but treating it as unknown costs at most one COM_INIT_DB later
	// and removes the doubt entirely.
	c.VarSig = ""
	c.BoundDB = ""
	return nil
}

// reset sends COM_RESET_CONNECTION, or the ROLLBACK fallback.
func (p *Pool) reset(c *Conn) error {
	if p.resetUnsupported.Load() {
		return c.Rollback()
	}

	c.ResetSequence()
	if err := c.WritePacket([]byte{0x01, 0x00, 0x00, 0x00, mysql.COM_RESET_CONNECTION}); err != nil {
		return err
	}
	if _, err := c.ReadOKPacket(); err != nil {
		var myErr *mysql.MyError
		if errors.As(err, &myErr) {
			// The server answered but does not know the command: remember it
			// and use ROLLBACK from now on.
			p.resetUnsupported.Store(true)
			p.log.Warn("backend does not support COM_RESET_CONNECTION, falling back to ROLLBACK")
			return c.Rollback()
		}
		return err
	}
	return nil
}

// keepalive pings parked connections so MySQL never closes them for
// inactivity, retires the ones past max_lifetime and closes the ones idle
// beyond idle_timeout — never below min_idle, which exists precisely so
// that some connections stay ready.
func (p *Pool) keepalive() {
	ticker := time.NewTicker(p.cfg.PingInterval.Std())
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
		}

		for n := len(p.idle); n > 0; n-- {
			var c *Conn
			select {
			case c = <-p.idle:
			default:
				n = 0
				continue
			}

			if p.expired(c) {
				p.counters.retired.Add(1)
				p.log.Debug("retiring backend connection, max_lifetime reached")
				p.Discard(c)
				continue
			}
			if p.cfg.IdleTimeout > 0 && len(p.idle) >= p.cfg.MinIdle &&
				time.Since(c.pooledAt) > p.cfg.IdleTimeout.Std() {
				p.log.Debug("closing pooled connection, idle timeout reached")
				p.Discard(c)
				continue
			}
			if time.Since(c.lastPing) >= p.cfg.PingInterval.Std() {
				if err := c.Ping(); err != nil {
					p.log.Warn("pooled connection lost, dropping it", "error", err)
					p.Discard(c)
					continue
				}
				c.lastPing = time.Now()
			}

			select {
			case p.idle <- c:
			default:
				p.Discard(c)
			}
		}
	}
}
