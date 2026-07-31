// Package firewall refuses statements before they reach the backend.
//
// It is the emergency brake: when a plugin starts issuing a query that is
// taking the database down, a rule in conf.d and a reload stop it in
// seconds, without a deployment and without a restart. Rules can be armed
// in dry-run first, so you find out what a rule would have blocked before
// it blocks anything.
package firewall

import (
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

// Rule refuses the statements matching an expression.
type Rule struct {
	Name  string `yaml:"name"`
	Match string `yaml:"match"`
	// Message is what the client is told. Empty uses a default that names
	// the rule, which is usually what you want in the PHP error log.
	Message string `yaml:"message"`
	// DryRun logs the match and lets the statement through. It is how a
	// rule is tried on production traffic before it refuses anything.
	DryRun bool `yaml:"dry_run"`

	re *regexp.Regexp
}

// Validate checks a rule as written in a drop-in, before any prefix is
// known.
func (r Rule) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("a block rule has no name")
	}
	if r.Match == "" {
		return fmt.Errorf("block rule %q has no match expression", r.Name)
	}
	probe := strings.ReplaceAll(r.Match, config.PrefixPlaceholder, "wp_")
	if _, err := regexp.Compile(probe); err != nil {
		return fmt.Errorf("block rule %q has an invalid match expression: %w", r.Name, err)
	}
	return nil
}

// Verdict is what the firewall decided about one statement.
type Verdict struct {
	// Rule is the name of the rule that matched.
	Rule string
	// Blocked is false for a dry-run match: the statement runs, and the
	// match is worth logging.
	Blocked bool
	// Message is what to tell the client when Blocked.
	Message string
}

// Firewall holds the active rules. It is safe for concurrent use and its
// rules can be replaced while gora runs.
type Firewall struct {
	rules atomic.Pointer[[]Rule]

	blocked  atomic.Uint64
	dryMatch atomic.Uint64
}

// Stats counts what the firewall has done since gora started.
type Stats struct {
	Rules         int    `json:"rules"`
	Blocked       uint64 `json:"blocked"`
	DryRunMatches uint64 `json:"dry_run_matches"`
}

// New compiles the rules against the table prefix.
func New(rules []Rule, prefix string) (*Firewall, error) {
	f := &Firewall{}
	if err := f.SetRules(rules, prefix); err != nil {
		return nil, err
	}
	return f, nil
}

// SetRules replaces the rules atomically (hot reload). On error nothing
// changes.
func (f *Firewall) SetRules(rules []Rule, prefix string) error {
	compiled, err := compile(rules, prefix)
	if err != nil {
		return err
	}
	f.rules.Store(&compiled)
	return nil
}

func compile(rules []Rule, prefix string) ([]Rule, error) {
	out := make([]Rule, 0, len(rules))
	for _, rule := range rules {
		expr, err := config.ExpandPrefix(rule.Match, prefix)
		if err != nil {
			return nil, fmt.Errorf("block rule %q: %w", rule.Name, err)
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("block rule %q: %w", rule.Name, err)
		}
		rule.re = re
		if rule.Message == "" {
			rule.Message = "statement refused by gora firewall rule '" + rule.Name + "'"
		}
		out = append(out, rule)
	}
	return out, nil
}

// Len returns how many rules are active.
func (f *Firewall) Len() int {
	rules := f.rules.Load()
	if rules == nil {
		return 0
	}
	return len(*rules)
}

// Stat returns the counters.
func (f *Firewall) Stat() Stats {
	return Stats{
		Rules:         f.Len(),
		Blocked:       f.blocked.Load(),
		DryRunMatches: f.dryMatch.Load(),
	}
}

// Check reports whether a statement matches a rule. The second return value
// says whether anything matched at all; a match with Blocked false is a
// dry-run rule, which the caller should log and then let through.
func (f *Firewall) Check(query string) (Verdict, bool) {
	rules := f.rules.Load()
	if rules == nil || len(*rules) == 0 {
		return Verdict{}, false
	}

	for i := range *rules {
		rule := &(*rules)[i]
		if !rule.re.MatchString(query) {
			continue
		}
		if rule.DryRun {
			f.dryMatch.Add(1)
			return Verdict{Rule: rule.Name}, true
		}
		f.blocked.Add(1)
		return Verdict{Rule: rule.Name, Blocked: true, Message: rule.Message}, true
	}
	return Verdict{}, false
}
