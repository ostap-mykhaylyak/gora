package confd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const everySection = `
name: example
rules:
  - name: product-meta
    match: "(?i)^SELECT post_id FROM {prefix}postmeta"
    ttl: 10m
    invalidate_on: ["{prefix}postmeta"]
rewrites:
  - name: drop-order-by-rand
    match: "(?i)\\s*ORDER\\s+BY\\s+RAND\\s*\\(\\s*\\)"
    replace: ""
block:
  - name: no-truncate
    match: "(?i)^TRUNCATE"
    message: "truncate is not allowed here"
throttle:
  - name: heavy-search
    match: "(?i)LIKE '%"
    max_concurrent: 2
    wait: 1s
`

func TestLoadEverySection(t *testing.T) {
	set, err := Load(write(t, map[string]string{"example.yaml": everySection, "notes.txt": "ignored"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Cache) != 1 || len(set.Rewrites) != 1 || len(set.Blocks) != 1 || len(set.Throttles) != 1 {
		t.Fatalf("loaded %+v, want one rule in each section", set)
	}
	if set.Len() != 4 {
		t.Fatalf("Len = %d, want 4", set.Len())
	}
	if set.Throttles[0].MaxConcurrent != 2 {
		t.Fatalf("max_concurrent = %d, want 2", set.Throttles[0].MaxConcurrent)
	}
}

// conf.d is optional, and a missing directory is not a reason to refuse to
// start.
func TestLoadMissingDirectory(t *testing.T) {
	set, err := Load(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("Load on a missing directory: %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("got %d rules, want none", set.Len())
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "cache rule without invalidation",
			files: map[string]string{"a.yaml": "rules:\n  - name: x\n    match: \"^SELECT\"\n"},
			want:  "invalidate_on",
		},
		{
			name:  "rewrite without a match",
			files: map[string]string{"a.yaml": "rewrites:\n  - name: x\n    replace: \"\"\n"},
			want:  "no match expression",
		},
		{
			name:  "block with a broken expression",
			files: map[string]string{"a.yaml": "block:\n  - name: x\n    match: \"^SELECT (\"\n"},
			want:  "invalid match expression",
		},
		{
			name:  "throttle without a limit",
			files: map[string]string{"a.yaml": "throttle:\n  - name: x\n    match: \"^SELECT\"\n"},
			want:  "max_concurrent",
		},
		{
			name:  "unknown key",
			files: map[string]string{"a.yaml": "rules:\n  - name: x\n    matches: \"^SELECT\"\n    invalidate_on: [wp_posts]\n"},
			want:  "matches",
		},
		{
			name:  "unknown section",
			files: map[string]string{"a.yaml": "blocks:\n  - name: x\n    match: \"^SELECT\"\n"},
			want:  "blocks",
		},
		{
			name: "duplicate names in one section",
			files: map[string]string{
				"a.yaml": "block:\n  - name: x\n    match: \"^SELECT\"\n",
				"b.yaml": "block:\n  - name: x\n    match: \"^DELETE\"\n",
			},
			want: "already defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(t, tt.files))
			if err == nil {
				t.Fatal("an invalid drop-in was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// The sections are separate namespaces: a cache rule and a block rule may
// well be two halves of the same idea and deserve the same name.
func TestNamesAreUniquePerSection(t *testing.T) {
	files := map[string]string{"a.yaml": `
rules:
  - name: heavy-search
    match: "(?i)^SELECT"
    invalidate_on: [wp_posts]
throttle:
  - name: heavy-search
    match: "(?i)^SELECT"
    max_concurrent: 2
`}
	if _, err := Load(write(t, files)); err != nil {
		t.Fatalf("the same name in two sections was refused: %v", err)
	}
}

// Files are read in filename order, which is what makes 10-base.yaml and
// 20-overrides.yaml behave the way the numbers suggest.
func TestFilesAreReadInOrder(t *testing.T) {
	files := map[string]string{
		"20-second.yaml": "block:\n  - name: second\n    match: \"^DELETE\"\n",
		"10-first.yaml":  "block:\n  - name: first\n    match: \"^SELECT\"\n",
	}
	set, err := Load(write(t, files))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if set.Blocks[0].Name != "first" || set.Blocks[1].Name != "second" {
		t.Fatalf("rules came back as %q, %q", set.Blocks[0].Name, set.Blocks[1].Name)
	}
}
