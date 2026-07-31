package profile

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/mysqltest"
	"github.com/ostap-mykhaylyak/gora/internal/pool"
)

func testConfig(t *testing.T) config.Profiling {
	t.Helper()
	return config.Profiling{
		Enabled:         true,
		SlowQuery:       config.Duration(100 * time.Millisecond),
		ReportInterval:  config.Duration(time.Minute),
		TopQueries:      10,
		SuggestIndexes:  true,
		SuggestRewrites: true,
		AdviceFile:      filepath.Join(t.TempDir(), "advice.json"),
	}
}

func newProfiler(t *testing.T, cfg config.Profiling, p *pool.Pool, w *bytes.Buffer) *Profiler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(cfg, p, log)
}

// Executions of the same shape are one line in the report, whatever the
// literals were.
func TestObserveAggregatesByShape(t *testing.T) {
	p := newProfiler(t, testConfig(t), nil, &bytes.Buffer{})

	p.Observe("wordpress", "SELECT * FROM wp_posts WHERE ID = 1", 10*time.Millisecond, 1, false, nil)
	p.Observe("wordpress", "SELECT * FROM wp_posts WHERE ID = 2", 30*time.Millisecond, 1, false, nil)
	p.Observe("wordpress", "SELECT * FROM wp_posts WHERE ID = 3", 0, 1, true, nil)

	stats := p.takeStats()
	if len(stats) != 1 {
		t.Fatalf("got %d statement shapes, want 1", len(stats))
	}
	st := stats[0]
	if st.Calls != 3 || st.Cached != 1 {
		t.Fatalf("calls = %d, cached = %d, want 3 and 1", st.Calls, st.Cached)
	}
	if st.Total != 40*time.Millisecond {
		t.Fatalf("total = %s, want 40ms", st.Total)
	}
	// The average is over the calls that reached the database: counting the
	// cached ones would make the cache look like it made queries faster.
	if st.Avg() != 20*time.Millisecond {
		t.Fatalf("avg = %s, want 20ms", st.Avg())
	}
	if st.Max != 30*time.Millisecond {
		t.Fatalf("max = %s, want 30ms", st.Max)
	}
	if got := int(st.HitRatio()); got != 33 {
		t.Fatalf("hit ratio = %d%%, want 33%%", got)
	}
}

// The heaviest execution is the one kept as an example, because it is the
// one worth explaining.
func TestSampleIsTheSlowestExecution(t *testing.T) {
	p := newProfiler(t, testConfig(t), nil, &bytes.Buffer{})

	p.Observe("wordpress", "SELECT * FROM wp_posts WHERE ID = 1", 10*time.Millisecond, 1, false, nil)
	p.Observe("wordpress", "SELECT * FROM wp_posts WHERE ID = 999", time.Second, 1, false, nil)

	stats := p.takeStats()
	if !strings.Contains(stats[0].Sample, "999") {
		t.Fatalf("sample = %q, want the slowest execution", stats[0].Sample)
	}
}

// A statement that took eleven seconds is news now, not at the next report.
func TestSlowStatementIsLoggedImmediately(t *testing.T) {
	var out bytes.Buffer
	p := newProfiler(t, testConfig(t), nil, &out)

	p.Observe("wordpress", "SELECT SLEEP(1)", 500*time.Millisecond, 0, false, nil)
	if !strings.Contains(out.String(), "slow statement") {
		t.Fatalf("the slow statement was not logged: %q", out.String())
	}

	out.Reset()
	p.Observe("wordpress", "SELECT 1", time.Millisecond, 1, false, nil)
	if strings.Contains(out.String(), "slow statement") {
		t.Fatalf("a fast statement was logged as slow: %q", out.String())
	}
}

// A cache hit costs nothing and must not appear in the slow log, however
// long the original execution took.
func TestCachedStatementsAreNeverSlow(t *testing.T) {
	var out bytes.Buffer
	p := newProfiler(t, testConfig(t), nil, &out)

	p.Observe("wordpress", "SELECT 1", time.Second, 1, true, nil)
	if strings.Contains(out.String(), "slow statement") {
		t.Fatalf("a cache hit was logged as a slow statement: %q", out.String())
	}
}

// Each report describes its own interval: a total accumulated since startup
// tells you less every hour it runs.
func TestReportResetsTheInterval(t *testing.T) {
	p := newProfiler(t, testConfig(t), nil, &bytes.Buffer{})

	p.Observe("wordpress", "SELECT 1", time.Millisecond, 1, false, nil)
	p.Report(context.Background())

	if stats := p.takeStats(); len(stats) != 0 {
		t.Fatalf("got %d shapes after a report, want the counters reset", len(stats))
	}
}

// The report names the antipatterns it saw, and the advice survives in the
// file.
func TestReportProducesAndStoresAdvice(t *testing.T) {
	cfg := testConfig(t)
	cfg.SuggestIndexes = false
	var out bytes.Buffer
	p := newProfiler(t, cfg, nil, &out)

	p.Observe("wordpress", "SELECT ID FROM wp_posts ORDER BY RAND() LIMIT 1", time.Second, 1, false, nil)
	p.Report(context.Background())

	advice := p.Advice()
	if len(advice) != 1 {
		t.Fatalf("got %d suggestions, want 1", len(advice))
	}
	if advice[0].Kind != KindRewrite {
		t.Fatalf("kind = %q, want %q", advice[0].Kind, KindRewrite)
	}

	// And it is on disk, where it can be read after a restart.
	stored, err := ReadAdvice(cfg.AdviceFile)
	if err != nil {
		t.Fatalf("ReadAdvice: %v", err)
	}
	if len(stored) != 1 || stored[0].Kind != KindRewrite {
		t.Fatalf("stored advice = %+v", stored)
	}
}

// The whole index advisor, against a backend that answers EXPLAIN.
func TestIndexAdviceFromExplain(t *testing.T) {
	backend := mysqltest.Start(t, "gora", "secret")
	backend.Answer("EXPLAIN", []string{"id", "select_type", "table", "type", "key", "rows", "Extra"},
		[][]any{{int64(1), "SIMPLE", "wp_postmeta", "ALL", "", int64(50000), "Using where"}})

	p, err := pool.New(config.Backend{
		Address:        backend.Addr,
		Username:       "gora",
		Password:       "secret",
		ConnectTimeout: config.Duration(2 * time.Second),
	}, config.Pool{
		MaxOpen:        2,
		MaxIdle:        2,
		PingInterval:   config.Duration(time.Second),
		AcquireTimeout: config.Duration(2 * time.Second),
	}, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	t.Cleanup(p.Close)

	cfg := testConfig(t)
	cfg.SuggestRewrites = false
	prof := newProfiler(t, cfg, p, &bytes.Buffer{})

	prof.Observe("wordpress", "SELECT meta_value FROM wp_postmeta WHERE meta_key = '_price'",
		2*time.Second, 3, false, nil)
	prof.Report(context.Background())

	advice := prof.Advice()
	if len(advice) != 1 {
		t.Fatalf("got %d suggestions, want 1: %+v", len(advice), advice)
	}
	a := advice[0]
	if a.Kind != KindIndex {
		t.Fatalf("kind = %q, want %q", a.Kind, KindIndex)
	}
	if a.Table != "wp_postmeta" {
		t.Fatalf("table = %q, want wp_postmeta", a.Table)
	}
	want := "ALTER TABLE wp_postmeta ADD INDEX idx_gora_meta_key (meta_key);"
	if a.Apply != want {
		t.Fatalf("apply = %q, want %q", a.Apply, want)
	}
}

// A plan that uses an index has nothing to suggest.
func TestNoAdviceWhenTheQueryUsesAnIndex(t *testing.T) {
	backend := mysqltest.Start(t, "gora", "secret")
	backend.Answer("EXPLAIN", []string{"table", "type", "key", "rows"},
		[][]any{{"wp_postmeta", "ref", "meta_key", int64(50000)}})

	p, err := pool.New(config.Backend{
		Address:        backend.Addr,
		Username:       "gora",
		Password:       "secret",
		ConnectTimeout: config.Duration(2 * time.Second),
	}, config.Pool{
		MaxOpen:        2,
		MaxIdle:        2,
		PingInterval:   config.Duration(time.Second),
		AcquireTimeout: config.Duration(2 * time.Second),
	}, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	t.Cleanup(p.Close)

	cfg := testConfig(t)
	cfg.SuggestRewrites = false
	prof := newProfiler(t, cfg, p, &bytes.Buffer{})

	prof.Observe("wordpress", "SELECT meta_value FROM wp_postmeta WHERE meta_key = '_price'",
		2*time.Second, 3, false, nil)
	prof.Report(context.Background())

	if advice := prof.Advice(); len(advice) != 0 {
		t.Fatalf("got %d suggestions for a query using an index: %+v", len(advice), advice)
	}
}
