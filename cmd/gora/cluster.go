package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/replication"
	"github.com/ostap-mykhaylyak/gora/internal/topology"
)

// cluster builds the topology and the replication manager for a one-off
// command, without starting the proxy. These run from a shell while the
// service is either up or down, and they must work either way.
func cluster(configPath string, stdout io.Writer) (*topology.Topology, *replication.Manager, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.Replication.Enabled {
		return nil, nil, fmt.Errorf("replication is disabled in %s", configPath)
	}

	// Progress goes to stdout, where the operator is looking; the manager's
	// own log would otherwise be a second, quieter copy of it.
	log := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	topo, err := topology.New(cfg.Backend, cfg.Pool, cfg.Routing, log)
	if err != nil {
		return nil, nil, err
	}
	manager, err := replication.New(cfg.Replication, topo, log)
	if err != nil {
		topo.Close()
		return nil, nil, err
	}
	return topo, manager, nil
}

// initCluster configures the servers into a replicating cluster.
func initCluster(configPath string, stdout io.Writer) error {
	topo, manager, err := cluster(configPath, stdout)
	if err != nil {
		return err
	}
	defer topo.Close()

	return manager.Provision(context.Background(), stdout)
}

// promote makes a node the primary by hand. It is what `failover: manual`
// leaves you to do, and the state file is how the running gora finds out.
func promote(configPath, addr string, stdout io.Writer) error {
	topo, manager, err := cluster(configPath, stdout)
	if err != nil {
		return err
	}
	defer topo.Close()

	// The status of every node is read first: promoting the replica that is
	// furthest behind, because nobody looked, is a way to lose more data
	// than the failure did.
	ctx := context.Background()
	manager.Reconcile(ctx)
	for _, st := range manager.Status() {
		fmt.Fprintf(stdout, "%s: following %s, lag %ds, io %v, sql %v\n",
			st.Address, orNone(st.SourceHost), st.LagSeconds, st.IORunning, st.SQLRunning)
	}

	if err := manager.Promote(ctx, addr); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\n%s is now the primary.\n", addr)
	fmt.Fprintln(stdout, "A running gora picks this up on its next cluster check; nothing needs restarting.")
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "nobody"
	}
	return s
}
