package profile

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"

	"github.com/ostap-mykhaylyak/gora/internal/statement"
)

// minScanRows is how big a table scan has to be before it is worth an
// index. Under it MySQL is often right to scan, and a suggestion would be
// noise.
const minScanRows = 1000

// maxExplained bounds how many statements one report explains. EXPLAIN is
// cheap but it is not free, and it borrows a pooled connection.
const maxExplained = 10

// suggestIndexes explains the heaviest statements against the real schema.
func (p *Profiler) suggestIndexes(ctx context.Context, stats []Stat) []Advice {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var advice []Advice
	explained := 0
	for _, st := range stats {
		if explained >= maxExplained {
			break
		}
		if st.Database == "" || !isExplainable(st.Sample) {
			continue
		}
		explained++

		row, err := p.explain(ctx, st.Database, st.Sample)
		if err != nil {
			p.log.Debug("could not explain a statement", "error", err, "query", st.Digest)
			continue
		}
		if a, ok := adviseFromExplain(st, row); ok {
			advice = append(advice, a)
		}
	}
	return advice
}

// explainRow is the part of an EXPLAIN answer gora reads.
type explainRow struct {
	Table string
	Type  string
	Key   string
	Rows  uint64
	Extra string
}

// explain runs EXPLAIN on a pooled connection.
func (p *Profiler) explain(ctx context.Context, db, query string) (explainRow, error) {
	var row explainRow

	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return row, err
	}
	if conn.BoundDB != db {
		if err := conn.UseDB(db); err != nil {
			p.pool.Release(conn)
			return row, err
		}
		conn.BoundDB = db
	}
	r, err := conn.Execute("EXPLAIN " + query)
	p.pool.ReleaseClean(conn)
	if err != nil {
		return row, err
	}
	return parseExplain(r)
}

// parseExplain reads the first row of an EXPLAIN result by column name,
// because the column order has changed between MySQL versions and reading
// it positionally is how a tool starts giving confident wrong answers.
func parseExplain(r *mysql.Result) (explainRow, error) {
	var row explainRow
	if r == nil || r.Resultset == nil || len(r.Values) == 0 {
		return row, fmt.Errorf("EXPLAIN returned no rows")
	}

	for i, f := range r.Fields {
		if f == nil {
			continue
		}
		name := strings.ToLower(string(f.Name))
		switch name {
		case "table":
			row.Table, _ = r.GetStringByName(0, string(f.Name))
		case "type":
			row.Type, _ = r.GetStringByName(0, string(f.Name))
		case "key":
			row.Key, _ = r.GetStringByName(0, string(f.Name))
		case "rows":
			row.Rows, _ = r.GetUintByName(0, string(f.Name))
		case "extra":
			row.Extra, _ = r.GetStringByName(0, string(f.Name))
		}
		_ = i
	}
	return row, nil
}

// adviseFromExplain turns one EXPLAIN row into a suggestion, when there is
// one worth making.
func adviseFromExplain(st Stat, row explainRow) (Advice, bool) {
	fullScan := strings.EqualFold(row.Type, "ALL") && row.Key == ""
	if !fullScan || row.Rows < minScanRows {
		return Advice{}, false
	}

	table := row.Table
	if table == "" {
		table, _ = singleTable(st.Digest)
	}
	if table == "" {
		return Advice{}, false
	}

	// A leading-wildcard search cannot be helped by an ordinary index, and
	// suggesting one anyway is the classic useless advice.
	if leadingWildRe.MatchString(st.Digest) {
		if col, ok := likeColumn(st.Digest); ok {
			return fulltextAdvice(st.Database, table, col, st.Digest, st.Calls, row.Rows), true
		}
		return Advice{}, false
	}

	// Columns are only extracted for single-table statements: guessing a
	// composite index across a join is how you end up with an index nothing
	// uses.
	if _, single := singleTable(st.Digest); !single {
		return Advice{
			Kind:     KindIndex,
			Database: st.Database,
			Table:    table,
			Query:    st.Digest,
			Calls:    st.Calls,
			Reason: fmt.Sprintf(
				"this statement scans %s (about %d rows) without using an index, %d times in the interval; "+
					"it joins several tables, so gora does not guess which index to add",
				table, row.Rows, st.Calls),
		}, true
	}

	cols := filterColumns(st.Digest)
	if len(cols) == 0 {
		return Advice{}, false
	}
	return Advice{
		Kind:     KindIndex,
		Database: st.Database,
		Table:    table,
		Query:    st.Digest,
		Calls:    st.Calls,
		Reason: fmt.Sprintf(
			"this statement scans %s (about %d rows) without using an index, %d times in the interval",
			table, row.Rows, st.Calls),
		Apply: fmt.Sprintf("ALTER TABLE %s ADD INDEX idx_gora_%s (%s);",
			table, strings.Join(cols, "_"), strings.Join(cols, ", ")),
	}, true
}

var (
	fromTableRe = regexp.MustCompile("(?i)\\bFROM\\s+`?([\\w$]+)`?")
	joinRe      = regexp.MustCompile(`(?i)\bJOIN\b|,\s*` + "`?" + `[\w$]+` + "`?" + `\s+(?:AS\s+)?[\w$]+\s+WHERE`)
	whereRe     = regexp.MustCompile(`(?is)\bWHERE\b(.*?)(?:\bGROUP\s+BY\b|\bORDER\s+BY\b|\bLIMIT\b|$)`)
	whereColRe  = regexp.MustCompile("(?i)`?([\\w$]+)`?\\s*(?:=|IN\\s*\\(|>=|<=|>|<|LIKE)")
	likeColRe   = regexp.MustCompile("(?i)`?([\\w$]+)`?\\s+LIKE\\s+'?%")
)

// sqlWords are the words that look like columns to a regular expression and
// are not.
var sqlWords = map[string]bool{
	"and": true, "or": true, "not": true, "null": true, "is": true,
	"select": true, "from": true, "where": true, "in": true, "like": true,
	"true": true, "false": true, "case": true, "when": true, "then": true,
	"else": true, "end": true, "as": true, "on": true, "by": true,
}

// isExplainable reports whether EXPLAIN can be run on the statement safely.
// Only reads: EXPLAIN on an UPDATE is valid SQL in MySQL 8, but gora will
// not send a write to the database on its own initiative.
func isExplainable(query string) bool {
	return statement.Classify(query) == statement.KindSelect
}

// singleTable returns the table a statement reads and whether it is the
// only one.
func singleTable(query string) (string, bool) {
	m := fromTableRe.FindStringSubmatch(query)
	if m == nil {
		return "", false
	}
	if joinRe.MatchString(query) || len(fromTableRe.FindAllString(query, -1)) > 1 {
		return m[1], false
	}
	return m[1], true
}

// filterColumns returns up to three columns from the WHERE clause, in the
// order they appear, which is the order a composite index would want them
// in for equality predicates.
func filterColumns(query string) []string {
	m := whereRe.FindStringSubmatch(query)
	if m == nil {
		return nil
	}

	var cols []string
	seen := map[string]bool{}
	for _, match := range whereColRe.FindAllStringSubmatch(m[1], -1) {
		col := match[1]
		if sqlWords[strings.ToLower(col)] || seen[col] {
			continue
		}
		seen[col] = true
		cols = append(cols, col)
		if len(cols) == 3 {
			break
		}
	}
	return cols
}

// likeColumn returns the column a leading-wildcard LIKE is applied to.
func likeColumn(query string) (string, bool) {
	m := likeColRe.FindStringSubmatch(query)
	if m == nil {
		return "", false
	}
	if sqlWords[strings.ToLower(m[1])] {
		return "", false
	}
	return m[1], true
}
