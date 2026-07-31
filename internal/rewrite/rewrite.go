// Package rewrite rewrites statements on their way to the backend.
//
// It exists for the SQL you cannot fix at the source: a plugin you did not
// write, a theme nobody maintains, a query that ships with something you
// depend on. A rewrite changes what the database is asked, so gora ships
// none enabled and the profiler suggests them rather than applying them.
package rewrite

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

// Rule replaces every match of an expression with a replacement, which may
// reference captures ($1) and may be empty to delete the match.
type Rule struct {
	Name    string `yaml:"name"`
	Match   string `yaml:"match"`
	Replace string `yaml:"replace"`

	re *regexp.Regexp
}

// Validate checks a rule as written in a drop-in, before any prefix is
// known. The expression is compiled with a stand-in prefix, because
// "{prefix}" inside a regular expression is a repeat count and would be
// reported as a syntax error nobody can act on.
func (r Rule) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("a rewrite has no name")
	}
	if r.Match == "" {
		return fmt.Errorf("rewrite %q has no match expression", r.Name)
	}
	probe := strings.ReplaceAll(r.Match, config.PrefixPlaceholder, "wp_")
	if _, err := regexp.Compile(probe); err != nil {
		return fmt.Errorf("rewrite %q has an invalid match expression: %w", r.Name, err)
	}
	return nil
}

// Rewriter applies the configured rules. It is safe for concurrent use and
// its rules can be replaced while gora runs.
type Rewriter struct {
	rules atomic.Pointer[[]Rule]
	log   *slog.Logger
}

// New compiles the rules against the table prefix.
func New(rules []Rule, prefix string, log *slog.Logger) (*Rewriter, error) {
	r := &Rewriter{log: log}
	if err := r.SetRules(rules, prefix); err != nil {
		return nil, err
	}
	return r, nil
}

// SetRules replaces the rules atomically (hot reload). On error nothing
// changes.
func (r *Rewriter) SetRules(rules []Rule, prefix string) error {
	compiled, err := compile(rules, prefix)
	if err != nil {
		return err
	}
	r.rules.Store(&compiled)
	return nil
}

func compile(rules []Rule, prefix string) ([]Rule, error) {
	out := make([]Rule, 0, len(rules))
	for _, rule := range rules {
		expr, err := config.ExpandPrefix(rule.Match, prefix)
		if err != nil {
			return nil, fmt.Errorf("rewrite %q: %w", rule.Name, err)
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("rewrite %q: %w", rule.Name, err)
		}
		rule.re = re
		out = append(out, rule)
	}
	return out, nil
}

// Len returns how many rules are active.
func (r *Rewriter) Len() int {
	rules := r.rules.Load()
	if rules == nil {
		return 0
	}
	return len(*rules)
}

// Apply returns the rewritten statement and the names of the rules that
// changed it. With no rules configured — the normal case — it costs one
// atomic load and returns immediately, because this runs on every query.
func (r *Rewriter) Apply(query string) (string, []string) {
	rules := r.rules.Load()
	if rules == nil || len(*rules) == 0 {
		return query, nil
	}

	var applied []string
	for i := range *rules {
		rule := &(*rules)[i]
		if !rule.re.MatchString(query) {
			continue
		}
		query = rule.re.ReplaceAllString(query, rule.Replace)
		applied = append(applied, rule.Name)
	}
	return query, applied
}
