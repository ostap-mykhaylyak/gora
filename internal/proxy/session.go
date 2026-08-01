package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"

	"github.com/ostap-mykhaylyak/gora/internal/cache"
	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/firewall"
	"github.com/ostap-mykhaylyak/gora/internal/pool"
	"github.com/ostap-mykhaylyak/gora/internal/profile"
	"github.com/ostap-mykhaylyak/gora/internal/rewrite"
	"github.com/ostap-mykhaylyak/gora/internal/statement"
	"github.com/ostap-mykhaylyak/gora/internal/throttle"
	"github.com/ostap-mykhaylyak/gora/internal/topology"
)

// maxTrackedSets caps the SET statements replayed on connection reuse.
// A session that sets more than this is doing something gora should not try
// to reproduce, so it gets pinned instead.
const maxTrackedSets = 20

// maxTrackedTxWrites caps the writes remembered inside one transaction for
// re-invalidation at COMMIT. Past it the whole cache is flushed instead:
// remembering an unbounded list would turn a bulk import into a memory leak.
const maxTrackedTxWrites = 128

// session implements server.Handler for one client connection.
//
// With multiplexing enabled (the default) the backend connection goes back
// to the pool after every statement that leaves no state behind, so many
// client sessions share few backend connections. Sessions holding state —
// open transactions, temporary tables, locks, prepared statements, user
// variables — keep their connection attached (pinned) and behave exactly
// like a direct connection would.
//
// While a client idles holding an attached connection (PHP parsing a large
// CSV in the middle of a transaction), a pinger keeps that connection alive
// with COM_PING, so MySQL never drops it and the next statement does not
// come back as "server has gone away". If the connection is lost anyway,
// the next command transparently attaches a fresh one.
type session struct {
	ctx      context.Context
	srv      *Server
	topo     *topology.Topology
	cfg      config.Pool
	routing  config.Routing
	cache    *cache.Cache // nil when the cache is disabled
	rewriter *rewrite.Rewriter
	firewall *firewall.Firewall
	throttle *throttle.Limiter
	prof     *profile.Profiler // nil when profiling is off
	log      *slog.Logger

	mu   sync.Mutex // guards everything below, including against the pinger
	conn *pool.Conn
	// node is where conn came from. A session can move between nodes, but
	// only while it is holding nothing that would have to move with it.
	node    *topology.Node
	db      string
	lastUse time.Time
	// lastWrite arms the sticky window: after writing, this session's own
	// reads stay on the primary long enough for replication to catch up.
	lastWrite time.Time

	// authenticated flips once the handshake has succeeded. The protocol
	// library calls UseDB while it is still parsing the handshake response,
	// before the password has been checked, so this is what keeps a failed
	// login from costing a backend connection.
	authenticated bool

	inTx      bool
	mux       bool     // per-query release enabled
	pinned    bool     // tied to this connection for the rest of the session
	holdNext  bool     // keep the connection for one more statement
	openStmts int      // prepared statements alive on the connection
	setStmts  []string // tracked SETs, replayed on connection reuse
	varSig    string   // signature of setStmts

	// Cache state.
	txWrites    []string // writes seen inside the open transaction
	txOverflow  bool
	cacheUnsafe bool // the session did something the cache cannot follow

	// Pairing state for SQL_CALC_FOUND_ROWS listings.
	calcPending    string // listing executed on the backend, awaiting its count
	foundRows      uint64 // count to serve for the next FOUND_ROWS()
	foundRowsKnown bool   // whether foundRows is armed

	stopPing chan struct{}
	pingDone chan struct{}
}

func newSession(ctx context.Context, srv *Server, log *slog.Logger) *session {
	s := &session{
		ctx:      ctx,
		srv:      srv,
		topo:     srv.topo,
		cfg:      srv.cfg,
		routing:  srv.routing,
		cache:    srv.cache,
		rewriter: srv.rewriter,
		firewall: srv.firewall,
		throttle: srv.throttle,
		prof:     srv.prof,
		log:      log,
		mux:      srv.cfg.Multiplexing,
		lastUse:  time.Now(),
		stopPing: make(chan struct{}),
		pingDone: make(chan struct{}),
	}
	go s.pinger()
	return s
}

// close returns the session's backend connection to the pool.
func (s *session) close() {
	close(s.stopPing)
	<-s.pingDone

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.node.Pool().Release(s.conn)
		s.conn = nil
	}
	if s.pinned {
		s.srv.numPinned.Add(-1)
		s.pinned = false
	}
}

// backend returns a connection for a statement of this kind, moving the
// session to another node when routing calls for it. Must be called with
// s.mu held.
func (s *session) backend(kind statement.Kind) (*pool.Conn, error) {
	want, err := s.route(kind)
	if err != nil {
		return nil, err
	}

	if s.conn != nil {
		if s.node == want {
			return s.conn, nil
		}
		// The statement belongs elsewhere and the session is free to move:
		// park this connection before taking one from the other node.
		c := s.conn
		s.conn, s.node = nil, nil
		c.Owner().ReleaseClean(c)
	}

	// Two attempts: a parked connection can fail preparation because it died
	// while parked, and a freshly dialed one deserves the second try.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		c, err := want.Pool().Acquire(s.ctx)
		if err != nil {
			return nil, backendError(want, err)
		}
		if err := s.prepareConn(c); err != nil {
			lastErr = err
			if isConnError(err) {
				want.Pool().Discard(c)
				continue
			}
			// A MySQL-level error (unknown database, denied access): the
			// connection is fine, the request is not.
			want.Pool().Release(c)
			return nil, err
		}
		s.conn, s.node = c, want
		return c, nil
	}
	return nil, lastErr
}

// route decides which node a statement of this kind belongs on.
//
// A session holding anything — an open transaction, a temporary table, a
// statement whose companion reads a counter off the connection — does not
// move: that state lives on one connection on one server, and following it
// around is the reason multiplexing has safety rules at all.
func (s *session) route(kind statement.Kind) (*topology.Node, error) {
	if s.conn != nil && (!s.releasable() || s.holdNext) {
		return s.node, nil
	}

	if writeBound(kind) || s.inTx {
		if !s.topo.WritesAccepted() {
			return nil, errWritesRefused
		}
		return s.topo.Primary(), nil
	}

	switch kind {
	case statement.KindSelect, statement.KindOther:
		if s.stickyActive() {
			// This session has just written. Replication is asynchronous,
			// so for a moment the only server that certainly has its data
			// is the one it wrote to.
			return s.topo.Primary(), nil
		}
		return s.topo.PickReader(), nil
	default:
		return s.topo.Primary(), nil
	}
}

// writeBound reports whether a statement must run on the primary.
func writeBound(kind statement.Kind) bool {
	switch kind {
	case statement.KindWrite, statement.KindBegin, statement.KindCommit,
		statement.KindRollback, statement.KindUnsafe:
		return true
	default:
		return false
	}
}

// stickyActive reports whether this session's reads are still tied to the
// primary after a write of its own.
func (s *session) stickyActive() bool {
	if !s.topo.HasReplicas() || s.routing.StickyAfterWrite <= 0 || s.lastWrite.IsZero() {
		return false
	}
	return time.Since(s.lastWrite) < s.routing.StickyAfterWrite.Std()
}

// errWritesRefused is what a client is told when the primary cannot take
// writes. It carries the error code MySQL itself uses for a read-only
// server, so clients recognise it instead of seeing a proxy failure.
var errWritesRefused = mysql.NewError(mysql.ER_OPTION_PREVENTS_STATEMENT,
	"gora: the primary database is not accepting writes right now")

// backendError turns a pool failure into something a client can read: a
// backend that is down is a database error, not a proxy stack trace.
func backendError(n *topology.Node, err error) error {
	if errors.Is(err, pool.ErrBackendDown) {
		return mysql.NewError(mysql.ER_UNKNOWN_ERROR,
			"gora: database node "+n.Address+" is unreachable")
	}
	return err
}

// prepareConn aligns a pooled connection with this session's environment:
// it clears foreign session variables, binds the database and replays the
// tracked SETs. In the steady state every WordPress session issues the same
// SET NAMES, the signatures match and nothing is sent at all.
// Must be called with s.mu held.
func (s *session) prepareConn(c *pool.Conn) error {
	if c.VarSig != s.varSig && c.VarSig != "" {
		// The connection carries another session's variables.
		if err := c.Owner().ResetConn(c); err != nil {
			return err
		}
	}
	if s.db != "" && c.BoundDB != s.db {
		if err := c.UseDB(s.db); err != nil {
			return err
		}
		c.BoundDB = s.db
	}
	if c.VarSig != s.varSig {
		for _, stmt := range s.setStmts {
			if _, err := c.Execute(stmt); err != nil {
				return err
			}
		}
		c.VarSig = s.varSig
	}
	return nil
}

// maybeRelease returns the connection to the pool when the session state
// allows it. Must be called with s.mu held, after the result has been fully
// read: results are buffered, so replying to the client does not need the
// backend connection any more.
func (s *session) maybeRelease() {
	if !s.mux || s.conn == nil || !s.releasable() {
		return
	}
	if s.holdNext {
		s.holdNext = false
		return
	}
	c := s.conn
	s.conn, s.node = nil, nil
	c.Owner().ReleaseClean(c)
}

// releasable reports whether the session is holding nothing that lives on
// its current connection. Both letting go of a connection and moving to
// another node depend on it. Must be called with s.mu held.
func (s *session) releasable() bool {
	if s.pinned || s.inTx || s.openStmts > 0 {
		return false
	}
	if s.conn == nil {
		return true
	}
	// The status flags of the last OK/EOF packet catch implicit transactions
	// (autocommit=0) that keyword tracking alone would miss.
	return !s.conn.IsInTransaction() && s.conn.IsAutoCommit()
}

// pin ties the session to its connection for the rest of its life.
// Must be called with s.mu held.
func (s *session) pin(reason string) {
	if s.pinned {
		return
	}
	s.pinned = true
	s.srv.numPinned.Add(1)
	s.log.Debug("session pinned to its backend connection", "reason", reason)
}

// trackSafety updates transaction and pinning state after a successful
// statement. Must be called with s.mu held.
func (s *session) trackSafety(kind statement.Kind, query string, r *mysql.Result) {
	switch kind {
	case statement.KindBegin:
		s.inTx = true
	case statement.KindCommit, statement.KindRollback:
		s.inTx = false
		s.lastWrite = time.Now()
	case statement.KindWrite:
		// The sticky window starts here: this session's own reads stay on
		// the primary until replication has plausibly caught up.
		s.lastWrite = time.Now()
	case statement.KindUnsafe:
		s.pin("untracked session command (autocommit/XA)")
	}
	if r != nil && r.Status&mysql.SERVER_STATUS_IN_TRANS != 0 {
		s.inTx = true
	}

	if pinDetectRe.MatchString(query) {
		s.pin("temporary table, lock or transaction setting")
	}
	// User variables persist on the connection with values gora cannot
	// reproduce elsewhere. Checked on the fingerprint so that a literal
	// like 'user@example.com' does not look like one.
	if strings.Contains(query, "@") && userVarRe.MatchString(statement.Fingerprint(query)) {
		s.pin("user-defined variables")
	}

	// The companion statement (SELECT FOUND_ROWS(), SELECT LAST_INSERT_ID())
	// has to run on this same connection.
	if holdDetectRe.MatchString(query) || (r != nil && r.InsertId > 0) {
		s.holdNext = true
	}
}

// trackSet records a successful SET statement: replayable ones join the
// session environment, untrackable ones pin. Must be called with s.mu held.
func (s *session) trackSet(query string, act setAction) {
	switch act {
	case setTrack:
		for _, existing := range s.setStmts {
			if existing == query {
				return // wpdb re-sends the same SET NAMES
			}
		}
		if len(s.setStmts) >= maxTrackedSets {
			s.pin("too many session settings to replay")
			return
		}
		s.setStmts = append(s.setStmts, query)
		s.varSig = varSignature(s.setStmts)
		if s.conn != nil {
			s.conn.VarSig = s.varSig
		}
	case setPin:
		s.pin("untrackable SET statement")
	case setNone, setIgnore:
	}
}

// finish records activity and handles connection-level failures: on a
// network error the backend connection is dropped so the next command gets
// a fresh one. MySQL-level errors (bad query, duplicate key) leave the
// connection attached, because the connection is fine.
// Must be called with s.mu held.
func (s *session) finish(err error) {
	s.lastUse = time.Now()
	if err == nil || s.conn == nil {
		return
	}
	if isConnError(err) {
		s.log.Warn("backend connection lost, will reattach on the next command", "error", err)
		s.node.Pool().Discard(s.conn)
		s.conn, s.node = nil, nil
	}
}

// isConnError distinguishes a broken connection from a server-side error:
// if MySQL replied, the connection is fine.
func isConnError(err error) bool {
	var myErr *mysql.MyError
	return !errors.As(err, &myErr)
}

// queryWatchdog kills the backend statement when it exceeds max_query_time.
// KILL QUERY interrupts the statement and not the connection, so the client
// gets a clean error and its session survives.
func (s *session) queryWatchdog(c *pool.Conn, query string) func() {
	if s.cfg.MaxQueryTime <= 0 || c.ThreadID == 0 {
		return func() {}
	}
	threadID := c.ThreadID
	owner := c.Owner()
	limit := s.cfg.MaxQueryTime.Std()
	timer := time.AfterFunc(limit, func() {
		s.log.Warn("statement exceeded max_query_time, killing it",
			"max_query_time", limit, "query", query)
		if err := owner.KillQuery(threadID); err != nil {
			s.log.Error("could not kill the runaway statement", "error", err)
		}
	})
	return func() { timer.Stop() }
}

// pinger keeps an attached backend connection alive while the client is
// idle, so a worker that holds its session during a long computation does
// not come back to a connection MySQL has already closed.
func (s *session) pinger() {
	defer close(s.pingDone)
	ticker := time.NewTicker(s.cfg.PingInterval.Std())
	defer ticker.Stop()

	for {
		select {
		case <-s.stopPing:
			return
		case <-ticker.C:
		}

		s.mu.Lock()
		if s.conn != nil && time.Since(s.lastUse) >= s.cfg.PingInterval.Std() {
			if err := s.conn.Ping(); err != nil {
				s.log.Warn("keepalive ping failed, dropping the backend connection", "error", err)
				s.node.Pool().Discard(s.conn)
				s.conn, s.node = nil, nil
			} else {
				s.lastUse = time.Now()
			}
		}
		s.mu.Unlock()
	}
}

// --- server.Handler implementation ---

// markAuthenticated is called by the Server once the handshake succeeded.
func (s *session) markAuthenticated() {
	s.mu.Lock()
	s.authenticated = true
	s.mu.Unlock()
}

// UseDB handles COM_INIT_DB and the database selected during the handshake.
func (s *session) UseDB(dbName string) error {
	defer s.srv.active()()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.db = dbName

	// During the handshake the password has not been checked yet: remember
	// the database and bind it on the first real command. Otherwise anybody
	// able to reach the port could make gora open backend connections by
	// failing to log in.
	if !s.authenticated {
		return nil
	}

	if s.conn == nil {
		// COM_INIT_DB: the client is waiting for a verdict on this database
		// name, so it is checked against the backend right away.
		if _, err := s.backend(statement.KindOther); err != nil {
			return err
		}
		s.maybeRelease()
		return nil
	}

	err := s.conn.UseDB(dbName)
	s.finish(err)
	if err == nil && s.conn != nil {
		s.conn.BoundDB = dbName
		s.maybeRelease()
	}
	return err
}

// HandleQuery handles COM_QUERY.
func (s *session) HandleQuery(query string) (*mysql.Result, error) {
	defer s.srv.active()()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Rewrites come first: everything downstream — the firewall, the cache
	// key, the backend — must see the statement as it will actually run.
	if s.rewriter != nil {
		if rewritten, applied := s.rewriter.Apply(query); len(applied) > 0 {
			s.log.Debug("statement rewritten",
				"rules", strings.Join(applied, ","), "query", rewritten)
			query = rewritten
		}
	}
	if err := s.checkFirewall(query); err != nil {
		return nil, err
	}

	// Paginated listings: SQL_CALC_FOUND_ROWS and the FOUND_ROWS() that
	// follows are cached as one thing. Only outside transactions, where
	// reads have to see the session's own writes.
	if s.cacheActive() && !s.inTx {
		if cache.IsFoundRowsQuery(query) {
			return s.serveFoundRows(query)
		}
		if cache.HasCalcFoundRows(query) && s.cache.PairedCacheable(s.db, query) {
			return s.handleListing(query)
		}
	}
	// Anything else breaks a pending listing/count sequence.
	s.calcPending = ""
	s.foundRowsKnown = false

	kind := statement.Classify(query)
	var dur time.Duration
	exec := func() (*mysql.Result, error) {
		// Throttling happens here rather than at the top of the handler:
		// it protects the database, and a statement answered from memory
		// never reaches it.
		release, err := s.acquireSlot(query)
		if err != nil {
			return nil, err
		}
		defer release()

		c, err := s.backend(kind)
		if err != nil {
			return nil, err
		}
		stopWatchdog := s.queryWatchdog(c, query)
		start := time.Now()
		r, err := c.Execute(query)
		dur = time.Since(start)
		stopWatchdog()
		s.finish(err)
		return r, err
	}

	var (
		r        *mysql.Result
		err      error
		executed = true
	)
	// Inside a transaction the cache is bypassed entirely: a read must see
	// what this session has just written, and nothing else may see it.
	if s.cacheActive() && kind == statement.KindSelect && !s.inTx {
		var outcome cache.Outcome
		r, outcome, err = s.cache.Get(s.db, query, exec)
		executed = outcome == cache.OutcomeExecuted
	} else {
		r, err = exec()
	}
	s.observeProfile(query, dur, r, !executed, err)
	if err != nil {
		return r, err
	}

	if executed {
		s.trackSet(query, classifySet(query))
		s.trackSafety(kind, query, r)
		if s.cacheActive() {
			s.observe(kind, query)
		}
	}
	s.maybeRelease()
	return r, nil
}

// observeProfile forwards one execution to the profiler, when it is on.
// Must be called with s.mu held.
func (s *session) observeProfile(query string, dur time.Duration, r *mysql.Result, cached bool, err error) {
	if s.prof == nil {
		return
	}
	var rows uint64
	if r != nil {
		if r.HasResultset() {
			rows = uint64(len(r.Values))
		} else {
			rows = r.AffectedRows
		}
	}
	s.prof.Observe(s.db, query, dur, rows, cached, err)
}

// checkFirewall refuses a statement a rule says must not run. A dry-run
// match is logged and let through, which is how a rule is tried against
// production traffic before it refuses anything.
func (s *session) checkFirewall(query string) error {
	if s.firewall == nil {
		return nil
	}
	verdict, matched := s.firewall.Check(query)
	if !matched {
		return nil
	}
	if !verdict.Blocked {
		s.log.Warn("firewall rule matched (dry run, statement allowed)",
			"rule", verdict.Rule, "query", query)
		return nil
	}
	s.log.Warn("statement refused by the firewall", "rule", verdict.Rule, "query", query)
	return mysql.NewError(mysql.ER_UNKNOWN_ERROR, verdict.Message)
}

// acquireSlot applies the throttle rules. The returned function releases
// the slot and is always safe to call.
func (s *session) acquireSlot(query string) (func(), error) {
	if s.throttle == nil {
		return func() {}, nil
	}
	release, rule, err := s.throttle.Acquire(query)
	if err != nil {
		s.log.Warn("statement throttled", "rule", rule, "query", query)
		return nil, mysql.NewError(mysql.ER_UNKNOWN_ERROR,
			fmt.Sprintf("gora: %s (throttle rule %q)", err, rule))
	}
	return release, nil
}

// handleListing serves or caches a SQL_CALC_FOUND_ROWS listing, keeping its
// rows and its total together. Must be called with s.mu held.
func (s *session) handleListing(query string) (*mysql.Result, error) {
	if r, foundRows, ok := s.cache.LookupPaired(s.db, query); ok {
		// Rows from memory: arm the answer to the FOUND_ROWS() that is
		// about to arrive, so pagination stays right without a roundtrip.
		s.foundRows = foundRows
		s.foundRowsKnown = true
		s.calcPending = ""
		s.lastUse = time.Now()
		s.observeProfile(query, 0, r, true, nil)
		s.maybeRelease()
		return r, nil
	}

	release, err := s.acquireSlot(query)
	if err != nil {
		return nil, err
	}
	defer release()

	c, err := s.backend(statement.KindSelect)
	if err != nil {
		return nil, err
	}
	stopWatchdog := s.queryWatchdog(c, query)
	start := time.Now()
	r, err := c.Execute(query)
	stopWatchdog()
	s.finish(err)
	s.observeProfile(query, time.Since(start), r, false, err)
	if err != nil {
		return r, err
	}

	s.cache.StorePaired(s.db, query, r)
	// The next FOUND_ROWS() belongs to this statement and reads a counter
	// living on this connection: it is not released here.
	s.calcPending = query
	s.foundRowsKnown = false
	return r, nil
}

// serveFoundRows answers SELECT FOUND_ROWS(): from the pairing cache when
// the listing before it was a hit, otherwise from the backend, in which
// case the count completes the entry stored a moment ago.
// Must be called with s.mu held.
func (s *session) serveFoundRows(query string) (*mysql.Result, error) {
	if s.foundRowsKnown {
		r := cache.FoundRowsResult(s.foundRows)
		s.foundRowsKnown = false
		s.calcPending = ""
		s.lastUse = time.Now()
		s.maybeRelease()
		return r, nil
	}

	c, err := s.backend(statement.KindSelect)
	if err != nil {
		return nil, err
	}
	r, err := c.Execute(query)
	s.finish(err)
	if err == nil && s.calcPending != "" && r != nil && r.Resultset != nil && len(r.Values) > 0 {
		if n, e := r.GetUint(0, 0); e == nil {
			s.cache.PairFoundRows(s.db, s.calcPending, n)
		}
	}
	s.calcPending = ""
	s.maybeRelease()
	return r, err
}

// cacheActive reports whether this session may use the cache.
// Must be called with s.mu held.
func (s *session) cacheActive() bool {
	return s.cache != nil && !s.cacheUnsafe
}

// observe keeps the cache in step with what the session just did.
// Must be called with s.mu held.
func (s *session) observe(kind statement.Kind, query string) {
	switch kind {
	case statement.KindWrite:
		// Invalidate straight away so other sessions stop reading entries
		// that are about to be wrong. Inside a transaction the write is
		// also remembered and replayed at COMMIT, because in the meantime
		// another session may have repopulated those entries with data
		// that predates the commit.
		s.cache.InvalidateWrite(s.db, query)
		if s.inTx {
			if len(s.txWrites) >= maxTrackedTxWrites {
				s.txOverflow = true
			} else {
				s.txWrites = append(s.txWrites, query)
			}
		}
	case statement.KindCommit:
		s.endTx(true)
	case statement.KindRollback:
		s.endTx(false)
	case statement.KindUnsafe:
		s.log.Debug("cache disabled for this session", "query", query)
		s.cacheUnsafe = true
	case statement.KindSelect, statement.KindBegin, statement.KindOther:
		// Nothing to invalidate.
	}
}

// endTx closes the transaction bookkeeping; on commit the writes it
// recorded are invalidated a second time. Must be called with s.mu held.
func (s *session) endTx(commit bool) {
	if commit {
		if s.txOverflow {
			s.cache.Flush("a transaction with too many writes was committed")
		} else {
			for _, q := range s.txWrites {
				s.cache.InvalidateWrite(s.db, q)
			}
		}
	}
	s.txWrites = nil
	s.txOverflow = false
}

// HandleFieldList handles COM_FIELD_LIST, which only old clients send.
func (s *session) HandleFieldList(table string, fieldWildcard string) ([]*mysql.Field, error) {
	defer s.srv.active()()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.backend(statement.KindOther)
	if err != nil {
		return nil, err
	}
	fields, err := c.FieldList(table, fieldWildcard)
	s.finish(err)
	if err == nil {
		s.maybeRelease()
	}
	return fields, err
}

// HandleStmtPrepare handles COM_STMT_PREPARE. A statement handle lives on
// one specific backend connection, so the session keeps it attached until
// every statement is closed.
func (s *session) HandleStmtPrepare(query string) (int, int, any, error) {
	defer s.srv.active()()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Prepared statements are never rewritten — the text is a contract with
	// the client, which holds a handle to it — but the firewall still has a
	// say, or a blocked statement would come back through the front door.
	if err := s.checkFirewall(query); err != nil {
		return 0, 0, nil, err
	}

	// A prepared statement lives on one connection on one server, and its
	// text is only classified when it is executed: it goes to the primary,
	// where a write is allowed to be.
	c, err := s.backend(statement.KindWrite)
	if err != nil {
		return 0, 0, nil, err
	}
	stmt, err := c.Prepare(query)
	s.finish(err)
	if err != nil {
		return 0, 0, nil, err
	}
	s.openStmts++
	return stmt.ParamNum(), stmt.ColumnNum(), stmt, nil
}

// HandleStmtExecute handles COM_STMT_EXECUTE.
func (s *session) HandleStmtExecute(context any, query string, args []any) (*mysql.Result, error) {
	defer s.srv.active()()

	stmt, ok := context.(*client.Stmt)
	if !ok {
		return nil, mysql.NewError(mysql.ER_UNKNOWN_STMT_HANDLER, "unknown prepared statement")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	release, err := s.acquireSlot(query)
	if err != nil {
		return nil, err
	}
	defer release()

	stopWatchdog := func() {}
	if s.conn != nil {
		stopWatchdog = s.queryWatchdog(s.conn, query)
	}
	start := time.Now()
	r, err := stmt.Execute(args...)
	stopWatchdog()
	s.finish(err)
	s.observeProfile(query, time.Since(start), r, false, err)
	if err == nil {
		kind := statement.Classify(query)
		s.trackSafety(kind, query, r)
		if s.cacheActive() && kind != statement.KindSelect {
			// Prepared reads are never served from the cache — the
			// parameters are not part of the statement text — but prepared
			// writes still have to invalidate what they touch.
			s.observe(kind, query)
		}
	}
	return r, err
}

// HandleStmtClose handles COM_STMT_CLOSE.
func (s *session) HandleStmtClose(context any) error {
	defer s.srv.active()()

	stmt, ok := context.(*client.Stmt)
	if !ok {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	err := stmt.Close()
	s.finish(err)
	if s.openStmts > 0 {
		s.openStmts--
	}
	if err == nil {
		s.maybeRelease()
	}
	return err
}

// HandleOtherCommand handles the commands the protocol library does not.
//
// COM_RESET_CONNECTION is what mysqlnd sends to recycle a persistent
// connection, and it is exactly what gora does between sessions anyway, so
// it is honoured. The rest is refused with a message that names the command
// instead of a generic failure: multi-statement mode in particular would
// desynchronise the protocol, since gora relays one result set per query.
func (s *session) HandleOtherCommand(cmd byte, data []byte) error {
	defer s.srv.active()()

	switch cmd {
	case mysql.COM_RESET_CONNECTION:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.conn != nil {
			// Release resets the connection on the way to the pool, which is
			// precisely the semantics the client asked for.
			s.node.Pool().Release(s.conn)
			s.conn, s.node = nil, nil
		}
		s.resetState()
		return nil

	case mysql.COM_SET_OPTION:
		return mysql.NewError(mysql.ER_UNKNOWN_ERROR,
			"multi-statement mode is not supported by gora: send one statement per query")

	default:
		return mysql.NewError(mysql.ER_UNKNOWN_ERROR,
			"command not supported by gora")
	}
}

// resetState forgets everything the session was carrying on its connection.
// Must be called with s.mu held.
func (s *session) resetState() {
	s.inTx = false
	s.holdNext = false
	s.openStmts = 0
	s.setStmts = nil
	s.varSig = ""
	s.txWrites = nil
	s.txOverflow = false
	s.cacheUnsafe = false
	s.calcPending = ""
	s.foundRowsKnown = false
	if s.pinned {
		s.srv.numPinned.Add(-1)
		s.pinned = false
	}
	s.lastUse = time.Now()
}
