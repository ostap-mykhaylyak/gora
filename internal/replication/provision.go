package replication

import (
	"context"
	"fmt"
	"io"
	"time"
)

// systemSchemas are the databases every MySQL has; anything else is data
// somebody put there.
const systemSchemas = "'mysql','information_schema','performance_schema','sys'"

// Provision turns the configured servers into a replicating cluster.
//
// It is the answer to "I started two empty MySQL containers": gora checks
// that each can replicate, gives them distinct identities, creates the
// replication account, seeds the replicas if there is anything to seed, and
// starts them following the primary. Nothing about it is written to a
// my.cnf by hand.
//
// It refuses rather than guesses. A replica with data of its own is not
// touched: gora will not decide on your behalf that a database is expendable.
func (m *Manager) Provision(ctx context.Context, out io.Writer) error {
	primaryAddr := m.topo.Primary().Address

	fmt.Fprintf(out, "primary %s\n", primaryAddr)
	primary, err := m.connect(ctx, primaryAddr)
	if err != nil {
		return err
	}
	defer primary.Close()

	fmt.Fprintf(out, "  version %s\n", primary.version)
	if err := m.prepareNode(primary, 1, false, out); err != nil {
		return err
	}
	if err := m.createReplicationUser(primary, out); err != nil {
		return err
	}

	primaryHasData, err := hasUserData(primary)
	if err != nil {
		return err
	}

	for i, node := range m.topo.Replicas() {
		fmt.Fprintf(out, "replica %s\n", node.Address)
		if err := m.provisionReplica(ctx, node.Address, primaryAddr, uint32(i+2), primaryHasData, out); err != nil {
			return fmt.Errorf("replica %s: %w", node.Address, err)
		}
	}

	if err := m.state.Save(primaryAddr, "provisioned"); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nthe cluster is set up. Check it with: gora status")
	return nil
}

// provisionReplica configures one replica to follow the primary.
func (m *Manager) provisionReplica(ctx context.Context, addr, primaryAddr string, serverID uint32, primaryHasData bool, out io.Writer) error {
	replica, err := m.connect(ctx, addr)
	if err != nil {
		return err
	}
	defer replica.Close()

	fmt.Fprintf(out, "  version %s\n", replica.version)
	if err := m.prepareNode(replica, serverID, true, out); err != nil {
		return err
	}

	replicaHasData, err := hasUserData(replica)
	if err != nil {
		return err
	}
	if replicaHasData {
		// It may already be a replica of ours, in which case there is
		// nothing to do and nothing to destroy.
		if status, err := replica.Execute(replica.dialect.showReplicaStatus()); err == nil && status != nil && len(status.Values) > 0 {
			if host, ok := rowByName(status, replica.dialect.statusField("Source_Host", "Master_Host")); ok && sameHost(host, primaryAddr) {
				fmt.Fprintln(out, "  already replicating from the primary, left alone")
				return nil
			}
		}
		return fmt.Errorf("it has databases of its own and is not replicating from %s; "+
			"gora will not overwrite them — empty it, or seed it from the primary yourself, then run --init-cluster again", primaryAddr)
	}

	if primaryHasData {
		fmt.Fprintln(out, "  the primary has data: cloning it")
		if err := m.clone(ctx, replica, primaryAddr, out); err != nil {
			return err
		}
		// CLONE restarts the server, so the connection is gone.
		replica.Close()
		if replica, err = m.waitForNode(ctx, addr, out); err != nil {
			return err
		}
		defer replica.Close()
	}

	if err := m.startReplication(replica, primaryAddr, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "  replicating")
	return nil
}

// prepareNode makes a server capable of replication: binary logging, GTIDs,
// an identity of its own, and — for a replica — a refusal to take writes.
func (m *Manager) prepareNode(a *admin, serverID uint32, replica bool, out io.Writer) error {
	logBin, err := scalarUint(a.Conn, "SELECT @@log_bin")
	if err != nil {
		return fmt.Errorf("%s: reading log_bin: %w", a.addr, err)
	}
	if logBin != 1 {
		// The one thing that cannot be turned on at runtime.
		return fmt.Errorf("%s has binary logging off, and it cannot be enabled without a restart: "+
			"start mysqld with --log-bin (MySQL 8 has it on by default)", a.addr)
	}

	if err := m.enableGTID(a, out); err != nil {
		return err
	}

	// Every server in a replication topology needs an identity of its own,
	// and MySQL 8 gives them all the same default.
	if err := setGlobal(a, "server_id", fmt.Sprint(serverID)); err != nil {
		return err
	}
	fmt.Fprintf(out, "  server_id %d\n", serverID)

	if replica {
		// super_read_only rather than read_only: the second still lets
		// anybody with SUPER write, which on a replica means anybody with
		// SUPER can break replication by accident.
		if err := setGlobal(a, "super_read_only", "ON"); err != nil {
			return err
		}
		fmt.Fprintln(out, "  read-only")
	} else if err := setGlobal(a, "super_read_only", "OFF"); err != nil {
		return err
	}
	return nil
}

// enableGTID turns on GTID mode, online, in the order MySQL requires.
func (m *Manager) enableGTID(a *admin, out io.Writer) error {
	mode, err := scalarString(a.Conn, "SELECT @@gtid_mode")
	if err != nil {
		return fmt.Errorf("%s: reading gtid_mode: %w", a.addr, err)
	}
	if mode == "ON" {
		fmt.Fprintln(out, "  GTID already on")
		return nil
	}

	// The sequence is not optional: MySQL refuses to jump straight to ON,
	// because every server in the topology has to be able to understand
	// both kinds of transaction while the change goes round.
	steps := []struct{ name, value string }{
		{"enforce_gtid_consistency", "ON"},
		{"gtid_mode", "OFF_PERMISSIVE"},
		{"gtid_mode", "ON_PERMISSIVE"},
		{"gtid_mode", "ON"},
	}
	for _, step := range steps {
		if err := setGlobal(a, step.name, step.value); err != nil {
			return fmt.Errorf("enabling GTID on %s: %w", a.addr, err)
		}
	}
	fmt.Fprintln(out, "  GTID enabled")
	return nil
}

// setGlobal sets a global variable, persistently when the server supports
// it: a server_id that goes back to its default on the next restart is a
// cluster that breaks the next time somebody reboots a container.
func setGlobal(a *admin, name, value string) error {
	if atLeast(a.version, 8, 0, 0) {
		if err := a.exec("SET PERSIST %s = %s", name, value); err == nil {
			return nil
		}
		// Persisting can be refused (read-only file system, a variable that
		// does not allow it); the setting still matters for this run.
	}
	return a.exec("SET GLOBAL %s = %s", name, value)
}

// createReplicationUser creates the account the replicas connect with.
func (m *Manager) createReplicationUser(a *admin, out io.Writer) error {
	user := m.cfg.User
	if err := a.exec("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", user, escape(m.cfg.Password)); err != nil {
		return err
	}
	// The password is set explicitly as well, so re-running --init-cluster
	// after changing it in the configuration does what it looks like it does.
	if err := a.exec("ALTER USER '%s'@'%%' IDENTIFIED BY '%s'", user, escape(m.cfg.Password)); err != nil {
		return err
	}
	if err := a.exec("GRANT REPLICATION SLAVE ON *.* TO '%s'@'%%'", user); err != nil {
		return err
	}
	// BACKUP_ADMIN is what the clone plugin needs on the donor side. It is
	// granted whether or not cloning turns out to be necessary, so that
	// adding a replica later does not need another privileged visit.
	if atLeast(a.version, 8, 0, 17) {
		a.try("GRANT BACKUP_ADMIN ON *.* TO '%s'@'%%'", user)
	}
	fmt.Fprintf(out, "  replication user %s\n", user)
	return nil
}

// startReplication points a replica at the primary and starts it.
func (m *Manager) startReplication(a *admin, primaryAddr string, out io.Writer) error {
	host, port := splitHostPort(primaryAddr)
	d := a.dialect

	a.try("%s", d.stopReplica())

	// GET_SOURCE_PUBLIC_KEY lets the replica authenticate over a plain
	// connection with MySQL 8's default password plugin, which otherwise
	// refuses to send the password without TLS or a copy of the key.
	stmt := fmt.Sprintf("%s %s = '%s', %s = %s, %s = '%s', %s = '%s', %s = 1",
		d.changeSource(),
		d.opt("HOST"), host,
		d.opt("PORT"), port,
		d.opt("USER"), m.cfg.User,
		d.opt("PASSWORD"), escape(m.cfg.Password),
		d.opt("AUTO_POSITION"))
	if atLeast(a.version, 8, 0, 0) {
		stmt += ", " + d.opt("SSL") + " = 0, GET_" + d.opt("PUBLIC_KEY") + " = 1"
	}
	if err := a.exec("%s", stmt); err != nil {
		return err
	}
	return a.exec("%s", d.startReplica())
}

// clone copies the primary into an empty replica using MySQL's own clone
// plugin, which is the only way to seed a server without a shell, a dump
// file and somewhere to put it.
func (m *Manager) clone(ctx context.Context, a *admin, primaryAddr string, out io.Writer) error {
	if !atLeast(a.version, 8, 0, 17) {
		return fmt.Errorf("the primary has data and this server (%s) is older than 8.0.17, which has no clone plugin: "+
			"seed it from a dump of the primary and run --init-cluster again", a.version)
	}

	// The plugin may already be there; installing it twice is an error, not
	// a problem.
	a.try("INSTALL PLUGIN clone SONAME 'mysql_clone.so'")

	host, port := splitHostPort(primaryAddr)
	if err := a.exec("SET GLOBAL clone_valid_donor_list = '%s:%s'", host, port); err != nil {
		return err
	}
	fmt.Fprintf(out, "  cloning from %s (the server will restart)\n", primaryAddr)

	// CLONE ends by restarting the server, so losing the connection here is
	// the expected outcome, not a failure.
	if err := a.exec("CLONE INSTANCE FROM '%s'@'%s':%s IDENTIFIED BY '%s'",
		m.cfg.User, host, port, escape(m.cfg.Password)); err != nil {
		if !isDisconnect(err) {
			return err
		}
	}
	return nil
}

// waitForNode waits for a server to answer again after a clone restarted it.
func (m *Manager) waitForNode(ctx context.Context, addr string, out io.Writer) (*admin, error) {
	fmt.Fprintln(out, "  waiting for it to come back")

	deadline := time.Now().Add(5 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		a, err := m.connect(ctx, addr)
		if err == nil {
			return a, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("%s did not come back after the clone: %w", addr, lastErr)
}

// hasUserData reports whether a server holds anything but the system
// databases.
func hasUserData(a *admin) (bool, error) {
	n, err := scalarUint(a.Conn,
		"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME NOT IN ("+systemSchemas+")")
	if err != nil {
		return false, fmt.Errorf("%s: listing databases: %w", a.addr, err)
	}
	return n > 0, nil
}

// escape makes a password safe to put in a statement. gora builds these
// statements itself, from its own configuration, but a password with a
// quote in it must not be able to turn into SQL.
func escape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'', '"', '\\':
			out = append(out, '\\', s[i])
		case 0:
			out = append(out, '\\', '0')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// isDisconnect reports whether an error is the connection going away, which
// for CLONE is success.
func isDisconnect(err error) bool {
	msg := err.Error()
	for _, sub := range []string{"connection", "EOF", "closed", "reset", "broken pipe"} {
		if indexFold(msg, sub) >= 0 || indexFold(msg, upper(sub)) >= 0 {
			return true
		}
	}
	return false
}

func upper(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'a' && out[i] <= 'z' {
			out[i] -= 32
		}
	}
	return string(out)
}
