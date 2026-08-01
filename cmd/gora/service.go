package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/cache"
	"github.com/ostap-mykhaylyak/gora/internal/confd"
	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/firewall"
	"github.com/ostap-mykhaylyak/gora/internal/profile"
	"github.com/ostap-mykhaylyak/gora/internal/proxy"
	"github.com/ostap-mykhaylyak/gora/internal/replication"
	"github.com/ostap-mykhaylyak/gora/internal/rewrite"
	"github.com/ostap-mykhaylyak/gora/internal/status"
	"github.com/ostap-mykhaylyak/gora/internal/throttle"
	"github.com/ostap-mykhaylyak/gora/internal/topology"
)

// stopTimeout bounds how long `gora stop` waits for the process to go away
// before reporting that it is still there. It is deliberately not
// configurable: a stop that hangs is a bug to investigate, not a knob.
const stopTimeout = 15 * time.Second

// start runs gora in the foreground until SIGTERM or SIGINT. systemd calls
// exactly this; running it from a shell behaves the same way, which is what
// makes a container image or a quick manual test possible without a unit.
func start(configPath string, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logWriter, closeLog, err := openLog(cfg.Log)
	if err != nil {
		return err
	}
	defer closeLog()
	log := newLogger(logWriter, cfg.Log)

	if pid, ok := runningPID(); ok {
		return fmt.Errorf("gora is already running (pid %d)", pid)
	}
	if err := writePIDFile(); err != nil {
		// Not fatal: gora still proxies. Only `gora stop/reload/restart`
		// need the pid file, and systemd tracks the process itself.
		log.Warn("could not write the pid file, stop and reload will not find this instance",
			"path", pidFilePath, "error", err)
	} else {
		defer removePIDFile()
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	if cfg.Log.Console() {
		fmt.Fprintf(stdout, "gora %s started (pid %d)\n", version, os.Getpid())
	}
	return serve(ctx, cfg, configPath, log)
}

// serve runs everything gora is made of until ctx is cancelled. It is
// separate from start so that the tests can drive the whole assembly —
// pool, listener, status socket — with a context of their own.
func serve(ctx context.Context, cfg config.Config, configPath string, log *slog.Logger) error {
	started := time.Now()
	log.Info("gora started",
		"version", version, "pid", os.Getpid(), "config", configPath,
		"listen", cfg.Listen.Address, "backend", cfg.Backend.Address)

	topo, err := topology.New(cfg.Backend, cfg.Pool, cfg.Routing, log)
	if err != nil {
		return err
	}
	defer topo.Close()
	go topo.Run(ctx)
	if topo.HasReplicas() {
		log.Info("read/write split enabled",
			"primary", cfg.Backend.Address, "replicas", len(cfg.Backend.Replicas),
			"sticky_after_write", cfg.Routing.StickyAfterWrite,
			"max_replica_lag", cfg.Routing.MaxReplicaLag)
	}

	// Everything gora does on its own behalf goes to the primary. The cache
	// warm-up in particular must: it refetches right after a write, and a
	// replica that has not received it yet would put the state from before
	// the write into the cache.
	backendPool := topo.Primary().Pool()

	rulesDir := rulesDirFor(cfg, configPath)
	rules, err := confd.Load(rulesDir)
	if err != nil {
		return err
	}

	var queryCache *cache.Cache
	if cfg.Cache.Enabled {
		queryCache, err = cache.New(cfg.Cache, backendPool, rules.Cache, log)
		if err != nil {
			return err
		}
		log.Info("query cache enabled",
			"table_prefix", cfg.Cache.TablePrefix, "rules", len(rules.Cache), "rules_dir", rulesDir)
		if cfg.Cache.Warmup {
			warmer := cache.NewWarmer(queryCache, backendPool, log)
			go warmer.Run(ctx)
			queryCache.SetRefetch(warmer.Trigger)
		}
	}

	// The traffic rules always exist, possibly with nothing in them, so a
	// reload can bring rules in without a restart.
	traffic, err := newTraffic(rules, cfg.Cache.TablePrefix, log)
	if err != nil {
		return err
	}
	traffic.log(log, rulesDir)

	var replicator *replication.Manager
	if cfg.Replication.Enabled {
		replicator, err = replication.New(cfg.Replication, topo, log)
		if err != nil {
			return err
		}
		go replicator.Run(ctx)
		log.Info("replication management enabled",
			"failover", cfg.Replication.Failover,
			"failover_delay", cfg.Replication.FailoverDelay,
			"state_file", cfg.Replication.StateFile)
	}

	var profiler *profile.Profiler
	if cfg.Profiling.Enabled {
		profiler = profile.New(cfg.Profiling, backendPool, log)
		go profiler.Run(ctx)
		log.Info("profiling enabled",
			"slow_query", cfg.Profiling.SlowQuery,
			"report_interval", cfg.Profiling.ReportInterval,
			"advice_file", cfg.Profiling.AdviceFile)
	}

	listenTLS, err := clientTLS(cfg.Listen)
	if err != nil {
		return err
	}
	if listenTLS != nil {
		log.Info("client TLS enabled", "cert", cfg.Listen.TLS.Cert)
	}

	srv := proxy.New(proxy.Options{
		Listen:   cfg.Listen,
		Users:    cfg.Users,
		PoolCfg:  cfg.Pool,
		Routing:  cfg.Routing,
		Topology: topo,
		Cache:    queryCache,
		Rewriter: traffic.rewriter,
		Firewall: traffic.firewall,
		Throttle: traffic.throttle,
		Profiler: profiler,
		TLS:      listenTLS,
		Log:      log,
	})

	if cfg.Status.Socket != "" {
		collect := func() status.Snapshot {
			snap := status.Snapshot{
				Version:       version,
				PID:           os.Getpid(),
				UptimeSeconds: int64(time.Since(started).Seconds()),
				ConfigPath:    configPath,
				Listen:        cfg.Listen.Address,
				Backend:       cfg.Backend.Address,
				Clients:       srv.Stat(),
				Pool:          backendPool.Stat(),
				Nodes:         topo.Stat(),
				Firewall:      traffic.firewall.Stat(),
				Throttle:      traffic.throttle.Stat(),
				Rewrites:      traffic.rewriter.Len(),
			}
			if queryCache != nil {
				rep := queryCache.ReportStats()
				snap.Cache = &rep
			}
			if replicator != nil {
				snap.Replication = replicator.Status()
			}
			return snap
		}
		go func() {
			if err := status.Serve(ctx, cfg.Status.Socket, collect, log); err != nil {
				log.Warn("status socket unavailable", "socket", cfg.Status.Socket, "error", err)
			}
		}()
	}

	go watchReloads(ctx, configPath, rulesDir, queryCache, traffic, log)

	if err := srv.Run(ctx); err != nil {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

// clientTLS loads the certificate gora presents to clients, if configured.
func clientTLS(cfg config.Listen) (*tls.Config, error) {
	if !cfg.TLS.Enabled() {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
	if err != nil {
		return nil, fmt.Errorf("loading listen.tls certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// rulesDirFor returns the conf.d directory: the configured one, or the
// conf.d sitting next to config.yaml.
func rulesDirFor(cfg config.Config, configPath string) string {
	if cfg.Cache.RulesDir != "" {
		return cfg.Cache.RulesDir
	}
	return filepath.Join(filepath.Dir(configPath), "conf.d")
}

// watchReloads applies the conf.d drop-ins again on every SIGHUP, without
// dropping a single client connection. Anything that no longer parses is
// reported and ignored: a reload must never be able to take down an
// instance that is serving traffic.
//
// The main configuration file is re-read only to validate it — listeners,
// pool and credentials are not swapped under running sessions. The rules
// are the part meant to change while gora runs, which is what makes adding
// one during an incident a `systemctl reload` away.
func watchReloads(ctx context.Context, configPath, rulesDir string, queryCache *cache.Cache, tr *traffic, log *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		}

		cfg, err := config.Load(configPath)
		if err != nil {
			log.Error("reload failed, the running configuration is unchanged", "error", err)
			continue
		}
		rules, err := confd.Load(rulesDir)
		if err != nil {
			log.Error("reload failed, keeping the previous rules", "error", err)
			continue
		}
		// The traffic rules go first: they compile without touching the
		// cache, so a bad expression is caught before anything is flushed.
		if err := tr.setRules(rules, cfg.Cache.TablePrefix); err != nil {
			log.Error("reload failed, keeping the previous rules", "error", err)
			continue
		}
		if queryCache != nil {
			if err := queryCache.SetRules(rules.Cache); err != nil {
				log.Error("reload failed, keeping the previous rules", "error", err)
				continue
			}
		}
		log.Info("configuration reloaded", "config", configPath, "rules_dir", rulesDir,
			"cache_rules", len(rules.Cache), "rewrites", len(rules.Rewrites),
			"blocks", len(rules.Blocks), "throttles", len(rules.Throttles))
	}
}

// stop asks the running instance to shut down and waits for it to be gone.
func stop(stdout io.Writer) error {
	pid, ok := runningPID()
	if !ok {
		return fmt.Errorf("gora is not running (no live process in %s)", pidFilePath)
	}
	if err := terminate(pid); err != nil {
		return fmt.Errorf("stopping gora (pid %d): %w", pid, err)
	}

	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			fmt.Fprintf(stdout, "gora stopped (pid %d)\n", pid)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("gora (pid %d) is still running %s after SIGTERM", pid, stopTimeout)
}

// restart stops a running instance, if any, and then runs in the foreground.
// It does not daemonize: under systemd the right command is `systemctl
// restart gora`, and everywhere else the foreground process is the service.
func restart(configPath string, stdout io.Writer) error {
	if _, ok := runningPID(); ok {
		if err := stop(stdout); err != nil {
			return err
		}
	}
	return start(configPath, stdout)
}

// reload makes the running instance re-read its configuration.
func reload(stdout io.Writer) error {
	pid, ok := runningPID()
	if !ok {
		return fmt.Errorf("gora is not running (no live process in %s)", pidFilePath)
	}
	if err := hangup(pid); err != nil {
		return fmt.Errorf("reloading gora (pid %d): %w", pid, err)
	}
	fmt.Fprintf(stdout, "reload requested (pid %d); check the log for the outcome\n", pid)
	return nil
}

// printStatus queries the running instance through its status socket.
func printStatus(configPath string, asJSON bool, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.Status.Socket == "" {
		return fmt.Errorf("status.socket is disabled in %s", configPath)
	}

	snap, err := status.Query(cfg.Status.Socket)
	if err != nil {
		return fmt.Errorf("is gora running? %w", err)
	}

	// The same snapshot the daemon holds, for whatever collects it. It is
	// the readable report that is derived, not the other way round.
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(snap)
	}

	fmt.Fprintf(stdout, "gora %s, pid %d, up %s\n", snap.Version, snap.PID, snap.Uptime())
	fmt.Fprintf(stdout, "config:   %s\n", snap.ConfigPath)
	fmt.Fprintf(stdout, "listen:   %s\n", snap.Listen)
	fmt.Fprintf(stdout, "clients:  %d connected, %d pinned, %d statements running\n",
		snap.Clients.Clients, snap.Clients.Pinned, snap.Clients.Active)

	for _, n := range snap.Nodes {
		state := "up"
		if !n.Up {
			state = "DOWN"
		}
		if n.ReadOnly {
			state += ", read-only"
		}
		if n.Role == topology.RoleReplica {
			if n.LagSeconds < 0 {
				state += ", lag unknown"
			} else {
				state += fmt.Sprintf(", lag %ds", n.LagSeconds)
			}
			if !n.Eligible {
				state += ", NOT serving reads"
			}
		}
		fmt.Fprintf(stdout, "%-9s %s (%s)\n", string(n.Role)+":", n.Address, state)
		fmt.Fprintf(stdout, "          %d/%d connections open, %d idle, %d dials, %d retired\n",
			n.Pool.Open, n.Pool.MaxOpen, n.Pool.Idle, n.Pool.Dials, n.Pool.Retired)
		if n.Pool.BreakerOpen {
			fmt.Fprintln(stdout, "          circuit breaker OPEN")
		}
		if n.LastError != "" {
			fmt.Fprintf(stdout, "          last error: %s\n", n.LastError)
		}
		for _, r := range snap.Replication {
			if r.Address != n.Address {
				continue
			}
			switch {
			case !r.Checked:
				fmt.Fprintln(stdout, "          replication: could not be read")
			case r.SourceHost == "":
				fmt.Fprintln(stdout, "          replication: not replicating from anybody")
			default:
				threads := "io+sql running"
				if !r.IORunning || !r.SQLRunning {
					threads = fmt.Sprintf("io %v, sql %v — STOPPED", r.IORunning, r.SQLRunning)
				}
				fmt.Fprintf(stdout, "          replication: from %s, %s\n", r.SourceHost, threads)
			}
			if r.LastError != "" {
				fmt.Fprintf(stdout, "          replication error: %s\n", r.LastError)
			}
		}
	}
	// Waits are the number that says max_open is too small; without them the
	// average is noise, so it is only printed when there were any.
	if snap.Pool.Waits > 0 {
		fmt.Fprintf(stdout, "          %d waits (avg %.1f ms), %d timed out\n",
			snap.Pool.Waits, snap.Pool.AvgWaitMillis, snap.Pool.WaitTimeouts)
	} else {
		fmt.Fprintln(stdout, "          no client ever waited for a connection")
	}

	// Traffic rules are only worth a line when they exist, but once they do,
	// what they have actually done is the thing you came to find out.
	if snap.Firewall.Rules > 0 || snap.Throttle.Rules > 0 || snap.Rewrites > 0 {
		fmt.Fprintf(stdout, "traffic:  %d rewrite, %d block, %d throttle rule(s)\n",
			snap.Rewrites, snap.Firewall.Rules, snap.Throttle.Rules)
		if snap.Firewall.Rules > 0 {
			fmt.Fprintf(stdout, "          %d statements refused, %d dry-run matches\n",
				snap.Firewall.Blocked, snap.Firewall.DryRunMatches)
		}
		if snap.Throttle.Rules > 0 {
			fmt.Fprintf(stdout, "          %d statements waited for a slot, %d refused\n",
				snap.Throttle.Waits, snap.Throttle.Rejects)
		}
	}

	if c := snap.Cache; c != nil {
		total := c.Hits + c.Misses
		ratio := 0.0
		if total > 0 {
			ratio = float64(c.Hits) / float64(total) * 100
		}
		prefix := c.Prefix
		if prefix == "" {
			prefix = "(not detected yet)"
		}
		fmt.Fprintf(stdout, "cache:    prefix %s, %d entries, %.1f MiB\n",
			prefix, c.Entries, float64(c.Bytes)/(1<<20))
		fmt.Fprintf(stdout, "          hit ratio %.1f%% (%d hits / %d misses)\n",
			ratio, c.Hits, c.Misses)
		for name, src := range c.Sources {
			fmt.Fprintf(stdout, "          %-22s %d hits, %d entries, %.1f MiB\n",
				name, src.Hits, src.Entries, float64(src.Bytes)/(1<<20))
		}
	}
	return nil
}

// printAdvice prints what the profiler has suggested, newest first. It
// reads the file, so it works whether or not gora is running — the point of
// keeping the advice on disk is that you can look at it on Monday.
func printAdvice(configPath string, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if !cfg.Profiling.Enabled {
		return fmt.Errorf("profiling is disabled in %s, so there is no advice to show", configPath)
	}
	if cfg.Profiling.AdviceFile == "" {
		return fmt.Errorf("profiling.advice_file is empty in %s: the advice is only logged", configPath)
	}

	advice, err := profile.ReadAdvice(cfg.Profiling.AdviceFile)
	if err != nil {
		return err
	}
	if len(advice) == 0 {
		fmt.Fprintf(stdout, "no advice yet (%s)\n", cfg.Profiling.AdviceFile)
		return nil
	}

	fmt.Fprintf(stdout, "%d suggestion(s) from %s\n", len(advice), cfg.Profiling.AdviceFile)
	for _, a := range advice {
		fmt.Fprintf(stdout, "\n[%s] seen %d time(s), last %s\n",
			a.Kind, a.Seen, a.LastSeen.Format(time.RFC3339))
		if a.Table != "" {
			fmt.Fprintf(stdout, "  table:  %s\n", a.Table)
		}
		if a.Query != "" {
			fmt.Fprintf(stdout, "  query:  %s\n", a.Query)
		}
		fmt.Fprintf(stdout, "  why:    %s\n", a.Reason)
		if a.Apply != "" {
			fmt.Fprintf(stdout, "  apply:  %s\n", a.Apply)
		}
	}
	return nil
}

// checkConfig validates the configuration without starting anything, and
// prints what gora would run with. It is what you put in front of a reload
// in a deployment script.
func checkConfig(configPath string, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "%s is valid\n", configPath)
	fmt.Fprintf(stdout, "  listen:   %s", cfg.Listen.Address)
	if cfg.Listen.MaxConnections > 0 {
		fmt.Fprintf(stdout, " (max %d clients)", cfg.Listen.MaxConnections)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  primary:  %s as %s\n", cfg.Backend.Address, cfg.Backend.Username)
	for _, replica := range cfg.Backend.Replicas {
		fmt.Fprintf(stdout, "  replica:  %s\n", replica)
	}
	if len(cfg.Backend.Replicas) > 0 {
		fmt.Fprintf(stdout, "  routing:  reads on replicas, sticky %s after a write, max lag %s\n",
			cfg.Routing.StickyAfterWrite, cfg.Routing.MaxReplicaLag)
	}
	fmt.Fprintf(stdout, "  clients:  %d account(s) authenticate against gora\n", len(cfg.Users))
	multiplexing := "off (one backend connection per session)"
	if cfg.Pool.Multiplexing {
		multiplexing = "on (connections shared between statements)"
	}
	fmt.Fprintf(stdout, "  pool:     max %d, idle %d-%d, multiplexing %s\n",
		cfg.Pool.MaxOpen, cfg.Pool.MinIdle, cfg.Pool.MaxIdle, multiplexing)

	socket := cfg.Status.Socket
	if socket == "" {
		socket = "disabled"
	}
	fmt.Fprintf(stdout, "  status:   %s\n", socket)
	fmt.Fprintf(stdout, "  log:      %s, level %s, format %s\n", cfg.Log.Path, cfg.Log.Level, cfg.Log.Format)

	if cfg.Cache.Enabled {
		prefix := cfg.Cache.TablePrefix
		if prefix == config.AutoPrefix {
			prefix = "auto (detected from the database)"
		}
		fmt.Fprintf(stdout, "  cache:    enabled, prefix %s\n", prefix)
	} else {
		fmt.Fprintln(stdout, "  cache:    disabled")
	}

	// The conf.d drop-ins are the part most likely to be wrong, and the part
	// a reload applies, so validating them is most of the value here. They
	// are compiled, not merely parsed: a rule using {prefix} with an
	// automatic table prefix has to be caught now, not at the next reload.
	rulesDir := rulesDirFor(cfg, configPath)
	rules, err := confd.Load(rulesDir)
	if err != nil {
		return err
	}
	if _, err := newTraffic(rules, cfg.Cache.TablePrefix, slog.New(slog.DiscardHandler)); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "  conf.d:   %d rule(s) from %s\n", rules.Len(), rulesDir)
	listRules(stdout, "cache", ruleNames(rules.Cache, func(r cache.Rule) string { return r.Name }))
	listRules(stdout, "rewrite", ruleNames(rules.Rewrites, func(r rewrite.Rule) string { return r.Name }))
	listRules(stdout, "block", ruleNames(rules.Blocks, func(r firewall.Rule) string {
		if r.DryRun {
			return r.Name + " (dry run)"
		}
		return r.Name
	}))
	listRules(stdout, "throttle", ruleNames(rules.Throttles, func(r throttle.Rule) string {
		return fmt.Sprintf("%s (max %d concurrent)", r.Name, r.MaxConcurrent)
	}))
	return nil
}

func ruleNames[T any](rules []T, name func(T) string) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, name(r))
	}
	return out
}

func listRules(stdout io.Writer, section string, names []string) {
	for _, n := range names {
		fmt.Fprintf(stdout, "              %-9s %s\n", section, n)
	}
}

// --- pid file ---

// writePIDFile records the running process so stop, restart and reload can
// find it without systemd.
func writePIDFile() error {
	if err := os.MkdirAll(runtimeDir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", runtimeDir, err)
	}
	pid := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(pidFilePath, []byte(pid+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", pidFilePath, err)
	}
	return nil
}

func removePIDFile() {
	_ = os.Remove(pidFilePath)
}

// runningPID returns the pid of the live instance. A pid file left behind by
// a crash names a process that no longer exists (or was replaced by an
// unrelated one that gora cannot signal): it is treated as absent, so a
// crash never blocks the next start.
func runningPID() (int, bool) {
	data, err := os.ReadFile(pidFilePath)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if !processAlive(pid) {
		return 0, false
	}
	return pid, true
}
