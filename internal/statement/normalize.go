package statement

import "strings"

// Normalize returns the statement with leading comments removed and every
// run of whitespace outside string literals collapsed to a single space.
//
// It is what gora keys the cache on. Two queries that differ only by
// indentation are the same query, and WordPress plugins produce plenty of
// those: without this, a cache entry is a cache entry per formatting style.
//
// Literals are copied through untouched, which is the whole difficulty.
// Collapsing whitespace inside them would make WHERE post_title = 'a  b'
// and WHERE post_title = 'a b' share one entry, and the cache would start
// answering with the wrong rows.
func Normalize(query string) string {
	q := StripLeading(query)

	var b strings.Builder
	b.Grow(len(q))

	pendingSpace := false
	for i := 0; i < len(q); {
		ch := q[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n':
			i++
			if b.Len() > 0 {
				pendingSpace = true
			}
		case ch == '\'' || ch == '"' || ch == '`':
			end := skipQuoted(q, i)
			if pendingSpace {
				b.WriteByte(' ')
				pendingSpace = false
			}
			b.WriteString(q[i:end])
			i = end
		default:
			if pendingSpace {
				b.WriteByte(' ')
				pendingSpace = false
			}
			b.WriteByte(ch)
			i++
		}
	}
	return b.String()
}

// skipQuoted returns the index just past the quoted run starting at start,
// which may be a string literal or a backquoted identifier.
func skipQuoted(q string, start int) int {
	quote := q[start]
	i := start + 1
	for i < len(q) {
		switch q[i] {
		case '\\':
			if quote == '`' {
				i++ // backslash is not an escape inside identifiers
				continue
			}
			i += 2
		case quote:
			if i+1 < len(q) && q[i+1] == quote {
				i += 2 // a doubled quote stands for the quote itself
				continue
			}
			return i + 1
		default:
			i++
		}
	}
	return i
}
