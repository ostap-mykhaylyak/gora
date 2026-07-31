package cache

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

// Rule caches the SELECTs matching a regular expression and drops them when
// a write touches one of the listed tables.
type Rule struct {
	Name         string          `yaml:"name"`
	Match        string          `yaml:"match"`
	TTL          config.Duration `yaml:"ttl"`
	InvalidateOn []string        `yaml:"invalidate_on"`

	// re is Match with the prefix expanded, compiled. It is filled in when
	// the rule is bound to a table prefix, which may happen well after
	// loading: with table_prefix: auto the prefix is not known until gora
	// sees the first database.
	re *regexp.Regexp
}

// Validate checks a rule as written in a drop-in, before any prefix is
// known. The expression is compiled against a stand-in prefix, because
// "{prefix}" inside a regular expression is a repeat count and would be
// reported as a syntax error nobody can act on.
func (r Rule) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("a cache rule has no name")
	}
	if r.Match == "" {
		return fmt.Errorf("cache rule %q has no match expression", r.Name)
	}
	probe := strings.ReplaceAll(r.Match, config.PrefixPlaceholder, "wp_")
	if _, err := regexp.Compile(probe); err != nil {
		return fmt.Errorf("cache rule %q has an invalid match expression: %w", r.Name, err)
	}
	if len(r.InvalidateOn) == 0 {
		// A rule with no invalidation serves stale rows until its TTL
		// expires, which is never what the author meant.
		return fmt.Errorf("cache rule %q needs invalidate_on: the tables whose writes drop it", r.Name)
	}
	if r.TTL < 0 {
		return fmt.Errorf("cache rule %q has a negative ttl", r.Name)
	}
	return nil
}

// compileRules binds rules to a table prefix.
func compileRules(rules []Rule, prefix string) ([]Rule, error) {
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		expr, err := config.ExpandPrefix(r.Match, prefix)
		if err != nil {
			return nil, fmt.Errorf("cache rule %q: %w", r.Name, err)
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("cache rule %q: %w", r.Name, err)
		}
		r.re = re

		tables := make([]string, len(r.InvalidateOn))
		for i, t := range r.InvalidateOn {
			table, err := config.ExpandPrefix(t, prefix)
			if err != nil {
				return nil, fmt.Errorf("cache rule %q: %w", r.Name, err)
			}
			tables[i] = table
		}
		r.InvalidateOn = tables
		out = append(out, r)
	}
	return out, nil
}
