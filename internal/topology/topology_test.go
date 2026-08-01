package topology

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/mysqltest"
)

func poolConfig() config.Pool {
	return config.Pool{
		MaxOpen:        4,
		MaxIdle:        4,
		PingInterval:   config.Duration(time.Second),
		AcquireTimeout: config.Duration(time.Second),
	}
}

func routingConfig() config.Routing {
	return config.Routing{
		MaxReplicaLag:  config.Duration(5 * time.Second),
		HealthInterval: config.Duration(20 * time.Millisecond),
	}
}

// build starts one fake server per address requested and returns the
// topology in front of them.
func build(t *testing.T, replicas int, routing config.Routing) (*Topology, *mysqltest.Server, []*mysqltest.Server) {
	t.Helper()

	primary := mysqltest.Start(t, "gora", "secret")
	backend := config.Backend{
		Address:        primary.Addr,
		Username:       "gora",
		Password:       "secret",
		ConnectTimeout: config.Duration(time.Second),
	}

	var servers []*mysqltest.Server
	for i := 0; i < replicas; i++ {
		r := mysqltest.Start(t, "gora", "secret")
		servers = append(servers, r)
		backend.Replicas = append(backend.Replicas, r.Addr)
	}

	topo, err := New(backend, poolConfig(), routing,
		filepath.Join(t.TempDir(), "cluster.json"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(topo.Close)
	return topo, primary, servers
}

// With no replicas every read goes where it always went.
func TestPickReaderWithoutReplicas(t *testing.T) {
	topo, _, _ := build(t, 0, routingConfig())

	if topo.HasReplicas() {
		t.Fatal("HasReplicas = true with no replicas configured")
	}
	if got := topo.PickReader(); got != topo.Primary() {
		t.Fatalf("PickReader = %s, want the primary", got.Address)
	}
}

// Reads are spread over the replicas rather than all landing on the first.
func TestPickReaderRotates(t *testing.T) {
	topo, _, servers := build(t, 3, config.Routing{HealthInterval: config.Duration(time.Second)})

	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		seen[topo.PickReader().Address]++
	}
	for _, s := range servers {
		if seen[s.Addr] == 0 {
			t.Fatalf("replica %s never served a read: %v", s.Addr, seen)
		}
	}
	if seen[topo.Primary().Address] != 0 {
		t.Fatalf("the primary served %d reads with healthy replicas available", seen[topo.Primary().Address])
	}
}

// A replica that stops answering stops receiving reads, and the primary
// takes them back rather than the reads failing.
func TestUnhealthyReplicaIsSkipped(t *testing.T) {
	topo, _, _ := build(t, 1, routingConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go topo.Run(ctx)

	replica := topo.Replicas()[0]
	waitFor(t, 2*time.Second, func() bool { return replica.Up() })

	// The lag is unknown here — the fake answers no replication status — and
	// with max_replica_lag set that alone takes it out of the rotation:
	// a replica whose lag gora cannot read may be a day behind.
	if got := topo.PickReader(); got != topo.Primary() {
		t.Fatalf("PickReader = %s, want the primary while the lag is unknown", got.Address)
	}
}

// Without a lag limit, a replica that answers is good enough.
func TestReplicaWithUnknownLagIsUsedWhenLagIsNotChecked(t *testing.T) {
	topo, _, servers := build(t, 1, config.Routing{HealthInterval: config.Duration(time.Second)})

	got := topo.PickReader()
	if got.Address != servers[0].Addr {
		t.Fatalf("PickReader = %s, want the replica", got.Address)
	}
}

// The health check reads the replication status and the read-only flag.
func TestHealthCheckReadsLagAndReadOnly(t *testing.T) {
	topo, _, servers := build(t, 1, routingConfig())
	servers[0].Answer("SHOW REPLICA STATUS",
		[]string{"Seconds_Behind_Source"}, [][]any{{int64(2)}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go topo.Run(ctx)

	waitFor(t, 2*time.Second, func() bool {
		lag, ok := topo.Replicas()[0].Lag()
		return ok && lag == 2*time.Second
	})

	// Two seconds behind, five allowed: it serves reads.
	if got := topo.PickReader(); got == topo.Primary() {
		t.Fatal("PickReader returned the primary while the replica was within the lag limit")
	}
	if !topo.WritesAccepted() {
		t.Fatal("WritesAccepted = false against a writable primary")
	}
}

// A replica further behind than the limit is taken out of the rotation.
func TestLaggingReplicaIsNotEligible(t *testing.T) {
	topo, _, servers := build(t, 1, routingConfig())
	servers[0].Answer("SHOW REPLICA STATUS",
		[]string{"Seconds_Behind_Source"}, [][]any{{int64(600)}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go topo.Run(ctx)

	waitFor(t, 2*time.Second, func() bool {
		lag, ok := topo.Replicas()[0].Lag()
		return ok && lag == 600*time.Second
	})
	if got := topo.PickReader(); got != topo.Primary() {
		t.Fatalf("PickReader = %s, want the primary", got.Address)
	}
}

// A primary that has become read-only is a failover that happened without
// telling gora, and it must stop taking writes.
func TestReadOnlyPrimaryRefusesWrites(t *testing.T) {
	topo, primary, _ := build(t, 0, routingConfig())
	primary.Answer("@@global.read_only", []string{"@@global.read_only"}, [][]any{{int64(1)}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go topo.Run(ctx)

	waitFor(t, 2*time.Second, func() bool { return !topo.WritesAccepted() })
}

// A node that cannot be reached is reported as down, with the reason.
func TestDeadNodeIsMarkedDown(t *testing.T) {
	backend := config.Backend{
		Address:        "127.0.0.1:1", // nothing listens there
		Username:       "gora",
		ConnectTimeout: config.Duration(200 * time.Millisecond),
	}
	topo, err := New(backend, poolConfig(), routingConfig(), "", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(topo.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go topo.Run(ctx)

	waitFor(t, 3*time.Second, func() bool { return !topo.Primary().Up() })
	if topo.WritesAccepted() {
		t.Fatal("WritesAccepted = true with the primary down")
	}

	stats := topo.Stat()
	if len(stats) != 1 {
		t.Fatalf("got %d nodes, want 1", len(stats))
	}
	if stats[0].LastError == "" {
		t.Fatal("the node is down and the status does not say why")
	}
}

// Nodes start assumed reachable: refusing traffic for the first health
// interval would make every restart a short outage.
func TestNodesStartUp(t *testing.T) {
	topo, _, _ := build(t, 1, routingConfig())

	if !topo.Primary().Up() {
		t.Fatal("the primary starts down")
	}
	if !topo.WritesAccepted() {
		t.Fatal("writes are refused before the first health check")
	}
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition still false after %s", limit)
}
