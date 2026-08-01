package replication

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/mysqltest"
	"github.com/ostap-mykhaylyak/gora/internal/topology"
)

const adminUser = "root"
const adminPass = "admin-secret"

// cluster starts a fake primary and the requested number of fake replicas,
// and returns the manager in front of them.
func cluster(t *testing.T, replicas int, cfg config.Replication) (*Manager, *mysqltest.Server, []*mysqltest.Server) {
	t.Helper()

	primary := mysqltest.Start(t, adminUser, adminPass)
	backend := config.Backend{
		Address:        primary.Addr,
		Username:       adminUser,
		Password:       adminPass,
		ConnectTimeout: config.Duration(time.Second),
	}
	var servers []*mysqltest.Server
	for i := 0; i < replicas; i++ {
		r := mysqltest.Start(t, adminUser, adminPass)
		servers = append(servers, r)
		backend.Replicas = append(backend.Replicas, r.Addr)
	}

	// A fake server that looks like a healthy MySQL 8.
	for _, s := range append([]*mysqltest.Server{primary}, servers...) {
		s.Answer("SELECT VERSION()", []string{"VERSION()"}, [][]any{{"8.0.36"}})
		s.Answer("@@log_bin", []string{"@@log_bin"}, [][]any{{int64(1)}})
		s.Answer("@@gtid_mode", []string{"@@gtid_mode"}, [][]any{{"ON"}})
		s.Answer("COUNT(*) FROM information_schema.SCHEMATA",
			[]string{"c"}, [][]any{{int64(0)}})
	}

	routing := config.Routing{HealthInterval: config.Duration(time.Minute)}
	topo, err := topology.New(backend, config.Pool{
		MaxOpen:        2,
		MaxIdle:        2,
		PingInterval:   config.Duration(time.Second),
		AcquireTimeout: config.Duration(time.Second),
	}, routing, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("topology.New: %v", err)
	}
	t.Cleanup(topo.Close)

	if cfg.AdminUsername == "" {
		cfg.AdminUsername = adminUser
		cfg.AdminPassword = adminPass
	}
	if cfg.User == "" {
		cfg.User = "gora_repl"
		cfg.Password = "repl-secret"
	}
	if cfg.StateFile == "" {
		cfg.StateFile = filepath.Join(t.TempDir(), "cluster.json")
	}

	m, err := New(cfg, topo, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, primary, servers
}

// Provisioning a pair of empty servers: the replication account is created
// on the primary and the replica is pointed at it.
func TestProvision(t *testing.T) {
	m, primary, replicas := cluster(t, 1, config.Replication{Enabled: true})

	if err := m.Provision(context.Background(), io.Discard); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if n := primary.Count("CREATE USER IF NOT EXISTS 'gora_repl'"); n != 1 {
		t.Fatalf("the replication user was created %d times: %q", n, primary.Queries())
	}
	if n := primary.Count("GRANT REPLICATION SLAVE"); n != 1 {
		t.Fatal("the replication user was not granted REPLICATION SLAVE")
	}
	if n := primary.Count("SET PERSIST server_id = 1"); n != 1 {
		t.Fatalf("the primary was not given server_id 1: %q", primary.Queries())
	}
	// The primary must not be left read-only, whatever it was before.
	if n := primary.Count("super_read_only = OFF"); n != 1 {
		t.Fatal("the primary was not made writable")
	}

	replica := replicas[0]
	if n := replica.Count("SET PERSIST server_id = 2"); n != 1 {
		t.Fatalf("the replica was not given a distinct server_id: %q", replica.Queries())
	}
	if n := replica.Count("super_read_only = ON"); n != 1 {
		t.Fatal("the replica was not made read-only")
	}
	if n := replica.Count("CHANGE REPLICATION SOURCE TO"); n != 1 {
		t.Fatalf("the replica was not pointed at the primary: %q", replica.Queries())
	}
	if n := replica.Count("SOURCE_AUTO_POSITION = 1"); n != 1 {
		t.Fatal("replication was not set up with GTID auto-positioning")
	}
	if n := replica.Count("START REPLICA"); n != 1 {
		t.Fatal("replication was not started")
	}
}

// A server old enough to use the previous vocabulary gets the previous
// vocabulary.
func TestProvisionSpeaksTheOldDialect(t *testing.T) {
	m, _, replicas := cluster(t, 1, config.Replication{Enabled: true})
	replica := replicas[0]
	replica.Answer("SELECT VERSION()", []string{"VERSION()"}, [][]any{{"5.7.44-log"}})

	if err := m.Provision(context.Background(), io.Discard); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if n := replica.Count("CHANGE MASTER TO"); n != 1 {
		t.Fatalf("a 5.7 replica was sent 8.0 syntax: %q", replica.Queries())
	}
	if n := replica.Count("START SLAVE"); n != 1 {
		t.Fatal("a 5.7 replica was sent START REPLICA")
	}
	if n := replica.Count("SET PERSIST"); n != 0 {
		t.Fatal("SET PERSIST was used on 5.7, which does not have it")
	}
}

// GTID is turned on in the order MySQL requires, and only when it is off.
func TestProvisionEnablesGTID(t *testing.T) {
	m, primary, _ := cluster(t, 1, config.Replication{Enabled: true})
	primary.Answer("@@gtid_mode", []string{"@@gtid_mode"}, [][]any{{"OFF"}})

	if err := m.Provision(context.Background(), io.Discard); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	queries := strings.Join(primary.Queries(), "\n")
	steps := []string{
		"enforce_gtid_consistency = ON",
		"gtid_mode = OFF_PERMISSIVE",
		"gtid_mode = ON_PERMISSIVE",
		"gtid_mode = ON",
	}
	last := -1
	for _, step := range steps {
		i := strings.Index(queries, step)
		if i < 0 {
			t.Fatalf("GTID was enabled without %q: %s", step, queries)
		}
		if i < last {
			t.Fatalf("the GTID steps were sent out of order, %q came too early", step)
		}
		last = i
	}
}

// Binary logging cannot be turned on at runtime, so gora says so instead of
// setting up a cluster that cannot replicate.
func TestProvisionRefusesWithoutBinaryLogging(t *testing.T) {
	m, primary, _ := cluster(t, 1, config.Replication{Enabled: true})
	primary.Answer("@@log_bin", []string{"@@log_bin"}, [][]any{{int64(0)}})

	err := m.Provision(context.Background(), io.Discard)
	if err == nil {
		t.Fatal("a primary without binary logging was accepted")
	}
	if !strings.Contains(err.Error(), "--log-bin") {
		t.Fatalf("error %q does not say what to do about it", err)
	}
}

// A replica with databases of its own is not touched.
func TestProvisionRefusesToOverwriteAReplicaWithData(t *testing.T) {
	m, _, replicas := cluster(t, 1, config.Replication{Enabled: true})
	replicas[0].Answer("COUNT(*) FROM information_schema.SCHEMATA",
		[]string{"c"}, [][]any{{int64(3)}})

	err := m.Provision(context.Background(), io.Discard)
	if err == nil {
		t.Fatal("a replica with data was provisioned over")
	}
	if !strings.Contains(err.Error(), "will not overwrite") {
		t.Fatalf("error %q does not explain the refusal", err)
	}
	if n := replicas[0].Count("CHANGE REPLICATION SOURCE"); n != 0 {
		t.Fatal("the replica was reconfigured anyway")
	}
}

// Promotion: the new primary stops replicating, forgets where it was
// replicating from, and becomes writable.
func TestPromote(t *testing.T) {
	m, _, replicas := cluster(t, 2, config.Replication{Enabled: true})
	target := replicas[0]

	if err := m.Promote(context.Background(), target.Addr); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	if n := target.Count("STOP REPLICA"); n != 1 {
		t.Fatalf("the new primary was not stopped: %q", target.Queries())
	}
	if n := target.Count("RESET REPLICA ALL"); n != 1 {
		t.Fatal("the new primary kept its replication settings, and would follow the old one again after a restart")
	}
	if n := target.Count("super_read_only = OFF"); n != 1 {
		t.Fatal("the new primary was not made writable")
	}
	if m.topo.Primary().Address != target.Addr {
		t.Fatalf("the topology still points at %s", m.topo.Primary().Address)
	}

	// The other replica now follows the new primary.
	if n := replicas[1].Count("CHANGE REPLICATION SOURCE TO"); n != 1 {
		t.Fatalf("the other replica was not repointed: %q", replicas[1].Queries())
	}
}

// A promotion is written down, or a restart of gora would go back to
// writing to a server that is now a replica.
func TestPromoteIsRecorded(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "cluster.json")
	m, _, replicas := cluster(t, 1, config.Replication{Enabled: true, StateFile: stateFile})

	if err := m.Promote(context.Background(), replicas[0].Addr); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	state := NewState(stateFile)
	if err := state.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Primary != replicas[0].Addr {
		t.Fatalf("the state file records %q as the primary, want %q", state.Primary, replicas[0].Addr)
	}
	if state.PromotedAt.IsZero() {
		t.Fatal("the state file does not record when the promotion happened")
	}
}

// And it is picked up by an instance that did not perform it, which is how
// `gora --promote` reaches a running service.
func TestPromotionIsAdoptedFromTheStateFile(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "cluster.json")
	m, _, replicas := cluster(t, 1, config.Replication{Enabled: true, StateFile: stateFile})

	// Another process promoted the replica and wrote it down.
	other := NewState(stateFile)
	if err := other.Save(replicas[0].Addr, "promoted by hand"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m.Reconcile(context.Background())
	if got := m.topo.Primary().Address; got != replicas[0].Addr {
		t.Fatalf("the running instance still thinks %s is the primary", got)
	}
}

// A replica following somebody else is repointed at the current primary.
func TestReconcileRepointsAStrayReplica(t *testing.T) {
	m, _, replicas := cluster(t, 1, config.Replication{Enabled: true})
	replica := replicas[0]
	replica.Answer("SHOW REPLICA STATUS", []string{
		"Source_Host", "Replica_IO_Running", "Replica_SQL_Running", "Seconds_Behind_Source",
	}, [][]any{{"10.9.9.9", "Yes", "Yes", int64(0)}})

	m.Reconcile(context.Background())

	if n := replica.Count("CHANGE REPLICATION SOURCE TO"); n != 1 {
		t.Fatalf("a replica following the wrong server was not repointed: %q", replica.Queries())
	}
}

// Manual failover is the default, and it changes nothing on its own.
func TestManualFailoverDoesNotPromote(t *testing.T) {
	m, _, _ := cluster(t, 1, config.Replication{
		Enabled:       true,
		Failover:      config.FailoverManual,
		FailoverDelay: config.Duration(time.Millisecond),
	})

	// Pretend the primary has been unreachable for a while.
	m.downSince = time.Now().Add(-time.Hour)
	primary := m.topo.Primary()
	primary.Pool().Close()

	before := m.topo.Primary().Address
	m.watchPrimary(context.Background(), primary)
	if m.topo.Primary().Address != before {
		t.Fatal("manual failover promoted a replica on its own")
	}
}

func TestDialectVersions(t *testing.T) {
	tests := []struct {
		version string
		modern  bool
	}{
		{"8.0.36", true},
		{"8.0.22", true},
		{"8.0.21", false},
		{"5.7.44-log", false},
		{"8.4.0", true},
		{"9.1.0", true},
		{"", false},
	}
	for _, tt := range tests {
		if got := dialectFor(tt.version).modern; got != tt.modern {
			t.Errorf("dialectFor(%q).modern = %v, want %v", tt.version, got, tt.modern)
		}
	}
}

// Setting up replication means sending a password; it must not come back
// out in a log line or an error.
func TestPasswordsAreRedacted(t *testing.T) {
	stmts := []string{
		"CREATE USER 'gora_repl'@'%' IDENTIFIED BY 'hunter2'",
		"CHANGE REPLICATION SOURCE TO SOURCE_HOST = 'a', SOURCE_PASSWORD = 'hunter2', SOURCE_AUTO_POSITION = 1",
		"CHANGE MASTER TO MASTER_PASSWORD = 'hunter2'",
	}
	for _, stmt := range stmts {
		if got := redact(stmt); strings.Contains(got, "hunter2") {
			t.Errorf("redact(%q) = %q, the password is still there", stmt, got)
		}
	}
}

func TestEscape(t *testing.T) {
	if got := escape(`it's`); got != `it\'s` {
		t.Errorf("escape = %q", got)
	}
	if got := escape(`a\b`); got != `a\\b` {
		t.Errorf("escape = %q", got)
	}
}
