// Package topology knows which MySQL servers gora talks to, which of them
// is the primary, and which are healthy enough to read from.
//
// Everything above it asks two questions: where do writes go, and where may
// this read go. The answers change while gora runs — a replica falls
// behind, a node stops answering, the primary comes back — and they change
// without anybody restarting anything.
package topology

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/pool"
)

// Role says what a node is for.
type Role string

const (
	// RolePrimary takes the writes.
	RolePrimary Role = "primary"
	// RoleReplica takes reads.
	RoleReplica Role = "replica"
)

// Node is one MySQL server and gora's pool of connections to it.
//
// The role is not a constant: a promotion turns a replica into the primary
// while gora is running, and the sessions holding connections to it carry
// on as if nothing had happened.
type Node struct {
	Address string

	role atomic.Pointer[Role]
	pool *pool.Pool
	log  *slog.Logger

	up         atomic.Bool
	readOnly   atomic.Bool
	lagSeconds atomic.Int64 // -1 when unknown
	lastError  atomic.Pointer[string]
	checks     atomic.Uint64
	failures   atomic.Uint64
}

// Pool returns the node's connection pool.
func (n *Node) Pool() *pool.Pool { return n.pool }

// Role returns what the node is for right now.
func (n *Node) Role() Role {
	if r := n.role.Load(); r != nil {
		return *r
	}
	return RoleReplica
}

func (n *Node) setRole(r Role) { n.role.Store(&r) }

// Up reports whether the last health check reached the node.
func (n *Node) Up() bool { return n.up.Load() }

// Lag returns how far behind the node is, and whether that is known.
func (n *Node) Lag() (time.Duration, bool) {
	s := n.lagSeconds.Load()
	if s < 0 {
		return 0, false
	}
	return time.Duration(s) * time.Second, true
}

// NodeStats is a node as `gora status` reports it.
type NodeStats struct {
	Address    string     `json:"address"`
	Role       Role       `json:"role"`
	Up         bool       `json:"up"`
	ReadOnly   bool       `json:"read_only"`
	LagSeconds int64      `json:"lag_seconds"` // -1 when unknown
	Eligible   bool       `json:"eligible_for_reads"`
	LastError  string     `json:"last_error,omitempty"`
	Pool       pool.Stats `json:"pool"`
}

// Topology is the set of nodes gora forwards to.
type Topology struct {
	cfg config.Routing
	log *slog.Logger

	mu       sync.RWMutex
	primary  *Node
	replicas []*Node

	// next rotates the reads across the eligible replicas. Round-robin is
	// enough: gora is not trying to be a load balancer, it is trying not to
	// send everything to one machine.
	next atomic.Uint64

	stop chan struct{}
}

// New builds the topology and its pools. Nothing is dialled here: a node
// that is down must delay traffic, not startup.
func New(backend config.Backend, poolCfg config.Pool, routing config.Routing, log *slog.Logger) (*Topology, error) {
	t := &Topology{cfg: routing, log: log, stop: make(chan struct{})}

	primary, err := t.newNode(backend, backend.Address, RolePrimary, poolCfg, log)
	if err != nil {
		return nil, err
	}
	t.primary = primary

	for _, addr := range backend.Replicas {
		replica, err := t.newNode(backend, addr, RoleReplica, poolCfg, log)
		if err != nil {
			t.Close()
			return nil, err
		}
		t.replicas = append(t.replicas, replica)
	}
	return t, nil
}

func (t *Topology) newNode(backend config.Backend, addr string, role Role, poolCfg config.Pool, log *slog.Logger) (*Node, error) {
	nodeCfg := backend
	nodeCfg.Address = addr

	nodeLog := log.With("node", addr, "role", string(role))
	p, err := pool.New(nodeCfg, poolCfg, nodeLog, nil)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", addr, err)
	}

	n := &Node{Address: addr, pool: p, log: nodeLog}
	n.setRole(role)
	// Assumed reachable until a check says otherwise: refusing traffic for
	// the first health interval would make every restart an outage.
	n.up.Store(true)
	n.lagSeconds.Store(-1)
	return n, nil
}

// Primary returns the node writes go to.
func (t *Topology) Primary() *Node {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.primary
}

// HasReplicas reports whether there is anything to split reads onto.
func (t *Topology) HasReplicas() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.replicas) > 0
}

// Replicas returns the read nodes.
func (t *Topology) Replicas() []*Node {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]*Node(nil), t.replicas...)
}

// Nodes returns every node, primary first.
func (t *Topology) Nodes() []*Node {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]*Node{t.primary}, t.replicas...)
}

// Node returns the node at an address.
func (t *Topology) Node(address string) (*Node, bool) {
	for _, n := range t.Nodes() {
		if n.Address == address {
			return n, true
		}
	}
	return nil, false
}

// Promote makes a replica the primary and demotes the old one, which
// becomes a replica of the new primary as soon as it answers again.
//
// This only changes gora's idea of the cluster. Telling MySQL about it is
// the replication manager's job, and it calls this once the servers agree.
func (t *Topology) Promote(n *Node) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if n == t.primary {
		return fmt.Errorf("node %s is already the primary", n.Address)
	}
	idx := -1
	for i, r := range t.replicas {
		if r == n {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("node %s is not part of this cluster", n.Address)
	}

	old := t.primary
	t.replicas[idx] = old
	old.setRole(RoleReplica)
	// The demoted node keeps whatever lag it last reported; it is about to
	// be repointed, and until a check says otherwise its lag is unknown.
	old.lagSeconds.Store(-1)

	t.primary = n
	n.setRole(RolePrimary)
	n.lagSeconds.Store(0)
	t.log.Warn("primary changed", "new_primary", n.Address, "old_primary", old.Address)
	return nil
}

// PickReader returns the node a read should go to: an eligible replica in
// turn, or the primary when there is none. It never returns nil — a read
// has to go somewhere, and the primary is where it went before there were
// replicas at all.
func (t *Topology) PickReader() *Node {
	t.mu.RLock()
	primary, replicas := t.primary, t.replicas
	t.mu.RUnlock()

	n := len(replicas)
	if n == 0 {
		return primary
	}

	start := int(t.next.Add(1) % uint64(n))
	for i := 0; i < n; i++ {
		candidate := replicas[(start+i)%n]
		if t.eligible(candidate) {
			return candidate
		}
	}
	// Every replica is down or too far behind: the reads go where the data
	// certainly is.
	return primary
}

// eligible reports whether a replica may serve reads right now.
func (t *Topology) eligible(n *Node) bool {
	if !n.up.Load() {
		return false
	}
	if t.cfg.MaxReplicaLag <= 0 {
		return true
	}
	lag := n.lagSeconds.Load()
	if lag < 0 {
		// Unknown lag: gora could not read the replication status. Trusting
		// it would mean serving reads from a node that may be a day behind.
		return false
	}
	return time.Duration(lag)*time.Second <= t.cfg.MaxReplicaLag.Std()
}

// WritesAccepted reports whether the primary can currently take a write.
func (t *Topology) WritesAccepted() bool {
	p := t.Primary()
	return p.up.Load() && !p.readOnly.Load()
}

// Stat returns every node, primary first.
func (t *Topology) Stat() []NodeStats {
	nodes := t.Nodes()
	out := make([]NodeStats, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, t.stat(n))
	}
	return out
}

func (t *Topology) stat(n *Node) NodeStats {
	st := NodeStats{
		Address:    n.Address,
		Role:       n.Role(),
		Up:         n.up.Load(),
		ReadOnly:   n.readOnly.Load(),
		LagSeconds: n.lagSeconds.Load(),
		Pool:       n.pool.Stat(),
	}
	if n.Role() == RoleReplica {
		st.Eligible = t.eligible(n)
	} else {
		st.Eligible = n.up.Load()
	}
	if msg := n.lastError.Load(); msg != nil {
		st.LastError = *msg
	}
	return st
}

// Close shuts down every pool.
func (t *Topology) Close() {
	select {
	case <-t.stop:
	default:
		close(t.stop)
	}
	for _, n := range t.Nodes() {
		if n != nil {
			n.pool.Close()
		}
	}
}

// Run checks the nodes until ctx is cancelled.
func (t *Topology) Run(ctx context.Context) {
	// A first round straight away: waiting one interval before knowing
	// anything means routing blind for exactly as long as the interval.
	t.checkAll(ctx)

	ticker := time.NewTicker(t.cfg.HealthInterval.Std())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stop:
			return
		case <-ticker.C:
		}
		t.checkAll(ctx)
	}
}
