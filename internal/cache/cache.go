// Package cache implements gora's WordPress-aware query cache.
//
// Three families of reads are served from memory:
//
//   - the autoloaded options query (wp_load_alloptions), the single hottest
//     query of every WordPress pageload;
//   - transient reads, which are WordPress's own database-backed cache;
//   - any SELECT matching a rule from the conf.d drop-ins, e.g. the
//     WooCommerce profile.
//
// Correctness comes from write-driven invalidation, with a TTL as a safety
// net: every write flowing through gora drops exactly the entries it can
// affect. Writes on the options table are attributed to individual option
// names when the SQL allows it — wpdb's does — so a transient update does
// not evict the whole options cache. A write gora cannot parse flushes
// everything: when in doubt it prefers a database roundtrip to a stale
// answer.
package cache

import (
	"container/list"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"

	"github.com/ostap-mykhaylyak/gora/internal/config"
	"github.com/ostap-mykhaylyak/gora/internal/pool"
	"github.com/ostap-mykhaylyak/gora/internal/statement"
)

// ttlJitter spreads expiry times by up to this fraction of the TTL. Without
// it, everything cached during a traffic spike expires during the next one,
// and the stampede protection has to work for nothing.
const ttlJitter = 0.1

// sharedWait bounds how long a session waits for another session's identical
// query. Past it, it runs the query itself: a stuck leader must not take
// every reader down with it.
const sharedWait = 10 * time.Second

// Outcome says where a result came from.
type Outcome int

const (
	// OutcomeExecuted means this session ran the query on the backend.
	OutcomeExecuted Outcome = iota
	// OutcomeHit means it came from the cache.
	OutcomeHit
	// OutcomeShared means another session was already running the identical
	// query and this one waited for its result instead of adding load.
	OutcomeShared
)

// Cache is safe for concurrent use by every session.
type Cache struct {
	// cfg is swapped, not mutated: the tuning settings — sizes, TTL, which
	// built-ins are on — can change while gora runs, and every reader takes
	// the whole thing at once rather than half of an old one and half of a
	// new one.
	cfg  atomic.Pointer[config.Cache]
	pool *pool.Pool
	log  *slog.Logger

	// Rules and the table prefix they are compiled against. With
	// table_prefix: auto the prefix is discovered from the first database
	// gora sees, so both can change after startup.
	rulesMu   sync.RWMutex
	rawRules  []Rule
	rules     []Rule
	pats      *patterns
	detecting bool

	mu       sync.Mutex
	entries  map[string]*entry
	lru      *list.List // *entry, front = most recently used
	byTag    map[string]map[*entry]struct{}
	bytes    int
	sources  map[string]*sourceStat
	learned  map[string]string // db -> the exact alloptions query seen
	inflight map[string]*call

	// hits counts lookups served from memory, misses counts cacheable
	// queries that had to reach the backend. Queries that are not cacheable
	// at all — most of a WordPress workload — are counted in neither, so
	// the ratio describes the cache and not the traffic.
	hits, misses uint64

	// refetch, when set, is called after the alloptions entry of a database
	// is invalidated, so it is repopulated before the next pageload pays for
	// it.
	refetch func(db, query string)
}

type entry struct {
	key     string
	result  *mysql.Result
	expires time.Time
	tags    []string
	source  string
	bytes   int
	elem    *list.Element

	// For paired SQL_CALC_FOUND_ROWS entries: the FOUND_ROWS() count that
	// belongs with these rows. Until it is captured the entry is not served,
	// because rows without their matching count corrupt pagination.
	foundRows    uint64
	hasFoundRows bool
}

// call is one in-flight execution other sessions can wait on.
type call struct {
	done chan struct{}
	r    *mysql.Result
	err  error
}

// sourceStat aggregates per-source statistics (alloptions, transients, and
// each conf.d rule).
type sourceStat struct {
	Hits    uint64 `json:"hits"`
	Entries int    `json:"entries"`
	Bytes   int    `json:"bytes"`
}

// New builds the cache. rawRules come from LoadRuleDir with their {prefix}
// placeholders still in place.
func New(cfg config.Cache, p *pool.Pool, rawRules []Rule, log *slog.Logger) (*Cache, error) {
	c := &Cache{
		pool:     p,
		log:      log,
		rawRules: rawRules,
		entries:  make(map[string]*entry),
		lru:      list.New(),
		byTag:    make(map[string]map[*entry]struct{}),
		sources:  make(map[string]*sourceStat),
		learned:  make(map[string]string),
		inflight: make(map[string]*call),
	}
	c.cfg.Store(&cfg)
	if cfg.TablePrefix != config.AutoPrefix {
		if err := c.bindPrefix(cfg.TablePrefix); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// bindPrefix compiles the built-in patterns and the conf.d rules against a
// table prefix.
func (c *Cache) bindPrefix(prefix string) error {
	rules, err := compileRules(c.rawRules, prefix)
	if err != nil {
		return err
	}
	c.rulesMu.Lock()
	c.pats = newPatterns(prefix)
	c.rules = rules
	c.rulesMu.Unlock()
	return nil
}

// conf returns the settings in force. It is one atomic load, on paths that
// run for every cacheable statement.
func (c *Cache) conf() config.Cache { return *c.cfg.Load() }

// SetConfig replaces the tuning settings. Entries already held are left
// alone: a smaller budget is enforced by the next insertion rather than by
// throwing away what is currently answering queries.
func (c *Cache) SetConfig(cfg config.Cache) { c.cfg.Store(&cfg) }

// SetRules replaces the conf.d rules (hot reload) and flushes the cache:
// entries may descend from rules that no longer exist.
func (c *Cache) SetRules(rawRules []Rule) error {
	c.rulesMu.RLock()
	pats := c.pats
	c.rulesMu.RUnlock()

	c.rulesMu.Lock()
	c.rawRules = rawRules
	c.rulesMu.Unlock()

	if pats != nil {
		if err := c.bindPrefix(pats.prefix); err != nil {
			return err
		}
	}
	c.Flush("rules reloaded")
	return nil
}

// SetRefetch installs the warm-up callback (see Warmer).
func (c *Cache) SetRefetch(fn func(db, query string)) {
	c.mu.Lock()
	c.refetch = fn
	c.mu.Unlock()
}

// Prefix returns the table prefix in use, empty while it is still unknown.
func (c *Cache) Prefix() string {
	c.rulesMu.RLock()
	defer c.rulesMu.RUnlock()
	if c.pats == nil {
		return ""
	}
	return c.pats.prefix
}

// ready returns the compiled patterns and rules, and whether the cache can
// work on this database yet. With table_prefix: auto the first call starts
// the detection in the background and reports not-ready: a few uncached
// pageloads at startup are better than guessing the prefix.
func (c *Cache) ready(db string) (*patterns, []Rule, bool) {
	c.rulesMu.RLock()
	pats, rules := c.pats, c.rules
	c.rulesMu.RUnlock()
	if pats != nil {
		return pats, rules, true
	}

	c.rulesMu.Lock()
	if c.pats == nil && !c.detecting && db != "" {
		c.detecting = true
		go c.detectPrefix(db)
	}
	c.rulesMu.Unlock()
	return nil, nil, false
}

// tableNameRe guards the database name interpolated into the detection
// query. Anything else is not a database gora will go looking into.
var tableNameRe = regexp.MustCompile(`^[A-Za-z0-9_$]+$`)

// detectPrefix finds the WordPress table prefix by looking for the options
// table. Multisite installations have several (wp_options, wp_2_options):
// the shortest name is the base prefix, which is the one wpdb uses for the
// queries gora caches.
func (c *Cache) detectPrefix(db string) {
	defer func() {
		c.rulesMu.Lock()
		c.detecting = false
		c.rulesMu.Unlock()
	}()

	if !tableNameRe.MatchString(db) {
		c.log.Warn("cannot detect the table prefix: unexpected database name",
			"database", db, "hint", "set cache.table_prefix explicitly")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		c.log.Warn("cannot detect the table prefix yet", "error", err)
		return
	}
	query := fmt.Sprintf(
		"SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = '%s' "+
			"AND TABLE_NAME LIKE '%%options' ORDER BY CHAR_LENGTH(TABLE_NAME) LIMIT 1", db)
	r, err := conn.Execute(query)
	c.pool.Release(conn)
	if err != nil {
		c.log.Warn("cannot detect the table prefix", "error", err)
		return
	}
	if r == nil || len(r.Values) == 0 {
		c.log.Warn("cannot detect the table prefix: no options table found",
			"database", db, "hint", "set cache.table_prefix explicitly")
		return
	}

	table, err := r.GetString(0, 0)
	if err != nil {
		c.log.Warn("cannot detect the table prefix", "error", err)
		return
	}
	// The LIKE in the query should guarantee the suffix, but the answer
	// comes from outside gora and slicing it on trust is how a proxy panics
	// on a database that is not what it expected.
	if !strings.HasSuffix(table, "options") {
		c.log.Warn("cannot detect the table prefix: unexpected answer from the backend",
			"table", table, "hint", "set cache.table_prefix explicitly")
		return
	}
	prefix := strings.TrimSuffix(table, "options")
	if prefix == "" {
		c.log.Warn("the options table has no prefix, refusing to cache on bare table names",
			"table", table, "hint", "set cache.table_prefix explicitly")
		return
	}

	if err := c.bindPrefix(prefix); err != nil {
		c.log.Error("the conf.d rules do not compile with the detected prefix",
			"prefix", prefix, "error", err)
		return
	}
	c.log.Info("table prefix detected", "prefix", prefix, "database", db)
}

// Tag namespaces: writes on a table, writes on one option, the alloptions
// snapshot.
func tagTable(db, table string) string { return "t\x00" + db + "\x00" + table }
func tagOption(db, name string) string { return "o\x00" + db + "\x00" + name }
func tagAlloptions(db string) string   { return "a\x00" + db }

func key(db, query string) string       { return db + "\x00" + statement.Normalize(query) }
func pairedKey(db, query string) string { return db + "\x00paired\x00" + statement.Normalize(query) }

// Get serves a read from memory, or runs exec and caches what it returns.
//
// When several sessions ask for the same cacheable query at the same time,
// one of them executes it and the others wait for that result: on a cold
// cache the autoloaded options query is asked for by every PHP worker at
// once, and without this they would all ask the database.
func (c *Cache) Get(db, query string, exec func() (*mysql.Result, error)) (*mysql.Result, Outcome, error) {
	pats, rules, ok := c.ready(db)
	if !ok {
		r, err := exec()
		return r, OutcomeExecuted, err
	}
	ttl, tags, source, cacheable := c.classify(pats, rules, db, query)
	if !cacheable {
		r, err := exec()
		return r, OutcomeExecuted, err
	}

	k := key(db, query)
	if r, hit := c.lookup(k); hit {
		return r, OutcomeHit, nil
	}

	c.mu.Lock()
	if existing, running := c.inflight[k]; running {
		c.mu.Unlock()
		select {
		case <-existing.done:
			if existing.err == nil {
				return existing.r, OutcomeShared, nil
			}
			// The leader failed. Falling through to our own attempt would
			// stampede exactly when the database is in trouble, so its
			// error is passed on.
			return nil, OutcomeShared, existing.err
		case <-time.After(sharedWait):
			c.log.Warn("gave up waiting for another session's query", "query", query)
			r, err := exec()
			return r, OutcomeExecuted, err
		}
	}
	leader := &call{done: make(chan struct{})}
	c.inflight[k] = leader
	c.mu.Unlock()

	r, err := exec()
	leader.r, leader.err = r, err
	close(leader.done)

	c.mu.Lock()
	delete(c.inflight, k)
	c.mu.Unlock()

	if err == nil {
		c.store(k, db, query, r, ttl, tags, source, true)
	}
	return r, OutcomeExecuted, err
}

// Warm caches a result the warmer fetched on its own initiative. It is
// neither a hit nor a miss and must not move the counters.
func (c *Cache) Warm(db, query string, r *mysql.Result) {
	pats, rules, ok := c.ready(db)
	if !ok {
		return
	}
	ttl, tags, source, cacheable := c.classify(pats, rules, db, query)
	if !cacheable {
		return
	}
	c.store(key(db, query), db, query, r, ttl, tags, source, false)
}

// lookup returns a cached result, counting the hit.
func (c *Cache) lookup(k string) (*mysql.Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[k]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		c.removeLocked(e)
		return nil, false
	}
	c.lru.MoveToFront(e.elem)
	c.hits++
	if st := c.sources[e.source]; st != nil {
		st.Hits++
	}
	return e.result, true
}

// store caches a result. Reaching store means the query was cacheable but
// not cached: that is what a miss is, and why the ratio only ever describes
// cacheable traffic.
func (c *Cache) store(k, db, query string, r *mysql.Result, ttl time.Duration, tags []string, source string, countMiss bool) {
	if r == nil || !r.HasResultset() {
		return
	}
	size := resultSize(r)
	if size > c.conf().MaxResultBytes {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if countMiss {
		c.misses++
	}
	c.insertLocked(k, r, ttl, tags, source, size)

	// Remember the exact alloptions query so the warmer can replay it.
	if source == sourceAlloptions {
		c.learned[db] = statement.Normalize(query)
	}
}

// insertLocked adds or replaces an entry and enforces both bounds: the
// number of entries and the bytes they hold. Caller holds mu.
func (c *Cache) insertLocked(k string, r *mysql.Result, ttl time.Duration, tags []string, source string, size int) *entry {
	if old, exists := c.entries[k]; exists {
		c.removeLocked(old)
	}
	for c.lru.Len() >= c.conf().MaxEntries {
		c.removeLocked(c.lru.Back().Value.(*entry))
	}

	e := &entry{
		key:     k,
		result:  r,
		expires: time.Now().Add(withJitter(ttl)),
		tags:    tags,
		source:  source,
		bytes:   size,
	}
	e.elem = c.lru.PushFront(e)
	c.entries[k] = e
	c.bytes += size

	st := c.sources[source]
	if st == nil {
		st = &sourceStat{}
		c.sources[source] = st
	}
	st.Entries++
	st.Bytes += size

	for _, tag := range tags {
		set := c.byTag[tag]
		if set == nil {
			set = make(map[*entry]struct{})
			c.byTag[tag] = set
		}
		set[e] = struct{}{}
	}

	// Byte budget: a thousand small entries and a thousand large ones cost
	// very different amounts of memory, so max_entries alone cannot bound it.
	for c.conf().MaxBytes > 0 && c.bytes > c.conf().MaxBytes && c.lru.Len() > 1 {
		c.removeLocked(c.lru.Back().Value.(*entry))
	}
	return e
}

// withJitter spreads expiry times so entries created together do not all
// expire together.
func withJitter(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return ttl
	}
	return ttl + time.Duration(rand.Float64()*ttlJitter*float64(ttl))
}

const (
	sourceAlloptions = "alloptions"
	sourceTransients = "transients"
)

// classify decides whether a SELECT may be cached, for how long, under which
// invalidation tags and which statistics source.
func (c *Cache) classify(pats *patterns, rules []Rule, db, query string) (time.Duration, []string, string, bool) {
	if unsafeForCache(query) {
		return 0, nil, "", false
	}

	if c.conf().AutoloadOptions && pats.autoload.MatchString(query) {
		return c.conf().DefaultTTL.Std(), []string{
			tagAlloptions(db),
			tagTable(db, pats.optionsTable),
		}, sourceAlloptions, true
	}

	if c.conf().Transients {
		if m := pats.option.FindStringSubmatch(query); m != nil {
			if isTransientName(m[1]) {
				return c.conf().DefaultTTL.Std(), []string{
					tagOption(db, m[1]),
					tagTable(db, pats.optionsTable),
				}, sourceTransients, true
			}
			// A non-transient option read one row at a time is left alone:
			// it is already covered by the alloptions snapshot.
			return 0, nil, "", false
		}
		// The batch form WordPress actually emits: a transient and its
		// timeout companion in one IN list. Cached only when every name in
		// it is a transient, tagged per name so any write drops it.
		if m := pats.optionIn.FindStringSubmatch(query); m != nil {
			names := extractQuotedList(m[1])
			if len(names) > 0 {
				tags := make([]string, 0, len(names)+1)
				tags = append(tags, tagTable(db, pats.optionsTable))
				allTransient := true
				for _, n := range names {
					if !isTransientName(n) {
						allTransient = false
						break
					}
					tags = append(tags, tagOption(db, n))
				}
				if allTransient {
					return c.conf().DefaultTTL.Std(), tags, sourceTransients, true
				}
			}
		}
	}

	return c.matchRules(rules, db, query)
}

func (c *Cache) matchRules(rules []Rule, db, query string) (time.Duration, []string, string, bool) {
	for i := range rules {
		r := &rules[i]
		if !r.re.MatchString(query) {
			continue
		}
		ttl := r.TTL.Std()
		if ttl <= 0 {
			ttl = c.conf().DefaultTTL.Std()
		}
		tags := make([]string, 0, len(r.InvalidateOn))
		for _, table := range r.InvalidateOn {
			tags = append(tags, tagTable(db, table))
		}
		return ttl, tags, "rule:" + r.Name, true
	}
	return 0, nil, "", false
}

// InvalidateWrite drops the entries a write statement may have affected.
func (c *Cache) InvalidateWrite(db, query string) {
	pats, _, ok := c.ready(db)
	if !ok {
		return
	}
	table, parsed := extractTable(query)
	if !parsed {
		c.Flush("unparseable write statement")
		return
	}

	c.mu.Lock()
	optionsWrite := table == pats.optionsTable
	hitAutoload := false
	switch {
	case optionsWrite:
		if names := extractOptionNames(query); len(names) > 0 {
			// An attributable options write drops the single-option entries,
			// and the alloptions snapshot only if an autoloaded option can
			// be involved.
			hitAutoload = writeHitsAutoload(names)
			if hitAutoload {
				c.dropTagLocked(tagAlloptions(db))
			}
			for _, name := range names {
				c.dropTagLocked(tagOption(db, name))
			}
		} else {
			hitAutoload = true
			c.dropTagLocked(tagTable(db, table))
		}
	default:
		c.dropTagLocked(tagTable(db, table))
	}
	refetch := c.refetch
	warm, learned := c.learned[db]
	c.mu.Unlock()

	// Only re-warm when this write actually dropped the snapshot, so
	// transient churn no longer triggers pointless refetches.
	if optionsWrite && hitAutoload && learned && refetch != nil {
		refetch(db, warm)
	}
}

// Flush empties the cache and asks the warmer to repopulate the alloptions
// snapshots it has learned.
func (c *Cache) Flush(reason string) {
	c.mu.Lock()
	if n := len(c.entries); n > 0 {
		c.log.Debug("cache flushed", "entries", n, "reason", reason)
	}
	c.entries = make(map[string]*entry)
	c.lru.Init()
	c.byTag = make(map[string]map[*entry]struct{})
	c.bytes = 0
	c.sources = make(map[string]*sourceStat)
	refetch := c.refetch
	warm := make(map[string]string, len(c.learned))
	for db, q := range c.learned {
		warm[db] = q
	}
	c.mu.Unlock()

	if refetch != nil {
		for db, q := range warm {
			refetch(db, q)
		}
	}
}

// Report is a snapshot of the cache for `gora status`.
type Report struct {
	Hits    uint64                `json:"hits"`
	Misses  uint64                `json:"misses"`
	Entries int                   `json:"entries"`
	Bytes   int                   `json:"bytes"`
	Prefix  string                `json:"table_prefix"`
	Sources map[string]sourceStat `json:"sources"`
}

// ReportStats returns the full snapshot, per source included.
func (c *Cache) ReportStats() Report {
	c.mu.Lock()
	defer c.mu.Unlock()

	rep := Report{
		Hits:    c.hits,
		Misses:  c.misses,
		Entries: len(c.entries),
		Bytes:   c.bytes,
		Prefix:  c.Prefix(),
		Sources: make(map[string]sourceStat, len(c.sources)),
	}
	for name, st := range c.sources {
		rep.Sources[name] = *st
	}
	return rep
}

// resultSize approximates the memory a cached result holds. Results read
// from the wire keep every packet in RawPkg; synthetic ones are measured
// from their parts.
func resultSize(r *mysql.Result) int {
	if n := len(r.RawPkg); n > 0 {
		return n
	}
	n := 64
	for _, rd := range r.RowDatas {
		n += len(rd)
	}
	for _, f := range r.Fields {
		if f != nil {
			n += len(f.Data)
		}
	}
	return n
}

func (c *Cache) dropTagLocked(tag string) {
	for e := range c.byTag[tag] {
		c.removeLocked(e)
	}
}

func (c *Cache) removeLocked(e *entry) {
	delete(c.entries, e.key)
	c.lru.Remove(e.elem)
	c.bytes -= e.bytes
	if st := c.sources[e.source]; st != nil {
		st.Entries--
		st.Bytes -= e.bytes
	}
	for _, tag := range e.tags {
		set := c.byTag[tag]
		delete(set, e)
		if len(set) == 0 {
			delete(c.byTag, tag)
		}
	}
}

// String makes the cache printable in startup logs.
func (c *Cache) String() string {
	c.rulesMu.RLock()
	defer c.rulesMu.RUnlock()
	prefix := "(detecting)"
	if c.pats != nil {
		prefix = c.pats.prefix
	}
	return fmt.Sprintf("cache{prefix=%s rules=%d}", prefix, len(c.rawRules))
}
