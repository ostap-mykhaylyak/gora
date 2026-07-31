package cache

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

// prefixPlaceholder is expanded to the WordPress table prefix when a rule is
// compiled, so a drop-in works on any installation without being edited.
const prefixPlaceholder = "{prefix}"

// Rule caches the SELECTs matching a regular expression and drops them when
// a write touches one of the listed tables.
type Rule struct {
	Name         string          `yaml:"name"`
	Match        string          `yaml:"match"`
	TTL          config.Duration `yaml:"ttl"`
	InvalidateOn []string        `yaml:"invalidate_on"`

	// re is Match with the prefix expanded, compiled. It is filled in when
	// the rule is bound to a table prefix, which may be later than loading:
	// with table_prefix: auto the prefix is not known until gora sees the
	// first database.
	re *regexp.Regexp
}

// ruleFile is the layout of one conf.d drop-in.
type ruleFile struct {
	Name  string `yaml:"name"`
	Rules []Rule `yaml:"rules"`
}

// LoadRuleDir reads every *.yaml drop-in in dir, in filename order. A
// missing directory is not an error: conf.d is optional.
//
// The regexes are validated here but compiled later, once the table prefix
// is known — which is why a placeholder is substituted before the syntax
// check: "{prefix}" inside a regular expression is a repeat count, and
// would be reported as a syntax error the user cannot make sense of.
func LoadRuleDir(dir string) ([]Rule, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(e.Name())); ext != ".yaml" && ext != ".yml" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var rules []Rule
	seen := make(map[string]string) // rule name -> file it came from
	for _, name := range names {
		path := filepath.Join(dir, name)
		fileRules, err := loadRuleFile(path)
		if err != nil {
			return nil, err
		}
		for _, r := range fileRules {
			if where, dup := seen[r.Name]; dup {
				return nil, fmt.Errorf("%s: rule %q is already defined in %s", path, r.Name, where)
			}
			seen[r.Name] = path
			rules = append(rules, r)
		}
	}
	return rules, nil
}

func loadRuleFile(path string) ([]Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var file ruleFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	for i, r := range file.Rules {
		if r.Name == "" {
			return nil, fmt.Errorf("%s: rules[%d] has no name", path, i)
		}
		if r.Match == "" {
			return nil, fmt.Errorf("%s: rule %q has no match expression", path, r.Name)
		}
		if _, err := regexp.Compile(expandPrefix(r.Match, "wp_")); err != nil {
			return nil, fmt.Errorf("%s: rule %q has an invalid match expression: %w", path, r.Name, err)
		}
		if len(r.InvalidateOn) == 0 {
			// A rule with no invalidation serves stale rows until its TTL
			// expires, which is never what the author meant.
			return nil, fmt.Errorf("%s: rule %q needs invalidate_on: the tables whose writes drop it", path, r.Name)
		}
		if r.TTL < 0 {
			return nil, fmt.Errorf("%s: rule %q has a negative ttl", path, r.Name)
		}
	}
	return file.Rules, nil
}

// compileRules binds rules to a table prefix.
func compileRules(rules []Rule, prefix string) ([]Rule, error) {
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		re, err := regexp.Compile(expandPrefix(r.Match, prefix))
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", r.Name, err)
		}
		r.re = re
		tables := make([]string, len(r.InvalidateOn))
		for i, t := range r.InvalidateOn {
			tables[i] = expandPrefix(t, prefix)
		}
		r.InvalidateOn = tables
		out = append(out, r)
	}
	return out, nil
}

func expandPrefix(s, prefix string) string {
	return strings.ReplaceAll(s, prefixPlaceholder, prefix)
}
