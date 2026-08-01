package topology

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/mysqltest"
)

// A cluster usually starts as one server. The second one arrives later, on
// a working site, and that must not be a restart.
func TestAddReplicaToASingleNode(t *testing.T) {
	topo, _, _ := build(t, 0, config.Routing{HealthInterval: config.Duration(time.Minute)})
	if topo.HasReplicas() {
		t.Fatal("a single-node topology has replicas")
	}

	added := mysqltest.Start(t, "gora", "secret")
	if err := topo.AddReplica(added.Addr); err != nil {
		t.Fatalf("AddReplica: %v", err)
	}

	if !topo.HasReplicas() {
		t.Fatal("the node was not added")
	}
	if got := topo.PickReader(); got.Address != added.Addr {
		t.Fatalf("PickReader = %s, want the node just added", got.Address)
	}
}

func TestAddReplicaRefusesDuplicatesAndNonsense(t *testing.T) {
	topo, primary, servers := build(t, 1, config.Routing{HealthInterval: config.Duration(time.Minute)})

	if err := topo.AddReplica(servers[0].Addr); err == nil {
		t.Error("a node already in the cluster was added again")
	}
	if err := topo.AddReplica(primary.Addr); err == nil {
		t.Error("the primary was added as a replica")
	}
	if err := topo.AddReplica("not-an-address"); err == nil {
		t.Error("a string that is not host:port was accepted")
	}
}

// Removing a node stops it receiving traffic.
func TestRemoveNode(t *testing.T) {
	topo, _, servers := build(t, 2, config.Routing{HealthInterval: config.Duration(time.Minute)})

	if err := topo.RemoveNode(servers[0].Addr); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	if _, ok := topo.Node(servers[0].Addr); ok {
		t.Fatal("the removed node is still in the cluster")
	}
	for i := 0; i < 10; i++ {
		if got := topo.PickReader(); got.Address == servers[0].Addr {
			t.Fatal("the removed node is still serving reads")
		}
	}
}

// The primary is not removable: gora would be left with nowhere to write.
func TestRemovePrimaryIsRefused(t *testing.T) {
	topo, primary, _ := build(t, 1, config.Routing{HealthInterval: config.Duration(time.Minute)})

	err := topo.RemoveNode(primary.Addr)
	if err == nil {
		t.Fatal("the primary was removed")
	}
	// The message has to say what to do instead, or it is a refusal without
	// a way forward.
	if !strings.Contains(err.Error(), "promote") {
		t.Fatalf("error %q does not say to promote another node first", err)
	}
	if _, ok := topo.Node(primary.Addr); !ok {
		t.Fatal("the primary is gone anyway")
	}
}

// Membership changes are recorded, and picked up by an instance that did
// not make them: that is how `gora --add-replica` reaches a running
// service without a control socket.
func TestMembershipIsAdoptedFromTheStateFile(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "cluster.json")
	primary := mysqltest.Start(t, "gora", "secret")
	backend := config.Backend{
		Address:        primary.Addr,
		Username:       "gora",
		Password:       "secret",
		ConnectTimeout: config.Duration(time.Second),
	}
	topo, err := New(backend, poolConfig(),
		config.Routing{HealthInterval: config.Duration(20 * time.Millisecond)},
		stateFile, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(topo.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go topo.Run(ctx)

	// Another process adds a node and writes it down.
	added := mysqltest.Start(t, "gora", "secret")
	other := NewState(stateFile)
	if err := other.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := other.AddNode(added.Addr, "added elsewhere"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		_, ok := topo.Node(added.Addr)
		return ok
	})

	// And removing it the same way takes it back out.
	if err := other.RemoveNode(added.Addr, "removed elsewhere"); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		_, ok := topo.Node(added.Addr)
		return !ok
	})
}

// The effective membership is the configuration plus what was added minus
// what was removed, so both ways of changing it keep working.
func TestStateMembers(t *testing.T) {
	s := NewState("")
	configured := []string{"a:3306", "b:3306"}

	if got := s.Members(configured); len(got) != 2 {
		t.Fatalf("members = %v, want the configured pair", got)
	}

	_ = s.AddNode("c:3306", "added")
	if got := s.Members(configured); len(got) != 3 || got[2] != "c:3306" {
		t.Fatalf("members = %v, want the added node as well", got)
	}

	_ = s.RemoveNode("a:3306", "removed")
	got := s.Members(configured)
	if len(got) != 2 || got[0] != "b:3306" || got[1] != "c:3306" {
		t.Fatalf("members = %v, want the configured node removed", got)
	}

	// Adding back something that was removed clears the removal, rather
	// than leaving a node that is in both lists and therefore invisible.
	_ = s.AddNode("a:3306", "added back")
	if got := s.Members(configured); len(got) != 3 {
		t.Fatalf("members = %v, want the node back", got)
	}
}

// A node added at runtime survives a restart of gora.
func TestMembershipSurvivesARestart(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "cluster.json")
	primary := mysqltest.Start(t, "gora", "secret")
	added := mysqltest.Start(t, "gora", "secret")
	backend := config.Backend{
		Address:        primary.Addr,
		Username:       "gora",
		Password:       "secret",
		ConnectTimeout: config.Duration(time.Second),
	}
	routing := config.Routing{HealthInterval: config.Duration(time.Minute)}

	first, err := New(backend, poolConfig(), routing, stateFile, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := first.State().AddNode(added.Addr, "added"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	first.Close()

	// A new process, the same configuration, the same state file.
	second, err := New(backend, poolConfig(), routing, stateFile, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(second.Close)

	if _, ok := second.Node(added.Addr); !ok {
		t.Fatal("the node added at runtime was forgotten across a restart")
	}
}
