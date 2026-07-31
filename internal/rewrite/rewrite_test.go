package rewrite

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

func newRewriter(t *testing.T, rules []Rule, prefix string) *Rewriter {
	t.Helper()
	r, err := New(rules, prefix, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// The rewrite everyone eventually needs: a plugin sorting a whole table at
// random on every pageload.
func TestRewriteRemovesOrderByRand(t *testing.T) {
	r := newRewriter(t, []Rule{{
		Name:  "drop-order-by-rand",
		Match: `(?i)\s*ORDER\s+BY\s+RAND\s*\(\s*\)`,
	}}, "wp_")

	got, applied := r.Apply("SELECT ID FROM wp_posts ORDER BY RAND() LIMIT 5")
	if want := "SELECT ID FROM wp_posts LIMIT 5"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if len(applied) != 1 || applied[0] != "drop-order-by-rand" {
		t.Fatalf("applied = %v, want the rule name", applied)
	}
}

func TestRewriteWithCaptures(t *testing.T) {
	r := newRewriter(t, []Rule{{
		Name:    "limit-guard",
		Match:   `(?i)LIMIT\s+(\d+),\s*\d+`,
		Replace: "LIMIT $1, 20",
	}}, "wp_")

	got, _ := r.Apply("SELECT ID FROM wp_posts LIMIT 40, 500")
	if want := "SELECT ID FROM wp_posts LIMIT 40, 20"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A statement nothing matches must come back untouched, and cheaply: this
// runs on every query.
func TestRewriteLeavesOtherStatementsAlone(t *testing.T) {
	r := newRewriter(t, []Rule{{Name: "x", Match: `(?i)ORDER BY RAND\(\)`}}, "wp_")

	query := "SELECT ID FROM wp_posts WHERE post_status = 'publish'"
	got, applied := r.Apply(query)
	if got != query {
		t.Fatalf("got %q, want it unchanged", got)
	}
	if applied != nil {
		t.Fatalf("applied = %v, want none", applied)
	}
}

func TestRewriteWithoutRules(t *testing.T) {
	r := newRewriter(t, nil, "wp_")
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0", r.Len())
	}
	query := "SELECT 1"
	if got, applied := r.Apply(query); got != query || applied != nil {
		t.Fatalf("Apply changed a statement with no rules configured: %q %v", got, applied)
	}
}

func TestRewriteExpandsThePrefix(t *testing.T) {
	r := newRewriter(t, []Rule{{
		Name:    "meta",
		Match:   `{prefix}postmeta`,
		Replace: "shop7_postmeta",
	}}, "shop7_")

	got, applied := r.Apply("SELECT * FROM shop7_postmeta")
	if len(applied) != 1 {
		t.Fatalf("the rule did not match the prefixed table: %q", got)
	}
}

// A rule using {prefix} while the prefix is discovered at runtime would
// never fire, and would look like a rule that simply does not work.
func TestRewriteRefusesPlaceholderWithAutoPrefix(t *testing.T) {
	_, err := New([]Rule{{Name: "x", Match: `{prefix}postmeta`}}, config.AutoPrefix, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("a placeholder was accepted with an automatic prefix")
	}
	if !strings.Contains(err.Error(), "cache.table_prefix") {
		t.Fatalf("error %q does not say how to fix it", err)
	}
}

// A failed reload must leave the rules that were working in place.
func TestSetRulesKeepsTheOldRulesOnError(t *testing.T) {
	r := newRewriter(t, []Rule{{Name: "keep", Match: `(?i)ORDER BY RAND\(\)`}}, "wp_")

	if err := r.SetRules([]Rule{{Name: "broken", Match: "("}}, "wp_"); err == nil {
		t.Fatal("a broken expression was accepted")
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want the previous rule still in place", r.Len())
	}
}

func TestRuleValidate(t *testing.T) {
	if err := (Rule{Match: "^SELECT"}).Validate(); err == nil {
		t.Error("a rewrite without a name was accepted")
	}
	if err := (Rule{Name: "x"}).Validate(); err == nil {
		t.Error("a rewrite without a match expression was accepted")
	}
	if err := (Rule{Name: "x", Match: "("}).Validate(); err == nil {
		t.Error("a rewrite with a broken expression was accepted")
	}
	// The placeholder must survive validation: the prefix is not known yet.
	if err := (Rule{Name: "x", Match: "{prefix}posts"}).Validate(); err != nil {
		t.Errorf("a rule using {prefix} was refused at load time: %v", err)
	}
}
