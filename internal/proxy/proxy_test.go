package proxy

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"

	"github.com/ostap-mykhaylyak/gora/internal/cache"
	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/firewall"
	"github.com/ostap-mykhaylyak/gora/internal/mysqltest"
	"github.com/ostap-mykhaylyak/gora/internal/pool"
	"github.com/ostap-mykhaylyak/gora/internal/rewrite"
	"github.com/ostap-mykhaylyak/gora/internal/throttle"
	"github.com/ostap-mykhaylyak/gora/internal/topology"
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

// setup describes the assembly a test wants in front of the fake backends.
// The zero value is a bare proxy on one node: no replicas, no cache, no
// traffic rules.
type setup struct {
	listen    config.Listen
	pool      config.Pool
	routing   config.Routing
	cache     *config.Cache
	rules     []cache.Rule
	rewrites  []rewrite.Rule
	blocks    []firewall.Rule
	throttles []throttle.Rule

	// replicas is how many read replicas to run behind the proxy, and
	// primaryDown makes the primary a dead address, which is how the
	// degraded path is exercised.
	replicas    int
	primaryDown bool

	// health runs the health check loop. It is off unless a test needs it,
	// because it opens connections of its own and the tests that count
	// connections are counting the client's.
	health bool

	// Filled in by startWith.
	replicaServers []*mysqltest.Server
}

// start runs a fake backend and a bare proxy in front of it.
func start(t *testing.T, listen config.Listen, poolCfg config.Pool) (*mysqltest.Server, *Server) {
	return startWith(t, &setup{listen: listen, pool: poolCfg})
}

// startWith runs the fake backends and the proxy the setup describes, and
// returns the primary backend and the proxy.
func startWith(t *testing.T, s *setup) (*mysqltest.Server, *Server) {
	t.Helper()

	backend := mysqltest.Start(t, backendUser, backendPass)
	log := slog.New(slog.DiscardHandler)

	primaryAddr := backend.Addr
	if s.primaryDown {
		primaryAddr = "127.0.0.1:1" // nothing listens there
	}
	backendCfg := config.Backend{
		Address:        primaryAddr,
		Username:       backendUser,
		Password:       backendPass,
		ConnectTimeout: config.Duration(time.Second),
	}
	for i := 0; i < s.replicas; i++ {
		replica := mysqltest.Start(t, backendUser, backendPass)
		s.replicaServers = append(s.replicaServers, replica)
		backendCfg.Replicas = append(backendCfg.Replicas, replica.Addr)
	}

	routing := s.routing
	if routing.HealthInterval <= 0 {
		routing.HealthInterval = config.Duration(50 * time.Millisecond)
	}

	topo, err := topology.New(backendCfg, s.pool, routing, "", log)
	if err != nil {
		t.Fatalf("topology.New: %v", err)
	}
	t.Cleanup(topo.Close)

	var queryCache *cache.Cache
	if s.cache != nil {
		queryCache, err = cache.New(*s.cache, topo.Primary().Pool(), s.rules, log)
		if err != nil {
			t.Fatalf("cache.New: %v", err)
		}
	}

	rewriter, err := rewrite.New(s.rewrites, "wp_", log)
	if err != nil {
		t.Fatalf("rewrite.New: %v", err)
	}
	fw, err := firewall.New(s.blocks, "wp_")
	if err != nil {
		t.Fatalf("firewall.New: %v", err)
	}
	limiter, err := throttle.New(s.throttles, "wp_")
	if err != nil {
		t.Fatalf("throttle.New: %v", err)
	}

	listen := s.listen
	if listen.Address == "" {
		listen.Address = "127.0.0.1:0"
	}
	srv := New(Options{
		Listen:   listen,
		Users:    []config.User{{Username: clientUser, Password: clientPass}},
		PoolCfg:  s.pool,
		Topology: topo,
		Cache:    queryCache,
		Rewriter: rewriter,
		Firewall: fw,
		Throttle: limiter,
		Log:      log,
	})

	ctx, cancel := context.WithCancel(context.Background())
	if s.health || s.replicas > 0 || s.primaryDown {
		go topo.Run(ctx)
	}

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

// primaryPool is the pool the tests assert against when they are checking
// how connections are handed back.
func primaryPool(srv *Server) *pool.Pool { return srv.topo.Primary().Pool() }

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
	waitFor(t, time.Second, func() bool { return primaryPool(srv).Stat().Idle == 1 })
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
	if idle := primaryPool(srv).Stat().Idle; idle != 0 {
		t.Fatalf("idle = %d during a transaction, want 0", idle)
	}

	if _, err := c.Execute("COMMIT"); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	waitFor(t, time.Second, func() bool { return primaryPool(srv).Stat().Idle == 1 })
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
			if idle := primaryPool(srv).Stat().Idle; idle != 0 {
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
	if idle := primaryPool(srv).Stat().Idle; idle != 0 {
		t.Fatalf("idle = %d with a prepared statement open, want 0", idle)
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, time.Second, func() bool { return primaryPool(srv).Stat().Idle == 1 })
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

	sess := &session{srv: srv, topo: srv.topo, log: slog.New(slog.DiscardHandler)}
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

// --- query cache ---

const allOptions = "SELECT option_name, option_value FROM wp_options WHERE autoload = 'yes'"

func cacheConfig() *config.Cache {
	return &config.Cache{
		Enabled:         true,
		TablePrefix:     "wp_",
		AutoloadOptions: true,
		Transients:      true,
		DefaultTTL:      config.Duration(time.Minute),
		MaxEntries:      100,
		MaxBytes:        1 << 20,
		MaxResultBytes:  1 << 20,
	}
}

// The point of the whole milestone, seen from the client: the second
// pageload does not reach MySQL.
func TestCachedReadDoesNotReachTheBackend(t *testing.T) {
	backend, srv := startWith(t, &setup{pool: poolConfig(), cache: cacheConfig()})
	c := connect(t, srv)

	for i := 0; i < 3; i++ {
		if _, err := c.Execute(allOptions); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	if n := backend.Count("WHERE autoload"); n != 1 {
		t.Fatalf("the backend saw the autoloaded options query %d times, want 1", n)
	}
}

// Inside a transaction a read must see this session's own writes, so the
// cache steps aside entirely.
func TestReadsInsideATransactionBypassTheCache(t *testing.T) {
	backend, srv := startWith(t, &setup{pool: poolConfig(), cache: cacheConfig()})
	c := connect(t, srv)

	if _, err := c.Execute(allOptions); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := c.Execute("BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := c.Execute(allOptions); err != nil {
		t.Fatalf("Execute in transaction: %v", err)
	}
	if n := backend.Count("WHERE autoload"); n != 2 {
		t.Fatalf("the backend saw the query %d times, want 2: the transaction was served from cache", n)
	}
}

// A write flowing through gora drops what it affects, for every session.
func TestWriteInvalidatesForOtherSessions(t *testing.T) {
	backend, srv := startWith(t, &setup{pool: poolConfig(), cache: cacheConfig()})

	reader := connect(t, srv)
	if _, err := reader.Execute(allOptions); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	writer := connect(t, srv)
	if _, err := writer.Execute("UPDATE wp_options SET option_value = 'x' WHERE option_name = 'siteurl'"); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}

	if _, err := reader.Execute(allOptions); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := backend.Count("WHERE autoload"); n != 2 {
		t.Fatalf("the backend saw the query %d times, want 2: a stale snapshot was served", n)
	}
}

// A listing and its total are cached together, so the second visitor gets
// both from memory and the pagination still adds up.
func TestPaginatedListingIsServedFromCache(t *testing.T) {
	rules := []cache.Rule{{
		Name:         "product-listing",
		Match:        `(?i)^SELECT SQL_CALC_FOUND_ROWS`,
		InvalidateOn: []string{"{prefix}posts"},
	}}
	backend, srv := startWith(t, &setup{pool: poolConfig(), cache: cacheConfig(), rules: rules})

	listing := "SELECT SQL_CALC_FOUND_ROWS ID FROM wp_posts WHERE post_type = 'product' LIMIT 0, 16"
	for i := 0; i < 2; i++ {
		c := connect(t, srv)
		if _, err := c.Execute(listing); err != nil {
			t.Fatalf("listing: %v", err)
		}
		if _, err := c.Execute("SELECT FOUND_ROWS()"); err != nil {
			t.Fatalf("FOUND_ROWS(): %v", err)
		}
	}

	if n := backend.Count("SQL_CALC_FOUND_ROWS"); n != 1 {
		t.Fatalf("the backend ran the listing %d times, want 1", n)
	}
	if n := backend.Count("FOUND_ROWS()"); n != 1 {
		t.Fatalf("the backend ran FOUND_ROWS() %d times, want 1", n)
	}
}

// --- traffic rules ---

// A rewrite must reach the backend, and the client must not notice.
func TestRewrittenStatementReachesTheBackend(t *testing.T) {
	backend, srv := startWith(t, &setup{
		pool: poolConfig(),
		rewrites: []rewrite.Rule{{
			Name:  "drop-order-by-rand",
			Match: `(?i)\s*ORDER\s+BY\s+RAND\s*\(\s*\)`,
		}},
	})
	c := connect(t, srv)

	if _, err := c.Execute("SELECT ID FROM wp_posts ORDER BY RAND() LIMIT 5"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := backend.Count("ORDER BY RAND"); n != 0 {
		t.Fatalf("the backend still received ORDER BY RAND(): %q", backend.Queries())
	}
	if n := backend.Count("SELECT ID FROM wp_posts LIMIT 5"); n != 1 {
		t.Fatalf("the backend did not receive the rewritten statement: %q", backend.Queries())
	}
}

// A blocked statement never reaches the database, and the client is told
// why in a way PHP will put in its error log.
func TestBlockedStatementIsRefused(t *testing.T) {
	backend, srv := startWith(t, &setup{
		pool: poolConfig(),
		blocks: []firewall.Rule{{
			Name:    "no-truncate",
			Match:   "(?i)^TRUNCATE",
			Message: "truncate is not allowed on this database",
		}},
	})
	c := connect(t, srv)

	_, err := c.Execute("TRUNCATE TABLE wp_postmeta")
	if err == nil {
		t.Fatal("a blocked statement succeeded")
	}
	if !strings.Contains(err.Error(), "truncate is not allowed") {
		t.Fatalf("error %q does not carry the configured message", err)
	}
	if n := backend.Count("TRUNCATE"); n != 0 {
		t.Fatalf("the blocked statement reached the backend: %q", backend.Queries())
	}

	// The session survives: the client can carry on.
	if _, err := c.Execute("SELECT 1"); err != nil {
		t.Fatalf("the session did not survive a blocked statement: %v", err)
	}
}

// A dry-run rule reports what it would have refused, and refuses nothing.
func TestDryRunBlockLetsTheStatementThrough(t *testing.T) {
	backend, srv := startWith(t, &setup{
		pool: poolConfig(),
		blocks: []firewall.Rule{{
			Name:   "watch-truncate",
			Match:  "(?i)^TRUNCATE",
			DryRun: true,
		}},
	})
	c := connect(t, srv)

	if _, err := c.Execute("TRUNCATE TABLE wp_postmeta"); err != nil {
		t.Fatalf("a dry-run rule blocked the statement: %v", err)
	}
	if n := backend.Count("TRUNCATE"); n != 1 {
		t.Fatalf("the backend saw the statement %d times, want 1", n)
	}
}

// Prepared statements must not be a way around the firewall.
func TestBlockedStatementCannotBePrepared(t *testing.T) {
	_, srv := startWith(t, &setup{
		pool:   poolConfig(),
		blocks: []firewall.Rule{{Name: "no-truncate", Match: "(?i)^TRUNCATE"}},
	})
	c := connect(t, srv)

	if _, err := c.Prepare("TRUNCATE TABLE wp_postmeta"); err == nil {
		t.Fatal("a blocked statement was prepared")
	}
}

// Past the limit the excess is refused rather than queued, and the client
// gets an error naming the rule instead of a connection that hangs.
func TestThrottleRefusesTheExcess(t *testing.T) {
	backend, srv := startWith(t, &setup{
		pool: poolConfig(),
		throttles: []throttle.Rule{{
			Name:          "heavy-search",
			Match:         "(?i)LIKE '%",
			MaxConcurrent: 1,
		}},
	})
	search := "SELECT ID FROM wp_posts WHERE post_title LIKE '%chair%'"
	backend.Delay("LIKE", 300*time.Millisecond)

	first := connect(t, srv)
	second := connect(t, srv)

	done := make(chan error, 1)
	go func() {
		_, err := first.Execute(search)
		done <- err
	}()

	// Give the first statement time to take the only slot.
	time.Sleep(100 * time.Millisecond)
	if _, err := second.Execute(search); err == nil {
		t.Fatal("a second concurrent execution was allowed past max_concurrent")
	} else if !strings.Contains(err.Error(), "heavy-search") {
		t.Fatalf("error %q does not name the throttle rule", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("the first execution failed: %v", err)
	}
	if n := backend.Count("LIKE"); n != 1 {
		t.Fatalf("the backend ran the statement %d times, want 1", n)
	}

	// Once the slot is free the same statement runs again.
	if _, err := second.Execute(search); err != nil {
		t.Fatalf("the statement was still refused after the slot freed up: %v", err)
	}
}

// A cache hit costs the database nothing, so it must not need a slot
// either.
func TestThrottleDoesNotApplyToCacheHits(t *testing.T) {
	_, srv := startWith(t, &setup{
		pool:  poolConfig(),
		cache: cacheConfig(),
		throttles: []throttle.Rule{{
			Name:          "everything",
			Match:         ".",
			MaxConcurrent: 1,
		}},
	})
	c := connect(t, srv)

	for i := 0; i < 3; i++ {
		if _, err := c.Execute(allOptions); err != nil {
			t.Fatalf("Execute: %v", err)
		}
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
