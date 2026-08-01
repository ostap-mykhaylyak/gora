package topology

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// State is what gora remembers about the cluster between restarts: who the
// primary is, and which nodes have been added or removed while it was
// running.
//
// Membership is kept as two lists rather than one, so that both ways of
// changing it keep working. The effective set is
//
//	(backend.replicas ∪ Added) − Removed
//
// which means a node added with `gora --add-replica` survives an edit of
// config.yaml, a node removed that way stays removed even though the
// configuration still lists it, and putting the node into config.yaml
// afterwards — the tidy thing to do — changes nothing.
type State struct {
	Primary   string   `json:"primary,omitempty"`
	Added     []string `json:"added,omitempty"`
	Removed   []string `json:"removed,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	// Reason records what last changed the state, for whoever reads this
	// file after the incident.
	Reason string `json:"reason,omitempty"`

	mu   sync.Mutex
	path string
}

// NewState opens the state at path. An empty path keeps it in memory, which
// means a promotion or a node added at runtime lasts until gora restarts.
func NewState(path string) *State { return &State{path: path} }

// Path returns the file the state is kept in, empty when it is not kept.
func (s *State) Path() string { return s.path }

// Load reads the file, if there is one.
func (s *State) Load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", s.path, err)
	}

	var loaded State
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("parsing %s: %w", s.path, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.Primary = loaded.Primary
	s.Added = loaded.Added
	s.Removed = loaded.Removed
	s.UpdatedAt = loaded.UpdatedAt
	s.Reason = loaded.Reason
	return nil
}

// SetPrimary records which node takes the writes.
func (s *State) SetPrimary(addr, reason string) error {
	s.mu.Lock()
	s.Primary = addr
	s.mu.Unlock()
	return s.save(reason)
}

// AddNode records a node that is part of the cluster from now on.
func (s *State) AddNode(addr, reason string) error {
	s.mu.Lock()
	s.Removed = without(s.Removed, addr)
	if !contains(s.Added, addr) {
		s.Added = append(s.Added, addr)
		sort.Strings(s.Added)
	}
	s.mu.Unlock()
	return s.save(reason)
}

// RemoveNode records a node that is no longer part of the cluster, whether
// or not the configuration still lists it.
func (s *State) RemoveNode(addr, reason string) error {
	s.mu.Lock()
	s.Added = without(s.Added, addr)
	if !contains(s.Removed, addr) {
		s.Removed = append(s.Removed, addr)
		sort.Strings(s.Removed)
	}
	s.mu.Unlock()
	return s.save(reason)
}

// Members applies the recorded changes to the configured replicas.
func (s *State) Members(configured []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(configured)+len(s.Added))
	seen := map[string]bool{}
	for _, addr := range append(append([]string(nil), configured...), s.Added...) {
		if seen[addr] || contains(s.Removed, addr) {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	return out
}

// PrimaryAddr returns the recorded primary, empty when none was recorded.
func (s *State) PrimaryAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Primary
}

// save writes the state out, through a temporary file and a rename so a
// reader never sees a half-written state.
func (s *State) save(reason string) error {
	s.mu.Lock()
	s.UpdatedAt = time.Now()
	s.Reason = reason
	data, err := json.MarshalIndent(s, "", "  ")
	path := s.path
	s.mu.Unlock()

	if err != nil {
		return err
	}
	if path == "" {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func without(list []string, s string) []string {
	out := list[:0]
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
