package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// maxAdvice caps how many suggestions are kept. Past it the least recently
// seen ones go: a file that grows forever stops being read.
const maxAdvice = 200

// Kind is what a suggestion asks you to do.
type Kind string

const (
	// KindIndex suggests adding an index.
	KindIndex Kind = "add-index"
	// KindFulltext suggests a FULLTEXT index for a search that no B-tree
	// index can help.
	KindFulltext Kind = "add-fulltext"
	// KindRedundant suggests dropping an index another one already covers.
	KindRedundant Kind = "drop-redundant-index"
	// KindUnused suggests dropping an index nothing has read.
	KindUnused Kind = "drop-unused-index"
	// KindRewrite suggests a conf.d rewrite for a known antipattern.
	KindRewrite Kind = "rewrite"
)

// Advice is one suggestion, with everything needed to act on it and
// nothing gora will do on its own.
type Advice struct {
	Kind Kind `json:"kind"`
	// Database and Table it concerns, when it concerns one.
	Database string `json:"database,omitempty"`
	Table    string `json:"table,omitempty"`
	// Reason explains why, in the terms of the workload that produced it.
	Reason string `json:"reason"`
	// Apply is the statement to run, or the conf.d snippet to add.
	Apply string `json:"apply,omitempty"`
	// Query is the statement that prompted it, normalised.
	Query string `json:"query,omitempty"`
	// Calls is how often that statement ran in the interval that produced
	// this advice.
	Calls uint64 `json:"calls,omitempty"`

	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Seen is how many reports have produced this same suggestion. A
	// suggestion seen once may be a fluke; one seen every report for a week
	// is the workload.
	Seen int `json:"seen"`
}

// key identifies a suggestion across reports.
func (a Advice) key() string {
	return string(a.Kind) + "\x00" + a.Database + "\x00" + a.Table + "\x00" + a.Apply + "\x00" + a.Query
}

// Store keeps the suggestions gora has made, on disk.
//
// They are written to a file rather than only to the log because that is
// the difference between advice you find when you go looking and advice
// that scrolled past at four in the morning. Restarting gora does not lose
// them, and `gora --advice` prints them.
type Store struct {
	path string

	mu     sync.Mutex
	byKey  map[string]*Advice
	loaded bool
}

// NewStore opens the store at path. A path of "" keeps the suggestions in
// memory only.
func NewStore(path string) *Store {
	return &Store{path: path, byKey: make(map[string]*Advice)}
}

// Load reads the file, if there is one. A file that cannot be parsed is
// reported and then replaced: advice is not data worth failing over.
func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = true

	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", s.path, err)
	}

	var list []Advice
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("parsing %s: %w", s.path, err)
	}
	for i := range list {
		a := list[i]
		s.byKey[a.key()] = &a
	}
	return nil
}

// Add records suggestions, merging them with what is already known: a
// suggestion made again is the same suggestion, seen once more.
func (s *Store) Add(advice ...Advice) {
	if len(advice) == 0 {
		return
	}
	now := time.Now()

	s.mu.Lock()
	for _, a := range advice {
		k := a.key()
		if existing, ok := s.byKey[k]; ok {
			existing.LastSeen = now
			existing.Seen++
			existing.Calls = a.Calls
			existing.Reason = a.Reason
			continue
		}
		a.FirstSeen = now
		a.LastSeen = now
		a.Seen = 1
		stored := a
		s.byKey[k] = &stored
	}
	s.evictLocked()
	s.mu.Unlock()
}

// evictLocked drops the least recently seen suggestions past the cap.
func (s *Store) evictLocked() {
	if len(s.byKey) <= maxAdvice {
		return
	}
	all := make([]*Advice, 0, len(s.byKey))
	for _, a := range s.byKey {
		all = append(all, a)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].LastSeen.Before(all[j].LastSeen) })
	for _, a := range all[:len(s.byKey)-maxAdvice] {
		delete(s.byKey, a.key())
	}
}

// List returns the suggestions, most recently seen first.
func (s *Store) List() []Advice {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Advice, 0, len(s.byKey))
	for _, a := range s.byKey {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// Save writes the store out, through a temporary file and a rename so a
// reader never sees a half-written list.
func (s *Store) Save() error {
	if s.path == "" {
		return nil
	}
	list := s.List()
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("creating the advice directory: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", s.path, err)
	}
	return nil
}

// ReadAdvice loads a store's file without starting anything, for the CLI.
func ReadAdvice(path string) ([]Advice, error) {
	s := NewStore(path)
	if err := s.Load(); err != nil {
		return nil, err
	}
	return s.List(), nil
}
