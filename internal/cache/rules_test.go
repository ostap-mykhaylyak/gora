package cache

import (
	"strings"
	"testing"
)

func TestRuleValidate(t *testing.T) {
	tests := []struct {
		name string
		rule Rule
		want string // substring the error must contain, empty means valid
	}{
		{
			name: "valid",
			rule: Rule{Name: "x", Match: `^SELECT \* FROM {prefix}posts`, InvalidateOn: []string{"{prefix}posts"}},
		},
		{
			name: "no name",
			rule: Rule{Match: "^SELECT", InvalidateOn: []string{"wp_posts"}},
			want: "no name",
		},
		{
			name: "no match",
			rule: Rule{Name: "x", InvalidateOn: []string{"wp_posts"}},
			want: "no match expression",
		},
		{
			name: "no invalidation",
			rule: Rule{Name: "x", Match: "^SELECT"},
			want: "invalidate_on",
		},
		{
			name: "broken expression",
			rule: Rule{Name: "x", Match: "^SELECT (", InvalidateOn: []string{"wp_posts"}},
			want: "invalid match expression",
		},
		{
			name: "negative ttl",
			rule: Rule{Name: "x", Match: "^SELECT", TTL: -1, InvalidateOn: []string{"wp_posts"}},
			want: "negative ttl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rule.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("an invalid rule was accepted")
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
