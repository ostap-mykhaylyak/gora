package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
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

	started := time.Now()
	log.Info("gora started",
		"version", version, "pid", os.Getpid(), "config", configPath,
		"listen", cfg.Listen.Address, "backend", cfg.Backend.Address)
	if cfg.Log.Console() {
		fmt.Fprintf(stdout, "gora %s started (pid %d)\n", version, os.Getpid())
	}
	// Said plainly rather than discovered by tcpdump: this build has no data
	// plane yet, the listener arrives with the proxy milestone.
	log.Warn("this build does not proxy traffic yet: no client listener is open")

	if cfg.Status.Socket != "" {
		collect := func() status.Snapshot {
			return status.Snapshot{
				Version:       version,
				PID:           os.Getpid(),
				UptimeSeconds: int64(time.Since(started).Seconds()),
				ConfigPath:    configPath,
				Listen:        cfg.Listen.Address,
				Backend:       cfg.Backend.Address,
			}
		}
		go func() {
			if err := status.Serve(ctx, cfg.Status.Socket, collect, log); err != nil {
				log.Warn("status socket unavailable", "socket", cfg.Status.Socket, "error", err)
			}
		}()
	}

	go watchReloads(ctx, configPath, log)

	<-ctx.Done()
	log.Info("shutdown complete")
	return nil
}

// watchReloads re-reads the configuration on every SIGHUP. A configuration
// that no longer parses is reported and ignored: a reload must never be
// able to take down an instance that is serving traffic.
func watchReloads(ctx context.Context, configPath string, log *slog.Logger) {
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
		log.Info("configuration re-read", "config", configPath)
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
	fmt.Fprintf(stdout, "backend:  %s\n", snap.Backend)
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
	socket := cfg.Status.Socket
	if socket == "" {
		socket = "disabled"
	}
	fmt.Fprintf(stdout, "  status:   %s\n", socket)
	fmt.Fprintf(stdout, "  log:      %s, level %s, format %s\n", cfg.Log.Path, cfg.Log.Level, cfg.Log.Format)
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
