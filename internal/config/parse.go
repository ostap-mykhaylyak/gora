package config

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Parse reads a configuration from memory. It is what Load is made of, and
// what lets an edit be validated before it reaches the disk: a file gora
// wrote is a file gora can start with.
func Parse(data []byte, name string) (Config, error) {
	cfg := Default()

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", name, err)
	}

	// No users defined: clients authenticate with the backend credentials.
	if len(cfg.Users) == 0 {
		cfg.Users = []User{{Username: cfg.Backend.Username, Password: cfg.Backend.Password}}
	}

	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid config %s: %w", name, err)
	}
	return cfg, nil
}

// unmarshalYAML is the one place outside Parse that decodes YAML, for
// checking a single value against the type it will have to be.
func unmarshalYAML(body string, out any) error {
	return yaml.Unmarshal([]byte(body), out)
}
