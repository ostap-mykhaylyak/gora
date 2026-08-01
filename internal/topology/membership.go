package topology

import (
	"fmt"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

// Membership: nodes come and go while gora is running.
//
// A cluster usually starts as one server. The second one arrives when the
// first is no longer enough, and it arrives on a Tuesday afternoon with
// customers on the site — which is the whole reason this is not a restart.

// AddReplica brings a node into the topology and starts using it for reads
// as soon as the next health check likes what it sees.
func (t *Topology) AddReplica(addr string) error {
	if err := validAddress(addr); err != nil {
		return err
	}

	t.mu.Lock()
	if t.primary != nil && t.primary.Address == addr {
		t.mu.Unlock()
		return fmt.Errorf("%s is the primary", addr)
	}
	for _, r := range t.replicas {
		if r.Address == addr {
			t.mu.Unlock()
			return fmt.Errorf("%s is already a replica", addr)
		}
	}
	backend, poolCfg := t.backend, t.poolCfg
	t.mu.Unlock()

	node, err := t.newNode(backend, addr, RoleReplica, poolCfg, t.log)
	if err != nil {
		return err
	}
	// Until a health check has read its replication status the lag is
	// unknown, which with max_replica_lag set keeps it out of the rotation.
	// A new replica is usually catching up, and reads should wait for it.
	node.lagSeconds.Store(-1)

	t.mu.Lock()
	t.replicas = append(t.replicas, node)
	t.mu.Unlock()

	t.log.Info("node added to the cluster", "node", addr)
	return nil
}

// RemoveNode takes a node out of the topology and closes its pool.
//
// Connections in use are not cut: closing a pool means it stops handing
// connections out and closes them as they come back, so the statements
// running on that node finish on it.
func (t *Topology) RemoveNode(addr string) error {
	t.mu.Lock()
	if t.primary != nil && t.primary.Address == addr {
		t.mu.Unlock()
		return fmt.Errorf("%s is the primary: promote another node first, then remove this one", addr)
	}

	idx := -1
	for i, r := range t.replicas {
		if r.Address == addr {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.mu.Unlock()
		return fmt.Errorf("%s is not part of this cluster", addr)
	}
	node := t.replicas[idx]
	t.replicas = append(t.replicas[:idx], t.replicas[idx+1:]...)
	t.mu.Unlock()

	node.pool.Close()
	t.log.Info("node removed from the cluster", "node", addr)
	return nil
}

// State returns the record the topology reads its membership from. The
// replication manager writes promotions through it, and the command line
// writes membership changes through it; both reach a running gora the same
// way, by being on disk when it next looks.
func (t *Topology) State() *State { return t.state }

// syncState applies a state file changed by another process — `gora
// --add-replica`, `gora --promote` — to a running topology.
func (t *Topology) syncState() {
	if t.state.Path() == "" {
		return
	}
	if err := t.state.Load(); err != nil {
		t.log.Warn("could not re-read the cluster state", "error", err)
		return
	}

	t.mu.RLock()
	configured := t.configuredReplicas
	t.mu.RUnlock()
	wanted := t.state.Members(configured)

	// Anything in the state that is not here yet.
	for _, addr := range wanted {
		if _, ok := t.Node(addr); ok {
			continue
		}
		if err := t.AddReplica(addr); err != nil {
			t.log.Warn("could not add a node the state file lists", "node", addr, "error", err)
		}
	}

	// Anything here that the state no longer lists. The primary is never
	// dropped this way: a state file that removes the node gora is writing
	// to would be a configuration change that takes the site down.
	for _, node := range t.Replicas() {
		if !contains(wanted, node.Address) {
			if err := t.RemoveNode(node.Address); err != nil {
				t.log.Warn("could not remove a node the state file no longer lists",
					"node", node.Address, "error", err)
			}
		}
	}

	if recorded := t.state.PrimaryAddr(); recorded != "" && recorded != t.Primary().Address {
		node, ok := t.Node(recorded)
		if !ok {
			return
		}
		if err := t.Promote(node); err != nil {
			t.log.Warn("could not adopt the recorded primary", "recorded", recorded, "error", err)
			return
		}
		t.log.Warn("adopted a primary promoted elsewhere", "primary", recorded)
	}
}

// validAddress keeps a typo out of the topology, where it would become a
// node that is permanently down.
func validAddress(addr string) error { return config.CheckAddress(addr) }
