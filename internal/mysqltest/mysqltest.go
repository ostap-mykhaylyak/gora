// Package mysqltest runs an in-process MySQL server for tests.
//
// It speaks the real wire protocol (the same library gora serves clients
// with), so pool and proxy tests exercise handshakes, result sets and
// connection reuse rather than a mock of them. It is only ever imported
// from _test.go files, so it is not part of the shipped binary.
package mysqltest

import (
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
)

// Server is a fake MySQL backend and a record of what it was asked to do.
type Server struct {
	Addr string

	// Accepted counts connections that reached the handshake, which is how
	// tests tell reuse from re-dialling.
	Accepted atomic.Int64
	// Resets counts COM_RESET_CONNECTION commands.
	Resets atomic.Int64
	// Kills counts KILL QUERY statements.
	Kills atomic.Int64

	mu      sync.Mutex
	queries []string
}

// Start runs a fake backend accepting the given credentials, stopped
// automatically when the test ends.
func Start(t *testing.T, user, password string) *Server {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the fake backend: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	conf := server.NewServer("8.0.36", mysql.DEFAULT_COLLATION_ID,
		mysql.AUTH_NATIVE_PASSWORD, nil, nil)
	auth := server.NewInMemoryAuthenticationHandler(mysql.AUTH_NATIVE_PASSWORD)
	if err := auth.AddUser(user, password); err != nil {
		t.Fatalf("adding the fake backend user: %v", err)
	}

	s := &Server{Addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.Accepted.Add(1)
			go func() {
				conn, err := server.NewCustomizedConn(c, conf, auth, &handler{srv: s})
				if err != nil {
					return
				}
				// A real MySQL reports autocommit in the status flags of
				// every OK and EOF packet. Without this the client sees
				// IsAutoCommit() == false and gora, correctly, refuses to
				// return the connection to the pool.
				conn.SetStatus(mysql.SERVER_STATUS_AUTOCOMMIT)
				for !conn.Closed() {
					if conn.HandleCommand() != nil {
						return
					}
				}
			}()
		}
	}()
	return s
}

// Queries returns every statement the backend received, in order.
func (s *Server) Queries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

// Count returns how many received statements contain sub (case-insensitive).
func (s *Server) Count(sub string) int {
	sub = strings.ToUpper(sub)
	n := 0
	for _, q := range s.Queries() {
		if strings.Contains(strings.ToUpper(q), sub) {
			n++
		}
	}
	return n
}

type handler struct{ srv *Server }

func (h *handler) UseDB(string) error { return nil }

func (h *handler) HandleQuery(query string) (*mysql.Result, error) {
	h.srv.mu.Lock()
	h.srv.queries = append(h.srv.queries, query)
	h.srv.mu.Unlock()

	upper := strings.ToUpper(strings.TrimSpace(query))
	switch {
	case strings.HasPrefix(upper, "KILL QUERY"):
		h.srv.Kills.Add(1)
		return okResult(), nil
	case strings.HasPrefix(upper, "SELECT"):
		rs, err := mysql.BuildSimpleTextResultset([]string{"v"}, [][]any{{int64(1)}})
		if err != nil {
			return nil, err
		}
		return mysql.NewResult(rs), nil
	default:
		// SET, BEGIN, COMMIT, DDL: an OK packet, like the real server.
		return okResult(), nil
	}
}

// okResult is an OK packet carrying the autocommit flag. Without it
// client.Conn reports IsAutoCommit() == false, and gora would never release
// a connection back to the pool.
func okResult() *mysql.Result {
	return &mysql.Result{Status: mysql.SERVER_STATUS_AUTOCOMMIT}
}

func (h *handler) HandleFieldList(string, string) ([]*mysql.Field, error) { return nil, nil }

func (h *handler) HandleStmtPrepare(query string) (int, int, any, error) {
	h.srv.mu.Lock()
	h.srv.queries = append(h.srv.queries, query)
	h.srv.mu.Unlock()
	return 0, 0, nil, nil
}

func (h *handler) HandleStmtExecute(any, string, []any) (*mysql.Result, error) {
	return okResult(), nil
}

func (h *handler) HandleStmtClose(any) error { return nil }

func (h *handler) HandleOtherCommand(cmd byte, _ []byte) error {
	if cmd == mysql.COM_RESET_CONNECTION {
		h.srv.Resets.Add(1)
	}
	return nil
}
