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
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of config.yaml.
type Config struct {
	Listen  Listen  `yaml:"listen"`
	Backend Backend `yaml:"backend"`
	Users   []User  `yaml:"users"`
	Pool    Pool    `yaml:"pool"`
	Cache   Cache   `yaml:"cache"`

	Profiling Profiling `yaml:"profiling"`
	Status    Status    `yaml:"status"`
	Log       Log       `yaml:"log"`
}

// Listen configures the client-facing listener: the address WordPress points
// DB_HOST at.
type Listen struct {
	Address string `yaml:"address"`
	// MaxConnections caps concurrent client connections (0 = unlimited).
	// Connections beyond the cap are refused immediately rather than piling
	// up behind a saturated backend.
	MaxConnections int `yaml:"max_connections"`
	// DrainTimeout is how long a shutdown waits for statements already
	// running before it closes client connections anyway.
	DrainTimeout Duration `yaml:"drain_timeout"`
	// TLS encrypts client connections when both files are set.
	TLS ListenTLS `yaml:"tls"`
}

// ListenTLS holds the server certificate presented to clients.
type ListenTLS struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// Enabled reports whether client-facing TLS is configured.
func (t ListenTLS) Enabled() bool { return t.Cert != "" || t.Key != "" }

// Backend is the MySQL server gora forwards to. Username and password are
// gora's own credentials: backend connections belong to gora, not to the
// clients that authenticate against it.
type Backend struct {
	Address  string `yaml:"address"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// ConnectTimeout bounds a single dial to the backend.
	ConnectTimeout Duration   `yaml:"connect_timeout"`
	TLS            BackendTLS `yaml:"tls"`
}

// BackendTLS encrypts connections toward MySQL.
type BackendTLS struct {
	Enabled bool `yaml:"enabled"`
	// CA is an optional certificate authority (PEM); empty uses the system
	// roots.
	CA string `yaml:"ca"`
	// SkipVerify accepts any server certificate: encryption without
	// authentication, for self-signed setups.
	SkipVerify bool `yaml:"skip_verify"`
}

// User is an account clients authenticate with against gora. MySQL never
// sees these credentials. When no user is configured, clients authenticate
// with the backend credentials, so the simple setup needs them only once.
type User struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Pool controls the backend connection pool.
type Pool struct {
	// MaxOpen caps the total number of backend connections (leased + idle).
	MaxOpen int `yaml:"max_open"`
	// MaxIdle is how many connections stay parked after a session releases
	// them the slow way (with a reset roundtrip).
	MaxIdle int `yaml:"max_idle"`
	// MinIdle is how many connections gora opens at startup and keeps
	// available, so the first burst of traffic does not pay for handshakes.
	MinIdle int `yaml:"min_idle"`
	// PingInterval is how often idle connections receive a COM_PING, both
	// while parked and while attached to an inactive client, so MySQL's
	// wait_timeout never closes them.
	PingInterval Duration `yaml:"ping_interval"`
	// IdleTimeout closes pooled connections unused for this long (0 = never),
	// down to MinIdle.
	IdleTimeout Duration `yaml:"idle_timeout"`
	// MaxLifetime retires a backend connection this long after it was
	// opened (0 = never). It is what makes a MySQL restart, a failover or a
	// rotated certificate heal on their own instead of leaving gora holding
	// connections to a server that no longer exists.
	MaxLifetime Duration `yaml:"max_lifetime"`
	// AcquireTimeout bounds how long a client waits for a connection when
	// the pool is exhausted.
	AcquireTimeout Duration `yaml:"acquire_timeout"`
	// Multiplexing returns the backend connection to the pool between
	// queries whenever the session state allows it, so many client sessions
	// share few backend connections. Sessions holding state (transactions,
	// temporary tables, locks, prepared statements, user variables) keep
	// their connection automatically.
	Multiplexing bool `yaml:"multiplexing"`
	// MaxQueryTime kills backend queries running longer than this
	// (0 disables it), so one runaway statement cannot hold a pooled
	// connection hostage.
	MaxQueryTime Duration `yaml:"max_query_time"`
	// Breaker protects against an unreachable backend.
	Breaker Breaker `yaml:"breaker"`
}

// Breaker is the circuit breaker: after Failures consecutive connection
// failures gora fails fast instead of letting every PHP worker wait for its
// own timeout, and probes the backend until it recovers.
type Breaker struct {
	// Failures is how many consecutive dial failures open the circuit
	// (0 disables the breaker).
	Failures int `yaml:"failures"`
	// ProbeInterval is how often the backend is probed while it is open.
	ProbeInterval Duration `yaml:"probe_interval"`
}

// AutoPrefix asks gora to discover the WordPress table prefix from the
// database instead of being told.
const AutoPrefix = "auto"

// PrefixPlaceholder is what a conf.d drop-in writes instead of the table
// prefix, so the same file works on any installation.
const PrefixPlaceholder = "{prefix}"

// ExpandPrefix substitutes the table prefix into a drop-in expression.
//
// The cache discovers the prefix at runtime and rebinds its own rules when
// it does; the traffic rules are compiled once at startup, so a placeholder
// there needs a prefix that is known by then. Saying so is better than
// silently matching the literal text "{prefix}postmeta", which would look
// like a rule that simply never fires.
func ExpandPrefix(expr, prefix string) (string, error) {
	if !strings.Contains(expr, PrefixPlaceholder) {
		return expr, nil
	}
	if prefix == AutoPrefix || prefix == "" {
		return "", fmt.Errorf("%s needs a known table prefix: set cache.table_prefix instead of %q",
			PrefixPlaceholder, AutoPrefix)
	}
	return strings.ReplaceAll(expr, PrefixPlaceholder, prefix), nil
}

// Cache controls the WordPress-aware query cache.
type Cache struct {
	Enabled bool `yaml:"enabled"`
	// TablePrefix is WordPress's $table_prefix, or "auto" to detect it from
	// the first database gora sees. Installations that put several
	// prefixes behind one gora should name theirs explicitly.
	TablePrefix string `yaml:"table_prefix"`
	// AutoloadOptions caches the autoloaded options query.
	AutoloadOptions bool `yaml:"autoload_options"`
	// Transients caches transient reads from the options table.
	Transients bool `yaml:"transients"`
	// DefaultTTL is the safety expiry for cached entries; write-driven
	// invalidation is the mechanism that actually keeps them correct.
	DefaultTTL Duration `yaml:"default_ttl"`
	// MaxEntries bounds how many result sets are held.
	MaxEntries int `yaml:"max_entries"`
	// MaxBytes bounds how much memory they take (0 = unbounded). Entry
	// counts alone do not: a thousand small rows and a thousand large ones
	// are the same number and very different amounts of RAM.
	MaxBytes int `yaml:"max_bytes"`
	// MaxResultBytes skips caching results larger than this.
	MaxResultBytes int `yaml:"max_result_bytes"`
	// RulesDir holds the conf.d drop-ins. Empty means "conf.d next to the
	// configuration file".
	RulesDir string `yaml:"rules_dir"`
	// Warmup repopulates the autoloaded options snapshot in the background
	// right after a write invalidates it.
	Warmup bool `yaml:"warmup"`
}

// Profiling controls the query statistics, the slow statement log and the
// advice gora derives from them. All of it is off by default: it costs a
// fingerprint per statement and an EXPLAIN per report, which is little, but
// nothing is the right default for something you did not ask for.
type Profiling struct {
	Enabled bool `yaml:"enabled"`
	// SlowQuery logs any statement slower than this immediately
	// (0 disables it).
	SlowQuery Duration `yaml:"slow_query"`
	// ReportInterval is how often the aggregated report is logged. Each
	// report describes its own interval.
	ReportInterval Duration `yaml:"report_interval"`
	// TopQueries is how many statements the report details, heaviest first.
	TopQueries int `yaml:"top_queries"`
	// SuggestIndexes runs EXPLAIN on the heaviest statements and suggests
	// what to add, with the ALTER TABLE ready to run.
	SuggestIndexes bool `yaml:"suggest_indexes"`
	// SuggestRewrites scans statements for known antipatterns.
	SuggestRewrites bool `yaml:"suggest_rewrites"`
	// AdviceFile is where suggestions are kept, so they survive a restart
	// and can be read with `gora --advice`. Empty keeps them in memory.
	AdviceFile string `yaml:"advice_file"`
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
		Listen: Listen{
			Address:      "0.0.0.0:3306",
			DrainTimeout: Duration(10 * time.Second),
		},
		Backend: Backend{
			ConnectTimeout: Duration(5 * time.Second),
		},
		Pool: Pool{
			MaxOpen:        100,
			MaxIdle:        10,
			MinIdle:        0,
			PingInterval:   Duration(30 * time.Second),
			IdleTimeout:    Duration(5 * time.Minute),
			MaxLifetime:    Duration(time.Hour),
			AcquireTimeout: Duration(5 * time.Second),
			Multiplexing:   true,
			Breaker: Breaker{
				Failures:      3,
				ProbeInterval: Duration(2 * time.Second),
			},
		},
		Cache: Cache{
			Enabled:         true,
			TablePrefix:     AutoPrefix,
			AutoloadOptions: true,
			Transients:      true,
			DefaultTTL:      Duration(5 * time.Minute),
			MaxEntries:      10000,
			MaxBytes:        256 << 20, // 256 MiB
			MaxResultBytes:  1 << 20,   // 1 MiB
			Warmup:          true,
		},
		Profiling: Profiling{
			SlowQuery:       Duration(500 * time.Millisecond),
			ReportInterval:  Duration(10 * time.Minute),
			TopQueries:      20,
			SuggestIndexes:  true,
			SuggestRewrites: true,
			AdviceFile:      "/var/log/gora/advice.json",
		},
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

	// No users defined: clients authenticate with the backend credentials.
	if len(cfg.Users) == 0 {
		cfg.Users = []User{{Username: cfg.Backend.Username, Password: cfg.Backend.Password}}
	}

	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the configuration for consistency. Errors name the exact
// YAML path so the fix is obvious without reading the documentation.
func (c *Config) Validate() error {
	if err := c.validateListen(); err != nil {
		return err
	}
	if err := c.validateBackend(); err != nil {
		return err
	}
	if err := c.validatePool(); err != nil {
		return err
	}
	if err := c.validateCache(); err != nil {
		return err
	}
	if c.Profiling.Enabled {
		if c.Profiling.ReportInterval <= 0 {
			return fmt.Errorf("profiling.report_interval must be > 0")
		}
		if c.Profiling.TopQueries < 1 {
			return fmt.Errorf("profiling.top_queries must be >= 1")
		}
		if c.Profiling.SlowQuery < 0 {
			return fmt.Errorf("profiling.slow_query must be >= 0 (0 disables it)")
		}
		if c.Profiling.AdviceFile != "" && !strings.HasPrefix(c.Profiling.AdviceFile, "/") {
			return fmt.Errorf("profiling.advice_file %q must be an absolute path (empty keeps the advice in memory)",
				c.Profiling.AdviceFile)
		}
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

func (c *Config) validateListen() error {
	if err := validAddress("listen.address", c.Listen.Address); err != nil {
		return err
	}
	if c.Listen.MaxConnections < 0 {
		return fmt.Errorf("listen.max_connections must be >= 0 (0 = unlimited)")
	}
	if c.Listen.DrainTimeout < 0 {
		return fmt.Errorf("listen.drain_timeout must be >= 0 (0 closes clients immediately)")
	}
	if c.Listen.TLS.Enabled() && (c.Listen.TLS.Cert == "" || c.Listen.TLS.Key == "") {
		return fmt.Errorf("listen.tls needs both cert and key")
	}
	return nil
}

func (c *Config) validateBackend() error {
	if c.Backend.Address == "" {
		return fmt.Errorf("backend.address is required")
	}
	if err := validAddress("backend.address", c.Backend.Address); err != nil {
		return err
	}
	if c.Backend.Username == "" {
		return fmt.Errorf("backend.username is required")
	}
	if c.Backend.ConnectTimeout <= 0 {
		return fmt.Errorf("backend.connect_timeout must be > 0")
	}
	if !c.Backend.TLS.Enabled && (c.Backend.TLS.CA != "" || c.Backend.TLS.SkipVerify) {
		return fmt.Errorf("backend.tls is configured but backend.tls.enabled is false")
	}

	if len(c.Users) == 0 {
		return fmt.Errorf("at least one user is required (omit users entirely to reuse the backend credentials)")
	}
	for i, u := range c.Users {
		if u.Username == "" {
			return fmt.Errorf("users[%d].username is required", i)
		}
	}
	return nil
}

func (c *Config) validatePool() error {
	p := c.Pool
	if p.MaxOpen < 1 {
		return fmt.Errorf("pool.max_open must be >= 1")
	}
	if p.MaxIdle < 0 || p.MaxIdle > p.MaxOpen {
		return fmt.Errorf("pool.max_idle must be between 0 and pool.max_open (%d)", p.MaxOpen)
	}
	if p.MinIdle < 0 || p.MinIdle > p.MaxOpen {
		return fmt.Errorf("pool.min_idle must be between 0 and pool.max_open (%d)", p.MaxOpen)
	}
	if p.PingInterval <= 0 {
		return fmt.Errorf("pool.ping_interval must be > 0")
	}
	if p.IdleTimeout < 0 {
		return fmt.Errorf("pool.idle_timeout must be >= 0 (0 disables it)")
	}
	if p.MaxLifetime < 0 {
		return fmt.Errorf("pool.max_lifetime must be >= 0 (0 disables it)")
	}
	if p.MaxLifetime > 0 && p.MaxLifetime <= p.PingInterval {
		return fmt.Errorf("pool.max_lifetime (%s) must be longer than pool.ping_interval (%s), otherwise connections are retired as fast as they are opened",
			p.MaxLifetime, p.PingInterval)
	}
	if p.AcquireTimeout <= 0 {
		return fmt.Errorf("pool.acquire_timeout must be > 0")
	}
	if p.MaxQueryTime < 0 {
		return fmt.Errorf("pool.max_query_time must be >= 0 (0 disables it)")
	}
	if p.Breaker.Failures < 0 {
		return fmt.Errorf("pool.breaker.failures must be >= 0 (0 disables it)")
	}
	if p.Breaker.Failures > 0 && p.Breaker.ProbeInterval <= 0 {
		return fmt.Errorf("pool.breaker.probe_interval must be > 0")
	}
	return nil
}

// prefixRe is what a table prefix may look like: MySQL identifiers, no
// quoting games. It also catches the trailing space nobody notices.
var prefixRe = regexp.MustCompile(`^[A-Za-z0-9_$]+$`)

func (c *Config) validateCache() error {
	if !c.Cache.Enabled {
		return nil
	}
	cache := c.Cache
	if cache.TablePrefix == "" {
		return fmt.Errorf(`cache.table_prefix is required when the cache is enabled (use "auto" to detect it)`)
	}
	if cache.TablePrefix != AutoPrefix && !prefixRe.MatchString(cache.TablePrefix) {
		return fmt.Errorf("cache.table_prefix %q is not a valid table name prefix", cache.TablePrefix)
	}
	if cache.DefaultTTL <= 0 {
		return fmt.Errorf("cache.default_ttl must be > 0")
	}
	if cache.MaxEntries < 1 {
		return fmt.Errorf("cache.max_entries must be >= 1")
	}
	if cache.MaxResultBytes < 1 {
		return fmt.Errorf("cache.max_result_bytes must be >= 1")
	}
	if cache.MaxBytes < 0 {
		return fmt.Errorf("cache.max_bytes must be >= 0 (0 = unbounded)")
	}
	if cache.MaxBytes > 0 && cache.MaxBytes < cache.MaxResultBytes {
		return fmt.Errorf("cache.max_bytes (%d) is smaller than cache.max_result_bytes (%d): nothing would ever stay cached",
			cache.MaxBytes, cache.MaxResultBytes)
	}
	return nil
}

// validAddress accepts host:port, including the ":3306" and "0.0.0.0:3306"
// forms. A bare hostname is the most common mistake, so it gets its own
// message.
func validAddress(field, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		if !strings.Contains(addr, ":") {
			return fmt.Errorf("%s %q: the port is missing, write %q", field, addr, addr+":3306")
		}
		return fmt.Errorf("%s %q: %w", field, addr, err)
	}
	if port == "" {
		return fmt.Errorf("%s %q: the port is missing", field, addr)
	}
	return nil
}
