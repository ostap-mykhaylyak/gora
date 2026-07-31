package profile

import (
	"strings"
	"testing"
)

func TestSingleTable(t *testing.T) {
	tests := []struct {
		query  string
		table  string
		single bool
	}{
		{"SELECT * FROM wp_posts WHERE ID = ?", "wp_posts", true},
		{"SELECT * FROM `wp_posts` WHERE ID = ?", "wp_posts", true},
		{
			"SELECT * FROM wp_posts INNER JOIN wp_postmeta ON wp_posts.ID = wp_postmeta.post_id",
			"wp_posts", false,
		},
		{"SELECT (SELECT 1 FROM wp_options) FROM wp_posts", "wp_options", false},
		{"SHOW TABLES", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			table, single := singleTable(tt.query)
			if table != tt.table || single != tt.single {
				t.Fatalf("singleTable = (%q, %v), want (%q, %v)", table, single, tt.table, tt.single)
			}
		})
	}
}

func TestFilterColumns(t *testing.T) {
	tests := []struct {
		query string
		want  []string
	}{
		{"SELECT * FROM wp_postmeta WHERE meta_key = ?", []string{"meta_key"}},
		{"SELECT * FROM wp_posts WHERE post_type = ? AND post_status = ?", []string{"post_type", "post_status"}},
		{"SELECT * FROM wp_posts WHERE post_type = ? ORDER BY post_date DESC", []string{"post_type"}},
		{"SELECT * FROM wp_posts", nil},
		// Four predicates, three columns: a composite index of everything is
		// not an index, it is a copy of the table.
		{"SELECT * FROM t WHERE a = ? AND b = ? AND c = ? AND d = ?", []string{"a", "b", "c"}},
		// The same column twice is one column.
		{"SELECT * FROM t WHERE a = ? AND a = ?", []string{"a"}},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := filterColumns(tt.query)
			if len(got) != len(tt.want) {
				t.Fatalf("filterColumns = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("filterColumns = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestLikeColumn(t *testing.T) {
	col, ok := likeColumn("SELECT * FROM wp_posts WHERE post_title LIKE ?")
	if ok {
		t.Fatalf("a fingerprinted LIKE matched as a leading wildcard: %q", col)
	}
	col, ok = likeColumn("SELECT * FROM wp_posts WHERE post_title LIKE '%chair%'")
	if !ok || col != "post_title" {
		t.Fatalf("likeColumn = (%q, %v), want (post_title, true)", col, ok)
	}
}

func TestAdviseFromExplain(t *testing.T) {
	tests := []struct {
		name    string
		stat    Stat
		row     explainRow
		wantYes bool
		kind    Kind
		apply   string
	}{
		{
			name:    "full scan of a large table",
			stat:    Stat{Digest: "SELECT * FROM wp_postmeta WHERE meta_key = ?", Database: "wp", Calls: 500},
			row:     explainRow{Table: "wp_postmeta", Type: "ALL", Rows: 50000},
			wantYes: true,
			kind:    KindIndex,
			apply:   "ALTER TABLE wp_postmeta ADD INDEX idx_gora_meta_key (meta_key);",
		},
		{
			name: "small table",
			stat: Stat{Digest: "SELECT * FROM wp_terms WHERE name = ?", Database: "wp"},
			row:  explainRow{Table: "wp_terms", Type: "ALL", Rows: 40},
		},
		{
			name: "already using an index",
			stat: Stat{Digest: "SELECT * FROM wp_postmeta WHERE meta_key = ?", Database: "wp"},
			row:  explainRow{Table: "wp_postmeta", Type: "ref", Key: "meta_key", Rows: 90000},
		},
		{
			name:    "leading wildcard search",
			stat:    Stat{Digest: "SELECT * FROM wp_posts WHERE post_title LIKE '%chair%'", Database: "wp"},
			row:     explainRow{Table: "wp_posts", Type: "ALL", Rows: 80000},
			wantYes: true,
			kind:    KindFulltext,
			apply:   "ALTER TABLE wp_posts ADD FULLTEXT INDEX ft_gora_post_title (post_title);",
		},
		{
			name: "join: flagged, not guessed",
			stat: Stat{
				Digest:   "SELECT * FROM wp_posts INNER JOIN wp_postmeta ON wp_posts.ID = wp_postmeta.post_id WHERE meta_key = ?",
				Database: "wp",
			},
			row:     explainRow{Table: "wp_posts", Type: "ALL", Rows: 20000},
			wantYes: true,
			kind:    KindIndex,
			apply:   "", // no ALTER: gora does not guess indexes across a join
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			advice, ok := adviseFromExplain(tt.stat, tt.row)
			if ok != tt.wantYes {
				t.Fatalf("advice given = %v, want %v (%+v)", ok, tt.wantYes, advice)
			}
			if !ok {
				return
			}
			if advice.Kind != tt.kind {
				t.Fatalf("kind = %q, want %q", advice.Kind, tt.kind)
			}
			if advice.Apply != tt.apply {
				t.Fatalf("apply = %q, want %q", advice.Apply, tt.apply)
			}
			if advice.Reason == "" {
				t.Fatal("the advice does not say why")
			}
		})
	}
}

// EXPLAIN is only ever run on reads: running it on an UPDATE is valid SQL
// in MySQL 8, and gora does not send writes of its own accord.
func TestOnlyReadsAreExplained(t *testing.T) {
	if isExplainable("UPDATE wp_posts SET post_title = ? WHERE ID = ?") {
		t.Error("an UPDATE was considered explainable")
	}
	if isExplainable("DELETE FROM wp_posts WHERE ID = ?") {
		t.Error("a DELETE was considered explainable")
	}
	if !isExplainable("SELECT * FROM wp_posts") {
		t.Error("a SELECT was not considered explainable")
	}
}

// The ALTER statement has to be something you can paste into a client.
func TestSuggestedIndexIsValidSQL(t *testing.T) {
	advice, ok := adviseFromExplain(
		Stat{Digest: "SELECT * FROM wp_posts WHERE post_type = ? AND post_status = ?", Database: "wp"},
		explainRow{Table: "wp_posts", Type: "ALL", Rows: 30000},
	)
	if !ok {
		t.Fatal("no advice for a full scan")
	}
	if !strings.HasPrefix(advice.Apply, "ALTER TABLE wp_posts ADD INDEX ") || !strings.HasSuffix(advice.Apply, ";") {
		t.Fatalf("apply = %q", advice.Apply)
	}
	if !strings.Contains(advice.Apply, "(post_type, post_status)") {
		t.Fatalf("apply = %q, want both columns in order", advice.Apply)
	}
}
