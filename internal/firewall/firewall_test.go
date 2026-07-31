package firewall

import (
	"strings"
	"testing"
)

func newFirewall(t *testing.T, rules []Rule) *Firewall {
	t.Helper()
	f, err := New(rules, "wp_")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

func TestBlockedStatement(t *testing.T) {
	f := newFirewall(t, []Rule{{Name: "no-truncate", Match: "(?i)^TRUNCATE"}})

	verdict, matched := f.Check("TRUNCATE TABLE wp_postmeta")
	if !matched || !verdict.Blocked {
		t.Fatalf("verdict = %+v, matched = %v, want a block", verdict, matched)
	}
	if !strings.Contains(verdict.Message, "no-truncate") {
		t.Fatalf("the default message does not name the rule: %q", verdict.Message)
	}
	if got := f.Stat().Blocked; got != 1 {
		t.Fatalf("blocked = %d, want 1", got)
	}
}

func TestAllowedStatement(t *testing.T) {
	f := newFirewall(t, []Rule{{Name: "no-truncate", Match: "(?i)^TRUNCATE"}})

	if _, matched := f.Check("SELECT 1"); matched {
		t.Fatal("an unrelated statement matched")
	}
	if got := f.Stat().Blocked; got != 0 {
		t.Fatalf("blocked = %d, want 0", got)
	}
}

// Dry run is how a rule is tried against production traffic: it reports
// what it would have refused and refuses nothing.
func TestDryRunMatchesButDoesNotBlock(t *testing.T) {
	f := newFirewall(t, []Rule{{Name: "watch-deletes", Match: "(?i)^DELETE", DryRun: true}})

	verdict, matched := f.Check("DELETE FROM wp_posts WHERE ID = 1")
	if !matched {
		t.Fatal("the dry-run rule did not match")
	}
	if verdict.Blocked {
		t.Fatal("a dry-run rule blocked a statement")
	}
	st := f.Stat()
	if st.DryRunMatches != 1 || st.Blocked != 0 {
		t.Fatalf("stats = %+v, want one dry-run match and no blocks", st)
	}
}

func TestCustomMessage(t *testing.T) {
	f := newFirewall(t, []Rule{{
		Name:    "no-truncate",
		Match:   "(?i)^TRUNCATE",
		Message: "ask the DBA",
	}})

	verdict, _ := f.Check("TRUNCATE TABLE wp_posts")
	if verdict.Message != "ask the DBA" {
		t.Fatalf("message = %q, want the configured one", verdict.Message)
	}
}

// The first matching rule decides, so the order in conf.d is the order of
// precedence.
func TestFirstMatchWins(t *testing.T) {
	f := newFirewall(t, []Rule{
		{Name: "watch", Match: "(?i)^DELETE", DryRun: true},
		{Name: "block", Match: "(?i)^DELETE"},
	})

	verdict, _ := f.Check("DELETE FROM wp_posts")
	if verdict.Rule != "watch" || verdict.Blocked {
		t.Fatalf("verdict = %+v, want the first rule to decide", verdict)
	}
}

func TestNoRulesIsCheap(t *testing.T) {
	f := newFirewall(t, nil)
	if _, matched := f.Check("DELETE FROM wp_posts"); matched {
		t.Fatal("something matched with no rules configured")
	}
}

// A failed reload must leave the rules that were working in place.
func TestSetRulesKeepsTheOldRulesOnError(t *testing.T) {
	f := newFirewall(t, []Rule{{Name: "keep", Match: "(?i)^TRUNCATE"}})

	if err := f.SetRules([]Rule{{Name: "broken", Match: "("}}, "wp_"); err == nil {
		t.Fatal("a broken expression was accepted")
	}
	if f.Len() != 1 {
		t.Fatalf("Len = %d, want the previous rule still in place", f.Len())
	}
}

func TestRuleValidate(t *testing.T) {
	if err := (Rule{Match: "^SELECT"}).Validate(); err == nil {
		t.Error("a block rule without a name was accepted")
	}
	if err := (Rule{Name: "x"}).Validate(); err == nil {
		t.Error("a block rule without a match expression was accepted")
	}
	if err := (Rule{Name: "x", Match: "("}).Validate(); err == nil {
		t.Error("a block rule with a broken expression was accepted")
	}
}
