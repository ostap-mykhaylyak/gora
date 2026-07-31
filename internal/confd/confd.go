// Package confd reads the drop-in files that describe what gora does with
// traffic: what to cache, what to rewrite, what to refuse and what to slow
// down.
//
// They live in their own directory rather than in config.yaml because they
// are the part meant to change while gora runs. A reload applies them
// without dropping a connection, which is what makes adding a rule during
// an incident a one-line change instead of a deployment.
package confd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ostap-mykhaylyak/gora/internal/cache"
	"github.com/ostap-mykhaylyak/gora/internal/firewall"
	"github.com/ostap-mykhaylyak/gora/internal/rewrite"
	"github.com/ostap-mykhaylyak/gora/internal/throttle"
)

// Set is everything the drop-ins declared, in file order.
type Set struct {
	Cache     []cache.Rule
	Rewrites  []rewrite.Rule
	Blocks    []firewall.Rule
	Throttles []throttle.Rule
}

// Len returns the total number of rules, all sections together.
func (s Set) Len() int {
	return len(s.Cache) + len(s.Rewrites) + len(s.Blocks) + len(s.Throttles)
}

// file is the layout of one drop-in.
type file struct {
	Name     string          `yaml:"name"`
	Rules    []cache.Rule    `yaml:"rules"`
	Rewrites []rewrite.Rule  `yaml:"rewrites"`
	Block    []firewall.Rule `yaml:"block"`
	Throttle []throttle.Rule `yaml:"throttle"`
}

// validator is what every rule type implements, so this package can check
// them all without knowing what any of them mean.
type validator interface{ Validate() error }

// Load reads every *.yaml drop-in in dir, in filename order. A missing
// directory is not an error: conf.d is optional.
func Load(dir string) (Set, error) {
	var set Set
	if dir == "" {
		return set, nil
	}

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return set, nil
	}
	if err != nil {
		return set, fmt.Errorf("reading %s: %w", dir, err)
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

	// Rule names are unique per section, so a cache rule and a block rule
	// may share a name while two cache rules may not.
	seen := map[string]map[string]string{}
	for _, name := range names {
		path := filepath.Join(dir, name)
		f, err := parse(path)
		if err != nil {
			return Set{}, err
		}

		for _, r := range f.Rules {
			if err := check(path, "cache rule", r.Name, r, seen); err != nil {
				return Set{}, err
			}
			set.Cache = append(set.Cache, r)
		}
		for _, r := range f.Rewrites {
			if err := check(path, "rewrite", r.Name, r, seen); err != nil {
				return Set{}, err
			}
			set.Rewrites = append(set.Rewrites, r)
		}
		for _, r := range f.Block {
			if err := check(path, "block rule", r.Name, r, seen); err != nil {
				return Set{}, err
			}
			set.Blocks = append(set.Blocks, r)
		}
		for _, r := range f.Throttle {
			if err := check(path, "throttle rule", r.Name, r, seen); err != nil {
				return Set{}, err
			}
			set.Throttles = append(set.Throttles, r)
		}
	}
	return set, nil
}

func parse(path string) (file, error) {
	var f file
	data, err := os.ReadFile(path)
	if err != nil {
		return f, fmt.Errorf("reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("parsing %s: %w", path, err)
	}
	return f, nil
}

// check validates one rule and refuses a name already used in its section.
func check(path, kind, name string, r validator, seen map[string]map[string]string) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	section := seen[kind]
	if section == nil {
		section = map[string]string{}
		seen[kind] = section
	}
	if where, dup := section[name]; dup {
		return fmt.Errorf("%s: %s %q is already defined in %s", path, kind, name, where)
	}
	section[name] = path
	return nil
}
