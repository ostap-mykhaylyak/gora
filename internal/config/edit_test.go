package config

import (
	"strings"
	"testing"
)

const sample = `# gora configuration
# Written by --init.

listen:
  # Address WordPress connects to.
  address: "0.0.0.0:3306"
  max_connections: 0

backend:
  address: "10.0.0.10:3306"
  username: "wordpress"
  password: "change-me"

cache:
  enabled: true
  default_ttl: 5m   # safety expiry
`

// The whole point: everything not being changed comes back exactly as it
// went in, comments and blank lines included.
func TestSetValueLeavesTheRestAlone(t *testing.T) {
	out, err := SetValue([]byte(sample), "cache.default_ttl", "10m")
	if err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	got := string(out)
	if !strings.Contains(got, "default_ttl: 10m") {
		t.Fatalf("the value was not changed:\n%s", got)
	}
	// The comment stays in the column it was in: a file where the
	// explanations line up is a file somebody lined up.
	if want, have := commentColumn(sample), commentColumn(got); want != have {
		t.Fatalf("the comment moved from column %d to %d:\n%s", want, have, got)
	}
	for _, keep := range []string{
		"# gora configuration",
		"  # Address WordPress connects to.",
		`  address: "0.0.0.0:3306"`,
		"\n\nbackend:",
	} {
		if !strings.Contains(got, keep) {
			t.Fatalf("the edit lost %q:\n%s", keep, got)
		}
	}
	if lines := strings.Count(got, "\n"); lines != strings.Count(sample, "\n") {
		t.Fatalf("the file changed length: %d lines, was %d", lines, strings.Count(sample, "\n"))
	}
}

// commentColumn returns where the comment on the default_ttl line starts.
func commentColumn(s string) int {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "default_ttl") {
			return strings.Index(line, "#")
		}
	}
	return -1
}

func TestSetValueQuotesWhatNeedsIt(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{"10m", "10m"},
		{"true", "true"},
		{"100", "100"},
		{"/var/log/gora", "/var/log/gora"},
		{"10.0.0.11:3306", `"10.0.0.11:3306"`}, // a colon needs quoting
		{"p@ss w0rd#1", `"p@ss w0rd#1"`},       // spaces and a comment character
		{`say "hi"`, `"say \"hi\""`},           // quotes are escaped
		{"", `""`},                             // and empty is a value too
	}
	for _, tt := range tests {
		out, err := SetValue([]byte(sample), "backend.password", tt.value)
		if err != nil {
			t.Fatalf("SetValue(%q): %v", tt.value, err)
		}
		want := "  password: " + tt.want
		if !strings.Contains(string(out), want) {
			t.Errorf("SetValue(%q) did not produce %q:\n%s", tt.value, want, out)
		}
	}
}

// A password with a # in it must not turn into a comment, which is the
// failure that would look like the password simply being wrong.
func TestSetValueSurvivesReparsing(t *testing.T) {
	out, err := SetValue([]byte(sample), "backend.password", "p@ss #1 word")
	if err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	cfg, err := Parse(out, "test")
	if err != nil {
		t.Fatalf("the edited file does not parse: %v", err)
	}
	if cfg.Backend.Password != "p@ss #1 word" {
		t.Fatalf("password came back as %q", cfg.Backend.Password)
	}
}

// A setting the file does not mention is created, in its section.
func TestSetValueCreatesAMissingKey(t *testing.T) {
	out, err := SetValue([]byte(sample), "cache.max_entries", "5000")
	if err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "  default_ttl: 5m   # safety expiry\n  max_entries: 5000") {
		t.Fatalf("the new key did not land in its section:\n%s", got)
	}
}

// And so is a whole section.
func TestSetValueCreatesAMissingSection(t *testing.T) {
	out, err := SetValue([]byte(sample), "profiling.enabled", "true")
	if err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "profiling:\n  enabled: true") {
		t.Fatalf("the new section is missing:\n%s", got)
	}
	if !strings.Contains(got, `password: "change-me"`) {
		t.Fatalf("the rest of the file was disturbed:\n%s", got)
	}
}

func TestSetValueNested(t *testing.T) {
	out, err := SetValue([]byte(sample), "listen.tls.cert", "/etc/gora/server.crt")
	if err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	if !strings.Contains(string(out), "  tls:\n    cert: /etc/gora/server.crt") {
		t.Fatalf("the nested key was not created:\n%s", out)
	}
}

func TestDeleteValue(t *testing.T) {
	out, err := DeleteValue([]byte(sample), "cache.default_ttl")
	if err != nil {
		t.Fatalf("DeleteValue: %v", err)
	}
	if strings.Contains(string(out), "default_ttl") {
		t.Fatalf("the setting is still there:\n%s", out)
	}
	if !strings.Contains(string(out), "enabled: true") {
		t.Fatalf("its neighbour went with it:\n%s", out)
	}

	// Removing something that is not there is not an error: the point of
	// removing it is that it should not be there.
	if _, err := DeleteValue([]byte(sample), "cache.max_bytes"); err != nil {
		t.Fatalf("DeleteValue on a missing key: %v", err)
	}
}

func TestAddAndRemoveUser(t *testing.T) {
	out, err := AddUser([]byte(sample), "wordpress", "secret")
	if err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	cfg, err := Parse(out, "test")
	if err != nil {
		t.Fatalf("the edited file does not parse: %v", err)
	}
	if len(cfg.Users) != 1 || cfg.Users[0].Username != "wordpress" || cfg.Users[0].Password != "secret" {
		t.Fatalf("users = %+v", cfg.Users)
	}

	// A second one joins the list rather than replacing it.
	out, err = AddUser(out, "reporting", "other")
	if err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	cfg, err = Parse(out, "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Users) != 2 {
		t.Fatalf("users = %+v, want two", cfg.Users)
	}

	out, err = RemoveUser(out, "wordpress")
	if err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	cfg, err = Parse(out, "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Users) != 1 || cfg.Users[0].Username != "reporting" {
		t.Fatalf("users = %+v, want only the other one", cfg.Users)
	}
}

// The catalogue and the file are built from the same tags, so every setting
// the parser understands is one the command line can reach.
func TestSettingsCoverTheConfiguration(t *testing.T) {
	cfg := Default()
	settings := Settings(cfg)

	byPath := map[string]Setting{}
	for _, s := range settings {
		byPath[s.Path] = s
	}
	for _, path := range []string{
		"listen.address", "listen.tls.cert",
		"backend.address", "backend.username", "backend.connect_timeout", "backend.tls.enabled",
		"pool.max_open", "pool.multiplexing",
		"cache.enabled", "cache.default_ttl",
		"routing.sticky_after_write",
		"replication.failover",
		"cluster.state_file",
		"profiling.enabled", "status.socket", "log.level",
	} {
		if _, ok := byPath[path]; !ok {
			t.Errorf("setting %q is in the configuration but not in the catalogue", path)
		}
	}

	// The ones a running gora applies are marked, and the ones it cannot
	// are not.
	if !byPath["cache.default_ttl"].Hot {
		t.Error("cache.default_ttl is applied on reload but not marked hot")
	}
	if byPath["listen.address"].Hot {
		t.Error("listen.address cannot be changed without rebinding the listener, but is marked hot")
	}
	if !byPath["backend.replicas"].List {
		t.Error("backend.replicas is a list and should say so")
	}
}

func TestCheckValue(t *testing.T) {
	cfg := Default()
	number, _ := Lookup(cfg, "pool.max_open")
	flag, _ := Lookup(cfg, "cache.enabled")
	duration, _ := Lookup(cfg, "cache.default_ttl")

	if err := CheckValue(number, "not a number"); err == nil {
		t.Error("a number accepted text")
	}
	if err := CheckValue(flag, "yes"); err == nil {
		t.Error("a true/false accepted yes")
	}
	if err := CheckValue(duration, "10"); err == nil {
		t.Error("a duration accepted a unit-less number")
	}
	if err := CheckValue(duration, "10m"); err != nil {
		t.Errorf("a duration refused 10m: %v", err)
	}
}
