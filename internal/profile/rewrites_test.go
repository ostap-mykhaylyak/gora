package profile

import (
	"strings"
	"testing"
)

func TestSuggestRewrites(t *testing.T) {
	tests := []struct {
		name     string
		digest   string
		wantAny  bool
		wantSaid string // substring the reason must contain
		hasFix   bool   // whether a ready-made rule comes with it
	}{
		{
			name:     "order by rand",
			digest:   "SELECT ID FROM wp_posts ORDER BY RAND() LIMIT ?",
			wantAny:  true,
			wantSaid: "sorts the whole table",
			hasFix:   true,
		},
		{
			name:     "sql_calc_found_rows",
			digest:   "SELECT SQL_CALC_FOUND_ROWS ID FROM wp_posts LIMIT ?, ?",
			wantAny:  true,
			wantSaid: "deprecated since MySQL 8.0.17",
			hasFix:   true,
		},
		{
			name:     "leading wildcard",
			digest:   "SELECT ID FROM wp_posts WHERE post_title LIKE '%chair%'",
			wantAny:  true,
			wantSaid: "no rewrite is safe here",
		},
		{
			name:     "huge offset",
			digest:   "SELECT ID FROM wp_posts LIMIT 100000, 20",
			wantAny:  true,
			wantSaid: "read and discard every row",
		},
		{
			name:   "ordinary statement",
			digest: "SELECT ID FROM wp_posts WHERE post_type = ?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			advice := suggestRewrites([]Stat{{Digest: tt.digest, Database: "wp", Calls: 10}})
			if !tt.wantAny {
				if len(advice) != 0 {
					t.Fatalf("got %d suggestions for an ordinary statement: %+v", len(advice), advice)
				}
				return
			}
			if len(advice) != 1 {
				t.Fatalf("got %d suggestions, want 1", len(advice))
			}
			a := advice[0]
			if !strings.Contains(a.Reason, tt.wantSaid) {
				t.Fatalf("reason = %q, want it to mention %q", a.Reason, tt.wantSaid)
			}
			if tt.hasFix && a.Apply == "" {
				t.Fatal("a suggestion with a known fix came without one")
			}
			if !tt.hasFix && a.Apply != "" {
				t.Fatalf("a suggestion with no safe fix came with one: %q", a.Apply)
			}
		})
	}
}

// The SQL_CALC_FOUND_ROWS advice must not tell a WooCommerce shop to remove
// it: the pagination reads FOUND_ROWS() straight after, and would get the
// wrong total.
func TestCalcFoundRowsAdviceWarnsAboutPagination(t *testing.T) {
	advice := suggestRewrites([]Stat{{
		Digest: "SELECT SQL_CALC_FOUND_ROWS ID FROM wp_posts LIMIT ?, ?",
		Calls:  100,
	}})
	if len(advice) != 1 {
		t.Fatalf("got %d suggestions, want 1", len(advice))
	}
	if !strings.Contains(advice[0].Reason, "do not remove it") {
		t.Fatalf("reason = %q, want it to warn against removing it", advice[0].Reason)
	}
}
