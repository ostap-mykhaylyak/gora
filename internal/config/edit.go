package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Editing configuration files.
//
// gora edits YAML by finding the line a setting lives on and changing that
// line, rather than by decoding the file and writing it back out. A
// round-trip through a YAML library returns a file that parses the same and
// reads differently: the blank lines are gone, the comments have moved, and
// the template that explained what every setting does now explains it in
// one dense block.
//
// The configuration is a hand-written file people read. Everything not
// being changed comes out byte for byte as it went in.

// indentStep is how far one level of nesting is indented in the files gora
// writes. It only matters when a key has to be created.
const indentStep = 2

// SetValue returns data with path — "cache.default_ttl" — set to value,
// creating the key, and any section above it, if they are missing.
func SetValue(data []byte, path, value string) ([]byte, error) {
	parts := strings.Split(path, ".")
	if path == "" || len(parts) == 0 {
		return nil, fmt.Errorf("no setting named")
	}

	lines := splitLines(data)
	rendered := renderValue(value)

	// Walk down the path, one level at a time, inside the block the
	// previous level opened.
	start, end, indent := 0, len(lines), 0
	for i, part := range parts {
		idx := findKey(lines, start, end, indent, part)
		last := i == len(parts)-1

		if idx < 0 {
			// The rest of the path does not exist: write it out from here.
			insert := buildBranch(parts[i:], rendered, indent)
			at := insertPoint(lines, start, end, indent)
			lines = append(lines[:at], append(insert, lines[at:]...)...)
			return joinLines(lines), nil
		}
		if last {
			line, err := setLineValue(lines[idx], rendered)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			lines[idx] = line
			return joinLines(lines), nil
		}

		// Descend into the block this key opens.
		if v := valueOf(lines[idx]); v != "" && !strings.HasPrefix(v, "#") {
			return nil, fmt.Errorf("%s: %s is a value, not a section", path, strings.Join(parts[:i+1], "."))
		}
		start, end = blockRange(lines, idx, indent)
		indent += indentStep
	}
	return joinLines(lines), nil
}

// DeleteValue returns data with the line path lives on removed. A setting
// that is not there is not an error: the point of removing it is that it
// should not be there.
func DeleteValue(data []byte, path string) ([]byte, error) {
	parts := strings.Split(path, ".")
	lines := splitLines(data)

	start, end, indent := 0, len(lines), 0
	for i, part := range parts {
		idx := findKey(lines, start, end, indent, part)
		if idx < 0 {
			return data, nil
		}
		if i == len(parts)-1 {
			return joinLines(append(lines[:idx], lines[idx+1:]...)), nil
		}
		start, end = blockRange(lines, idx, indent)
		indent += indentStep
	}
	return data, nil
}

// --- the line model ---

var keyRe = regexp.MustCompile(`^(\s*)([A-Za-z0-9_-]+)\s*:(.*)$`)

func splitLines(data []byte) []string {
	s := string(data)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	// A trailing newline produces an empty last element; keep it so the
	// file ends the way it started.
	return lines
}

func joinLines(lines []string) []byte { return []byte(strings.Join(lines, "\n")) }

// keyOf returns the key a line defines and its indentation, or ok = false
// when the line is blank, a comment, or a list item.
func keyOf(line string) (key string, indent int, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
		return "", 0, false
	}
	m := keyRe.FindStringSubmatch(line)
	if m == nil {
		return "", 0, false
	}
	return m[2], len(m[1]), true
}

// valueOf returns what follows the colon, without the comment.
func valueOf(line string) string {
	m := keyRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	value, _ := splitComment(m[3])
	return strings.TrimSpace(value)
}

// splitComment separates a value from the comment after it, which has to
// survive the edit: it is usually the sentence explaining the setting.
func splitComment(s string) (value, comment string) {
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			}
		case c == '\'' || c == '"':
			inQuote = c
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i], s[i:]
		}
	}
	return s, ""
}

// findKey returns the line index of key at the given indentation, within
// [start, end).
func findKey(lines []string, start, end, indent int, key string) int {
	for i := start; i < end && i < len(lines); i++ {
		k, ind, ok := keyOf(lines[i])
		if !ok {
			continue
		}
		if ind < indent {
			// Left the block without finding it.
			return -1
		}
		if ind == indent && k == key {
			return i
		}
	}
	return -1
}

// blockRange returns the lines belonging to the key at idx: everything
// after it until something is indented at or above the key's own level.
func blockRange(lines []string, idx, indent int) (start, end int) {
	start = idx + 1
	for i := start; i < len(lines); i++ {
		k, ind, ok := keyOf(lines[i])
		if !ok {
			continue
		}
		if ind <= indent {
			return start, i
		}
		_ = k
	}
	return start, len(lines)
}

// insertPoint returns where a new key should go inside a block: after its
// last real line, so a new setting lands with its section rather than after
// the blank line that separates it from the next one.
func insertPoint(lines []string, start, end, indent int) int {
	at := start
	for i := start; i < end && i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if k, ind, ok := keyOf(lines[i]); ok && ind < indent {
			_ = k
			break
		}
		at = i + 1
	}
	if at > len(lines) {
		at = len(lines)
	}
	return at
}

// buildBranch writes out a path that does not exist yet.
func buildBranch(parts []string, value string, indent int) []string {
	out := make([]string, 0, len(parts))
	for i, part := range parts {
		pad := strings.Repeat(" ", indent+i*indentStep)
		if i == len(parts)-1 {
			out = append(out, fmt.Sprintf("%s%s: %s", pad, part, value))
		} else {
			out = append(out, fmt.Sprintf("%s%s:", pad, part))
		}
	}
	return out
}

// setLineValue replaces the value on a line, keeping its indentation and
// whatever comment followed it.
func setLineValue(line, value string) (string, error) {
	m := keyRe.FindStringSubmatch(line)
	if m == nil {
		return "", fmt.Errorf("cannot edit the line %q", line)
	}
	old, comment := splitComment(m[3])
	if strings.TrimSpace(old) == "" && comment == "" && strings.HasPrefix(strings.TrimSpace(m[3]), "-") {
		return "", fmt.Errorf("it is a list, not a single value")
	}

	head := fmt.Sprintf("%s%s: %s", m[1], m[2], value)
	if comment == "" {
		return head, nil
	}
	// Keep the comment in the column it was in. A file where the
	// explanations line up is a file somebody lined up, and an edit is no
	// reason to undo that.
	column := len(m[1]) + len(m[2]) + 1 + len(old)
	pad := column - len(head)
	if pad < 1 {
		pad = 1
	}
	return head + strings.Repeat(" ", pad) + strings.TrimSpace(comment), nil
}

// bareValue matches what can be written without quotes. Anything else is
// quoted, which is why an address ends up in quotes: a colon in a plain
// scalar is where YAML files start meaning something other than they look.
var bareValue = regexp.MustCompile(`^[A-Za-z0-9_./+-]+$`)

func renderValue(value string) string {
	if value == "" {
		return `""`
	}
	if value == "[]" || value == "{}" {
		return value
	}
	if bareValue.MatchString(value) {
		return value
	}
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
