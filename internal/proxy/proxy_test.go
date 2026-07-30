package proxy

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/mysqltest"
	"github.com/ostap-mykhaylyak/gora/internal/pool"
)

const (
	backendUser = "gora"
	backendPass = "backend-secret"
	clientUser  = "wordpress"
	clientPass  = "client-secret"
)

func poolConfig() config.Pool {
	return config.Pool{
		MaxOpen:        4,
		MaxIdle:        4,
		PingInterval:   config.Duration(time.Second),
		AcquireTimeout: config.Duration(2 * time.Second),
		Multiplexing:   true,
	}
}

// start runs a fake backend and a proxy in front of it, and returns both.
func start(t *testing.T, listen config.Listen, poolCfg config.Pool) (*mysqltest.Server, *Server) {
	t.Helper()

	backend := mysqltest.Start(t, backendUser, backendPass)
	log := slog.New(slog.DiscardHandler)

	p, err := pool.New(config.Backend{
		Address:        backend.Addr,
		Username:       backendUser,
		Password:       backendPass,
		ConnectTimeout: config.Duration(2 * time.Second),
	}, poolCfg, log, nil)
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	t.Cleanup(p.Close)

	if listen.Address == "" {
		listen.Address = "127.0.0.1:0"
	}
	srv := New(Options{
		Listen:  listen,
		Users:   []config.User{{Username: clientUser, Password: clientPass}},
		PoolCfg: poolCfg,
		Pool:    p,
		Log:     log,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after the context was cancelled")
		}
	})

	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never started listening")
	}
	return backend, srv
}

func connect(t *testing.T, srv *Server) *client.Conn {
	t.Helper()
	c, err := client.Connect(srv.Addr(), clientUser, clientPass, "wordpress")
	if err != nil {
		t.Fatalf("connecting to the proxy: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// The basic contract: a client authenticates against gora and gets results
// from the backend behind it.
func TestQueryIsForwarded(t *testing.T) {
	backend, srv := start(t, config.Listen{}, poolConfig())
	c := connect(t, srv)

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
}

// The client's credentials are gora's, not MySQL's: the backend never sees
// them, and a wrong one never reaches the database at all.
func TestClientCredentialsAreCheckedByGora(t *testing.T) {
	backend, srv := start(t, config.Listen{}, poolConfig())

	if _, err := client.Connect(srv.Addr(), clientUser, "wrong", "wordpress"); err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if n := backend.Accepted.Load(); n != 0 {
		t.Fatalf("the backend was contacted %d times for a failed client login, want 0", n)
	}
}

// Multiplexing: between two statements the connection belongs to the pool,
// not to the session.
func TestConnectionIsReleasedBetweenStatements(t *testing.T) {
	_, srv := start(t, config.Listen{}, poolConfig())
	c := connect(t, srv)

	if _, err := c.Execute("SELECT 1"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	waitFor(t, time.Second, func() bool { return srv.pool.Stat().Idle == 1 })
	if pinned := srv.Stat().Pinned; pinned != 0 {
		t.Fatalf("pinned sessions = %d, want 0", pinned)
	}
}

// Two sessions in sequence share one backend connection: that is the ratio
// between PHP workers and real MySQL connections gora exists to change.
func TestSessionsShareOneBackendConnection(t *testing.T) {
	backend, srv := start(t, config.Listen{}, poolConfig())

	for i := 0; i < 3; i++ {
		c, err := client.Connect(srv.Addr(), clientUser, clientPass, "wordpress")
		if err != nil {
			t.Fatalf("connecting: %v", err)
		}
		if _, err := c.Execute("SELECT 1"); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		_ = c.Close()
	}

	if n := backend.Accepted.Load(); n != 1 {
		t.Fatalf("backend accepted %d connections for 3 sessions, want 1", n)
	}
}

// An open transaction must keep its connection: releasing it would hand
// another client a session in the middle of somebody else's transaction.
func TestTransactionKeepsItsConnection(t *testing.T) {
	_, srv := start(t, config.Listen{}, poolConfig())
	c := connect(t, srv)

	if _, err := c.Execute("BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := c.Execute("UPDATE wp_options SET option_value = 'x' WHERE option_name = 'y'"); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if idle := srv.pool.Stat().Idle; idle != 0 {
		t.Fatalf("idle = %d during a transaction, want 0", idle)
	}

	if _, err := c.Execute("COMMIT"); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	waitFor(t, time.Second, func() bool { return srv.pool.Stat().Idle == 1 })
}

// State gora cannot reproduce on another connection pins the session.
func TestStatefulSessionsArePinned(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"temporary table", "CREATE TEMPORARY TABLE t (id INT)"},
		{"table lock", "LOCK TABLES wp_posts WRITE"},
		{"advisory lock", "SELECT GET_LOCK('import', 10)"},
		{"user variable", "SET @counter = 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, srv := start(t, config.Listen{}, poolConfig())
			c := connect(t, srv)

			if _, err := c.Execute(tt.query); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if pinned := srv.Stat().Pinned; pinned != 1 {
				t.Fatalf("pinned sessions = %d, want 1", pinned)
			}
			if idle := srv.pool.Stat().Idle; idle != 0 {
				t.Fatalf("idle = %d, want 0: a pinned session released its connection", idle)
			}
		})
	}
}

// A literal containing @ is not a user-defined variable, and mistaking one
// for the other would pin every session that looks up a user by e-mail.
func TestEmailLiteralDoesNotPin(t *testing.T) {
	_, srv := start(t, config.Listen{}, poolConfig())
	c := connect(t, srv)

	if _, err := c.Execute("SELECT ID FROM wp_users WHERE user_email = 'a@b.com'"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if pinned := srv.Stat().Pinned; pinned != 0 {
		t.Fatalf("pinned sessions = %d, want 0", pinned)
	}
}

// SET NAMES does not pin: it is tracked and replayed when the session lands
// on a different connection. In the steady state every WordPress session
// sends the same one, the signatures match and nothing is replayed at all.
func TestSetNamesIsTrackedNotPinned(t *testing.T) {
	backend, srv := start(t, config.Listen{}, poolConfig())

	for i := 0; i < 2; i++ {
		c, err := client.Connect(srv.Addr(), clientUser, clientPass, "wordpress")
		if err != nil {
			t.Fatalf("connecting: %v", err)
		}
		if _, err := c.Execute("SET NAMES utf8mb4"); err != nil {
			t.Fatalf("SET NAMES: %v", err)
		}
		if _, err := c.Execute("SELECT 1"); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if pinned := srv.Stat().Pinned; pinned != 0 {
			t.Fatalf("pinned sessions = %d after SET NAMES, want 0", pinned)
		}
		_ = c.Close()
	}

	if n := backend.Accepted.Load(); n != 1 {
		t.Fatalf("backend accepted %d connections, want 1", n)
	}
	// Once per session: the second one lands on a connection whose variable
	// signature already matches, so nothing is replayed.
	if n := backend.Count("SET NAMES"); n != 2 {
		t.Fatalf("the backend saw SET NAMES %d times, want 2 (once per session)", n)
	}
}

// A prepared statement lives on one specific connection, so the session
// holds it until the statement is closed.
func TestPreparedStatementHoldsTheConnection(t *testing.T) {
	_, srv := start(t, config.Listen{}, poolConfig())
	c := connect(t, srv)

	stmt, err := c.Prepare("SELECT 1")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if idle := srv.pool.Stat().Idle; idle != 0 {
		t.Fatalf("idle = %d with a prepared statement open, want 0", idle)
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, time.Second, func() bool { return srv.pool.Stat().Idle == 1 })
}

// The connection cap refuses new clients instead of letting them queue up
// behind a saturated backend.
func TestMaxConnections(t *testing.T) {
	_, srv := start(t, config.Listen{MaxConnections: 1}, poolConfig())
	connect(t, srv)

	waitFor(t, time.Second, func() bool { return srv.Stat().Clients == 1 })

	c2, err := client.Connect(srv.Addr(), clientUser, clientPass, "wordpress")
	if err == nil {
		_ = c2.Close()
		t.Fatal("a second client was accepted past max_connections")
	}
}

// Commands gora does not implement must fail with an error that names the
// problem, not with a closed connection.
func TestUnsupportedCommandIsRefusedCleanly(t *testing.T) {
	_, srv := start(t, config.Listen{}, poolConfig())

	sess := &session{srv: srv, log: slog.New(slog.DiscardHandler)}
	err := sess.HandleOtherCommand(mysql.COM_SET_OPTION, nil)
	if err == nil {
		t.Fatal("COM_SET_OPTION was accepted, which would desynchronise the protocol")
	}
	var myErr *mysql.MyError
	if !asMyError(err, &myErr) {
		t.Fatalf("error %v is not a MySQL error the client can read", err)
	}
}

// COM_RESET_CONNECTION is what mysqlnd sends to recycle a persistent
// connection: gora honours it and forgets the session state.
func TestResetConnectionClearsSessionState(t *testing.T) {
	_, srv := start(t, config.Listen{}, poolConfig())

	sess := &session{srv: srv, pool: srv.pool, log: slog.New(slog.DiscardHandler)}
	sess.inTx = true
	sess.setStmts = []string{"SET NAMES utf8mb4"}
	sess.varSig = "x"
	sess.pin("test")

	if err := sess.HandleOtherCommand(mysql.COM_RESET_CONNECTION, nil); err != nil {
		t.Fatalf("COM_RESET_CONNECTION: %v", err)
	}
	if sess.inTx || sess.pinned || sess.setStmts != nil || sess.varSig != "" {
		t.Fatalf("session state survived the reset: %+v", sess)
	}
	if pinned := srv.Stat().Pinned; pinned != 0 {
		t.Fatalf("pinned sessions = %d after a reset, want 0", pinned)
	}
}

func asMyError(err error, target **mysql.MyError) bool {
	e, ok := err.(*mysql.MyError)
	if ok {
		*target = e
	}
	return ok
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
