// Package statement classifies SQL text without parsing it.
//
// gora needs to know a few things about every statement it forwards — does
// it read or write, does it open or close a transaction, does it leave
// state on the connection — and it needs to know them at the speed of a
// keyword comparison, on the hot path of every query. A real parser would
// answer more questions than gora asks and cost more than the proxying
// itself.
//
// The package is a leaf on purpose: the pool, the proxy and (later) the
// cache and the profiler all classify statements, and none of them should
// have to import each other to do it.
package statement

import (
	"regexp"
	"strings"
)

// Kind is what a statement does, as far as gora cares.
type Kind int

const (
	// KindOther is anything gora neither tracks nor reasons about.
	KindOther Kind = iota
	// KindSelect is a plain read.
	KindSelect
	// KindWrite is any statement that may change data (DML, DDL, CALL).
	KindWrite
	// KindBegin opens a transaction.
	KindBegin
	// KindCommit commits one.
	KindCommit
	// KindRollback rolls one back.
	KindRollback
	// KindUnsafe changes session semantics gora cannot follow (autocommit,
	// XA). Whoever sees it should stop making assumptions about the session.
	KindUnsafe
)

// String makes Kind readable in logs and test failures.
func (k Kind) String() string {
	switch k {
	case KindSelect:
		return "select"
	case KindWrite:
		return "write"
	case KindBegin:
		return "begin"
	case KindCommit:
		return "commit"
	case KindRollback:
		return "rollback"
	case KindUnsafe:
		return "unsafe"
	default:
		return "other"
	}
}

var autocommitRe = regexp.MustCompile(`(?i)\bautocommit\b`)

// Classify inspects the leading keyword of a statement.
func Classify(query string) Kind {
	q := StripLeading(query)
	word, rest := FirstWord(q)

	switch strings.ToUpper(word) {
	case "SELECT", "TABLE", "VALUES":
		// TABLE and VALUES are the MySQL 8 read forms. WITH is deliberately
		// absent: a CTE can end in a write, so it stays unclassified.
		return KindSelect
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "TRUNCATE",
		"ALTER", "DROP", "CREATE", "RENAME", "LOAD", "CALL", "GRANT", "REVOKE":
		return KindWrite
	case "BEGIN":
		return KindBegin
	case "START":
		if next, _ := FirstWord(rest); strings.EqualFold(next, "TRANSACTION") {
			return KindBegin
		}
		return KindOther
	case "COMMIT":
		return KindCommit
	case "ROLLBACK":
		// ROLLBACK TO SAVEPOINT leaves the transaction open.
		if next, _ := FirstWord(rest); strings.EqualFold(next, "TO") {
			return KindOther
		}
		return KindRollback
	case "SET":
		if autocommitRe.MatchString(rest) {
			return KindUnsafe
		}
		return KindOther
	case "XA":
		return KindUnsafe
	default:
		return KindOther
	}
}

// StripLeading removes whitespace and /* ... */ comments from the front of a
// statement. WordPress and its plugins prefix queries with comments often
// enough (query monitors, index hints) that ignoring them is not optional.
func StripLeading(q string) string {
	for {
		q = strings.TrimLeft(q, " \t\r\n")
		if strings.HasPrefix(q, "/*") {
			end := strings.Index(q, "*/")
			if end < 0 {
				return ""
			}
			q = q[end+2:]
			continue
		}
		return q
	}
}

// FirstWord returns the leading identifier and everything after it.
func FirstWord(q string) (string, string) {
	q = strings.TrimLeft(q, " \t\r\n")
	i := 0
	for i < len(q) {
		ch := q[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' {
			i++
			continue
		}
		break
	}
	return q[:i], q[i:]
}
