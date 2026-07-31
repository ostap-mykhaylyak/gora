package profile

import (
	"fmt"
	"regexp"
)

// Known antipatterns.
//
// These are the four that show up in WordPress installations often enough
// to be worth naming, and they are all things gora can see without asking
// the database anything. Two of them have a safe automatic fix and come
// with the conf.d rewrite ready to paste; the other two do not, and saying
// so is more useful than suggesting something that would change results.

var (
	orderByRandRe   = regexp.MustCompile(`(?i)\bORDER\s+BY\s+RAND\s*\(\s*\)`)
	calcFoundRowsRe = regexp.MustCompile(`(?i)\bSQL_CALC_FOUND_ROWS\b`)
	leadingWildRe   = regexp.MustCompile(`(?i)\bLIKE\s+'%`)
	bigOffsetRe     = regexp.MustCompile(`(?i)\bLIMIT\s+(\d{5,})\s*,|\bOFFSET\s+(\d{5,})\b`)
)

// suggestRewrites scans the interval's heaviest statements for antipatterns.
func suggestRewrites(stats []Stat) []Advice {
	var advice []Advice

	for _, st := range stats {
		q := st.Digest

		switch {
		case orderByRandRe.MatchString(q):
			advice = append(advice, Advice{
				Kind:     KindRewrite,
				Database: st.Database,
				Query:    q,
				Calls:    st.Calls,
				Reason: "ORDER BY RAND() reads and sorts the whole table on every execution; " +
					"removing it changes which rows come back, not how many",
				Apply: "conf.d: rewrites: [{name: drop-order-by-rand, " +
					`match: "(?i)\\s*ORDER\\s+BY\\s+RAND\\s*\\(\\s*\\)", replace: ""}]`,
			})

		case calcFoundRowsRe.MatchString(q):
			advice = append(advice, Advice{
				Kind:     KindRewrite,
				Database: st.Database,
				Query:    q,
				Calls:    st.Calls,
				Reason: "SQL_CALC_FOUND_ROWS is deprecated since MySQL 8.0.17 and costs a full scan; " +
					"on WooCommerce do not remove it, the pagination reads FOUND_ROWS() right after — " +
					"add a cache rule for the listing instead, which caches the rows and the total together",
				Apply: "conf.d: rules: [{name: product-listing, match: " +
					`"(?i)^SELECT SQL_CALC_FOUND_ROWS ...", ttl: 2m, invalidate_on: [...]}]`,
			})

		case leadingWildRe.MatchString(q):
			advice = append(advice, Advice{
				Kind:     KindRewrite,
				Database: st.Database,
				Query:    q,
				Calls:    st.Calls,
				Reason: "a LIKE pattern starting with % cannot use any B-tree index, so this scans the table; " +
					"no rewrite is safe here — a FULLTEXT index or a search plugin is the fix",
			})

		case bigOffsetRe.MatchString(q):
			advice = append(advice, Advice{
				Kind:     KindRewrite,
				Database: st.Database,
				Query:    q,
				Calls:    st.Calls,
				Reason: "a large OFFSET makes MySQL read and discard every row before it; " +
					"paginate by key (WHERE id > last_id) instead — no rewrite can do this for you",
			})
		}
	}
	return advice
}

// fulltextAdvice suggests a FULLTEXT index for a search no B-tree can help.
func fulltextAdvice(db, table, column, query string, calls, rows uint64) Advice {
	apply := fmt.Sprintf("ALTER TABLE %s ADD FULLTEXT INDEX ft_gora_%s (%s);", table, column, column)
	return Advice{
		Kind:     KindFulltext,
		Database: db,
		Table:    table,
		Query:    query,
		Calls:    calls,
		Reason: fmt.Sprintf(
			"this search scans %s (about %d rows) with a leading-wildcard LIKE, which no ordinary index can serve; "+
				"a FULLTEXT index can, and a search plugin (Relevanssi, FiboSearch) can do better still",
			table, rows),
		Apply: apply,
	}
}
