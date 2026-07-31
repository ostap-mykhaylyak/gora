package cache

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/mysqltest"
	"github.com/ostap-mykhaylyak/gora/internal/pool"
)

const (
	db          = "wordpress"
	allOptions  = "SELECT option_name, option_value FROM wp_options WHERE autoload = 'yes'"
	transientIn = "SELECT option_name, option_value FROM wp_options WHERE option_name IN ('_transient_wc_x','_transient_timeout_wc_x')"
)

func testConfig() config.Cache {
	return config.Cache{
		Enabled:         true,
		TablePrefix:     "wp_",
		AutoloadOptions: true,
		Transients:      true,
		DefaultTTL:      config.Duration(time.Minute),
		MaxEntries:      100,
		MaxBytes:        1 << 20,
		MaxResultBytes:  1 << 20,
	}
}

func newCache(t *testing.T, cfg config.Cache, rules []Rule) *Cache {
	t.Helper()
	// The pool is only ever used to detect the table prefix, and these
	// tests configure one explicitly.
	c, err := New(cfg, nil, rules, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// result builds a result set with one row, standing in for whatever the
// backend would have returned.
func result(t *testing.T, value string) *mysql.Result {
	t.Helper()
	rs, err := mysql.BuildSimpleTextResultset([]string{"v"}, [][]any{{value}})
	if err != nil {
		t.Fatalf("building a result: %v", err)
	}
	return mysql.NewResult(rs)
}

// counting wraps a result in an exec function that records how many times
// it was called.
func counting(t *testing.T, value string, calls *atomic.Int64) func() (*mysql.Result, error) {
	return func() (*mysql.Result, error) {
		calls.Add(1)
		return result(t, value), nil
	}
}

func mustGet(t *testing.T, c *Cache, query string, exec func() (*mysql.Result, error)) Outcome {
	t.Helper()
	_, outcome, err := c.Get(db, query, exec)
	if err != nil {
		t.Fatalf("Get(%q): %v", query, err)
	}
	return outcome
}

// The autoloaded options query is the hottest query of every pageload, and
// the reason the cache exists.
func TestAlloptionsIsCached(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	var calls atomic.Int64

	if got := mustGet(t, c, allOptions, counting(t, "a", &calls)); got != OutcomeExecuted {
		t.Fatalf("first call outcome = %v, want executed", got)
	}
	if got := mustGet(t, c, allOptions, counting(t, "a", &calls)); got != OutcomeHit {
		t.Fatalf("second call outcome = %v, want hit", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("the backend was asked %d times, want 1", calls.Load())
	}
}

// Formatting is not part of a query's identity.
func TestWhitespaceDoesNotSplitEntries(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	var calls atomic.Int64

	mustGet(t, c, allOptions, counting(t, "a", &calls))
	spaced := "SELECT option_name,  option_value   FROM wp_options WHERE autoload = 'yes'"
	if got := mustGet(t, c, spaced, counting(t, "a", &calls)); got != OutcomeHit {
		t.Fatalf("outcome = %v, want hit: whitespace split the entry in two", got)
	}
}

// Transients are read in batches, one option and its expiry companion at a
// time. Recognising only the single-row form would cache nothing.
func TestTransientBatchIsCached(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	var calls atomic.Int64

	mustGet(t, c, transientIn, counting(t, "a", &calls))
	if got := mustGet(t, c, transientIn, counting(t, "a", &calls)); got != OutcomeHit {
		t.Fatalf("outcome = %v, want hit", got)
	}
}

// A batch containing anything that is not a transient is not a transient
// read, and gora has no invalidation story for it.
func TestMixedOptionBatchIsNotCached(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	mixed := "SELECT option_name, option_value FROM wp_options WHERE option_name IN ('_transient_x','siteurl')"
	var calls atomic.Int64

	mustGet(t, c, mixed, counting(t, "a", &calls))
	if got := mustGet(t, c, mixed, counting(t, "a", &calls)); got != OutcomeExecuted {
		t.Fatalf("outcome = %v, want executed", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("the backend was asked %d times, want 2", calls.Load())
	}
}

// Anything whose answer depends on the clock, the connection or a lock must
// never be answered from memory.
func TestVolatileReadsAreNeverCached(t *testing.T) {
	queries := []string{
		"SELECT * FROM wp_posts WHERE post_date < NOW()",
		"SELECT * FROM wp_posts ORDER BY RAND() LIMIT 1",
		"SELECT * FROM wp_posts WHERE ID = 1 FOR UPDATE",
		"SELECT GET_LOCK('import', 10)",
		"SELECT LAST_INSERT_ID()",
	}
	rules := []Rule{{Name: "everything", Match: "(?i)^SELECT", InvalidateOn: []string{"wp_posts"}}}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			c := newCache(t, testConfig(), rules)
			var calls atomic.Int64
			mustGet(t, c, q, counting(t, "a", &calls))
			if got := mustGet(t, c, q, counting(t, "a", &calls)); got != OutcomeExecuted {
				t.Fatalf("outcome = %v, want executed", got)
			}
		})
	}
}

// A write on one option must not cost the entries of the others.
func TestOptionWriteInvalidatesOnlyItsOwnEntry(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	other := "SELECT option_name, option_value FROM wp_options WHERE option_name IN ('_transient_other')"
	var calls atomic.Int64

	mustGet(t, c, transientIn, counting(t, "a", &calls))
	mustGet(t, c, other, counting(t, "b", &calls))

	c.InvalidateWrite(db, "UPDATE wp_options SET option_value = 'v' WHERE option_name = '_transient_wc_x'")

	if got := mustGet(t, c, transientIn, counting(t, "a", &calls)); got != OutcomeExecuted {
		t.Fatalf("the written transient was still served from cache (%v)", got)
	}
	if got := mustGet(t, c, other, counting(t, "b", &calls)); got != OutcomeHit {
		t.Fatalf("an unrelated transient was evicted (%v)", got)
	}
}

// The one that decides whether the cache is worth having on WooCommerce:
// transients are written constantly, and they are stored with autoload='off',
// so they must not evict the alloptions snapshot.
func TestTransientWriteDoesNotEvictAlloptions(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	var calls atomic.Int64
	mustGet(t, c, allOptions, counting(t, "a", &calls))

	c.InvalidateWrite(db, "INSERT INTO wp_options (option_name, option_value, autoload) "+
		"VALUES ('_transient_timeout_wc_x', '1700000000', 'off') "+
		"ON DUPLICATE KEY UPDATE option_name = VALUES(option_name)")

	if got := mustGet(t, c, allOptions, counting(t, "a", &calls)); got != OutcomeHit {
		t.Fatalf("a transient write evicted the alloptions snapshot (%v)", got)
	}
}

// A real option write, on the other hand, must drop it.
func TestOptionWriteEvictsAlloptions(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	var calls atomic.Int64
	mustGet(t, c, allOptions, counting(t, "a", &calls))

	c.InvalidateWrite(db, "UPDATE wp_options SET option_value = 'https://example.com' WHERE option_name = 'siteurl'")

	if got := mustGet(t, c, allOptions, counting(t, "a", &calls)); got != OutcomeExecuted {
		t.Fatalf("the alloptions snapshot survived an option write (%v)", got)
	}
}

// When gora cannot tell what a write touched, it throws everything away.
func TestUnparseableWriteFlushes(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	var calls atomic.Int64
	mustGet(t, c, allOptions, counting(t, "a", &calls))

	c.InvalidateWrite(db, "CALL rebuild_everything()")

	if got := mustGet(t, c, allOptions, counting(t, "a", &calls)); got != OutcomeExecuted {
		t.Fatalf("an unparseable write left entries behind (%v)", got)
	}
}

// conf.d rules cache what the built-ins know nothing about, and are dropped
// by writes on the tables they name.
func TestRuleCachesAndInvalidates(t *testing.T) {
	rules := []Rule{{
		Name:         "product-meta",
		Match:        `(?i)^SELECT post_id, meta_key, meta_value FROM {prefix}postmeta`,
		InvalidateOn: []string{"{prefix}postmeta"},
	}}
	c := newCache(t, testConfig(), rules)
	query := "SELECT post_id, meta_key, meta_value FROM wp_postmeta WHERE post_id IN (1,2,3)"
	var calls atomic.Int64

	mustGet(t, c, query, counting(t, "a", &calls))
	if got := mustGet(t, c, query, counting(t, "a", &calls)); got != OutcomeHit {
		t.Fatalf("the rule did not cache (%v)", got)
	}

	c.InvalidateWrite(db, "UPDATE wp_postmeta SET meta_value = 'x' WHERE post_id = 1")
	if got := mustGet(t, c, query, counting(t, "a", &calls)); got != OutcomeExecuted {
		t.Fatalf("a write on the invalidation table left the entry in place (%v)", got)
	}
}

// A write on some other table must not touch it.
func TestUnrelatedWriteKeepsRuleEntries(t *testing.T) {
	rules := []Rule{{
		Name:         "product-meta",
		Match:        `(?i)^SELECT post_id FROM {prefix}postmeta`,
		InvalidateOn: []string{"{prefix}postmeta"},
	}}
	c := newCache(t, testConfig(), rules)
	query := "SELECT post_id FROM wp_postmeta WHERE meta_key = 'x'"
	var calls atomic.Int64

	mustGet(t, c, query, counting(t, "a", &calls))
	c.InvalidateWrite(db, "UPDATE wp_comments SET comment_approved = '1' WHERE comment_ID = 5")
	if got := mustGet(t, c, query, counting(t, "a", &calls)); got != OutcomeHit {
		t.Fatalf("a write on another table dropped the entry (%v)", got)
	}
}

// On a cold cache every PHP worker asks for the autoloaded options at once.
// One of them should ask the database.
func TestConcurrentMissesAskTheBackendOnce(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	var calls atomic.Int64

	release := make(chan struct{})
	slow := func() (*mysql.Result, error) {
		calls.Add(1)
		<-release // hold the leader until every follower is waiting
		return result(t, "a"), nil
	}

	const workers = 20
	outcomes := make([]Outcome, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, outcome, err := c.Get(db, allOptions, slow)
			if err != nil {
				t.Errorf("Get: %v", err)
			}
			outcomes[i] = outcome
		}(i)
	}

	// Give the followers time to queue up behind the leader.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Fatalf("the backend was asked %d times, want 1", n)
	}
	executed, shared, hits := 0, 0, 0
	for _, o := range outcomes {
		switch o {
		case OutcomeExecuted:
			executed++
		case OutcomeShared:
			shared++
		case OutcomeHit:
			hits++
		}
	}
	if executed != 1 {
		t.Fatalf("%d sessions executed the query, want 1", executed)
	}
	if shared+hits != workers-1 {
		t.Fatalf("%d shared and %d hits, want %d between them", shared, hits, workers-1)
	}
}

// The entry budget is enforced.
func TestMaxEntriesEvictsTheLeastRecentlyUsed(t *testing.T) {
	cfg := testConfig()
	cfg.MaxEntries = 2
	rules := []Rule{{Name: "posts", Match: `(?i)^SELECT id`, InvalidateOn: []string{"wp_posts"}}}
	c := newCache(t, cfg, rules)

	var calls atomic.Int64
	for i := 0; i < 3; i++ {
		q := fmt.Sprintf("SELECT id FROM wp_posts WHERE ID = %d", i)
		mustGet(t, c, q, counting(t, "a", &calls))
	}
	if got := c.ReportStats().Entries; got != 2 {
		t.Fatalf("entries = %d, want 2", got)
	}
	// The first one is the oldest, so it is the one that went.
	if got := mustGet(t, c, "SELECT id FROM wp_posts WHERE ID = 0", counting(t, "a", &calls)); got != OutcomeExecuted {
		t.Fatalf("outcome = %v, want executed: the oldest entry survived", got)
	}
}

// Counting entries does not bound memory: the byte budget does.
func TestMaxBytesEvicts(t *testing.T) {
	rules := []Rule{{Name: "posts", Match: `(?i)^SELECT id`, InvalidateOn: []string{"wp_posts"}}}
	cfg := testConfig()
	cfg.MaxBytes = resultSize(result(t, "a")) + 8 // room for exactly one
	c := newCache(t, cfg, rules)

	var calls atomic.Int64
	mustGet(t, c, "SELECT id FROM wp_posts WHERE ID = 1", counting(t, "a", &calls))
	mustGet(t, c, "SELECT id FROM wp_posts WHERE ID = 2", counting(t, "a", &calls))

	if got := c.ReportStats().Entries; got != 1 {
		t.Fatalf("entries = %d, want 1: the byte budget was not enforced", got)
	}
}

// A result too large to be worth caching is executed and forgotten.
func TestOversizedResultIsNotCached(t *testing.T) {
	cfg := testConfig()
	cfg.MaxResultBytes = 1
	c := newCache(t, cfg, nil)

	var calls atomic.Int64
	mustGet(t, c, allOptions, counting(t, "a", &calls))
	if got := mustGet(t, c, allOptions, counting(t, "a", &calls)); got != OutcomeExecuted {
		t.Fatalf("outcome = %v, want executed", got)
	}
}

func TestEntriesExpire(t *testing.T) {
	cfg := testConfig()
	cfg.DefaultTTL = config.Duration(20 * time.Millisecond)
	c := newCache(t, cfg, nil)

	var calls atomic.Int64
	mustGet(t, c, allOptions, counting(t, "a", &calls))
	time.Sleep(60 * time.Millisecond) // past the TTL and its jitter
	if got := mustGet(t, c, allOptions, counting(t, "a", &calls)); got != OutcomeExecuted {
		t.Fatalf("outcome = %v, want executed: the entry outlived its TTL", got)
	}
}

// Reloading the rules cannot leave entries behind that no rule would
// produce any more.
func TestSetRulesFlushes(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	var calls atomic.Int64
	mustGet(t, c, allOptions, counting(t, "a", &calls))

	if err := c.SetRules(nil); err != nil {
		t.Fatalf("SetRules: %v", err)
	}
	if got := mustGet(t, c, allOptions, counting(t, "a", &calls)); got != OutcomeExecuted {
		t.Fatalf("outcome = %v, want executed: the reload left entries behind", got)
	}
}

// The hit ratio must describe the cache, not the traffic: queries that
// were never cacheable belong in neither column.
func TestStatsIgnoreUncacheableTraffic(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	var calls atomic.Int64

	for i := 0; i < 5; i++ {
		mustGet(t, c, "SELECT * FROM wp_posts WHERE ID = 1", counting(t, "a", &calls))
	}
	mustGet(t, c, allOptions, counting(t, "a", &calls)) // one miss
	mustGet(t, c, allOptions, counting(t, "a", &calls)) // one hit

	rep := c.ReportStats()
	if rep.Hits != 1 || rep.Misses != 1 {
		t.Fatalf("hits = %d, misses = %d, want 1 and 1", rep.Hits, rep.Misses)
	}
	if got := rep.Sources[sourceAlloptions].Hits; got != 1 {
		t.Fatalf("alloptions hits = %d, want 1", got)
	}
}

// With table_prefix: auto the prefix comes from the database. Until it is
// known nothing is cached — a few uncached pageloads at startup beat
// caching under a guessed prefix.
func TestPrefixIsDetected(t *testing.T) {
	backend := mysqltest.Start(t, "gora", "secret")
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

	cfg := testConfig()
	cfg.TablePrefix = config.AutoPrefix
	c, err := New(cfg, p, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var calls atomic.Int64
	if got := mustGet(t, c, allOptions, counting(t, "a", &calls)); got != OutcomeExecuted {
		t.Fatalf("outcome = %v, want executed while the prefix is unknown", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for c.Prefix() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := c.Prefix(); got != "wp_" {
		t.Fatalf("prefix = %q, want wp_", got)
	}

	mustGet(t, c, allOptions, counting(t, "a", &calls))
	if got := mustGet(t, c, allOptions, counting(t, "a", &calls)); got != OutcomeHit {
		t.Fatalf("outcome = %v, want hit once the prefix is known", got)
	}
}
