package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func advice(kind Kind, table string) Advice {
	return Advice{Kind: kind, Database: "wp", Table: table, Reason: "because", Apply: "ALTER TABLE " + table}
}

// The same suggestion made twice is one suggestion, seen twice. A
// suggestion seen once may be a fluke; one seen every report for a week is
// the workload.
func TestAddMergesRepeatedAdvice(t *testing.T) {
	s := NewStore("")

	s.Add(advice(KindIndex, "wp_posts"))
	s.Add(advice(KindIndex, "wp_posts"))

	list := s.List()
	if len(list) != 1 {
		t.Fatalf("got %d suggestions, want 1", len(list))
	}
	if list[0].Seen != 2 {
		t.Fatalf("seen = %d, want 2", list[0].Seen)
	}
	if list[0].FirstSeen.After(list[0].LastSeen) {
		t.Fatal("first_seen is after last_seen")
	}
}

func TestDifferentAdviceIsKeptApart(t *testing.T) {
	s := NewStore("")
	s.Add(advice(KindIndex, "wp_posts"), advice(KindIndex, "wp_postmeta"), advice(KindFulltext, "wp_posts"))

	if got := len(s.List()); got != 3 {
		t.Fatalf("got %d suggestions, want 3", got)
	}
}

// Advice outlives a restart: that is the whole reason it is on disk.
func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "advice.json")

	s := NewStore(path)
	s.Add(advice(KindIndex, "wp_posts"))
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := ReadAdvice(path)
	if err != nil {
		t.Fatalf("ReadAdvice: %v", err)
	}
	if len(reloaded) != 1 || reloaded[0].Table != "wp_posts" {
		t.Fatalf("reloaded = %+v", reloaded)
	}

	// And a suggestion made again after the restart is merged, not doubled.
	again := NewStore(path)
	if err := again.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	again.Add(advice(KindIndex, "wp_posts"))
	if list := again.List(); len(list) != 1 || list[0].Seen != 2 {
		t.Fatalf("after reloading and re-adding: %+v", list)
	}
}

func TestLoadMissingFile(t *testing.T) {
	list, err := ReadAdvice(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("ReadAdvice on a missing file: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("got %d suggestions, want none", len(list))
	}
}

// A store that grows forever stops being read, so the oldest go.
func TestStoreIsCapped(t *testing.T) {
	s := NewStore("")
	for i := 0; i < maxAdvice+50; i++ {
		s.Add(Advice{Kind: KindIndex, Table: "t", Apply: string(rune('a' + i%26)), Query: string(rune(i))})
	}
	if got := len(s.List()); got > maxAdvice {
		t.Fatalf("got %d suggestions, want at most %d", got, maxAdvice)
	}
}

// The file is written through a rename, so a reader never sees half a list.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "advice.json")

	s := NewStore(path)
	s.Add(advice(KindIndex, "wp_posts"))
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("a temporary file was left behind: %s", e.Name())
		}
	}
}
