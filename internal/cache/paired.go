package cache

import (
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Paginated listings.
//
// WordPress asks for a page of results and its total in two statements:
// SELECT SQL_CALC_FOUND_ROWS ... LIMIT 10 followed by SELECT FOUND_ROWS().
// The second one reads a counter MySQL left on the connection, so it cannot
// be cached on its own, and the first one is useless without it.
//
// gora caches the two together. The rows are stored when the listing runs,
// and the entry stays unserved until the count that belongs with it has
// been captured — serving rows with somebody else's count would silently
// corrupt "page X of Y" on a shop that looks perfectly healthy.

// PairedCacheable reports whether a SQL_CALC_FOUND_ROWS listing matches a
// conf.d rule, so the session knows whether to run the pairing dance.
func (c *Cache) PairedCacheable(db, query string) bool {
	_, _, _, ok := c.classifyPaired(db, query)
	return ok
}

// classifyPaired decides whether a listing may be cached. The built-in
// options and transient patterns never apply here: only conf.d rules do,
// because only the person writing the rule knows the listing is the same
// for every visitor.
func (c *Cache) classifyPaired(db, query string) (time.Duration, []string, string, bool) {
	if unsafeForPairing(query) {
		return 0, nil, "", false
	}
	_, rules, ok := c.ready(db)
	if !ok {
		return 0, nil, "", false
	}
	return c.matchRules(rules, db, query)
}

// LookupPaired returns the cached rows of a listing together with the
// FOUND_ROWS() count that belongs with them. ok is true only when both are
// present.
func (c *Cache) LookupPaired(db, query string) (*mysql.Result, uint64, bool) {
	k := pairedKey(db, query)

	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[k]
	if !ok || !e.hasFoundRows {
		return nil, 0, false
	}
	if time.Now().After(e.expires) {
		c.removeLocked(e)
		return nil, 0, false
	}
	c.lru.MoveToFront(e.elem)
	c.hits++
	if st := c.sources[e.source]; st != nil {
		st.Hits++
	}
	return e.result, e.foundRows, true
}

// StorePaired caches the rows of a listing. The count is filled in later by
// PairFoundRows, and until then LookupPaired refuses to serve the entry.
func (c *Cache) StorePaired(db, query string, r *mysql.Result) {
	if r == nil || !r.HasResultset() {
		return
	}
	ttl, tags, source, ok := c.classifyPaired(db, query)
	if !ok {
		return
	}
	size := resultSize(r)
	if size > c.cfg.MaxResultBytes {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.misses++
	c.insertLocked(pairedKey(db, query), r, ttl, tags, source, size)
}

// PairFoundRows records the count for a listing stored a moment ago,
// completing the entry so it can be served.
func (c *Cache) PairFoundRows(db, query string, foundRows uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[pairedKey(db, query)]; ok {
		e.foundRows = foundRows
		e.hasFoundRows = true
	}
}

// FoundRowsResult builds the synthetic answer to SELECT FOUND_ROWS() that
// goes with a cached listing.
func FoundRowsResult(n uint64) *mysql.Result {
	rs, err := mysql.BuildSimpleTextResultset([]string{"FOUND_ROWS()"}, [][]any{{int64(n)}})
	if err != nil {
		return &mysql.Result{}
	}
	return mysql.NewResult(rs)
}
