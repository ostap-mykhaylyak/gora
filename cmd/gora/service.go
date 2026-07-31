package main

import (
	"context"
	"crypto/tls"
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
	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/pool"
	"github.com/ostap-mykhaylyak/gora/internal/proxy"
	"github.com/ostap-mykhaylyak/gora/internal/status"
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

	backendPool, err := pool.New(cfg.Backend, cfg.Pool, log, nil)
	if err != nil {
		return err
	}
	defer backendPool.Close()

	rulesDir := rulesDirFor(cfg, configPath)
	rules, err := cache.LoadRuleDir(rulesDir)
	if err != nil {
		return err
	}

	var queryCache *cache.Cache
	if cfg.Cache.Enabled {
		queryCache, err = cache.New(cfg.Cache, backendPool, rules, log)
		if err != nil {
			return err
		}
		log.Info("query cache enabled",
			"table_prefix", cfg.Cache.TablePrefix, "rules", len(rules), "rules_dir", rulesDir)
		if cfg.Cache.Warmup {
			warmer := cache.NewWarmer(queryCache, backendPool, log)
			go warmer.Run(ctx)
			queryCache.SetRefetch(warmer.Trigger)
		}
	}

	listenTLS, err := clientTLS(cfg.Listen)
	if err != nil {
		return err
	}
	if listenTLS != nil {
		log.Info("client TLS enabled", "cert", cfg.Listen.TLS.Cert)
	}

	srv := proxy.New(proxy.Options{
		Listen:  cfg.Listen,
		Users:   cfg.Users,
		PoolCfg: cfg.Pool,
		Pool:    backendPool,
		Cache:   queryCache,
		TLS:     listenTLS,
		Log:     log,
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
			}
			if queryCache != nil {
				rep := queryCache.ReportStats()
				snap.Cache = &rep
			}
			return snap
		}
		go func() {
			if err := status.Serve(ctx, cfg.Status.Socket, collect, log); err != nil {
				log.Warn("status socket unavailable", "socket", cfg.Status.Socket, "error", err)
			}
		}()
	}

	go watchReloads(ctx, configPath, rulesDir, queryCache, log)

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
func watchReloads(ctx context.Context, configPath, rulesDir string, queryCache *cache.Cache, log *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		}

		if _, err := config.Load(configPath); err != nil {
			log.Error("reload failed, the running configuration is unchanged", "error", err)
			continue
		}
		rules, err := cache.LoadRuleDir(rulesDir)
		if err != nil {
			log.Error("reload failed, keeping the previous rules", "error", err)
			continue
		}
		if queryCache != nil {
			if err := queryCache.SetRules(rules); err != nil {
				log.Error("reload failed, keeping the previous rules", "error", err)
				continue
			}
		}
		log.Info("configuration reloaded", "config", configPath,
			"rules", len(rules), "rules_dir", rulesDir)
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
func printStatus(configPath string, stdout io.Writer) error {
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

	fmt.Fprintf(stdout, "gora %s, pid %d, up %s\n", snap.Version, snap.PID, snap.Uptime())
	fmt.Fprintf(stdout, "config:   %s\n", snap.ConfigPath)
	fmt.Fprintf(stdout, "listen:   %s\n", snap.Listen)
	fmt.Fprintf(stdout, "clients:  %d connected, %d pinned, %d statements running\n",
		snap.Clients.Clients, snap.Clients.Pinned, snap.Clients.Active)

	breaker := "closed (backend healthy)"
	if snap.Pool.BreakerOpen {
		breaker = "OPEN (backend unreachable)"
	}
	fmt.Fprintf(stdout, "backend:  %s, %d/%d connections open, %d idle, breaker %s\n",
		snap.Backend, snap.Pool.Open, snap.Pool.MaxOpen, snap.Pool.Idle, breaker)
	fmt.Fprintf(stdout, "pool:     %d dials, %d closed, %d retired\n",
		snap.Pool.Dials, snap.Pool.Discards, snap.Pool.Retired)
	// Waits are the number that says max_open is too small; without them the
	// average is noise, so it is only printed when there were any.
	if snap.Pool.Waits > 0 {
		fmt.Fprintf(stdout, "          %d waits (avg %.1f ms), %d timed out\n",
			snap.Pool.Waits, snap.Pool.AvgWaitMillis, snap.Pool.WaitTimeouts)
	} else {
		fmt.Fprintln(stdout, "          no client ever waited for a connection")
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
	fmt.Fprintf(stdout, "  backend:  %s as %s\n", cfg.Backend.Address, cfg.Backend.Username)
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

	// The conf.d drop-ins are the part most likely to be wrong, and the part
	// a reload applies, so validating them is most of the value here.
	if !cfg.Cache.Enabled {
		fmt.Fprintln(stdout, "  cache:    disabled")
		return nil
	}
	rulesDir := rulesDirFor(cfg, configPath)
	rules, err := cache.LoadRuleDir(rulesDir)
	if err != nil {
		return err
	}
	prefix := cfg.Cache.TablePrefix
	if prefix == config.AutoPrefix {
		prefix = "auto (detected from the database)"
	}
	fmt.Fprintf(stdout, "  cache:    prefix %s, %d rule(s) from %s\n", prefix, len(rules), rulesDir)
	for _, r := range rules {
		fmt.Fprintf(stdout, "              - %s\n", r.Name)
	}
	return nil
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
