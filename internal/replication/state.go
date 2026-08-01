package replication

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is what gora remembers about the cluster between restarts.
//
// Only one thing has to survive: which node is the primary. After a
// promotion the configuration file still names the old one, and a gora that
// came back believing it would try to write to a server that is now a
// replica — refused, correctly, and the cluster would sit there refusing
// writes until somebody edited a file.
type State struct {
	Primary    string    `json:"primary"`
	PromotedAt time.Time `json:"promoted_at,omitempty"`
	// Reason records why the primary changed, for whoever reads this file
	// after the incident.
	Reason string `json:"reason,omitempty"`

	path string
}

// NewState opens the state at path. An empty path keeps it in memory, which
// means a promotion lasts until gora restarts.
func NewState(path string) *State { return &State{path: path} }

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
	s.Primary = loaded.Primary
	s.PromotedAt = loaded.PromotedAt
	s.Reason = loaded.Reason
	return nil
}

// Save records the current primary, through a temporary file and a rename
// so a reader never sees a half-written state.
func (s *State) Save(primary, reason string) error {
	s.Primary = primary
	s.PromotedAt = time.Now()
	s.Reason = reason
	if s.path == "" {
		return nil
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
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
