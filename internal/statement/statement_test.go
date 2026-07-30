package statement

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		query string
		want  Kind
	}{
		{"SELECT * FROM wp_posts", KindSelect},
		{"  select 1", KindSelect},
		{"/* query monitor */ SELECT 1", KindSelect},
		{"TABLE wp_options", KindSelect},
		{"INSERT INTO wp_options (option_name) VALUES ('a')", KindWrite},
		{"UPDATE wp_posts SET post_title = 'x' WHERE ID = 1", KindWrite},
		{"DELETE FROM wp_postmeta WHERE post_id = 1", KindWrite},
		{"REPLACE INTO wp_options VALUES ('a','b')", KindWrite},
		{"ALTER TABLE wp_posts ADD INDEX (post_name)", KindWrite},
		{"CALL some_procedure()", KindWrite},
		{"BEGIN", KindBegin},
		{"START TRANSACTION", KindBegin},
		{"START SLAVE", KindOther},
		{"COMMIT", KindCommit},
		{"ROLLBACK", KindRollback},
		{"ROLLBACK TO SAVEPOINT s1", KindOther},
		{"SET NAMES utf8mb4", KindOther},
		{"SET autocommit = 0", KindUnsafe},
		{"SET @@session.autocommit = 0", KindUnsafe},
		{"XA START 'x'", KindUnsafe},
		{"SHOW TABLES", KindOther},
		{"WITH t AS (SELECT 1) DELETE FROM wp_posts", KindOther},
		{"", KindOther},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			if got := Classify(tt.query); got != tt.want {
				t.Fatalf("Classify(%q) = %s, want %s", tt.query, got, tt.want)
			}
		})
	}
}

func TestStripLeading(t *testing.T) {
	tests := []struct{ in, want string }{
		{"  SELECT 1", "SELECT 1"},
		{"/* a */ /* b */ SELECT 1", "SELECT 1"},
		{"/* unterminated SELECT 1", ""},
		{"SELECT 1", "SELECT 1"},
	}
	for _, tt := range tests {
		if got := StripLeading(tt.in); got != tt.want {
			t.Errorf("StripLeading(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFingerprint(t *testing.T) {
	tests := []struct{ in, want string }{
		{
			"SELECT * FROM wp_posts WHERE ID = 42",
			"SELECT * FROM wp_posts WHERE ID = ?",
		},
		{
			"SELECT * FROM wp_posts WHERE post_name = 'hello-world'",
			"SELECT * FROM wp_posts WHERE post_name = ?",
		},
		{
			"SELECT  *\n FROM   wp_posts",
			"SELECT * FROM wp_posts",
		},
		{
			"SELECT * FROM wp_options WHERE option_name IN ('a','b','c')",
			"SELECT * FROM wp_options WHERE option_name IN (?)",
		},
		{
			// An identifier ending in digits is not a literal.
			"SELECT * FROM wp_2024_stats",
			"SELECT * FROM wp_2024_stats",
		},
		{
			// Escaped and doubled quotes must not end the literal early.
			`SELECT * FROM t WHERE a = 'it''s' AND b = 'x\'y' AND c = 7`,
			"SELECT * FROM t WHERE a = ? AND b = ? AND c = ?",
		},
	}

	for _, tt := range tests {
		if got := Fingerprint(tt.in); got != tt.want {
			t.Errorf("Fingerprint(%q)\n got %q\nwant %q", tt.in, got, tt.want)
		}
	}
}

// Fingerprinting exists partly so that a literal containing @ cannot be
// mistaken for a user-defined variable by the multiplexing safety checks.
func TestFingerprintHidesAtSignsInLiterals(t *testing.T) {
	got := Fingerprint("SELECT ID FROM wp_users WHERE user_email = 'a@b.com'")
	if want := "SELECT ID FROM wp_users WHERE user_email = ?"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
