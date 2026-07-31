package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRules(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const goodRules = `
name: woocommerce
rules:
  - name: product-meta
    match: "(?i)^SELECT post_id, meta_key, meta_value FROM {prefix}postmeta"
    ttl: 10m
    invalidate_on: ["{prefix}postmeta"]
`

func TestLoadRuleDir(t *testing.T) {
	dir := writeRules(t, map[string]string{
		"woocommerce.yaml": goodRules,
		"notes.txt":        "ignored",
	})

	rules, err := LoadRuleDir(dir)
	if err != nil {
		t.Fatalf("LoadRuleDir: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("loaded %d rules, want 1", len(rules))
	}
	if rules[0].Name != "product-meta" {
		t.Fatalf("rule name = %q", rules[0].Name)
	}
}

// conf.d is optional, and a missing directory is not a reason to refuse to
// start.
func TestLoadRuleDirMissing(t *testing.T) {
	rules, err := LoadRuleDir(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("LoadRuleDir on a missing directory: %v", err)
	}
	if rules != nil {
		t.Fatalf("got %d rules, want none", len(rules))
	}
}

func TestLoadRuleDirErrors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "no name",
			files: map[string]string{"a.yaml": "rules:\n  - match: \"^SELECT\"\n    invalidate_on: [wp_posts]\n"},
			want:  "no name",
		},
		{
			name:  "no match",
			files: map[string]string{"a.yaml": "rules:\n  - name: x\n    invalidate_on: [wp_posts]\n"},
			want:  "no match expression",
		},
		{
			name:  "no invalidation",
			files: map[string]string{"a.yaml": "rules:\n  - name: x\n    match: \"^SELECT\"\n"},
			want:  "invalidate_on",
		},
		{
			name:  "broken regex",
			files: map[string]string{"a.yaml": "rules:\n  - name: x\n    match: \"^SELECT (\"\n    invalidate_on: [wp_posts]\n"},
			want:  "invalid match expression",
		},
		{
			name:  "unknown key",
			files: map[string]string{"a.yaml": "rules:\n  - name: x\n    matches: \"^SELECT\"\n    invalidate_on: [wp_posts]\n"},
			want:  "matches",
		},
		{
			name: "duplicate rule names",
			files: map[string]string{
				"a.yaml": "rules:\n  - name: x\n    match: \"^SELECT\"\n    invalidate_on: [wp_posts]\n",
				"b.yaml": "rules:\n  - name: x\n    match: \"^SELECT 2\"\n    invalidate_on: [wp_posts]\n",
			},
			want: "already defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadRuleDir(writeRules(t, tt.files))
			if err == nil {
				t.Fatal("an invalid drop-in was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// {prefix} is what makes a drop-in work on an installation whose tables are
// not called wp_something.
func TestCompileRulesExpandsThePrefix(t *testing.T) {
	rules, err := compileRules([]Rule{{
		Name:         "x",
		Match:        `^SELECT \* FROM {prefix}postmeta`,
		InvalidateOn: []string{"{prefix}postmeta"},
	}}, "shop7_")
	if err != nil {
		t.Fatalf("compileRules: %v", err)
	}
	if !rules[0].re.MatchString("SELECT * FROM shop7_postmeta WHERE post_id = 1") {
		t.Fatal("the compiled rule does not match the prefixed table")
	}
	if rules[0].InvalidateOn[0] != "shop7_postmeta" {
		t.Fatalf("invalidate_on = %q, want shop7_postmeta", rules[0].InvalidateOn[0])
	}
}
