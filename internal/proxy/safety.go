package proxy

import (
	"regexp"
	"strings"
)

// Multiplexing safety.
//
// A backend connection may go back to the pool between two statements only
// if the session left nothing behind on it. This file decides what "nothing"
// means. Every rule errs towards pinning: a false positive costs one
// dedicated connection — the behaviour of any proxy without multiplexing —
// while a false negative hands another client a connection carrying somebody
// else's temporary table, lock or variable.

// pinDetectRe matches constructs that tie a session to its connection for
// good: temporary tables, table and advisory locks, session-scoped
// transaction settings.
var pinDetectRe = regexp.MustCompile(`(?i)\b(?:CREATE\s+TEMPORARY\s+TABLE|LOCK\s+TABLES|GET_LOCK\s*\(|SET\s+TRANSACTION\b)`)

// holdDetectRe matches statements whose companion must run on the same
// connection, normally as the very next statement: the connection is kept
// attached for one more round instead of being pinned forever.
var holdDetectRe = regexp.MustCompile(`(?i)\b(?:SQL_CALC_FOUND_ROWS|FOUND_ROWS\s*\(|LAST_INSERT_ID\s*\(|ROW_COUNT\s*\()`)

// userVarRe matches user-defined variables (@x) while ignoring system ones
// (@@x). It runs on the fingerprinted statement, where literals are already
// collapsed to ?, so an e-mail address in a WHERE clause cannot trigger it.
var userVarRe = regexp.MustCompile(`(^|[^@\w])@[A-Za-z0-9_$.]`)

// Trackable SET statements: session settings gora can replay elsewhere to
// reproduce the environment. Anything else SET-shaped pins.
var (
	setRe          = regexp.MustCompile(`(?i)^\s*SET\b`)
	trackableSetRe = regexp.MustCompile(`(?i)^\s*SET\s+(?:NAMES\b|(?:SESSION\s+|@@SESSION\.)?\s*(?:character_set_\w+|collation_\w+|sql_mode|time_zone|wait_timeout|group_concat_max_len|sql_big_selects|net_read_timeout|net_write_timeout)\s*=)`)
	setGlobalRe    = regexp.MustCompile(`(?i)^\s*SET\s+(?:GLOBAL\b|@@GLOBAL\.)`)
)

// setAction describes how a SET statement affects multiplexing.
type setAction int

const (
	setNone   setAction = iota // not a SET statement
	setTrack                   // session setting, replayed on reuse
	setIgnore                  // server-wide, leaves no session state
	setPin                     // untrackable: pin the session
)

func classifySet(query string) setAction {
	if !setRe.MatchString(query) {
		return setNone
	}
	if trackableSetRe.MatchString(query) {
		return setTrack
	}
	if setGlobalRe.MatchString(query) {
		return setIgnore
	}
	return setPin
}

// varSignature identifies a replayable session environment: the ordered,
// deduplicated list of tracked SET statements. Two sessions with the same
// signature can share a connection without any resynchronisation, which in
// a WordPress fleet is the normal case — everyone sends the same SET NAMES.
func varSignature(statements []string) string {
	return strings.Join(statements, "\x00")
}
