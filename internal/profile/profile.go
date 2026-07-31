// Package profile turns gora's position into advice.
//
// A proxy sees every statement the application sends, with its timing, its
// row counts and whether the cache answered it. That is the data a slow
// query log gives you, plus the part a slow query log never has: how often
// the fast queries run. gora aggregates it by statement shape, reports it
// periodically, and — when asked — explains the heaviest ones against the
// schema and suggests what to do.
//
// It suggests. It never runs DDL on its own.
package profile

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/pool"
	"github.com/ostap-mykhaylyak/gora/internal/statement"
)

// Profiler aggregates executions by statement shape.
type Profiler struct {
	cfg   config.Profiling
	pool  *pool.Pool
	log   *slog.Logger
	store *Store

	mu     sync.Mutex
	digest map[string]*Stat
}

// Stat is what one statement shape did during the interval.
type Stat struct {
	Digest   string        `json:"digest"`
	Database string        `json:"database"`
	Sample   string        `json:"sample"`
	Calls    uint64        `json:"calls"`
	Cached   uint64        `json:"cached"`
	Errors   uint64        `json:"errors"`
	Rows     uint64        `json:"rows"`
	Total    time.Duration `json:"total"`
	Max      time.Duration `json:"max"`
}

// Avg is the mean execution time of the calls that reached the backend.
func (s Stat) Avg() time.Duration {
	backend := s.Calls - s.Cached
	if backend == 0 {
		return 0
	}
	return s.Total / time.Duration(backend)
}

// HitRatio is the share of calls the cache answered.
func (s Stat) HitRatio() float64 {
	if s.Calls == 0 {
		return 0
	}
	return float64(s.Cached) / float64(s.Calls) * 100
}

// New builds a profiler. The pool is used by the index advisor and may be
// nil when it is off.
func New(cfg config.Profiling, p *pool.Pool, log *slog.Logger) *Profiler {
	store := NewStore(cfg.AdviceFile)
	if err := store.Load(); err != nil {
		log.Warn("could not read the stored advice, starting a new file", "error", err)
	}
	return &Profiler{
		cfg:    cfg,
		pool:   p,
		log:    log,
		store:  store,
		digest: make(map[string]*Stat),
	}
}

// Advice returns what the profiler has suggested so far.
func (p *Profiler) Advice() []Advice { return p.store.List() }

// Observe records one execution. It runs on every statement, so it does the
// least it can: one fingerprint and one map lookup.
func (p *Profiler) Observe(db, query string, dur time.Duration, rows uint64, cached bool, err error) {
	digest := statement.Fingerprint(query)

	p.mu.Lock()
	st := p.digest[digest]
	if st == nil {
		st = &Stat{Digest: digest, Database: db, Sample: query}
		p.digest[digest] = st
	}
	st.Calls++
	st.Rows += rows
	if cached {
		st.Cached++
	} else {
		st.Total += dur
		if dur > st.Max {
			st.Max = dur
			st.Sample = query // keep the slowest example for EXPLAIN
			st.Database = db
		}
	}
	if err != nil {
		st.Errors++
	}
	p.mu.Unlock()

	// The slow query log is immediate: waiting for the next report to hear
	// about a statement that took eleven seconds is not a log, it is a
	// history lesson.
	if !cached && p.cfg.SlowQuery > 0 && dur >= p.cfg.SlowQuery.Std() {
		p.log.Warn("slow statement", "duration", dur, "database", db, "query", query)
	}
}

// Run reports periodically until ctx is cancelled.
func (p *Profiler) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.ReportInterval.Std())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		p.Report(ctx)
	}
}

// Report logs the interval's statistics, produces advice and resets the
// counters. Each report describes its own interval: a total accumulated
// since startup tells you less every hour it runs.
func (p *Profiler) Report(ctx context.Context) {
	stats := p.takeStats()
	if len(stats) == 0 {
		return
	}

	sort.Slice(stats, func(i, j int) bool { return stats[i].Total > stats[j].Total })

	p.log.Info("query report", "interval", p.cfg.ReportInterval, "statements", len(stats))
	top := stats
	if len(top) > p.cfg.TopQueries {
		top = top[:p.cfg.TopQueries]
	}
	for _, st := range top {
		p.log.Info("query",
			"calls", st.Calls,
			"total", st.Total.Round(time.Millisecond),
			"avg", st.Avg().Round(time.Microsecond),
			"max", st.Max.Round(time.Microsecond),
			"rows", st.Rows,
			"cache_hit_ratio", int(st.HitRatio()),
			"errors", st.Errors,
			"query", st.Digest)
	}

	var advice []Advice
	if p.cfg.SuggestRewrites {
		advice = append(advice, suggestRewrites(top)...)
	}
	if p.cfg.SuggestIndexes && p.pool != nil {
		advice = append(advice, p.suggestIndexes(ctx, top)...)
	}
	if len(advice) == 0 {
		return
	}

	p.store.Add(advice...)
	for _, a := range advice {
		p.log.Warn("advice", "kind", a.Kind, "reason", a.Reason, "apply", a.Apply, "query", a.Query)
	}
	if err := p.store.Save(); err != nil {
		p.log.Warn("could not write the advice file", "error", err)
	}
}

// takeStats returns the interval's statistics and starts a new interval.
func (p *Profiler) takeStats() []Stat {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]Stat, 0, len(p.digest))
	for _, st := range p.digest {
		out = append(out, *st)
	}
	p.digest = make(map[string]*Stat)
	return out
}
