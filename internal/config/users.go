package config

import (
	"fmt"
	"strings"
)

// The users list is edited by hand as well, because a list is not a value
// and "set users to something" is not what anybody wants to say. Adding an
// account is adding two lines; removing one is removing the lines it
// occupies. Everything around them is left alone, comments included — and
// in this file the comment above `users:` is the paragraph explaining that
// MySQL never sees these passwords.

// AddUser returns data with an account appended to the users list, creating
// the list if the file does not have one yet.
func AddUser(data []byte, username, password string) ([]byte, error) {
	if username == "" {
		return nil, fmt.Errorf("a user needs a name")
	}
	lines := splitLines(data)

	idx := findKey(lines, 0, len(lines), 0, "users")
	entry := []string{
		fmt.Sprintf("  - username: %s", renderValue(username)),
		fmt.Sprintf("    password: %s", renderValue(password)),
	}

	if idx < 0 {
		// No list at all: start one at the end of the file, after a blank
		// line so it does not run into whatever section is last.
		block := append([]string{"", "users:"}, entry...)
		lines = append(trimTrailingBlank(lines), block...)
		lines = append(lines, "")
		return joinLines(lines), nil
	}

	// An empty list written as `users: []` has to become a block before
	// anything can be appended to it.
	if valueOf(lines[idx]) == "[]" {
		line, err := setLineValue(lines[idx], "")
		if err != nil {
			return nil, err
		}
		lines[idx] = strings.TrimRight(line, " ")
	}

	start, end := blockRange(lines, idx, 0)
	at := listInsertPoint(lines, start, end)
	lines = append(lines[:at], append(entry, lines[at:]...)...)
	return joinLines(lines), nil
}

// RemoveUser returns data without the account named, and without the lines
// that belonged to it.
func RemoveUser(data []byte, username string) ([]byte, error) {
	lines := splitLines(data)
	idx := findKey(lines, 0, len(lines), 0, "users")
	if idx < 0 {
		return data, nil
	}
	start, end := blockRange(lines, idx, 0)

	itemStart := -1
	for i := start; i < end && i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		if itemStart >= 0 {
			// The item after the one we are removing: stop here.
			if named(lines[itemStart:i], username) {
				return joinLines(append(lines[:itemStart], lines[i:]...)), nil
			}
		}
		itemStart = i
	}
	if itemStart >= 0 && named(lines[itemStart:end], username) {
		return joinLines(append(lines[:itemStart], lines[end:]...)), nil
	}
	return data, nil
}

// named reports whether a list item is the account looked for.
func named(item []string, username string) bool {
	for _, line := range item {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok || strings.TrimSpace(key) != "username" {
			continue
		}
		value, _ = splitComment(value)
		return unquote(strings.TrimSpace(value)) == username
	}
	return false
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// listInsertPoint returns the line a new item should go on: after the last
// line of the list, before whatever follows it.
func listInsertPoint(lines []string, start, end int) int {
	at := start
	for i := start; i < end && i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		at = i + 1
	}
	if at > len(lines) {
		at = len(lines)
	}
	return at
}

// trimTrailingBlank drops the blank lines at the end of a file, so
// appending a section does not leave a gap growing with every edit.
func trimTrailingBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
