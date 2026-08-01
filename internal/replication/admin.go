package replication

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// adminTimeout bounds a single administrative connection attempt.
const adminTimeout = 10 * time.Second

// admin is a short-lived privileged connection to one node. Cluster
// operations do not go through the proxy's pool: they use credentials the
// proxy account does not have, and they must work when the pool is refusing
// to hand anything out.
type admin struct {
	*client.Conn
	addr    string
	dialect dialect
	version string
}

// connect opens an administrative connection and works out which
// replication vocabulary the server speaks.
func (m *Manager) connect(ctx context.Context, addr string) (*admin, error) {
	c, err := client.ConnectWithContext(ctx, addr, m.cfg.AdminUsername, m.cfg.AdminPassword, "", adminTimeout)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s as %s: %w", addr, m.cfg.AdminUsername, err)
	}

	version, err := scalarString(c, "SELECT VERSION()")
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("reading the version of %s: %w", addr, err)
	}
	return &admin{Conn: c, addr: addr, dialect: dialectFor(version), version: version}, nil
}

// exec runs a statement, wrapping the error with what was being attempted.
func (a *admin) exec(format string, args ...any) error {
	stmt := fmt.Sprintf(format, args...)
	if _, err := a.Execute(stmt); err != nil {
		return fmt.Errorf("%s: %s: %w", a.addr, redact(stmt), err)
	}
	return nil
}

// try runs a statement and swallows the error, for the ones that are
// allowed to fail: stopping replication that was never started, dropping
// something that is not there.
func (a *admin) try(format string, args ...any) {
	_, _ = a.Execute(fmt.Sprintf(format, args...))
}

// redact hides a password in a statement before it reaches a log or an
// error. Setting up replication means sending credentials; printing them
// back out would put them in the journal of every failed attempt.
func redact(stmt string) string {
	for _, marker := range []string{"IDENTIFIED BY", "IDENTIFIED WITH", "PASSWORD ="} {
		if i := indexFold(stmt, marker); i >= 0 {
			return stmt[:i] + marker + " <redacted>"
		}
	}
	if i := indexFold(stmt, "SOURCE_PASSWORD"); i >= 0 {
		return stmt[:i] + "SOURCE_PASSWORD = <redacted> ..."
	}
	if i := indexFold(stmt, "MASTER_PASSWORD"); i >= 0 {
		return stmt[:i] + "MASTER_PASSWORD = <redacted> ..."
	}
	return stmt
}

func indexFold(s, sub string) int {
	return strings.Index(strings.ToUpper(s), sub)
}

// scalarString runs a query and returns the first column of the first row.
func scalarString(c *client.Conn, query string) (string, error) {
	r, err := c.Execute(query)
	if err != nil {
		return "", err
	}
	if r == nil || len(r.Values) == 0 {
		return "", fmt.Errorf("%s returned no rows", query)
	}
	return r.GetString(0, 0)
}

// scalarUint runs a query and returns the first column of the first row as
// a number.
func scalarUint(c *client.Conn, query string) (uint64, error) {
	r, err := c.Execute(query)
	if err != nil {
		return 0, err
	}
	if r == nil || len(r.Values) == 0 {
		return 0, fmt.Errorf("%s returned no rows", query)
	}
	return r.GetUint(0, 0)
}

// rowByName reads a named column of the first row, for the wide status
// results whose column order nobody should depend on.
func rowByName(r *mysql.Result, name string) (string, bool) {
	if r == nil || len(r.Values) == 0 {
		return "", false
	}
	for _, f := range r.Fields {
		if f != nil && strings.EqualFold(string(f.Name), name) {
			v, err := r.GetStringByName(0, string(f.Name))
			if err != nil {
				return "", false
			}
			return v, true
		}
	}
	return "", false
}
