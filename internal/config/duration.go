package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a YAML string carrying a
// unit ("30s", "5m") or from the bare integer 0, which disables whatever it
// configures. A unit-less non-zero number is rejected with a message that
// says what to write instead: yaml.v3 would otherwise fail with a type
// error nobody can act on.
type Duration time.Duration

// Std returns the standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String makes Duration printable in logs and in the --check-config summary.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML accepts "30s"-style strings and the bare 0.
//
// The decision is made on the YAML tag rather than by trying to decode a
// string first: yaml.v3 happily decodes the scalar 30 into a string, which
// would send a unit-less number down the "invalid duration" path and hide
// the one message that tells the user what to write instead.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	switch node.Tag {
	case "!!str":
		var s string
		if err := node.Decode(&s); err != nil {
			return fmt.Errorf(`duration must be a value with a unit like "30s", or 0`)
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil

	case "!!int":
		var n int64
		if err := node.Decode(&n); err != nil {
			return fmt.Errorf(`duration must be a value with a unit like "30s", or 0`)
		}
		if n != 0 {
			return fmt.Errorf(`duration %d needs a unit: write "%ds" (or 0 to disable)`, n, n)
		}
		*d = 0
		return nil

	default:
		return fmt.Errorf(`duration must be a value with a unit like "30s", or 0`)
	}
}
