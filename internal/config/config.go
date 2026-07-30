// Package config loads and validates gora's YAML configuration.
//
// The whole configuration lives in one file (plus the conf.d drop-ins that
// later milestones read): gora has no SQL admin interface and no runtime
// mutation of its own settings, so the file on disk is always the truth.
package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the root of config.yaml.
type Config struct {
	Listen  Listen  `yaml:"listen"`
	Backend Backend `yaml:"backend"`
	Status  Status  `yaml:"status"`
	Log     Log     `yaml:"log"`
}

// Listen configures the client-facing listener: the address WordPress points
// DB_HOST at.
type Listen struct {
	Address string `yaml:"address"`
	// MaxConnections caps concurrent client connections (0 = unlimited).
	// Connections beyond the cap are refused immediately rather than piling
	// up behind a saturated backend.
	MaxConnections int `yaml:"max_connections"`
}

// Backend is the MySQL server gora forwards to. Username and password are
// gora's own credentials: backend connections belong to gora, not to the
// clients that authenticate against it.
type Backend struct {
	Address  string `yaml:"address"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Status exposes runtime state to `gora status` over a local unix socket.
// An empty socket disables it.
type Status struct {
	Socket string `yaml:"socket"`
}

// Log controls logging output.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	// Path is the directory gora.log is written to. The special values
	// "stdout" and "stderr" log to the console instead, which is what you
	// want in a container.
	Path string `yaml:"path"`
}

// Console reports whether logs go to the console instead of a file.
func (l Log) Console() bool { return l.Path == "stdout" || l.Path == "stderr" }

// Default returns the configuration gora runs with before the file is read.
// Every field the file omits keeps the value set here.
func Default() Config {
	return Config{
		Listen: Listen{Address: "0.0.0.0:3306"},
		Status: Status{Socket: "/run/gora/status.sock"},
		Log:    Log{Level: "info", Format: "text", Path: "/var/log/gora"},
	}
}

// Load reads, parses and validates the YAML file at path. Unknown keys are
// an error: a typo in a setting must not silently leave the default in
// place, because the symptom would show up months later as "gora ignores
// this option".
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading config: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the configuration for consistency. Errors name the exact
// YAML path so the fix is obvious without reading the documentation.
func (c *Config) Validate() error {
	if err := validAddress("listen.address", c.Listen.Address); err != nil {
		return err
	}
	if c.Listen.MaxConnections < 0 {
		return fmt.Errorf("listen.max_connections must be >= 0 (0 = unlimited)")
	}

	if c.Backend.Address == "" {
		return fmt.Errorf("backend.address is required")
	}
	if err := validAddress("backend.address", c.Backend.Address); err != nil {
		return err
	}
	if c.Backend.Username == "" {
		return fmt.Errorf("backend.username is required")
	}

	// Deliberately not filepath.IsAbs: gora runs on Linux, and the check
	// must give the same answer when the tests run on a Windows development
	// machine, where "/run/gora/status.sock" is not an absolute path.
	if c.Status.Socket != "" && !strings.HasPrefix(c.Status.Socket, "/") {
		return fmt.Errorf("status.socket %q must be an absolute path (empty disables it)", c.Status.Socket)
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q: must be debug, info, warn or error", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format %q: must be text or json", c.Log.Format)
	}
	if c.Log.Path == "" {
		return fmt.Errorf(`log.path is required (a directory, or "stdout"/"stderr" for the console)`)
	}
	return nil
}

// validAddress accepts host:port, including the ":3306" and "0.0.0.0:3306"
// forms. A bare hostname is the most common mistake, so it gets its own
// message.
func validAddress(field, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		if !strings.Contains(addr, ":") {
			return fmt.Errorf("%s %q: the port is missing, write %q", field, addr, addr+":3306")
		}
		return fmt.Errorf("%s %q: %w", field, addr, err)
	}
	if port == "" {
		return fmt.Errorf("%s %q: the port is missing", field, addr)
	}
	_ = host
	return nil
}
