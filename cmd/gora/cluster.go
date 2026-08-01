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

// The cluster commands run from a shell while the service is either up or
// down, and they must work either way. They do not talk to the running
// instance: they change the servers, write the result to the state file,
// and the running instance reads it on its next check. That is a control
// plane with one direction and no socket that could be asked to do
// anything else.

// cluster builds the topology and, when replication is managed, the manager
// for a one-off command.
func cluster(configPath string, stdout io.Writer) (config.Config, *topology.Topology, *replication.Manager, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return cfg, nil, nil, err
	}

	// Progress goes to stdout, where the operator is looking; the manager's
	// own log would otherwise be a second, quieter copy of it.
	log := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	topo, err := topology.New(cfg.Backend, cfg.Pool, cfg.Routing, cfg.Cluster.StateFile, log)
	if err != nil {
		return cfg, nil, nil, err
	}

	var manager *replication.Manager
	if cfg.Replication.Enabled {
		manager, err = replication.New(cfg.Replication, topo, log)
		if err != nil {
			topo.Close()
			return cfg, nil, nil, err
		}
	}
	return cfg, topo, manager, nil
}

// requireReplication is for the commands that configure MySQL itself.
func requireReplication(cfg config.Config, manager *replication.Manager, configPath string) error {
	if manager == nil {
		return fmt.Errorf("replication is disabled in %s: gora can route to the nodes you list, "+
			"but setting them up needs replication.enabled and an admin account", configPath)
	}
	return nil
}

// initCluster configures the servers into a replicating cluster.
func initCluster(configPath string, stdout io.Writer) error {
	cfg, topo, manager, err := cluster(configPath, stdout)
	if err != nil {
		return err
	}
	defer topo.Close()
	if err := requireReplication(cfg, manager, configPath); err != nil {
		return err
	}

	return manager.Provision(context.Background(), stdout)
}

// addReplica brings a node into the cluster while gora is running.
func addReplica(configPath, addr string, stdout io.Writer) error {
	cfg, topo, manager, err := cluster(configPath, stdout)
	if err != nil {
		return err
	}
	defer topo.Close()

	if err := config.CheckAddress(addr); err != nil {
		return err
	}
	if _, exists := topo.Node(addr); exists {
		return fmt.Errorf("%s is already part of the cluster", addr)
	}

	// With replication managed, the node is set up before it is announced:
	// a node in the state file is a node gora will send reads to, and a
	// replica that is not replicating yet would answer them with data from
	// whenever it was last written to.
	if manager != nil {
		if err := manager.AddReplica(context.Background(), addr, stdout); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(stdout, "replication is not managed by gora: %s is expected to be replicating already\n", addr)
	}

	if err := topo.State().AddNode(addr, "added from the command line"); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "\n%s is part of the cluster.\n", addr)
	reportPickup(cfg, stdout)
	return nil
}

// removeReplica takes a node out of the cluster while gora is running.
//
// It is deliberately not destructive: the node stops receiving traffic and
// keeps replicating. Taking a replica out for a backup or an upgrade is the
// common reason to do this, and tearing it down would be a surprising thing
// for a command called "remove" to do to a database.
func removeReplica(configPath, addr string, stdout io.Writer) error {
	cfg, topo, _, err := cluster(configPath, stdout)
	if err != nil {
		return err
	}
	defer topo.Close()

	if topo.Primary().Address == addr {
		return fmt.Errorf("%s is the primary: promote another node first with --promote, then remove this one", addr)
	}
	if _, exists := topo.Node(addr); !exists {
		return fmt.Errorf("%s is not part of the cluster", addr)
	}
	if err := topo.State().RemoveNode(addr, "removed from the command line"); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "%s will no longer receive traffic.\n", addr)
	fmt.Fprintln(stdout, "Its replication was left running; stop it yourself if the node is going away for good.")
	reportPickup(cfg, stdout)
	return nil
}

// reportPickup says how the change reaches a running instance, or that it
// will not.
func reportPickup(cfg config.Config, stdout io.Writer) {
	if cfg.Cluster.StateFile == "" {
		fmt.Fprintln(stdout, "cluster.state_file is empty, so this change was not recorded: "+
			"a running gora will not see it, and it will not survive a restart.")
		return
	}
	fmt.Fprintf(stdout, "A running gora picks this up within one health check (%s); nothing needs restarting.\n",
		cfg.Routing.HealthInterval)
}

// promote makes a node the primary by hand. It is what `failover: manual`
// leaves you to do, and the state file is how the running gora finds out.
func promote(configPath, addr string, stdout io.Writer) error {
	cfg, topo, manager, err := cluster(configPath, stdout)
	if err != nil {
		return err
	}
	defer topo.Close()
	if err := requireReplication(cfg, manager, configPath); err != nil {
		return err
	}

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
	reportPickup(cfg, stdout)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "nobody"
	}
	return s
}
