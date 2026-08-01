package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/status"
	"github.com/ostap-mykhaylyak/gora/internal/topology"
)

// topInterval is how often the live view refreshes. One second is what
// makes the rates mean "now" rather than "recently".
const topInterval = time.Second

// top shows what gora is doing, refreshed every second, until interrupted.
//
// `gora status` answers "what is the state"; this answers "what is
// happening", which is a different question and the one you have during an
// incident. The rates are computed here, from the difference between two
// snapshots, so the daemon keeps totals and nothing has to remember windows.
func top(configPath string, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.Status.Socket == "" {
		return fmt.Errorf("status.socket is disabled in %s", configPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		prev     status.Snapshot
		prevTime time.Time
		havePrev bool
	)
	ticker := time.NewTicker(topInterval)
	defer ticker.Stop()

	for {
		snap, err := status.Query(cfg.Status.Socket)
		now := time.Now()

		clearScreen(stdout)
		if err != nil {
			fmt.Fprintf(stdout, "gora is not answering on %s\n\n%v\n", cfg.Status.Socket, err)
			fmt.Fprintln(stdout, "\nstill watching; press Ctrl-C to stop")
		} else {
			var elapsed time.Duration
			if havePrev {
				elapsed = now.Sub(prevTime)
			}
			renderTop(stdout, snap, prev, elapsed)
			prev, prevTime, havePrev = snap, now, true
		}

		select {
		case <-ctx.Done():
			// Leave the terminal on a fresh line rather than mid-frame.
			fmt.Fprintln(stdout)
			return nil
		case <-ticker.C:
		}
	}
}

// clearScreen moves the cursor home and clears what is below it, which
// redraws without the flicker of erasing first and drawing after.
func clearScreen(w io.Writer) { fmt.Fprint(w, "\033[H\033[J") }

func renderTop(w io.Writer, snap, prev status.Snapshot, elapsed time.Duration) {
	fmt.Fprintf(w, "gora %s   up %s   %s\n",
		snap.Version, snap.Uptime(), time.Now().Format("15:04:05"))
	fmt.Fprintln(w, strings.Repeat("-", 72))

	// Traffic, as rates where there are two snapshots to subtract.
	qps, eps := rate(snap.Clients.Statements, prev.Clients.Statements, elapsed),
		rate(snap.Clients.Errors, prev.Clients.Errors, elapsed)
	fmt.Fprintf(w, "clients   %d connected, %d pinned, %d running\n",
		snap.Clients.Clients, snap.Clients.Pinned, snap.Clients.Active)
	if elapsed > 0 {
		fmt.Fprintf(w, "traffic   %.1f statements/s, %.1f errors/s (%d total, %d errors)\n",
			qps, eps, snap.Clients.Statements, snap.Clients.Errors)
	} else {
		fmt.Fprintf(w, "traffic   %d statements, %d errors\n",
			snap.Clients.Statements, snap.Clients.Errors)
	}

	if c := snap.Cache; c != nil {
		total := c.Hits + c.Misses
		ratio := 0.0
		if total > 0 {
			ratio = float64(c.Hits) / float64(total) * 100
		}
		var prevHits uint64
		if prev.Cache != nil {
			prevHits = prev.Cache.Hits
		}
		hitRate := rate(c.Hits, prevHits, elapsed)
		fmt.Fprintf(w, "cache     %.1f%% hit ratio, %d entries, %.1f MiB",
			ratio, c.Entries, float64(c.Bytes)/(1<<20))
		if elapsed > 0 {
			fmt.Fprintf(w, ", %.1f hits/s", hitRate)
		}
		fmt.Fprintln(w)
	}

	if snap.Firewall.Rules > 0 || snap.Throttle.Rules > 0 {
		fmt.Fprintf(w, "traffic rules  %d refused, %d throttled, %d dry-run matches\n",
			snap.Firewall.Blocked, snap.Throttle.Rejects, snap.Firewall.DryRunMatches)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "%-24s %-8s %-6s %-7s %-12s %s\n", "NODE", "ROLE", "STATE", "LAG", "CONNECTIONS", "READS")
	for _, n := range snap.Nodes {
		state := "up"
		switch {
		case !n.Up:
			state = "DOWN"
		case n.ReadOnly && n.Role == topology.RolePrimary:
			state = "RO"
		}
		lag := "-"
		if n.Role == topology.RoleReplica {
			if n.LagSeconds < 0 {
				lag = "?"
			} else {
				lag = fmt.Sprintf("%ds", n.LagSeconds)
			}
		}
		reads := "yes"
		if !n.Eligible {
			reads = "no"
		}
		fmt.Fprintf(w, "%-24s %-8s %-6s %-7s %-12s %s\n",
			n.Address, n.Role, state, lag,
			fmt.Sprintf("%d/%d", n.Pool.Open, n.Pool.MaxOpen), reads)
	}

	// Replication is only worth the lines when it is being managed.
	if len(snap.Replication) > 0 {
		fmt.Fprintln(w)
		for _, r := range snap.Replication {
			switch {
			case !r.Checked:
				fmt.Fprintf(w, "%-24s replication unreadable\n", r.Address)
			case r.SourceHost == "":
				fmt.Fprintf(w, "%-24s not replicating\n", r.Address)
			case r.IORunning && r.SQLRunning:
				fmt.Fprintf(w, "%-24s replicating from %s\n", r.Address, r.SourceHost)
			default:
				fmt.Fprintf(w, "%-24s REPLICATION STOPPED (io %v, sql %v)\n",
					r.Address, r.IORunning, r.SQLRunning)
			}
			if r.LastError != "" {
				fmt.Fprintf(w, "%-24s   %s\n", "", truncate(r.LastError, 60))
			}
		}
	}

	fmt.Fprintln(w, "\npress Ctrl-C to stop")
}

// rate turns two totals into a per-second figure. A counter that went
// backwards means the daemon restarted between frames, and no rate is
// better than a negative one.
func rate(now, before uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 || now < before {
		return 0
	}
	return float64(now-before) / elapsed.Seconds()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
