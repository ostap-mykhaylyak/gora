package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

func routingConfig(sticky time.Duration) config.Routing {
	return config.Routing{
		StickyAfterWrite: config.Duration(sticky),
		HealthInterval:   config.Duration(20 * time.Millisecond),
	}
}

// The point of the milestone: reads leave the primary alone.
func TestReadsGoToTheReplica(t *testing.T) {
	s := &setup{pool: poolConfig(), routing: routingConfig(time.Second), replicas: 1}
	primary, srv := startWith(t, s)
	replica := s.replicaServers[0]

	c := connect(t, srv)
	if _, err := c.Execute("SELECT ID FROM wp_posts"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if n := replica.Count("FROM wp_posts"); n != 1 {
		t.Fatalf("the replica ran the read %d times, want 1: %q", n, replica.Queries())
	}
	if n := primary.Count("FROM wp_posts"); n != 0 {
		t.Fatalf("the read reached the primary %d times, want 0", n)
	}
}

// And writes never leave it.
func TestWritesGoToThePrimary(t *testing.T) {
	s := &setup{pool: poolConfig(), routing: routingConfig(time.Second), replicas: 1}
	primary, srv := startWith(t, s)
	replica := s.replicaServers[0]

	c := connect(t, srv)
	if _, err := c.Execute("UPDATE wp_options SET option_value = 'x' WHERE option_name = 'y'"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if n := primary.Count("UPDATE wp_options"); n != 1 {
		t.Fatalf("the primary ran the write %d times, want 1", n)
	}
	if n := replica.Count("UPDATE wp_options"); n != 0 {
		t.Fatalf("a write reached a replica %d times, want 0", n)
	}
}

// Replication is asynchronous. A session that has just written must read
// its own writes back, or WooCommerce saves an order and then shows the
// customer a basket that still has everything in it.
func TestReadsStickToThePrimaryAfterAWrite(t *testing.T) {
	s := &setup{pool: poolConfig(), routing: routingConfig(2 * time.Second), replicas: 1}
	primary, srv := startWith(t, s)
	replica := s.replicaServers[0]

	c := connect(t, srv)
	if _, err := c.Execute("INSERT INTO wp_posts (post_title) VALUES ('new')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := c.Execute("SELECT ID FROM wp_posts WHERE post_title = 'new'"); err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	if n := primary.Count("SELECT ID FROM wp_posts"); n != 1 {
		t.Fatalf("the read after a write went to the primary %d times, want 1", n)
	}
	if n := replica.Count("SELECT ID FROM wp_posts"); n != 0 {
		t.Fatalf("the read after a write reached the replica %d times, want 0", n)
	}
}

// Once the window has passed, reads go back to the replica: the sticky
// window is a delay, not a switch.
func TestStickyWindowExpires(t *testing.T) {
	s := &setup{pool: poolConfig(), routing: routingConfig(50 * time.Millisecond), replicas: 1}
	_, srv := startWith(t, s)
	replica := s.replicaServers[0]

	c := connect(t, srv)
	if _, err := c.Execute("INSERT INTO wp_posts (post_title) VALUES ('new')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if _, err := c.Execute("SELECT ID FROM wp_posts"); err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	if n := replica.Count("SELECT ID FROM wp_posts"); n != 1 {
		t.Fatalf("the replica ran the read %d times after the window expired, want 1", n)
	}
}

// A session with the sticky window off is served by the replica straight
// away — which is why the default is not zero.
func TestWithoutStickyReadsGoStraightBack(t *testing.T) {
	s := &setup{pool: poolConfig(), routing: routingConfig(0), replicas: 1}
	_, srv := startWith(t, s)
	replica := s.replicaServers[0]

	c := connect(t, srv)
	if _, err := c.Execute("INSERT INTO wp_posts (post_title) VALUES ('new')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := c.Execute("SELECT ID FROM wp_posts"); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if n := replica.Count("SELECT ID FROM wp_posts"); n != 1 {
		t.Fatalf("the replica ran the read %d times, want 1", n)
	}
}

// Everything inside a transaction belongs to one server: a read there is
// reading uncommitted state that exists nowhere else.
func TestTransactionStaysOnThePrimary(t *testing.T) {
	s := &setup{pool: poolConfig(), routing: routingConfig(0), replicas: 1}
	primary, srv := startWith(t, s)
	replica := s.replicaServers[0]

	c := connect(t, srv)
	if _, err := c.Execute("BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := c.Execute("SELECT ID FROM wp_posts"); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if _, err := c.Execute("COMMIT"); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}

	if n := primary.Count("SELECT ID FROM wp_posts"); n != 1 {
		t.Fatalf("the read in a transaction went to the primary %d times, want 1", n)
	}
	if n := replica.Count("SELECT ID FROM wp_posts"); n != 0 {
		t.Fatalf("a read inside a transaction reached the replica %d times, want 0", n)
	}
}

// A replica too far behind is not a replica, it is a backup.
func TestLaggingReplicaIsNotUsed(t *testing.T) {
	s := &setup{
		pool: poolConfig(),
		routing: config.Routing{
			MaxReplicaLag:  config.Duration(5 * time.Second),
			HealthInterval: config.Duration(20 * time.Millisecond),
		},
		replicas: 1,
	}
	primary, srv := startWith(t, s)
	replica := s.replicaServers[0]
	replica.Answer("SHOW REPLICA STATUS",
		[]string{"Seconds_Behind_Source"}, [][]any{{int64(600)}})

	// Let the health check see the lag.
	waitFor(t, 2*time.Second, func() bool {
		for _, n := range srv.topo.Stat() {
			if n.Role == "replica" && n.LagSeconds == 600 {
				return true
			}
		}
		return false
	})

	c := connect(t, srv)
	if _, err := c.Execute("SELECT ID FROM wp_posts"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := primary.Count("SELECT ID FROM wp_posts"); n != 1 {
		t.Fatalf("the read went to the primary %d times, want 1: a lagging replica served it", n)
	}
}

// With the primary unreachable the site does not stop: reads keep coming
// from the replica, and writes are refused with the error MySQL itself uses
// for a read-only server rather than a connection timeout.
func TestDegradedModeServesReadsAndRefusesWrites(t *testing.T) {
	s := &setup{
		pool:        poolConfig(),
		routing:     routingConfig(0),
		replicas:    1,
		primaryDown: true,
	}
	_, srv := startWith(t, s)
	replica := s.replicaServers[0]

	waitFor(t, 3*time.Second, func() bool { return !srv.topo.Primary().Up() })

	c := connect(t, srv)
	if _, err := c.Execute("SELECT ID FROM wp_posts"); err != nil {
		t.Fatalf("a read failed while only the primary was down: %v", err)
	}
	if n := replica.Count("SELECT ID FROM wp_posts"); n != 1 {
		t.Fatalf("the replica served the read %d times, want 1", n)
	}

	_, err := c.Execute("UPDATE wp_options SET option_value = 'x' WHERE option_name = 'y'")
	if err == nil {
		t.Fatal("a write was accepted with the primary down")
	}
	if !strings.Contains(err.Error(), "not accepting writes") {
		t.Fatalf("error %q does not explain that writes are refused", err)
	}

	// And the session survives it: the next read still works.
	if _, err := c.Execute("SELECT ID FROM wp_posts"); err != nil {
		t.Fatalf("the session did not survive a refused write: %v", err)
	}
}

// Without replicas nothing changes: one node, everything on it.
func TestSingleNodeSendsEverythingToThePrimary(t *testing.T) {
	primary, srv := startWith(t, &setup{pool: poolConfig(), routing: routingConfig(time.Second)})

	c := connect(t, srv)
	if _, err := c.Execute("SELECT ID FROM wp_posts"); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if _, err := c.Execute("UPDATE wp_options SET option_value = 'x' WHERE option_name = 'y'"); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if n := primary.Count("wp_"); n != 2 {
		t.Fatalf("the primary saw %d statements, want 2", n)
	}
}
