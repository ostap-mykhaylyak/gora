package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func yamlUnmarshal(body string, out any) error {
	return yaml.Unmarshal([]byte(body), out)
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `
backend:
  address: "127.0.0.1:3306"
  username: "wordpress"
  password: "secret"
`

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if want := "0.0.0.0:3306"; cfg.Listen.Address != want {
		t.Errorf("listen.address = %q, want the default %q", cfg.Listen.Address, want)
	}
	if want := "/var/log/gora"; cfg.Log.Path != want {
		t.Errorf("log.path = %q, want the default %q", cfg.Log.Path, want)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q, want the default info", cfg.Log.Level)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := Load(write(t, minimal+"\ncach:\n  enabled: true\n"))
	if err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if !strings.Contains(err.Error(), "cach") {
		t.Fatalf("the error does not name the unknown key: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("a missing file was accepted")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // substring the error must contain
	}{
		{"backend missing", "listen:\n  address: \"0.0.0.0:3306\"\n", "backend.address"},
		{"backend without a user", "backend:\n  address: \"127.0.0.1:3306\"\n", "backend.username"},
		{"listen without a port", minimal + "\nlisten:\n  address: \"0.0.0.0\"\n", "port"},
		{"negative connection cap", minimal + "\nlisten:\n  max_connections: -1\n", "max_connections"},
		{"relative status socket", minimal + "\nstatus:\n  socket: \"gora.sock\"\n", "absolute"},
		{"unknown log level", minimal + "\nlog:\n  level: \"verbose\"\n", "log.level"},
		{"unknown log format", minimal + "\nlog:\n  format: \"xml\"\n", "log.format"},
		{"empty log path", minimal + "\nlog:\n  path: \"\"\n", "log.path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(t, tt.body))
			if err == nil {
				t.Fatal("an invalid configuration was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestStatusSocketCanBeDisabled(t *testing.T) {
	cfg, err := Load(write(t, minimal+"\nstatus:\n  socket: \"\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Status.Socket != "" {
		t.Fatalf("status.socket = %q, want empty", cfg.Status.Socket)
	}
}

// Duration exists because yaml.v3 cannot tell "5m" from a number and
// rejects a bare 0 for a time.Duration field with an unreadable message.
func TestDuration(t *testing.T) {
	type holder struct {
		D Duration `yaml:"d"`
	}

	tests := []struct {
		yaml    string
		want    time.Duration
		wantErr string
	}{
		{yaml: `d: 30s`, want: 30 * time.Second},
		{yaml: `d: 5m`, want: 5 * time.Minute},
		{yaml: `d: 0`, want: 0},
		{yaml: `d: 30`, wantErr: "needs a unit"},
		{yaml: `d: "abc"`, wantErr: "invalid duration"},
	}

	for _, tt := range tests {
		t.Run(tt.yaml, func(t *testing.T) {
			var h holder
			err := yamlUnmarshal(tt.yaml, &h)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if h.D.Std() != tt.want {
				t.Fatalf("d = %v, want %v", h.D.Std(), tt.want)
			}
		})
	}
}
