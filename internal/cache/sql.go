package cache

import (
	"regexp"
	"strings"

	"github.com/ostap-mykhaylyak/gora/internal/statement"
)

// patterns holds the WordPress-specific regexes, compiled for one table
// prefix. They match the SQL wpdb actually emits, which is why they are
// this literal: the goal is to recognise WordPress's own queries with
// certainty, not to parse SQL in general.
type patterns struct {
	prefix       string
	optionsTable string

	// autoload: SELECT option_name, option_value FROM {p}options WHERE autoload...
	autoload *regexp.Regexp
	// single option: SELECT option_value FROM {p}options WHERE option_name = '..' LIMIT 1
	option *regexp.Regexp
	// batch read: SELECT option_name, option_value FROM {p}options
	//             WHERE option_name IN ('_transient_x','_transient_timeout_x')
	// This is the form WordPress uses for transients; the single-row form
	// above is comparatively rare.
	optionIn *regexp.Regexp
}

func newPatterns(prefix string) *patterns {
	quoted := regexp.QuoteMeta(prefix)
	return &patterns{
		prefix:       prefix,
		optionsTable: prefix + "options",
		autoload: regexp.MustCompile(
			`(?i)^SELECT\s+option_name\s*,\s*option_value\s+FROM\s+` + quoted + `options\s+WHERE\s+autoload\b`),
		option: regexp.MustCompile(
			`(?i)^SELECT\s+option_value\s+FROM\s+` + quoted + `options\s+WHERE\s+option_name\s*=\s*'([^'\\]+)'\s+LIMIT\s+1\s*$`),
		optionIn: regexp.MustCompile(
			`(?i)^SELECT\s+option_name\s*,\s*option_value\s+FROM\s+` + quoted + `options\s+WHERE\s+option_name\s+IN\s*\(([^)]*)\)\s*$`),
	}
}

// Write-statement table extraction. Only plain single-table statements are
// recognised; anything else (multi-table UPDATEs, CALL, LOAD DATA) makes the
// caller flush instead of guessing.
var (
	insertTableRe = regexp.MustCompile(`(?i)^(?:INSERT|REPLACE)(?:\s+(?:LOW_PRIORITY|DELAYED|HIGH_PRIORITY|IGNORE))*\s+INTO\s+` + "`?" + `([\w$]+)` + "`?")
	updateTableRe = regexp.MustCompile(`(?i)^UPDATE\s+(?:LOW_PRIORITY\s+|IGNORE\s+)*` + "`?" + `([\w$]+)` + "`?" + `\s+SET\b`)
	deleteTableRe = regexp.MustCompile(`(?i)^DELETE\s+FROM\s+` + "`?" + `([\w$]+)` + "`?" + `(?:\s+WHERE\b|\s*$)`)
	ddlTableRe    = regexp.MustCompile(`(?i)^(?:TRUNCATE(?:\s+TABLE)?|DROP\s+TABLE(?:\s+IF\s+EXISTS)?|ALTER\s+TABLE|CREATE\s+TABLE(?:\s+IF\s+NOT\s+EXISTS)?)\s+` + "`?" + `([\w$]+)` + "`?")
)

// extractTable returns the single table a write statement touches. ok is
// false when the target cannot be determined safely.
func extractTable(query string) (string, bool) {
	q := statement.StripLeading(query)
	for _, re := range []*regexp.Regexp{insertTableRe, updateTableRe, deleteTableRe, ddlTableRe} {
		if m := re.FindStringSubmatch(q); m != nil {
			return m[1], true
		}
	}
	return "", false
}

// Option-name extraction for writes on the options table, matching the SQL
// wpdb generates for update_option, set_transient and delete_option.
var (
	optionNameEqRe     = regexp.MustCompile(`(?i)option_name\s*=\s*'([^'\\]*)'`)
	insertOptionNameRe = regexp.MustCompile(`(?i)\(\s*` + "`?" + `option_name` + "`?" + `\s*,[^)]*\)\s*VALUES\s*\(\s*'([^'\\]*)'`)
	quotedItemRe       = regexp.MustCompile(`'([^'\\]*)'`)
)

// extractOptionNames returns the option names a write on the options table
// refers to; empty means "could not tell".
func extractOptionNames(query string) []string {
	var names []string
	for _, m := range optionNameEqRe.FindAllStringSubmatch(query, -1) {
		names = append(names, m[1])
	}
	if m := insertOptionNameRe.FindStringSubmatch(query); m != nil {
		names = append(names, m[1])
	}
	return names
}

// extractQuotedList returns the single-quoted items of an IN (...) list.
func extractQuotedList(list string) []string {
	var names []string
	for _, m := range quotedItemRe.FindAllStringSubmatch(list, -1) {
		names = append(names, m[1])
	}
	return names
}

// isTransientName reports whether an option holds a WordPress transient.
// It also covers the _transient_timeout_* companion rows, which share the
// prefix.
func isTransientName(name string) bool {
	return strings.HasPrefix(name, "_transient_") ||
		strings.HasPrefix(name, "_site_transient_")
}

// writeHitsAutoload reports whether a write on the options table can affect
// an autoloaded option, and therefore the alloptions snapshot.
//
// Writes that only touch transients are treated as not affecting it:
// WordPress stores expiring transients with autoload='off', and WooCommerce
// writes those constantly. Invalidating the single hottest entry in the
// cache on every one of them is both unnecessary and ruinous for the hit
// rate. A write gora cannot attribute to any name counts as hitting
// autoload, because staying correct beats staying warm.
func writeHitsAutoload(names []string) bool {
	if len(names) == 0 {
		return true
	}
	for _, n := range names {
		if !isTransientName(n) {
			return true
		}
	}
	return false
}

// volatileSelectRe matches reads that must never be served from memory:
// locking reads, and anything whose answer depends on the session, the
// connection or the clock.
var volatileSelectRe = regexp.MustCompile(`(?i)\b(?:FOR\s+UPDATE|LOCK\s+IN\s+SHARE\s+MODE|LAST_INSERT_ID\s*\(|FOUND_ROWS\s*\(|ROW_COUNT\s*\(|CONNECTION_ID\s*\(|RAND\s*\(|UUID\w*\s*\(|NOW\s*\(|SYSDATE\s*\(|CURDATE\s*\(|CURTIME\s*\(|CURRENT_\w+|GET_LOCK\s*\(|SLEEP\s*\(|BENCHMARK\s*\()`)

var (
	calcFoundRowsRe  = regexp.MustCompile(`(?i)\bSQL_CALC_FOUND_ROWS\b`)
	foundRowsQueryRe = regexp.MustCompile(`(?i)^\s*SELECT\s+FOUND_ROWS\s*\(\s*\)\s*$`)
)

// unsafeForCache reports whether a query cannot go through the normal cache
// path. SQL_CALC_FOUND_ROWS queries are excluded here on purpose: they can
// only be cached through the paired path, which also caches the FOUND_ROWS()
// that follows them.
func unsafeForCache(query string) bool {
	return volatileSelectRe.MatchString(query) || calcFoundRowsRe.MatchString(query)
}

// unsafeForPairing reports whether a SQL_CALC_FOUND_ROWS query is
// uncacheable even on the paired path, because it carries something else
// volatile.
func unsafeForPairing(query string) bool {
	return volatileSelectRe.MatchString(query)
}

// HasCalcFoundRows reports whether the query asks MySQL to count the rows it
// would have returned without the LIMIT.
func HasCalcFoundRows(query string) bool { return calcFoundRowsRe.MatchString(query) }

// IsFoundRowsQuery reports whether the query is exactly SELECT FOUND_ROWS().
func IsFoundRowsQuery(query string) bool { return foundRowsQueryRe.MatchString(query) }
