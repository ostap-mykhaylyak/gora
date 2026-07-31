// Package proxy implements gora's client-facing MySQL listener.
//
// gora terminates the MySQL protocol on both sides: clients authenticate
// against the users in config.yaml, while backend connections belong to gora
// and are shared through the pool. Every client command is executed on the
// session's backend connection and the result is relayed back.
package proxy

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"

	"github.com/ostap-mykhaylyak/gora/internal/cache"
	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/firewall"
	"github.com/ostap-mykhaylyak/gora/internal/pool"
	"github.com/ostap-mykhaylyak/gora/internal/rewrite"
	"github.com/ostap-mykhaylyak/gora/internal/throttle"
)

// Options wires the Server's collaborators. Everything but the pool is
// optional: a nil collaborator is a feature that is off.
type Options struct {
	Listen   config.Listen
	Users    []config.User
	PoolCfg  config.Pool
	Pool     *pool.Pool
	Cache    *cache.Cache // nil disables the query cache
	Rewriter *rewrite.Rewriter
	Firewall *firewall.Firewall
	Throttle *throttle.Limiter
	TLS      *tls.Config // client-facing TLS, advertised when non-nil
	Log      *slog.Logger
}

// Server accepts client connections and serves the MySQL protocol.
type Server struct {
	addr       string
	maxClients int
	drain      time.Duration
	pool       *pool.Pool
	cfg        config.Pool
	cache      *cache.Cache
	rewriter   *rewrite.Rewriter
	firewall   *firewall.Firewall
	throttle   *throttle.Limiter
	log        *slog.Logger

	srvConf *server.Server
	auth    *server.InMemoryAuthenticationHandler

	// ready is closed once the listener is bound, and boundAddr is the
	// address it ended up on. With a port of 0 that is only known after
	// listening, which is how the tests reach the proxy.
	ready     chan struct{}
	boundAddr string

	wg      sync.WaitGroup
	clients sync.Map // net.Conn -> struct{}, the open client sockets

	numClients atomic.Int64
	numPinned  atomic.Int64
	numActive  atomic.Int64 // statements currently executing
}

// New creates a Server; call Run to start serving.
func New(o Options) *Server {
	// mysql_native_password keeps compatibility with every PHP mysqli and
	// mysqlnd build WordPress runs on, including the ones that would refuse
	// caching_sha2 over a plaintext socket.
	srvConf := server.NewServer("8.0.36-gora", mysql.DEFAULT_COLLATION_ID,
		mysql.AUTH_NATIVE_PASSWORD, nil, o.TLS)
	auth := server.NewInMemoryAuthenticationHandler(mysql.AUTH_NATIVE_PASSWORD)
	for _, u := range o.Users {
		// AddUser only fails for an unknown auth plugin, which is fixed here.
		_ = auth.AddUser(u.Username, u.Password)
	}

	return &Server{
		addr:       o.Listen.Address,
		maxClients: o.Listen.MaxConnections,
		drain:      o.Listen.DrainTimeout.Std(),
		pool:       o.Pool,
		cfg:        o.PoolCfg,
		cache:      o.Cache,
		rewriter:   o.Rewriter,
		firewall:   o.Firewall,
		throttle:   o.Throttle,
		log:        o.Log,
		srvConf:    srvConf,
		auth:       auth,
		ready:      make(chan struct{}),
	}
}

// Ready is closed once the listener accepts connections.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Addr is the address the listener bound to; valid once Ready is closed.
func (s *Server) Addr() string { return s.boundAddr }

// Stats is a snapshot of the client side.
type Stats struct {
	Clients int64 `json:"clients"`
	Pinned  int64 `json:"pinned_sessions"`
	Active  int64 `json:"active_statements"`
}

// Stat returns the current client-side state.
func (s *Server) Stat() Stats {
	return Stats{
		Clients: s.numClients.Load(),
		Pinned:  s.numPinned.Load(),
		Active:  s.numActive.Load(),
	}
}

// active marks a statement as running and returns the function that marks it
// finished. Shutdown waits on this counter, not on the sessions themselves:
// a WordPress fleet keeps its connections open and idle, and waiting for
// those to close would turn every restart into a drain timeout.
func (s *Server) active() func() {
	s.numActive.Add(1)
	return func() { s.numActive.Add(-1) }
}

// Run listens until ctx is cancelled, then lets the statements in flight
// finish before closing client connections.
func (s *Server) Run(ctx context.Context) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}
	s.boundAddr = ln.Addr().String()
	close(s.ready)
	s.log.Info("listening for clients", "address", s.boundAddr)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
		s.drainClients()
		s.clients.Range(func(key, _ any) bool {
			_ = key.(net.Conn).Close()
			return true
		})
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break // shutting down
			}
			s.log.Error("accept failed", "error", err)
			continue
		}

		if s.maxClients > 0 && s.numClients.Load() >= int64(s.maxClients) {
			s.log.Warn("client connection limit reached, refusing",
				"client", conn.RemoteAddr(), "max_connections", s.maxClients)
			_ = conn.Close()
			continue
		}

		s.numClients.Add(1)
		s.clients.Store(conn, struct{}{})
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.numClients.Add(-1)
			defer s.clients.Delete(conn)
			s.handle(ctx, conn)
		}()
	}

	s.wg.Wait()
	return nil
}

// drainClients waits for the statements already running to finish. Idle
// sessions are not waited for: they have nothing in flight to lose.
func (s *Server) drainClients() {
	if s.drain <= 0 || s.numActive.Load() == 0 {
		return
	}
	s.log.Info("waiting for the statements in flight", "active", s.numActive.Load(), "timeout", s.drain)

	deadline := time.Now().Add(s.drain)
	for time.Now().Before(deadline) {
		if s.numActive.Load() == 0 {
			s.log.Info("all statements finished")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.log.Warn("drain timeout, closing client connections anyway",
		"still_active", s.numActive.Load())
}

func (s *Server) handle(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	sess := newSession(ctx, s, s.log.With("client", clientConn.RemoteAddr()))
	defer sess.close()

	// The handshake authenticates the client and, when it selects a
	// database, calls UseDB, which attaches a backend connection eagerly.
	conn, err := server.NewCustomizedConn(clientConn, s.srvConf, s.auth, sess)
	if err != nil {
		s.log.Warn("client handshake failed", "client", clientConn.RemoteAddr(), "error", err)
		return
	}

	sess.markAuthenticated()
	sess.log.Debug("session opened", "user", conn.GetUser())
	for !conn.Closed() {
		if err := conn.HandleCommand(); err != nil {
			sess.log.Debug("session ended", "reason", err)
			return
		}
	}
	sess.log.Debug("session closed")
}
