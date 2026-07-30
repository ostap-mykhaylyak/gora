package proxy

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/pool"
	"github.com/ostap-mykhaylyak/gora/internal/statement"
)

// maxTrackedSets caps the SET statements replayed on connection reuse.
// A session that sets more than this is doing something gora should not try
// to reproduce, so it gets pinned instead.
const maxTrackedSets = 20

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
	ctx  context.Context
	srv  *Server
	pool *pool.Pool
	cfg  config.Pool
	log  *slog.Logger

	mu      sync.Mutex // guards everything below, including against the pinger
	conn    *pool.Conn
	db      string
	lastUse time.Time

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

	stopPing chan struct{}
	pingDone chan struct{}
}

func newSession(ctx context.Context, srv *Server, log *slog.Logger) *session {
	s := &session{
		ctx:      ctx,
		srv:      srv,
		pool:     srv.pool,
		cfg:      srv.cfg,
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
		s.pool.Release(s.conn)
		s.conn = nil
	}
	if s.pinned {
		s.srv.numPinned.Add(-1)
		s.pinned = false
	}
}

// backend returns the attached connection, acquiring and preparing one when
// the session has none. Must be called with s.mu held.
func (s *session) backend() (*pool.Conn, error) {
	if s.conn != nil {
		return s.conn, nil
	}

	// Two attempts: a parked connection can fail preparation because it died
	// while parked, and a freshly dialed one deserves the second try.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		c, err := s.pool.Acquire(s.ctx)
		if err != nil {
			return nil, err
		}
		if err := s.prepareConn(c); err != nil {
			lastErr = err
			if isConnError(err) {
				s.pool.Discard(c)
				continue
			}
			// A MySQL-level error (unknown database, denied access): the
			// connection is fine, the request is not.
			s.pool.Release(c)
			return nil, err
		}
		s.conn = c
		return c, nil
	}
	return nil, lastErr
}

// prepareConn aligns a pooled connection with this session's environment:
// it clears foreign session variables, binds the database and replays the
// tracked SETs. In the steady state every WordPress session issues the same
// SET NAMES, the signatures match and nothing is sent at all.
// Must be called with s.mu held.
func (s *session) prepareConn(c *pool.Conn) error {
	if c.VarSig != s.varSig && c.VarSig != "" {
		// The connection carries another session's variables.
		if err := s.pool.ResetConn(c); err != nil {
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
	if !s.mux || s.conn == nil {
		return
	}
	if s.pinned || s.inTx || s.openStmts > 0 {
		return
	}
	// The status flags of the last OK/EOF packet catch implicit transactions
	// (autocommit=0) that keyword tracking alone would miss.
	if s.conn.IsInTransaction() || !s.conn.IsAutoCommit() {
		return
	}
	if s.holdNext {
		s.holdNext = false
		return
	}
	c := s.conn
	s.conn = nil
	s.pool.ReleaseClean(c)
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
		s.pool.Discard(s.conn)
		s.conn = nil
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
	limit := s.cfg.MaxQueryTime.Std()
	timer := time.AfterFunc(limit, func() {
		s.log.Warn("statement exceeded max_query_time, killing it",
			"max_query_time", limit, "query", query)
		if err := s.pool.KillQuery(threadID); err != nil {
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
				s.pool.Discard(s.conn)
				s.conn = nil
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
		if _, err := s.backend(); err != nil {
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

	c, err := s.backend()
	if err != nil {
		return nil, err
	}

	stopWatchdog := s.queryWatchdog(c, query)
	r, err := c.Execute(query)
	stopWatchdog()
	s.finish(err)
	if err != nil {
		return r, err
	}

	s.trackSet(query, classifySet(query))
	s.trackSafety(statement.Classify(query), query, r)
	s.maybeRelease()
	return r, nil
}

// HandleFieldList handles COM_FIELD_LIST, which only old clients send.
func (s *session) HandleFieldList(table string, fieldWildcard string) ([]*mysql.Field, error) {
	defer s.srv.active()()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.backend()
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

	c, err := s.backend()
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

	stopWatchdog := func() {}
	if s.conn != nil {
		stopWatchdog = s.queryWatchdog(s.conn, query)
	}
	r, err := stmt.Execute(args...)
	stopWatchdog()
	s.finish(err)
	if err == nil {
		s.trackSafety(statement.Classify(query), query, r)
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
			s.pool.Release(s.conn)
			s.conn = nil
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
	if s.pinned {
		s.srv.numPinned.Add(-1)
		s.pinned = false
	}
	s.lastUse = time.Now()
}
