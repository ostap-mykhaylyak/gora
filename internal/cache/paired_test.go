package cache

import (
	"bytes"
	"testing"
)

const listing = "SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts WHERE post_type = 'product' LIMIT 0, 16"

func listingRules() []Rule {
	return []Rule{{
		Name:         "product-listing",
		Match:        `(?i)^SELECT SQL_CALC_FOUND_ROWS .* FROM {prefix}posts`,
		InvalidateOn: []string{"{prefix}posts", "{prefix}postmeta"},
	}}
}

// Rows without the count that belongs with them would silently corrupt
// "page X of Y", so an unpaired entry is not servable.
func TestListingIsNotServedBeforeItIsPaired(t *testing.T) {
	c := newCache(t, testConfig(), listingRules())

	c.StorePaired(db, listing, result(t, "a"))
	if _, _, ok := c.LookupPaired(db, listing); ok {
		t.Fatal("an unpaired listing was served")
	}

	c.PairFoundRows(db, listing, 128)
	r, count, ok := c.LookupPaired(db, listing)
	if !ok {
		t.Fatal("the paired listing was not served")
	}
	if count != 128 {
		t.Fatalf("count = %d, want 128", count)
	}
	if r == nil {
		t.Fatal("the rows are missing")
	}
}

// A listing is cacheable only when a conf.d rule says so: only the person
// writing the rule knows the page looks the same for every visitor.
func TestListingNeedsARule(t *testing.T) {
	c := newCache(t, testConfig(), nil)
	if c.PairedCacheable(db, listing) {
		t.Fatal("a listing was cacheable without any rule")
	}

	withRule := newCache(t, testConfig(), listingRules())
	if !withRule.PairedCacheable(db, listing) {
		t.Fatal("the rule did not make the listing cacheable")
	}
}

// A listing carrying something volatile stays out, rule or no rule.
func TestVolatileListingIsNotCacheable(t *testing.T) {
	c := newCache(t, testConfig(), listingRules())
	q := "SELECT SQL_CALC_FOUND_ROWS ID FROM wp_posts ORDER BY RAND() LIMIT 0, 16"
	if c.PairedCacheable(db, q) {
		t.Fatal("a listing with ORDER BY RAND() was cacheable")
	}
}

// The invalidation tables apply to paired entries exactly as they do to
// ordinary ones.
func TestWriteInvalidatesAPairedListing(t *testing.T) {
	c := newCache(t, testConfig(), listingRules())
	c.StorePaired(db, listing, result(t, "a"))
	c.PairFoundRows(db, listing, 128)

	c.InvalidateWrite(db, "UPDATE wp_postmeta SET meta_value = '3' WHERE post_id = 7")

	if _, _, ok := c.LookupPaired(db, listing); ok {
		t.Fatal("the listing survived a write on wp_postmeta")
	}
}

// The synthetic answer must look to the client exactly like the one MySQL
// would have sent: one row, one column, the count in it. It carries row
// data rather than parsed values, because that is what the server writes on
// the wire.
func TestFoundRowsResult(t *testing.T) {
	r := FoundRowsResult(42)
	if r == nil || !r.HasResultset() {
		t.Fatal("FoundRowsResult produced nothing to send")
	}
	if len(r.Fields) != 1 || string(r.Fields[0].Name) != "FOUND_ROWS()" {
		t.Fatalf("fields = %+v, want a single FOUND_ROWS() column", r.Fields)
	}
	if len(r.RowDatas) != 1 {
		t.Fatalf("got %d rows, want 1", len(r.RowDatas))
	}
	if !bytes.Contains(r.RowDatas[0], []byte("42")) {
		t.Fatalf("row %q does not carry the count", r.RowDatas[0])
	}
}

func TestIsFoundRowsQuery(t *testing.T) {
	yes := []string{"SELECT FOUND_ROWS()", "  select found_rows( )  "}
	no := []string{"SELECT FOUND_ROWS() FROM t", "SELECT 1"}
	for _, q := range yes {
		if !IsFoundRowsQuery(q) {
			t.Errorf("IsFoundRowsQuery(%q) = false, want true", q)
		}
	}
	for _, q := range no {
		if IsFoundRowsQuery(q) {
			t.Errorf("IsFoundRowsQuery(%q) = true, want false", q)
		}
	}
}
