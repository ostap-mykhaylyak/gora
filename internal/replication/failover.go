package replication

import (
	"context"
	"fmt"
)

// Promote makes the node at addr the primary.
//
// The order matters and is the reverse of what feels natural: the new
// primary is made writable first and recorded second, because a gora that
// crashes between the two comes back believing the old primary is still the
// primary — which is wrong but harmless — while the other order would leave
// it writing to a server that is still read-only.
func (m *Manager) Promote(ctx context.Context, addr string) error {
	node, ok := m.topo.Node(addr)
	if !ok {
		return fmt.Errorf("%s is not one of the configured nodes", addr)
	}
	old := m.topo.Primary()
	if node == old {
		return fmt.Errorf("%s is already the primary", addr)
	}

	conn, err := m.connect(ctx, addr)
	if err != nil {
		return fmt.Errorf("cannot promote %s: %w", addr, err)
	}
	defer conn.Close()

	d := conn.dialect
	conn.try("%s", d.stopReplica())
	// RESET ... ALL, not plain RESET: the connection details have to go, or
	// the new primary starts following its own old primary again the next
	// time somebody restarts it.
	if err := conn.exec("%s", d.resetReplica()); err != nil {
		return err
	}
	if err := setGlobal(conn, "super_read_only", "OFF"); err != nil {
		return err
	}
	if err := setGlobal(conn, "read_only", "OFF"); err != nil {
		return err
	}

	if err := m.topo.Promote(node); err != nil {
		return err
	}
	if err := m.topo.State().SetPrimary(addr, "promoted"); err != nil {
		m.log.Error("the promotion worked but could not be recorded; "+
			"a restart of gora would go back to the old primary", "error", err)
	}

	// Everything else follows the new primary. The old one is almost
	// certainly unreachable — that is usually why this is happening — and
	// gets repointed by the reconcile loop when it comes back.
	for _, other := range m.topo.Replicas() {
		if err := m.pointAt(ctx, other.Address, addr); err != nil {
			m.log.Warn("could not repoint a replica at the new primary",
				"node", other.Address, "primary", addr, "error", err)
		}
	}

	m.log.Warn("promotion complete", "primary", addr, "previous", old.Address)
	return nil
}

// pointAt makes one node replicate from another, and stops it accepting
// writes of its own.
func (m *Manager) pointAt(ctx context.Context, addr, primaryAddr string) error {
	conn, err := m.connect(ctx, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := setGlobal(conn, "super_read_only", "ON"); err != nil {
		return err
	}
	return m.startReplication(conn, primaryAddr, discard{})
}

// discard swallows the progress output of an operation nobody is watching.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
