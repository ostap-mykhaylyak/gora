// Package replication sets up and governs MySQL's own replication.
//
// gora does not copy data between servers itself. MySQL has done that for
// twenty years, with binary logs and GTIDs, and a proxy reimplementing it
// would be a proxy with a worse copy of a solved problem. What is missing
// is the part above: deciding which server is the primary, telling the
// others where to replicate from, noticing when that stops being true, and
// pointing the writes somewhere else.
//
// That is what this package does. Given empty MySQL servers and an
// administrative account, it configures them into a cluster; given a
// cluster, it keeps them agreeing with each other and with gora.
package replication

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/topology"
)

// reconcileInterval is how often the cluster is re-read. It is a constant
// rather than a setting: this loop opens an administrative connection per
// node, and checking more often than the health checks that feed it would
// buy nothing.
const reconcileInterval = 5 * time.Second

// Manager configures and governs the cluster.
type Manager struct {
	cfg  atomic.Pointer[config.Replication]
	topo *topology.Topology
	log  *slog.Logger

	mu     sync.Mutex
	status map[string]NodeStatus
	// downSince records when the primary stopped answering, so an automatic
	// failover can wait out a hiccup instead of promoting on it.
	downSince time.Time
}

// NodeStatus is what a node says about its own replication.
type NodeStatus struct {
	Address string `json:"address"`
	// SourceHost is who it replicates from, empty when it is not replicating.
	SourceHost string `json:"source_host,omitempty"`
	IORunning  bool   `json:"io_running"`
	SQLRunning bool   `json:"sql_running"`
	LagSeconds int64  `json:"lag_seconds"`
	LastError  string `json:"last_error,omitempty"`
	Checked    bool   `json:"checked"`
}

// New builds the manager. The cluster state — who is the primary, which
// nodes exist — belongs to the topology, which has already read it.
func New(cfg config.Replication, topo *topology.Topology, log *slog.Logger) (*Manager, error) {
	m := &Manager{
		topo:   topo,
		log:    log,
		status: make(map[string]NodeStatus),
	}
	m.cfg.Store(&cfg)
	return m, nil
}

// conf returns the settings in force.
func (m *Manager) conf() config.Replication { return *m.cfg.Load() }

// SetConfig replaces them while gora runs. Switching failover from manual
// to automatic during an incident is exactly when a restart is least
// welcome.
func (m *Manager) SetConfig(cfg config.Replication) { m.cfg.Store(&cfg) }

// Status returns what each node last said about its replication.
func (m *Manager) Status() []NodeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]NodeStatus, 0, len(m.status))
	for _, n := range m.topo.Nodes() {
		if st, ok := m.status[n.Address]; ok {
			out = append(out, st)
		}
	}
	return out
}

// Run keeps the cluster and gora agreeing with each other until ctx is
// cancelled: it re-reads each node's replication status, repoints anything
// following the wrong server, and — if that is what it was asked to do —
// promotes a replica when the primary is gone for good.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m.Reconcile(ctx)
	}
}

// Reconcile reads every node's replication status and fixes what it can.
func (m *Manager) Reconcile(ctx context.Context) {
	primary := m.topo.Primary()

	for _, node := range m.topo.Nodes() {
		st := m.readStatus(ctx, node)
		m.mu.Lock()
		m.status[node.Address] = st
		m.mu.Unlock()

		if node == primary || !st.Checked {
			continue
		}
		// A replica following somebody other than the current primary is
		// the state a promotion leaves behind, and the state a returning
		// old primary comes back in.
		if !sameHost(st.SourceHost, primary.Address) {
			m.log.Warn("replica is following the wrong server, repointing it",
				"node", node.Address, "following", st.SourceHost, "primary", primary.Address)
			if err := m.pointAt(ctx, node.Address, primary.Address); err != nil {
				m.log.Error("could not repoint the replica", "node", node.Address, "error", err)
			}
		}
	}

	m.watchPrimary(ctx, primary)
}

// watchPrimary decides whether the primary being unreachable has gone on
// long enough to do something about it.
func (m *Manager) watchPrimary(ctx context.Context, primary *topology.Node) {
	if primary.Up() {
		m.mu.Lock()
		m.downSince = time.Time{}
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	if m.downSince.IsZero() {
		m.downSince = time.Now()
	}
	down := time.Since(m.downSince)
	m.mu.Unlock()

	if down < m.conf().FailoverDelay.Std() {
		return
	}

	if m.conf().Failover != config.FailoverAutomatic {
		// Said once per reconcile while it lasts: an operator reading the
		// log during an outage should find the sentence that tells them
		// what gora is waiting for.
		m.log.Error("the primary has been unreachable and failover is manual",
			"primary", primary.Address, "down_for", down.Round(time.Second),
			"hint", "promote a replica with: gora --promote <address>")
		return
	}

	candidate := m.bestCandidate()
	if candidate == nil {
		m.log.Error("the primary is unreachable and no replica can be promoted",
			"primary", primary.Address)
		return
	}
	m.log.Warn("promoting a replica automatically",
		"candidate", candidate.Address, "primary_down_for", down.Round(time.Second))
	if err := m.Promote(ctx, candidate.Address); err != nil {
		m.log.Error("automatic failover failed", "candidate", candidate.Address, "error", err)
	}
}

// bestCandidate picks the replica to promote: reachable, actually applying
// what it receives, and the least far behind.
//
// gora does not compare GTID sets to find the most advanced node. With
// asynchronous replication the transactions a replica never received are
// gone whichever one you choose; pretending otherwise would be a promise
// this cannot keep.
func (m *Manager) bestCandidate() *topology.Node {
	m.mu.Lock()
	status := make(map[string]NodeStatus, len(m.status))
	for k, v := range m.status {
		status[k] = v
	}
	m.mu.Unlock()

	var best *topology.Node
	var bestLag int64 = -1
	for _, node := range m.topo.Replicas() {
		if !node.Up() {
			continue
		}
		st, ok := status[node.Address]
		if !ok || !st.SQLRunning {
			continue
		}
		if best == nil || st.LagSeconds < bestLag {
			best, bestLag = node, st.LagSeconds
		}
	}
	return best
}

// sameHost compares a replication source host with a node address.
func sameHost(sourceHost, address string) bool {
	if sourceHost == "" {
		return false
	}
	host := address
	if i := indexByte(address, ':'); i >= 0 {
		host = address[:i]
	}
	return sourceHost == host
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// splitHostPort splits an address gora already validated at load time.
func splitHostPort(addr string) (string, string) {
	if i := indexByte(addr, ':'); i >= 0 {
		return addr[:i], addr[i+1:]
	}
	return addr, "3306"
}

// readStatus asks a node about its replication.
func (m *Manager) readStatus(ctx context.Context, node *topology.Node) NodeStatus {
	st := NodeStatus{Address: node.Address, LagSeconds: -1}

	conn, err := m.connect(ctx, node.Address)
	if err != nil {
		st.LastError = err.Error()
		return st
	}
	defer conn.Close()

	r, err := conn.Execute(conn.dialect.showReplicaStatus())
	if err != nil {
		st.LastError = err.Error()
		return st
	}
	st.Checked = true
	if r == nil || len(r.Values) == 0 {
		return st // not replicating, which is what a primary looks like
	}

	d := conn.dialect
	st.SourceHost, _ = rowByName(r, d.statusField("Source_Host", "Master_Host"))
	io, _ := rowByName(r, d.statusField("Replica_IO_Running", "Slave_IO_Running"))
	sql, _ := rowByName(r, d.statusField("Replica_SQL_Running", "Slave_SQL_Running"))
	st.IORunning = io == "Yes"
	st.SQLRunning = sql == "Yes"

	if lag, ok := rowByName(r, d.statusField("Seconds_Behind_Source", "Seconds_Behind_Master")); ok && lag != "" {
		if n, err := parseInt(lag); err == nil {
			st.LagSeconds = n
		}
	}
	if msg, ok := rowByName(r, "Last_Error"); ok && msg != "" {
		st.LastError = msg
	}
	if msg, ok := rowByName(r, d.statusField("Last_IO_Error", "Last_IO_Error")); ok && msg != "" {
		st.LastError = msg
	}
	return st
}

func parseInt(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}
