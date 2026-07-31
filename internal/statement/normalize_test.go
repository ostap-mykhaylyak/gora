package statement

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"SELECT  *   FROM\n wp_posts", "SELECT * FROM wp_posts"},
		{"  SELECT 1  ", "SELECT 1"},
		{"/* plugin */\nSELECT 1", "SELECT 1"},
		{"SELECT 1", "SELECT 1"},
		{"", ""},
		{"SELECT `my  col` FROM t", "SELECT `my  col` FROM t"},
	}
	for _, tt := range tests {
		if got := Normalize(tt.in); got != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The reason Normalize is not a simple whitespace squeeze: two literals
// differing only by spacing are two different queries, and merging them
// would make the cache answer with the wrong rows.
func TestNormalizeKeepsLiteralsIntact(t *testing.T) {
	a := Normalize("SELECT * FROM t WHERE title = 'a  b'")
	b := Normalize("SELECT * FROM t WHERE title = 'a b'")
	if a == b {
		t.Fatalf("two different literals normalised to the same string: %q", a)
	}
	if want := "SELECT * FROM t WHERE title = 'a  b'"; a != want {
		t.Fatalf("got %q, want %q", a, want)
	}
}

func TestNormalizeHandlesEscapedQuotes(t *testing.T) {
	in := `SELECT * FROM t WHERE a = 'it''s  here' AND b = 'x\'y  z'`
	if got := Normalize(in); got != in {
		t.Fatalf("got %q, want it unchanged", got)
	}
}
