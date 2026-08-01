package topology

import (
	"context"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// checkTimeout bounds one node check. It is short on purpose: a health
// check that waits as long as a query is not a health check.
const checkTimeout = 3 * time.Second

// checkAll checks every node in parallel, so a node that has stopped
// answering does not delay the verdict on the others.
func (t *Topology) checkAll(ctx context.Context) {
	nodes := t.Nodes()
	done := make(chan struct{}, len(nodes))
	for _, n := range nodes {
		go func(n *Node) {
			t.check(ctx, n)
			done <- struct{}{}
		}(n)
	}
	for range nodes {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
}

// check asks a node three things: are you there, are you read-only, and —
// if it is a replica — how far behind are you.
func (t *Topology) check(ctx context.Context, n *Node) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	n.checks.Add(1)

	conn, err := n.pool.Acquire(ctx)
	if err != nil {
		n.markDown(err)
		return
	}
	defer n.pool.ReleaseClean(conn)

	readOnly, err := readOnlyFlag(conn)
	if err != nil {
		// Reachable but unreadable: the connection worked, the question did
		// not. That is a node that is up with one fact missing, not a node
		// that is down.
		n.log.Debug("could not read the node's read_only flag", "error", err)
	} else {
		n.readOnly.Store(readOnly)
	}

	if n.Role() == RoleReplica {
		if lag, ok := replicaLag(conn); ok {
			n.lagSeconds.Store(lag)
		} else {
			n.lagSeconds.Store(-1)
		}
	} else {
		n.lagSeconds.Store(0)
	}

	n.markUp()
}

// markUp records a successful check, logging the transition once.
func (n *Node) markUp() {
	n.lastError.Store(nil)
	if !n.up.Swap(true) {
		n.log.Info("node is back")
	}
}

// markDown records a failed check.
func (n *Node) markDown(err error) {
	n.failures.Add(1)
	msg := err.Error()
	n.lastError.Store(&msg)
	n.lagSeconds.Store(-1)
	if n.up.Swap(false) {
		n.log.Error("node is not answering", "error", err)
	}
}

// readOnlyFlag reports whether the server refuses writes. A primary that
// has become read-only is the failover that happened without telling gora.
func readOnlyFlag(conn interface {
	Execute(string, ...any) (*mysql.Result, error)
}) (bool, error) {
	r, err := conn.Execute("SELECT @@global.read_only")
	if err != nil {
		return false, err
	}
	if r == nil || len(r.Values) == 0 {
		return false, nil
	}
	v, err := r.GetUint(0, 0)
	if err != nil {
		return false, err
	}
	return v != 0, nil
}

// replicaLag reads how many seconds a replica is behind its source.
//
// The column was renamed in MySQL 8.0.22 along with the statement itself,
// so both spellings are tried: gora is not going to tell somebody their
// perfectly working MySQL 5.7 replica is unhealthy because of vocabulary.
func replicaLag(conn interface {
	Execute(string, ...any) (*mysql.Result, error)
}) (int64, bool) {
	for _, stmt := range []string{"SHOW REPLICA STATUS", "SHOW SLAVE STATUS"} {
		r, err := conn.Execute(stmt)
		if err != nil || r == nil || len(r.Values) == 0 {
			continue
		}
		for _, name := range []string{"Seconds_Behind_Source", "Seconds_Behind_Master"} {
			if !hasField(r, name) {
				continue
			}
			v, err := r.GetUintByName(0, name)
			if err != nil {
				// NULL: replication is not running at all, which is worse
				// than lagging.
				return 0, false
			}
			return int64(v), true
		}
	}
	return 0, false
}

func hasField(r *mysql.Result, name string) bool {
	for _, f := range r.Fields {
		if f != nil && strings.EqualFold(string(f.Name), name) {
			return true
		}
	}
	return false
}
